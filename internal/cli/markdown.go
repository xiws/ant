package cli

import (
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/charmbracelet/glamour"
	"golang.org/x/term"
)

// newRichTextRenderer 在 TTY 下启用 Glamour Markdown 渲染与彩色过程块。
//
// 宽度取终端列数（留 2 列余量，避免末列自动换行产生空行）；style 为空时用
// Glamour 的 dark 主题。渲染器创建失败时由调用方回退到纯文本渲染。
func newRichTextRenderer(out, errOut io.Writer, width int, style string) (*textRenderer, error) {
	if width <= 0 {
		width = 80
	}
	if width < 40 {
		width = 40
	}
	if width > 120 {
		width = 120
	}
	wrap := width - 2
	if wrap < 24 {
		wrap = 24
	}
	style = strings.TrimSpace(style)
	if style == "" {
		style = "dark"
	}
	// auto 会触发 OSC 背景查询（最长 5s），这里不采用；按 COLORFGBG 选择即可。
	if strings.EqualFold(style, "auto") {
		style = "dark"
	}
	md, err := glamour.NewTermRenderer(
		glamour.WithStylePath(style),
		glamour.WithWordWrap(wrap),
	)
	if err != nil && style != "dark" {
		// GLAMOUR_STYLE 写错时不要让整个 Markdown 渲染失效。
		md, err = glamour.NewTermRenderer(
			glamour.WithStylePath("dark"),
			glamour.WithWordWrap(wrap),
		)
	}
	if err != nil {
		return nil, err
	}
	r := newTextRenderer(out, errOut)
	r.rich = true
	r.width = width
	r.md = md
	return r, nil
}

// terminalWidth 读取输出终端的列数；非 *os.File（测试/管道）返回 0。
func terminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil || width <= 0 {
		return 0
	}
	return width
}

// colorEnabled 决定是否启用 ANSI 颜色与 Markdown 渲染。
//
// ui.color=always/never 显式生效；auto（默认）要求 TTY、未设置 NO_COLOR、
// TERM != dumb。--no-color 在配置层已写成 never。
func colorEnabled(uiColor string, isTTY bool, env []string) bool {
	switch strings.ToLower(strings.TrimSpace(uiColor)) {
	case "always":
		return true
	case "never":
		return false
	}
	if !isTTY {
		return false
	}
	if _, ok := envLookup(env, "NO_COLOR"); ok {
		return false
	}
	if v, _ := envLookup(env, "TERM"); strings.EqualFold(v, "dumb") {
		return false
	}
	return true
}

// markdownStyle 允许用 GLAMOUR_STYLE 覆盖主题；未设置时优先看 COLORFGBG，
// 浅色终端用 light，其余用 dark（不主动查询终端，避免 OSC 阻塞）。
func markdownStyle(env []string) string {
	if v, ok := envLookup(env, "GLAMOUR_STYLE"); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if fgbg, ok := envLookup(env, "COLORFGBG"); ok {
		parts := strings.Split(fgbg, ";")
		if n, err := strconv.Atoi(strings.TrimSpace(parts[len(parts)-1])); err == nil && n >= 7 && n <= 15 {
			return "light"
		}
	}
	return "dark"
}

func envLookup(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):], true
		}
	}
	return "", false
}

// appendMarkdown 缓存流式文本，并在遇到完整的 Markdown 块（空行或围栏结束）
// 时立刻渲染。这样既保留流式观感，又避免把半截表格/代码围栏渲染坏。
func (r *textRenderer) appendMarkdown(text string) {
	r.mdBuf += text
	for {
		ready, rest, ok := splitMarkdownBlock(r.mdBuf)
		if !ok {
			return
		}
		r.mdBuf = rest
		r.renderMarkdown(ready)
	}
}

func (r *textRenderer) flushMarkdown() {
	md := r.mdBuf
	r.mdBuf = ""
	r.renderMarkdown(md)
}

