package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiws/ant/internal/model"
)

// verbose 输出的有界预览，避免一次刷屏；完整内容在 artifact 与导出文件中。
const (
	verboseArgsBytes = 2 << 10
	verboseArgsLines = 40
	verboseOutBytes  = 4 << 10
	verboseOutLines  = 80
	verboseIndent    = "  "
	tracePromptHashN = 12
)

// traceRenderer 是 Renderer 的可选扩展；只有 textRenderer 实现。
type traceRenderer interface {
	NodeStart(NodeStartInfo)
	ModelRequest(ModelRequestInfo)
	ModelResponse(ModelResponseInfo)
	ToolOutput(ToolOutputInfo)
}

type NodeStartInfo struct {
	Node          model.NodeName
	Model         string
	Provider      string
	PromptVersion string
	PromptHash    string
	StableHash    string
	ToolsHash     string
	MessageCount  int
	ToolCount     int
	InputRef      string
}

type ModelRequestInfo struct {
	Node                model.NodeName
	Round               int
	Attempt             int
	MessageCount        int
	EstimatedInputToken int
	Tools               []string
	Stream              bool
}

type ModelResponseInfo struct {
	Node          model.NodeName
	Round         int
	Attempt       int
	FinishReason  string
	Usage         model.Usage
	Duration      time.Duration
	ToolCallCount int
	ErrorClass    string
	Code          string
	Message       string
}

type ToolOutputInfo struct {
	Tool       string
	OK         bool
	Status     string
	Content    []byte
	ContentRef string
	OutputRef  string
	Truncated  bool
}

// NodeStart 输出节点开始信息（verbose）。
func (r *textRenderer) NodeStart(info NodeStartInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := fmt.Sprintf("%s · model=%s · provider=%s · prompt=%s sha=%s · %d msg / %d tools",
		info.Node, info.Model, info.Provider,
		orDash(info.PromptVersion), shortHash(info.PromptHash),
		info.MessageCount, info.ToolCount)
	r.traceHeader("节点", colorToolBar, colorToolChip, line)
	if path := r.artifactPath(info.InputRef); path != "" {
		r.writeMutedLine("输入：" + path)
	}
}

// ModelRequest 输出模型请求摘要（verbose）。
func (r *textRenderer) ModelRequest(info ModelRequestInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	round := info.Round
	if round <= 0 {
		round = 1
	}
	line := fmt.Sprintf("请求 #%d · messages=%d · est=%s · tools=%d",
		round, info.MessageCount, formatCount(info.EstimatedInputToken), len(info.Tools))
	if info.Stream {
		line += " · stream"
	}
	r.traceHeader("请求", colorNoticeBar, colorNoticeChip, line)
}

// ModelResponse 输出模型响应摘要（verbose）。
func (r *textRenderer) ModelResponse(info ModelResponseInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	round := info.Round
	if round <= 0 {
		round = 1
	}
	if info.ErrorClass != "" || info.Code != "" || info.Message != "" {
		line := fmt.Sprintf("响应 #%d · 失败", round)
		if info.ErrorClass != "" {
			line += " " + info.ErrorClass
		}
		if info.Code != "" {
			line += "/" + info.Code
		}
		if info.Message != "" {
			line += "：" + oneLine(info.Message, 200)
		}
		r.traceHeader("响应", colorFailBar, colorFailChip, line)
		return
	}
	line := fmt.Sprintf("响应 #%d · finish=%s", round, orDash(info.FinishReason))
	if d := formatDuration(info.Duration); d != "" {
		line += " · " + d
	}
	line += fmt.Sprintf(" · in %s / out %s / cached %s",
		formatCount(info.Usage.Input), formatCount(info.Usage.Output), formatCount(info.Usage.Cached))
	if info.ToolCallCount > 0 {
		line += fmt.Sprintf(" · calls=%d", info.ToolCallCount)
	}
	r.traceHeader("响应", colorOKBar, colorOKChip, line)
}

// ToolOutput 输出工具结果预览（verbose）；完整内容由 artifact 路径指向。
func (r *textRenderer) ToolOutput(info ToolOutputInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body, clipped := formatToolContent(info.Content)
	if !r.rich {
		if !r.lineStart {
			fmt.Fprintln(r.out)
		}
		fmt.Fprintf(r.out, "[result] %s\n", info.Tool)
		r.writeIndented(body)
		r.writeResultRefs(info, clipped)
		r.lineStart = true
		return
	}
	r.closeThinking()
	r.flushMarkdown()
	r.beforeTool()
	label := "输出"
	bar, chip := colorOKBar, colorOKChip
	if !info.OK {
		label, bar, chip = "错误", colorFailBar, colorFailChip
	}
	fmt.Fprintf(r.out, "%s %s %s\n", colorBar(bar), colorChip(label, chip, colorOnChip), paint(info.Tool, "38;5;252"))
	r.writeIndented(body)
	r.writeResultRefs(info, clipped)
	r.outKind = blockTool
	r.outAny = true
	r.lineStart = true
}

func (r *textRenderer) writeResultRefs(info ToolOutputInfo, clipped bool) {
	var notes []string
	if clipped {
		notes = append(notes, "预览已截断")
	}
	if path := r.artifactPath(info.ContentRef); path != "" {
		notes = append(notes, "完整消息："+path)
	}
	if path := r.artifactPath(info.OutputRef); path != "" {
		notes = append(notes, "完整输出："+path)
	}
	if len(notes) == 0 {
		return
	}
	r.writeMutedLine("（" + strings.Join(notes, "；") + "）")
}

