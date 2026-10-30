package prompt

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

//go:embed prompts/*.md prompts/modes/*.md
var builtinFS embed.FS

// fileSource 实现 Source：内置 prompt + home/workspace 覆盖（§6.2）。
type fileSource struct {
	cfg     config.View
	workDir string
	homeDir string
}

// NewFileSource 构造默认 Source。cfg 可为 nil，此时只用内置 prompt。
func NewFileSource(cfg config.View) Source {
	fs := &fileSource{cfg: cfg}
	if p, ok := cfg.(config.Paths); ok {
		fs.workDir = p.WorkDir()
		fs.homeDir = p.HomeDir()
	}
	return fs
}

// NodePrompt 按 workspace > home > 内置 的优先级返回节点 prompt。
// 覆盖文件缺失时静默回落；覆盖文件为空视为显式清空（warning 由使用方记录）。
func (s *fileSource) NodePrompt(node model.NodeName) (Doc, error) {
	if !isKnownNode(node) {
		return Doc{}, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "unknown_node",
			Message: fmt.Sprintf("prompt: 未知节点 %q", node),
		}
	}
	builtin, err := s.readBuiltin("prompts/" + string(node) + ".md")
	if err != nil {
		return Doc{}, err
	}
	candidates := []struct {
		dir    string
		origin string
	}{
		{filepath.Join(s.workDir, ".ant", "prompts"), "workspace"},
		{filepath.Join(s.homeDir, "prompts"), "home"},
	}
	for _, c := range candidates {
		if c.dir == "" || c.dir == ".ant/prompts" || c.dir == "prompts" {
			continue
		}
		path := filepath.Join(c.dir, string(node)+".md")
		doc, ok, err := readOverride(path)
		if err != nil {
			return Doc{}, err
		}
		if ok {
			doc.Origin = c.origin
			return doc, nil
		}
	}
	return builtin, nil
}

// ModeOverlay 只返回内置 overlay：模式 prompt 不允许覆盖（D13）。
func (s *fileSource) ModeOverlay(mode model.Mode) (Doc, error) {
	if !mode.Valid() {
		return Doc{}, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "unknown_mode",
			Message: fmt.Sprintf("prompt: 未知模式 %q", mode),
		}
	}
	return s.readBuiltin("prompts/modes/" + string(mode) + ".md")
}

// UserPrompt 返回 home/workspace 的 prompt.md；缺失时返回空 Doc。
func (s *fileSource) UserPrompt(scope Scope) (Doc, error) {
	var path string
	switch scope {
	case ScopeHome:
		if s.homeDir != "" {
			path = filepath.Join(s.homeDir, "prompt.md")
		}
	case ScopeWorkspace:
		if s.workDir != "" {
			path = filepath.Join(s.workDir, ".ant", "prompt.md")
		}
	}
	if path == "" {
		return Doc{}, nil
	}
	doc, ok, err := readOverride(path)
	if err != nil || !ok {
		return Doc{}, err
	}
	doc.Origin = string(scope)
	return doc, nil
}

func (s *fileSource) readBuiltin(path string) (Doc, error) {
	b, err := builtinFS.ReadFile(path)
	if err != nil {
		return Doc{}, &model.Error{Class: model.ErrConfigInvalid, Code: "prompt_builtin", Message: fmt.Sprintf("内置 prompt %s 缺失：%v", path, err), Cause: err}
	}
	text := string(b)
	return Doc{Path: "builtin:" + path, Text: text, Version: versionOf(text), Origin: "builtin"}, nil
}

func readOverride(path string) (Doc, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Doc{}, false, nil
		}
		return Doc{}, false, &model.Error{Class: model.ErrConfigInvalid, Code: "prompt_read", Message: fmt.Sprintf("读取 %s 失败：%v", path, err), Cause: err}
	}
	if info.IsDir() {
		return Doc{}, false, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Doc{}, false, &model.Error{Class: model.ErrConfigInvalid, Code: "prompt_read", Message: fmt.Sprintf("读取 %s 失败：%v", path, err), Cause: err}
	}
	text := string(b)
	return Doc{Path: path, Text: text, Version: versionOf(text), Origin: "file"}, true, nil
}

func versionOf(text string) string {
	h := strings.TrimPrefix(hashString(text), "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

func isKnownNode(node model.NodeName) bool {
	for _, n := range model.AllNodes() {
		if n == node {
			return true
		}
	}
	return false
}
