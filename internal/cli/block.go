package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// blockKind 用于在“过程块”和“结果块”之间插入空行，避免连续工具行被撑开。
type blockKind int

const (
	blockNone blockKind = iota
	blockAnswer
	blockTool
)

// 过程块配色：思考=紫、工具调用=蓝、成功=绿、失败=红、提示=灰。
// 只使用前景/背景色，不使用 SGR 5/6，因此不会闪烁。
const (
	ansiReset = "\x1b[0m"

	colorThinkingBar  = 97
	colorThinkingChip = 60
	colorToolBar      = 33
	colorToolChip     = 25
	colorOKBar        = 34
	colorOKChip       = 28
	colorFailBar      = 160
	colorFailChip     = 124
	colorNoticeBar    = 244
	colorNoticeChip   = 238
	colorOnChip       = 231
	colorMuted        = 245
)

func ansiSGR(params ...string) string {
	return "\x1b[" + strings.Join(params, ";") + "m"
}

func ansiFg(n int) string { return ansiSGR("38;5;" + strconv.Itoa(n)) }
func ansiBg(n int) string { return ansiSGR("48;5;" + strconv.Itoa(n)) }

// paint 用给定 SGR 参数包裹文本；调用方负责只在彩色模式下使用。
func paint(s string, params ...string) string {
	if s == "" {
		return ""
	}
	return ansiSGR(params...) + s + ansiReset
}

// colorBar 返回一个实心左色块，作为过程块的视觉锚点。
func colorBar(n int) string {
	return ansiFg(n) + "▌" + ansiReset
}

// colorChip 返回带背景色的短标签，例如 “思考”“工具”“完成”。
func colorChip(label string, bg, fg int) string {
	return ansiBg(bg) + ansiFg(fg) + " " + label + " " + ansiReset
}

// beforeAnswer 在结果块前留一个空行；Markdown 块之间也保持一个空行。
func (r *textRenderer) beforeAnswer() {
	if r.outAny {
		fmt.Fprintln(r.out)
	}
}

// beforeTool 在过程块与结果块切换时留空行；连续的工具调用/结果不空行。
func (r *textRenderer) beforeTool() {
	if r.outAny && r.outKind != blockTool {
		fmt.Fprintln(r.out)
	}
}

// beforeErrBlock 在 stderr 的思考/提示块之间留空行。
func (r *textRenderer) beforeErrBlock() {
	if r.errAny {
		fmt.Fprintln(r.err)
	}
}

// truncateRunes 按 rune 截断并补省略号，避免长命令撑乱块。
func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}
