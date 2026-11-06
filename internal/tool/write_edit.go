package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/xiws/ant/internal/config"
)

// writeExec 实现 §10.9 write。
func writeExec(cfg config.View, deps Deps) func(context.Context, Invocation) (Result, error) {
	maxWrite := maxWriteBytesOf(cfg)
	return func(ctx context.Context, inv Invocation) (Result, error) {
		path, _ := inv.Args["path"].(string)
		content, _ := inv.Args["content"].(string)
		if strings.TrimSpace(path) == "" {
			return Result{OK: false, Err: invalidArgs("invalid_args", "缺少 path", "提供要写入的文件路径", nil)}, nil
		}
		if len(content) > maxWrite {
			return Result{OK: false, Err: &ToolError{
				Code:    "too_large",
				Message: fmt.Sprintf("写入内容 %d 字节，超过上限 %d", len(content), maxWrite),
				Hint:    "拆分文件、缩小内容，或调大 limits.maxWriteBytes",
				Details: map[string]any{"bytes": len(content), "limit": maxWrite},
			}}, nil
		}
		if strings.IndexByte(content, 0) >= 0 {
			return Result{OK: false, Err: &ToolError{
				Code:    "binary_file",
				Message: "write 只接受文本内容（检测到 NUL 字节）",
				Hint:    "二进制文件请用 bash 的专用工具生成",
			}}, nil
		}
		res, err := deps.Guard.Resolve(ctx, path, AccessWrite)
		if err != nil {
			return Result{OK: false, Err: pathError(err)}, nil
		}
		info, statErr := os.Stat(res.Abs)
		exists := statErr == nil
		if statErr != nil && !os.IsNotExist(statErr) {
			return Result{OK: false, Err: toolError(statErr)}, nil
		}
		if exists && info.IsDir() {
			return Result{OK: false, Err: invalidArgs("invalid_args",
				fmt.Sprintf("%s 是目录，不能作为文件写入", path),
				"用 write 创建文件，而不是覆盖目录", map[string]any{"path": path})}, nil
		}

		expected, _ := inv.Args["expectedHash"].(string)
		if exists {
			current, herr := fileSHA256(res.Abs)
			if herr != nil {
				return Result{OK: false, Err: toolError(herr)}, nil
			}
			if strings.TrimSpace(expected) != "" {
				if !hashMatches(expected, current) {
					return Result{OK: false, Err: hashMismatch(res.Rel, current, expected)}, nil
				}
			} else {
				last, ok := "", false
				if deps.Versions != nil {
					last, ok = lastReadHash(deps.Versions, res)
				}
				if !ok {
					return Result{OK: false, Err: &ToolError{
						Code:    "hash_mismatch",
						Message: "文件已存在，但本 session 尚未读取过它，也没有提供 expectedHash",
						Hint:    "先 read 该文件确认内容，或在 write 中带上读取时返回的 sha256",
						Details: map[string]any{"path": res.Rel, "sha256": current},
					}}, nil
				}
				if !hashMatches(last, current) {
					return Result{OK: false, Err: hashMismatch(res.Rel, current, last)}, nil
				}
			}
		}

		perm := fileModeOf(res.Abs)
		if err := writeFileAtomic(res.Abs, []byte(content), perm); err != nil {
			return Result{OK: false, Err: &ToolError{
				Code:    "internal",
				Message: "写入文件失败：" + err.Error(),
				Hint:    "检查目录权限与磁盘空间",
				Details: map[string]any{"path": res.Rel},
			}}, nil
		}
		sha := shaOf([]byte(content))
		if deps.Versions != nil {
			deps.Versions.RecordWrite(res.Abs, sha)
			if res.Rel != "" {
				deps.Versions.RecordWrite(res.Rel, sha)
			}
		}
		invalidateIndex(deps, res.Rel)
		return Result{OK: true, Data: map[string]any{
			"path":    formatPath(res.Rel, res.Abs),
			"bytes":   len(content),
			"size":    len(content),
			"sha256":  sha,
			"created": !exists,
		}}, nil
	}
}

