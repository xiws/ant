package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/xiws/ant/internal/model"
)

// FormatCount 把 token 数格式化为人类易读的 1.2k / 3.4M。
func FormatCount(n int) string {
	switch {
	case n < 0:
		return "0"
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1000)) + "k"
	default:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1_000_000)) + "M"
	}
}

func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

// FormatDuration 输出紧凑耗时：850ms / 8.8s / 2m03s。
func FormatDuration(ms int64) string {
	if ms <= 0 {
		return ""
	}
	d := ms
	switch {
	case d < 1000:
		return fmt.Sprintf("%dms", d)
	case d < 60_000:
		return trimZero(fmt.Sprintf("%.1f", float64(d)/1000)) + "s"
	default:
		return fmt.Sprintf("%dm%02ds", d/60_000, (d%60_000)/1000)
	}
}

// FormatUsage 输出一行 token 统计。
func FormatUsage(u model.Usage) string {
	return fmt.Sprintf("in %s / out %s / cached %s / reasoning %s",
		FormatCount(u.Input), FormatCount(u.Output), FormatCount(u.Cached), FormatCount(u.Reasoning))
}

// PrettyJSON 美化 JSON；非法 JSON 原样返回。
func PrettyJSON(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, trimmed, "", "  "); err != nil {
		return string(trimmed)
	}
	return buf.String()
}

// FenceFor 返回比内容中最长连续反引号更长的围栏。
func FenceFor(content string) string {
	longest, cur := 0, 0
	for i := 0; i < len(content); i++ {
		if content[i] == '`' {
			cur++
			if cur > longest {
				longest = cur
			}
		} else {
			cur = 0
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// Fenced 用安全围栏包裹内容。
func Fenced(lang, content string) string {
	fence := FenceFor(content)
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if lang != "" {
		return fence + lang + "\n" + content + fence
	}
	return fence + "\n" + content + fence
}

// OneLine 压成单行并按 rune 截断。
func OneLine(s string, limit int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " "))
	if limit > 0 {
		r := []rune(s)
		if len(r) > limit {
			s = string(r[:limit]) + "…"
		}
	}
	return s
}

// Truncate 按 rune 截断并补省略号。
func Truncate(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// ShortHash 取 sha256 前 12 位。
func ShortHash(hash string) string {
	hash = strings.TrimSpace(strings.TrimPrefix(hash, "sha256:"))
	if hash == "" {
		return "-"
	}
	if len(hash) > 12 {
		return hash[:12] + "…"
	}
	return hash
}

// StatusLabel 把状态翻译成中文标签。
func StatusLabel(status string) string {
	switch status {
	case "ok", "success":
		return "成功"
	case "failed", "error":
		return "失败"
	case "interrupted":
		return "中断"
	case "incomplete":
		return "未完成"
	case "":
		return "-"
	default:
		return status
	}
}
