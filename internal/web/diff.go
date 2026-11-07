package web

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// diffHunkHeader 匹配 `@@ -a,b +c,d @@ tail`。
var diffHunkHeader = regexp.MustCompile(`^@@\s+-(\d+)(?:,(\d+))?\s+\+(\d+)(?:,(\d+))?\s+@@(.*)$`)

const (
	maxDiffHunks = 50
	maxDiffLines = 2000
)

// ParseUnifiedDiff 解析 unified diff，用于 `edit` 卡片与右栏改动。
//
// 规则见 docs/20-web-serve.md §22.5.4：去掉 Markdown 代码围栏、支持
// diff --git/---/+++/@@、行类型 空格/-/+/\\，单文件最多 50 hunk、2000 行；
// 解析失败时降级为原始文本并给出 warning。
func ParseUnifiedDiff(raw, path string) *DiffView {
	text := stripDiffFence(raw)
	view := &DiffView{Path: path, Hunks: []DiffHunk{}, Raw: text}
	if strings.TrimSpace(text) == "" {
		view.Empty = true
		view.Warning = "diff 为空"
		return view
	}

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var (
		cur       = -1
		oldNo     int
		newNo     int
		lineCount int
	)
	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			if len(view.Hunks) >= maxDiffHunks {
				view.Truncated = true
				break
			}
			match := diffHunkHeader.FindStringSubmatch(line)
			if match == nil {
				// 非标准 hunk 头：不丢弃，作为上一条 hunk 的备注行。
				if cur >= 0 {
					view.Hunks[cur].Lines = append(view.Hunks[cur].Lines, DiffLine{Kind: "note", Text: line})
				}
				continue
			}
			hunk := DiffHunk{
				OldStart: atoiDefault(match[1], 0),
				OldCount: atoiDefault(match[2], 1),
				NewStart: atoiDefault(match[3], 0),
				NewCount: atoiDefault(match[4], 1),
				Header:   strings.TrimSpace(match[5]),
			}
			view.Hunks = append(view.Hunks, hunk)
			cur = len(view.Hunks) - 1
			oldNo, newNo = hunk.OldStart, hunk.NewStart
			continue
		}
		if cur < 0 {
			// hunk 之前的文件头/元信息：保留但不计入 +/-。
			continue
		}
		if lineCount >= maxDiffLines {
			view.Truncated = true
			break
		}
		switch {
		case strings.HasPrefix(line, "+"):
			view.Hunks[cur].Lines = append(view.Hunks[cur].Lines, DiffLine{Kind: "add", Text: line[1:], NewNo: newNo})
			newNo++
			view.Added++
			lineCount++
		case strings.HasPrefix(line, "-"):
			view.Hunks[cur].Lines = append(view.Hunks[cur].Lines, DiffLine{Kind: "del", Text: line[1:], OldNo: oldNo})
			oldNo++
			view.Removed++
			lineCount++
		case strings.HasPrefix(line, "\\"):
			view.Hunks[cur].Lines = append(view.Hunks[cur].Lines, DiffLine{Kind: "note", Text: line})
			lineCount++
		case strings.HasPrefix(line, " "):
			view.Hunks[cur].Lines = append(view.Hunks[cur].Lines, DiffLine{Kind: "ctx", Text: line[1:], OldNo: oldNo, NewNo: newNo})
			oldNo++
			newNo++
			lineCount++
		case line == "":
			// 尾部空行（split 的产物）忽略。
		default:
			// 无法识别的行：原样保留为 note，绝不猜测增删。
			view.Hunks[cur].Lines = append(view.Hunks[cur].Lines, DiffLine{Kind: "note", Text: line})
			lineCount++
		}
	}

	view.HunkCount = len(view.Hunks)
	if view.HunkCount == 0 {
		view.Empty = true
		view.Warning = "无法解析为 unified diff；已降级为原始文本"
	}
	return view
}

// stripDiffFence 去掉包裹 diff 的 Markdown 代码围栏。
func stripDiffFence(raw string) string {
	text := strings.TrimSpace(raw)
	if !strings.HasPrefix(text, "```") {
		return strings.TrimRight(text, "\n")
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "```") {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

func isDiffMetaLine(line string) bool {
	prefixes := []string{
		"diff --git ", "index ", "new file mode ", "deleted file mode ",
		"old mode ", "new mode ", "similarity index ", "rename from ",
		"rename to ", "--- ", "+++ ", "Binary files ",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func atoiDefault(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

// DiffStats 返回 `+n −m` 与 hunk 数；供 write 卡片等复用。
func DiffStats(view *DiffView) (added, removed, hunks int) {
	if view == nil {
		return 0, 0, 0
	}
	return view.Added, view.Removed, view.HunkCount
}

// DiffSummary 是 "…+8 −2" 的一行摘要。
func DiffSummary(view *DiffView) string {
	if view == nil {
		return ""
	}
	return fmt.Sprintf("+%d −%d", view.Added, view.Removed)
}