func lastReadHash(v VersionTable, res Resolved) (string, bool) {
	if res.Rel != "" {
		if sha, ok := v.LastRead(res.Rel); ok {
			return sha, true
		}
	}
	return v.LastRead(res.Abs)
}

func hashMismatch(path, current, expected string) *ToolError {
	return &ToolError{
		Code:    "hash_mismatch",
		Message: "文件在读取后已被修改，写入会覆盖他人的改动",
		Hint:    "重新 read 该文件，确认最新内容后再写入",
		Details: map[string]any{"path": filepath.ToSlash(path), "sha256": current, "expected": expected},
	}
}

func invalidateIndex(deps Deps, path string) {
	if deps.Index != nil && path != "" {
		deps.Index.Invalidate([]string{filepath.ToSlash(path)})
	}
}

// editExec 实现 §10.8 edit：unified diff，部分 hunk 不匹配时不落盘。
func editExec(cfg config.View, deps Deps) func(context.Context, Invocation) (Result, error) {
	maxWrite := maxWriteBytesOf(cfg)
	return func(ctx context.Context, inv Invocation) (Result, error) {
		path, _ := inv.Args["path"].(string)
		diffText, _ := inv.Args["diff"].(string)
		if strings.TrimSpace(path) == "" || strings.TrimSpace(diffText) == "" {
			return Result{OK: false, Err: invalidArgs("invalid_args", "缺少 path 或 diff",
				"提供目标文件与 unified diff", nil)}, nil
		}
		res, err := deps.Guard.Resolve(ctx, path, AccessWrite)
		if err != nil {
			return Result{OK: false, Err: pathError(err)}, nil
		}
		info, statErr := os.Stat(res.Abs)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				return Result{OK: false, Err: notFoundError(path, similarPaths(res.Abs, workDirOf(cfg)))}, nil
			}
			return Result{OK: false, Err: toolError(statErr)}, nil
		}
		if info.IsDir() {
			return Result{OK: false, Err: invalidArgs("invalid_args",
				fmt.Sprintf("%s 是目录", path), "edit 只能修改文件；新建文件用 write", nil)}, nil
		}
		if info.Size() > int64(maxWrite) {
			return Result{OK: false, Err: &ToolError{
				Code:    "too_large",
				Message: fmt.Sprintf("目标文件 %d 字节，超过编辑上限 %d", info.Size(), maxWrite),
				Hint:    "用更小的文件，或调大 limits.maxWriteBytes",
			}}, nil
		}
		raw, rerr := os.ReadFile(res.Abs)
		if rerr != nil {
			return Result{OK: false, Err: toolError(rerr)}, nil
		}
		if isBinary(raw) {
			return Result{OK: false, Err: &ToolError{
				Code:    "binary_file",
				Message: fmt.Sprintf("%s 是二进制文件，不能 edit", path),
				Hint:    "二进制文件请使用专用工具",
			}}, nil
		}
		expected, _ := inv.Args["expectedHash"].(string)
		current := shaOf(raw)
		if strings.TrimSpace(expected) != "" {
			if !hashMatches(expected, current) {
				return Result{OK: false, Err: hashMismatch(res.Rel, current, expected)}, nil
			}
		} else {
			last, ok := "", false
			if deps.Versions != nil {
				last, ok = lastReadHash(deps.Versions, res)
			}
			if !ok {
				return Result{OK: false, Err: &ToolError{
					Code:    "hash_mismatch",
					Message: "edit 前必须先 read 该文件，或提供 expectedHash",
					Hint:    "先 read 确认当前内容，再基于它生成 diff",
					Details: map[string]any{"path": res.Rel, "sha256": current},
				}}, nil
			}
			if !hashMatches(last, current) {
				return Result{OK: false, Err: hashMismatch(res.Rel, current, last)}, nil
			}
		}

		patch, perr := parseUnifiedDiff(diffText, path)
		if perr != nil {
			return Result{OK: false, Err: perr}, nil
		}
		if len(patch.hunks) == 0 {
			return Result{OK: false, Err: &ToolError{
				Code:    "no_hunks",
				Message: "diff 中没有任何 hunk",
				Hint:    "生成标准 unified diff：@@ -旧起始,行数 +新起始,行数 @@",
			}}, nil
		}

		crlf := detectCRLF(raw)
		source := string(raw)
		if crlf {
			source = strings.ReplaceAll(source, "\r\n", "\n")
		}
		result, stats, applyErr := applyHunks(source, patch.hunks)
		if applyErr != nil {
			return Result{OK: false, Err: applyErr}, nil
		}
		if stats.applied == 0 {
			return Result{OK: false, Err: &ToolError{
				Code:    "partial_hunks",
				Message: fmt.Sprintf("0/%d 个 hunk 能匹配当前文件", len(patch.hunks)),
				Hint:    "重新 read 文件，复制最新上下文后重试",
				Details: map[string]any{"applied": 0, "total": len(patch.hunks), "failed": stats.failed},
			}}, nil
		}
		if result == source {
			return Result{OK: false, Err: &ToolError{
				Code:    "unchanged",
				Message: "diff 应用后文件内容没有变化",
				Hint:    "检查 hunk 的 + 行是否与 - 行相同；需要改动请给出实际差异",
			}}, nil
		}
		if len(result) > maxWrite {
			return Result{OK: false, Err: &ToolError{
				Code:    "too_large",
				Message: fmt.Sprintf("编辑后内容 %d 字节，超过上限 %d", len(result), maxWrite),
				Hint:    "拆分为多次小改动，或调大 limits.maxWriteBytes",
			}}, nil
		}
		out := []byte(result)
		if crlf {
			out = []byte(strings.ReplaceAll(result, "\n", "\r\n"))
		}
		if stats.applied != len(patch.hunks) {
			return Result{OK: false, Err: &ToolError{
				Code:    "partial_hunks",
				Message: fmt.Sprintf("%d/%d 个 hunk 未能匹配，文件未修改", len(patch.hunks)-stats.applied, len(patch.hunks)),
				Hint:    "逐个 hunk 补足上下文（前后各 3 行）后重试；不要修改未匹配 hunk 之外的内容",
				Details: map[string]any{
					"applied": stats.applied,
					"total":   len(patch.hunks),
					"failed":  stats.failed,
					"context": contextAround(source, patch.hunks[0].oldStart, 6),
				},
			}}, nil
		}
		if err := writeFileAtomic(res.Abs, out, fileModeOf(res.Abs)); err != nil {
			return Result{OK: false, Err: &ToolError{
				Code:    "internal",
				Message: "写入编辑结果失败：" + err.Error(),
				Hint:    "检查目录权限与磁盘空间",
			}}, nil
		}
		sha := shaOf(out)
		if deps.Versions != nil {
			deps.Versions.RecordWrite(res.Abs, sha)
			if res.Rel != "" {
				deps.Versions.RecordWrite(res.Rel, sha)
			}
		}
		invalidateIndex(deps, res.Rel)
		return Result{OK: true, Data: map[string]any{
			"path":    formatPath(res.Rel, res.Abs),
			"hunks":   len(patch.hunks),
			"total":   len(patch.hunks),
			"applied": stats.applied,
			"added":   stats.added,
			"removed": stats.removed,
			"bytes":   len(out),
			"sha256":  sha,
		}}, nil
	}
}