func (r *textRenderer) renderMarkdown(md string) {
	md = strings.Trim(md, "\n")
	if strings.TrimSpace(md) == "" {
		return
	}
	rendered := md
	if r.md != nil {
		if out, err := r.md.Render(protectHTML(md)); err == nil {
			rendered = trimBlankRenderedLines(out)
		}
	}
	if rendered != "" {
		r.beforeAnswer()
		io.WriteString(r.out, rendered)
		if !strings.HasSuffix(rendered, "\n") {
			io.WriteString(r.out, "\n")
		}
		r.outKind = blockAnswer
		r.outAny = true
		r.lineStart = true
	}
}

// splitMarkdownBlock 找到第一个“完整块”的边界：围栏外的一个空行。
// 围栏内的空行不切分，未闭合的围栏也不会被提前渲染。
func splitMarkdownBlock(s string) (ready, rest string, ok bool) {
	inFence := false
	var fenceChar byte
	fenceLen := 0
	lineStart := 0
	for lineStart < len(s) {
		nl := strings.IndexByte(s[lineStart:], '\n')
		if nl < 0 {
			return "", s, false
		}
		lineEnd := lineStart + nl
		line := s[lineStart:lineEnd]
		wasInFence := inFence
		if ch, n, isFence := fenceMarker(line); isFence {
			switch {
			case !inFence:
				inFence, fenceChar, fenceLen = true, ch, n
			case ch == fenceChar && n >= fenceLen && strings.TrimSpace(strings.TrimLeft(line, " \t")[n:]) == "":
				inFence = false
			}
		}
		// 围栏闭合本身就是完整块：不必等后面的空行，长代码块也能边生成边显示。
		if wasInFence && !inFence {
			return s[:lineEnd+1], s[lineEnd+1:], true
		}
		if !inFence && strings.TrimSpace(line) == "" {
			return s[:lineEnd+1], s[lineEnd+1:], true
		}
		lineStart = lineEnd + 1
	}
	return "", s, false
}

// fenceMarker 识别 ``` 或 ~~~ 围栏（允许最多 3 个前导空格）。
func fenceMarker(line string) (ch byte, n int, ok bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 {
		return 0, 0, false
	}
	ch = trimmed[0]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	return ch, n, true
}

// protectHTML 把代码之外的“类 HTML 标签”尖括号转义成字面量。
// Glamour 会用 bluemonday 清洗 HTML，如果直接渲染，<div>、Vec<T>、List<String>
// 会被当成标签整段吞掉；转义后它们按普通文本显示，代码块与行内代码保持原样。
func protectHTML(md string) string {
	var b strings.Builder
	b.Grow(len(md) + 8)
	for i := 0; i < len(md); {
		if i == 0 || md[i-1] == '\n' {
			lineEnd := strings.IndexByte(md[i:], '\n')
			if lineEnd < 0 {
				lineEnd = len(md)
			} else {
				lineEnd += i
			}
			if ch, n, ok := fenceMarker(md[i:lineEnd]); ok {
				end := fencedBlockEnd(md, lineEnd, ch, n)
				b.WriteString(md[i:end])
				i = end
				continue
			}
		}
		if md[i] == '`' {
			n := 1
			for i+n < len(md) && md[i+n] == '`' {
				n++
			}
			if closeAt := closingBackticks(md, i+n, n); closeAt >= 0 {
				b.WriteString(md[i : closeAt+n])
				i = closeAt + n
				continue
			}
			b.WriteString(md[i : i+n])
			i += n
			continue
		}
		if md[i] == '<' && !escapedBackslash(md, i) && !inLinkDestination(md, i) && looksLikeHTMLTag(md[i:]) {
			b.WriteString(`\<`)
			i++
			continue
		}
		b.WriteByte(md[i])
		i++
	}
	return b.String()
}