// traceHeader 输出一行带色块的 verbose 过程块（调用方必须持有锁）。
func (r *textRenderer) traceHeader(chip string, bar, chipColor int, text string) {
	r.closeThinking()
	r.flushMarkdown()
	r.beforeTool()
	if r.rich {
		fmt.Fprintf(r.out, "%s %s %s\n", colorBar(bar), colorChip(chip, chipColor, colorOnChip), paint(text, "38;5;252"))
	} else {
		if !r.lineStart {
			fmt.Fprintln(r.out)
		}
		fmt.Fprintf(r.out, "[%s] %s\n", plainTracePrefix(chip), text)
	}
	r.outKind = blockTool
	r.outAny = true
	r.lineStart = true
}

func plainTracePrefix(chip string) string {
	switch chip {
	case "节点":
		return "node"
	case "请求":
		return "req"
	case "响应":
		return "resp"
	default:
		return strings.ToLower(chip)
	}
}

func (r *textRenderer) writeMutedLine(text string) {
	if r.rich {
		fmt.Fprintln(r.out, paint(verboseIndent+text, "38;5;"+strconv.Itoa(colorMuted)))
		return
	}
	fmt.Fprintln(r.out, verboseIndent+text)
}

// writeToolArgs 在 verbose 下追加完整工具参数（有界）。
func (r *textRenderer) writeToolArgs(args []byte) {
	text, _ := formatJSONPreview(args, verboseArgsBytes, verboseArgsLines)
	if strings.TrimSpace(text) == "" {
		return
	}
	r.writeIndented(text)
}

// writeIndented 按行缩进输出任意文本；超长内容已在上游截断。
func (r *textRenderer) writeIndented(text string) {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return
	}
	for _, line := range strings.Split(text, "\n") {
		if r.rich {
			fmt.Fprintln(r.out, paint(verboseIndent+line, "38;5;"+strconv.Itoa(colorMuted)))
		} else {
			fmt.Fprintln(r.out, verboseIndent+line)
		}
	}
}

func (r *textRenderer) artifactPath(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if filepath.IsAbs(ref) {
		return ref
	}
	if r.sessionPath != "" {
		return filepath.Join(r.sessionPath, filepath.FromSlash(ref))
	}
	return ref
}

func shortHash(hash string) string {
	hash = strings.TrimSpace(strings.TrimPrefix(hash, "sha256:"))
	if hash == "" {
		return "-"
	}
	if len(hash) > tracePromptHashN {
		return hash[:tracePromptHashN] + "…"
	}
	return hash
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

type toolMessageEnvelope struct {
	OK        bool            `json:"ok"`
	Data      json.RawMessage `json:"data"`
	Error     *toolErrBody    `json:"error"`
	Truncated bool            `json:"truncated"`
	OutputRef string          `json:"outputRef"`
}

type toolErrBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

// formatToolContent 把 tool 消息 JSON 转成适合终端阅读的文本。
func formatToolContent(raw []byte) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "(无输出)", false
	}
	var env toolMessageEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return boundText(string(trimmed), verboseOutBytes, verboseOutLines)
	}
	if !env.OK && env.Error != nil {
		var b strings.Builder
		if env.Error.Code != "" {
			b.WriteString(env.Error.Code)
			b.WriteString(": ")
		}
		b.WriteString(env.Error.Message)
		if env.Error.Hint != "" {
			b.WriteString("\n提示：")
			b.WriteString(env.Error.Hint)
		}
		return boundText(strings.TrimSpace(b.String()), verboseOutBytes, verboseOutLines)
	}
	data := bytes.TrimSpace(env.Data)
	if len(data) == 0 || string(data) == "null" {
		return "(无数据)", false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return formatJSONPreview(data, verboseOutBytes, verboseOutLines)
	}
	if content, ok := rawString(obj["content"]); ok && content != "" {
		return boundText(content, verboseOutBytes, verboseOutLines)
	}
	stdout, hasOut := rawString(obj["stdout"])
	stderr, hasErr := rawString(obj["stderr"])
	if hasOut || hasErr {
		var b strings.Builder
		if hasOut && stdout != "" {
			b.WriteString(stdout)
		}
		if hasErr && stderr != "" {
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
				b.WriteString("\n")
			}
			b.WriteString("[stderr]\n")
			b.WriteString(stderr)
		}
		if v, ok := rawInt(obj["exitCode"]); ok && v != 0 {
			fmt.Fprintf(&b, "\n[exit] %d", v)
		}
		if b.Len() == 0 {
			b.WriteString("(无输出)")
		}
		return boundText(strings.TrimSpace(b.String()), verboseOutBytes, verboseOutLines)
	}
	return formatJSONPreview(data, verboseOutBytes, verboseOutLines)
}

func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return "", false
}

func rawInt(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, true
	}
	return 0, false
}

// formatJSONPreview 美化 JSON 并做有界截断；非法 JSON 退化为原文。
func formatJSONPreview(raw []byte, maxBytes, maxLines int) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", false
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, trimmed, "", "  "); err != nil {
		return boundText(string(trimmed), maxBytes, maxLines)
	}
	return boundText(buf.String(), maxBytes, maxLines)
}

// boundText 同时按字节与行数截断，保证 UTF-8 完整。
func boundText(s string, maxBytes, maxLines int) (string, bool) {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.TrimRight(s, "\n")
	clipped := false
	if maxLines > 0 {
		lines := strings.Split(s, "\n")
		if len(lines) > maxLines {
			s = strings.Join(lines[:maxLines], "\n")
			clipped = true
		}
	}
	if maxBytes > 0 && len(s) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
		clipped = true
	}
	if clipped {
		s += "\n…"
	}
	return s, clipped
}