type diffLine struct {
	kind byte // ' ', '-', '+'
	text string
}

type diffHunk struct {
	oldStart int
	oldCount int
	newStart int
	newCount int
	lines    []diffLine
	raw      string
}

type parsedPatch struct {
	hunks []diffHunk
}

var hunkHeaderRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func parseUnifiedDiff(diff, path string) (*parsedPatch, *ToolError) {
	text := stripCodeFences(diff)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	patch := &parsedPatch{}
	var (
		cur          *diffHunk
		varOldHeader string
		varNewHeader string
		fileCount    int
	)
	flush := func() {
		if cur != nil {
			patch.hunks = append(patch.hunks, *cur)
			cur = nil
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			continue
		}
		if strings.HasPrefix(line, "--- ") {
			flush()
			fileCount++
			varOldHeader = parseHeaderPath(strings.TrimPrefix(line, "--- "))
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			varNewHeader = parseHeaderPath(strings.TrimPrefix(line, "+++ "))
			continue
		}
		if m := hunkHeaderRE.FindStringSubmatch(line); m != nil {
			flush()
			h := &diffHunk{
				oldStart: atoiDefault(m[1], 1),
				oldCount: atoiDefault(m[2], 1),
				newStart: atoiDefault(m[3], 1),
				newCount: atoiDefault(m[4], 1),
				raw:      line,
			}
			cur = h
			continue
		}
		if cur == nil {
			// 文件头 / 说明文字 / 空行，忽略。
			continue
		}
		if len(line) == 0 {
			cur.lines = append(cur.lines, diffLine{kind: ' ', text: ""})
			cur.raw += "\n" + line
			continue
		}
		switch line[0] {
		case ' ', '+', '-':
			cur.lines = append(cur.lines, diffLine{kind: line[0], text: line[1:]})
			cur.raw += "\n" + line
		case '\\':
			// "\ No newline at end of file"
		default:
			// 允许 hunk 后紧跟说明文字：结束当前 hunk。
			flush()
		}
	}
	flush()

	if fileCount > 1 {
		return nil, invalidArgs("invalid_args",
			fmt.Sprintf("diff 包含 %d 个文件，edit 一次只能改一个文件", fileCount),
			"每个文件单独调用 edit", map[string]any{"files": fileCount})
	}
	if varOldHeader != "" && varNewHeader != "" {
		if !sameDiffPath(varOldHeader, path) && !sameDiffPath(varNewHeader, path) {
			return nil, invalidArgs("invalid_args",
				fmt.Sprintf("diff 文件头 %q/%q 与 path %q 不一致", varOldHeader, varNewHeader, path),
				"path 与 diff 的文件头必须指向同一个文件；每个文件单独 edit",
				map[string]any{"old": varOldHeader, "new": varNewHeader, "path": path})
		}
	}
	if varOldHeader != "" && varNewHeader == "" {
		return nil, invalidArgs("invalid_args", "diff 缺少 +++ 文件头", "给出完整的 ---/+++ 头，或只给 hunk", nil)
	}
	for i := range patch.hunks {
		h := &patch.hunks[i]
		oldN, newN := 0, 0
		for _, l := range h.lines {
			switch l.kind {
			case ' ':
				oldN++
				newN++
			case '-':
				oldN++
			case '+':
				newN++
			}
		}
		if h.oldCount == 0 {
			h.oldCount = oldN
		}
		if h.newCount == 0 {
			h.newCount = newN
		}
		if oldN != h.oldCount || newN != h.newCount {
			return nil, &ToolError{
				Code:    "partial_hunks",
				Message: fmt.Sprintf("hunk %q 的行数与标题不一致（实际 -%d/+%d，标题 -%d/+%d）", h.raw, oldN, newN, h.oldCount, h.newCount),
				Hint:    "统一 diff 的 hunk 标题必须与实际行数一致；修正后重试",
				Details: map[string]any{"hunk": h.raw, "oldLines": oldN, "newLines": newN},
			}
		}
	}
	return patch, nil
}

func parseHeaderPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, '\t'); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.Trim(raw, `"`)
	if raw == "/dev/null" {
		return raw
	}
	return raw
}

func sameDiffPath(header, path string) bool {
	clean := func(s string) string {
		s = filepath.ToSlash(strings.TrimSpace(s))
		s = strings.TrimPrefix(s, "a/")
		s = strings.TrimPrefix(s, "b/")
		s = strings.TrimPrefix(s, "./")
		return filepath.ToSlash(filepath.Clean(s))
	}
	h := clean(header)
	p := clean(path)
	if h == p {
		return true
	}
	// 仅当一侧是绝对路径、另一侧是 workspace 相对路径时，才按尾段比较。
	if filepath.IsAbs(h) != filepath.IsAbs(p) {
		return strings.HasSuffix(h, "/"+p) || strings.HasSuffix(p, "/"+h)
	}
	return false
}

func atoiDefault(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

type applyStats struct {
	applied int
	added   int
	removed int
	failed  []string
}

func applyHunks(source string, hunks []diffHunk) (string, applyStats, *ToolError) {
	lines := splitLinesPreserve(source)
	stats := applyStats{}
	offset := 0
	for _, h := range hunks {
		var oldSeq, addedText []string
		for _, l := range h.lines {
			switch l.kind {
			case ' ':
				oldSeq = append(oldSeq, l.text)
			case '-':
				oldSeq = append(oldSeq, l.text)
				stats.removed++
			case '+':
				addedText = append(addedText, l.text)
				stats.added++
			}
		}
		expected := h.oldStart - 1 + offset
		idx, fuzzy := findSequence(lines, oldSeq, expected)
		if idx < 0 {
			stats.failed = append(stats.failed, h.raw)
			// 未匹配 hunk 不再增加 offset；继续检查后续 hunk，便于一次性报告。
			continue
		}
		newBlock := make([]string, 0, len(h.lines))
		oldPos := 0
		for _, l := range h.lines {
			switch l.kind {
			case ' ':
				// fuzzy 命中时保留源文件的实际缩进/行尾。
				if fuzzy && idx+oldPos < len(lines) {
					newBlock = append(newBlock, lines[idx+oldPos])
				} else {
					newBlock = append(newBlock, l.text)
				}
				oldPos++
			case '-':
				oldPos++
			case '+':
				newBlock = append(newBlock, l.text)
			}
		}
		next := make([]string, 0, len(lines)-(len(oldSeq)-len(newBlock)))
		next = append(next, lines[:idx]...)
		next = append(next, newBlock...)
		next = append(next, lines[idx+len(oldSeq):]...)
		lines = next
		offset += len(newBlock) - len(oldSeq)
		stats.applied++
	}
	return joinLinesPreserve(lines), stats, nil
}

// findSequence 在 expected 附近寻找旧行序列；先精确，再做去掉首尾空白的容错匹配。
func findSequence(lines, seq []string, expected int) (int, bool) {
	if len(seq) == 0 {
		if expected < 0 {
			expected = 0
		}
		if expected > len(lines) {
			expected = len(lines)
		}
		return expected, false
	}
	if idx := findExactNear(lines, seq, expected); idx >= 0 {
		return idx, false
	}
	return findFuzzyNear(lines, seq, expected), true
}

func findExactNear(lines, seq []string, expected int) int {
	if expected < 0 {
		expected = 0
	}
	if expected > len(lines)-len(seq) {
		expected = len(lines) - len(seq)
	}
	if expected < 0 {
		return -1
	}
	// 从 expected 向两侧扩散，优先选择距离最近的匹配。
	for d := 0; d <= len(lines); d++ {
		for _, idx := range []int{expected + d, expected - d} {
			if idx < 0 || idx+len(seq) > len(lines) {
				continue
			}
			if equalLines(lines[idx:idx+len(seq)], seq) {
				return idx
			}
		}
	}
	return -1
}

func findFuzzyNear(lines, seq []string, expected int) int {
	if expected < 0 {
		expected = 0
	}
	best := -1
	bestDist := 1 << 30
	for idx := 0; idx+len(seq) <= len(lines); idx++ {
		if !equalLinesFuzzy(lines[idx:idx+len(seq)], seq) {
			continue
		}
		d := idx - expected
		if d < 0 {
			d = -d
		}
		if d < bestDist {
			best, bestDist = idx, d
		}
	}
	return best
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalLinesFuzzy(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}

func contextAround(source string, start, radius int) []string {
	lines := splitLinesPreserve(source)
	if start < 1 {
		start = 1
	}
	from := start - 1 - radius
	if from < 0 {
		from = 0
	}
	to := start - 1 + radius
	if to > len(lines) {
		to = len(lines)
	}
	if from >= len(lines) {
		return nil
	}
	return lines[from:to]
}
