package tool

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

const (
	defaultMaxFileReadBytes = 262144
	defaultMaxFileReadLines = 2000
	defaultMaxWriteBytes    = 2 << 20
	binarySniffBytes        = 8 << 10
)

func maxFileReadBytesOf(cfg config.View) int {
	if n := settingsOf(cfg).Limits.MaxFileReadBytes; n > 0 {
		return n
	}
	return defaultMaxFileReadBytes
}

func maxFileReadLinesOf(cfg config.View) int {
	if n := settingsOf(cfg).Limits.MaxFileReadLines; n > 0 {
		return n
	}
	return defaultMaxFileReadLines
}

func maxWriteBytesOf(cfg config.View) int {
	if n := settingsOf(cfg).Limits.MaxWriteBytes; n > 0 {
		return n
	}
	return defaultMaxWriteBytes
}

// toolError 把 *model.Error / 普通错误统一成结构化工具错误。
func toolError(err error) *ToolError {
	return toolErrorFrom(err, nil)
}

func invalidArgs(code, message, hint string, details map[string]any) *ToolError {
	if code == "" {
		code = "invalid_args"
	}
	return &ToolError{Code: code, Message: message, Hint: hint, Details: details}
}

func pathError(err error) *ToolError {
	return toolError(err)
}

// isBinary 判断前 8 KB 是否含 NUL（§10.5）。
func isBinary(head []byte) bool {
	if len(head) > binarySniffBytes {
		head = head[:binarySniffBytes]
	}
	return bytes.IndexByte(head, 0) >= 0
}

func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:got], nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func shaOf(data []byte) string { return model.HashRef(data) }

// similarPaths 在目标不存在时给出同目录下名称相近的候选；优先返回相对 workspace 的路径。
func similarPaths(abs, baseDir string) []string {
	baseName := filepath.Base(abs)
	dir := filepath.Dir(abs)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	lower := strings.ToLower(baseName)
	type candidate struct {
		name  string
		score int
	}
	cands := make([]candidate, 0, 4)
	for _, e := range entries {
		name := e.Name()
		ln := strings.ToLower(name)
		score := 0
		switch {
		case ln == lower:
			score = 100
		case strings.HasPrefix(ln, lower) || strings.HasPrefix(lower, ln):
			score = 80
		case strings.Contains(ln, lower) || strings.Contains(lower, ln):
			score = 60
		case editDistanceAtMost(ln, lower, 3):
			score = 40
		}
		if score == 0 {
			continue
		}
		cands = append(cands, candidate{name: name, score: score})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].name < cands[j].name
	})
	out := make([]string, 0, 3)
	for _, c := range cands {
		if len(out) >= 3 {
			break
		}
		candidate := filepath.Join(dir, c.name)
		if baseDir != "" {
			if rel, err := filepath.Rel(baseDir, candidate); err == nil && !isOutsideRel(rel) {
				out = append(out, filepath.ToSlash(rel))
				continue
			}
		}
		out = append(out, filepath.ToSlash(candidate))
	}
	return out
}

func editDistanceAtMost(a, b string, max int) bool {
	if absInt(len(a)-len(b)) > max {
		return false
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = minInt(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if cur[j] < best {
				best = cur[j]
			}
		}
		if best > max {
			return false
		}
		prev, cur = cur, prev
	}
	return prev[len(b)] <= max
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func minInt(vs ...int) int {
	m := vs[0]
	for _, v := range vs[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

// writeFileAtomic 同目录临时文件 → 写入 → chmod → rename。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ant-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		// Windows 上 rename 覆盖已有文件可能失败。
		_ = os.Remove(path)
		if err2 := os.Rename(tmpName, path); err2 != nil {
			_ = os.Remove(tmpName)
			return err2
		}
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// fileModeOf 保留已有文件权限；新文件 0644。
func fileModeOf(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0o644
}

// splitLinesPreserve 保留末尾空行语义：以 \n 结尾时最后一项为空串。
func splitLinesPreserve(s string) []string {
	return strings.Split(s, "\n")
}

func joinLinesPreserve(lines []string) string { return strings.Join(lines, "\n") }

// detectCRLF 返回 CRLF 是否为主流行尾。
func detectCRLF(data []byte) bool {
	crlf := bytes.Count(data, []byte("\r\n"))
	lf := bytes.Count(data, []byte("\n"))
	return crlf > 0 && crlf*2 >= lf
}

// stripCodeFences 去掉模型常带的 Markdown 围栏。
func stripCodeFences(s string) string {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "```") {
		return s
	}
	if i := strings.IndexByte(trimmed, '\n'); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	if i := strings.LastIndex(trimmed, "```"); i >= 0 {
		trimmed = trimmed[:i]
	}
	return strings.TrimRight(trimmed, "\n")
}

// sanitizeOutput 去除 ANSI 控制序列与 NUL，保留换行。
func sanitizeOutput(s string) string {
	s = ansiPattern.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\x00", "")
	if !utf8.ValidString(s) {
		// 非法 UTF-8 用替换字符，避免把破损字节写进 JSON。
		s = strings.ToValidUTF8(s, "�")
	}
	return s
}

// scanLines 逐行读取，回调返回 false 提前停止。
func scanLines(r io.Reader, fn func(line string, n int) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	n := 0
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			n++
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if !fn(line, n) {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// normalizeHash 允许 expectedHash 带或不带 sha256: 前缀。
func hashMatches(expected, actual string) bool {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return false
	}
	if expected == actual {
		return true
	}
	return strings.TrimPrefix(expected, "sha256:") == strings.TrimPrefix(actual, "sha256:")
}

func formatPath(rel, abs string) string {
	if rel != "" {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(abs)
}

func notFoundError(path string, candidates []string) *ToolError {
	hint := "确认路径拼写；路径基于 workspace"
	if len(candidates) > 0 {
		hint = "是否想找：" + strings.Join(candidates, "、")
	}
	return &ToolError{
		Code:    "not_found",
		Message: fmt.Sprintf("文件不存在：%s", path),
		Hint:    hint,
		Details: map[string]any{"path": path, "candidates": candidates},
	}
}

// intArg 读取已校验的整数参数；缺省或类型不符时返回 fallback。
func intArg(args map[string]any, key string, fallback int) int {
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return fallback
}

// boolArg 读取已校验的布尔参数。
func boolArg(args map[string]any, key string, fallback bool) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return fallback
}