// fencedBlockEnd 从开围栏行尾开始，返回闭围栏之后的下标；未闭合则到末尾。
func fencedBlockEnd(s string, openLineEnd int, ch byte, n int) int {
	pos := openLineEnd
	if pos < len(s) && s[pos] == '\n' {
		pos++
	}
	for pos < len(s) {
		lineEnd := strings.IndexByte(s[pos:], '\n')
		if lineEnd < 0 {
			lineEnd = len(s)
		} else {
			lineEnd += pos
		}
		line := s[pos:lineEnd]
		if c, m, ok := fenceMarker(line); ok && c == ch && m >= n &&
			strings.TrimSpace(strings.TrimLeft(line, " \t")[m:]) == "" {
			if lineEnd < len(s) {
				return lineEnd + 1
			}
			return lineEnd
		}
		if lineEnd >= len(s) {
			break
		}
		pos = lineEnd + 1
	}
	return len(s)
}

// closingBackticks 找到与长度为 n 的反引号串匹配的闭串下标。
func closingBackticks(s string, from, n int) int {
	needle := strings.Repeat("`", n)
	for from < len(s) {
		j := strings.Index(s[from:], needle)
		if j < 0 {
			return -1
		}
		start := from + j
		end := start + n
		if (start == 0 || s[start-1] != '`') && (end >= len(s) || s[end] != '`') {
			return start
		}
		from = end
	}
	return -1
}

// looksLikeHTMLTag 判断 < 开头的一段是否是 HTML 标签/泛型尖括号。
// <https://...>、<mailto:...> 等自动链接保持原样。
func looksLikeHTMLTag(s string) bool {
	if strings.HasPrefix(s, "<!--") || strings.HasPrefix(s, "<!DOCTYPE") || strings.HasPrefix(s, "<![CDATA[") {
		return true
	}
	lower := strings.ToLower(s)
	for _, p := range []string{"<http://", "<https://", "<mailto:", "<ftp://", "<tel:"} {
		if strings.HasPrefix(lower, p) {
			return false
		}
	}
	i := 1
	if i < len(s) && s[i] == '/' {
		i++
	}
	if i >= len(s) || !isASCIIAlpha(s[i]) {
		return false
	}
	j := i
	for j < len(s) && isTagNameChar(s[j]) {
		j++
	}
	if j == i || j >= len(s) {
		return false
	}
	if s[j] != '>' && s[j] != '/' && s[j] != ' ' && s[j] != '\t' {
		return false
	}
	gt := strings.IndexByte(s[j:], '>')
	nl := strings.IndexByte(s[j:], '\n')
	return gt >= 0 && (nl < 0 || gt < nl)
}

func isASCIIAlpha(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isTagNameChar(c byte) bool {
	return isASCIIAlpha(c) || c >= '0' && c <= '9' || c == '-' || c == '_' || c == ':' || c == '.'
}

func escapedBackslash(s string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// inLinkDestination 避免破坏 [text](<url>) 这种尖括号链接目标。
func inLinkDestination(s string, i int) bool {
	return i >= 2 && s[i-1] == '(' && s[i-2] == ']'
}

// ansiCSI 匹配 SGR/CSI 序列，用于判断“只有 ANSI 和空格”的行。
var ansiCSI = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string { return ansiCSI.ReplaceAllString(s, "") }

// trimBlankRenderedLines 去掉 Glamour 在块首尾补的空白行（常带 ANSI 空格），
// 避免和 renderer 自己插入的空行叠加成两行。代码块内部的空行不受影响。
func trimBlankRenderedLines(s string) string {
	lines := strings.Split(s, "\n")
	start, end := 0, len(lines)
	for start < end && blankRenderedLine(lines[start]) {
		start++
	}
	for end > start && blankRenderedLine(lines[end-1]) {
		end--
	}
	return strings.Join(lines[start:end], "\n")
}

func blankRenderedLine(line string) bool {
	return strings.TrimSpace(stripANSI(line)) == ""
}
