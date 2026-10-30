package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

// builtinProtected 是永远不可写的目录（docs/10-security.md §12.2）。
var builtinProtected = []string{".git", ".ant"}

// pathGuard 做 workspace 约束、符号链接解析与保护路径校验。
type pathGuard struct {
	workDir    string   // 已 EvalSymlinks 的绝对路径
	allowRead  []string // 已解析的允许只读根
	allowWrite []string // 已解析的允许读写根
	protected  []string // 保护路径模式（slash 分隔）
}

// NewPathGuard 构造路径守卫；workDir 为空时依次回退到 cfg.Paths 与当前目录。
func NewPathGuard(cfg config.View, workDir string) PathGuard {
	g := &pathGuard{protected: append([]string(nil), builtinProtected...)}
	if workDir == "" {
		workDir = workDirOf(cfg)
	}
	if workDir == "" {
		if wd, err := os.Getwd(); err == nil {
			workDir = wd
		}
	}
	g.workDir = realAbs(workDir)
	if cfg != nil {
		s := cfg.Settings()
		g.allowRead = resolveRoots(g.workDir, s.Sandbox.AllowReadPaths)
		g.allowWrite = resolveRoots(g.workDir, s.Sandbox.AllowWritePaths)
		for _, p := range s.Sandbox.ProtectedPaths {
			if pat := normalizeProtected(p); pat != "" {
				g.protected = append(g.protected, pat)
			}
		}
	}
	return g
}

// Resolve 实现 PathGuard。
//
// 越界（含 `..` 逃逸）返回 Resolved{Outside:true}，由工具/审批层决定是否审批，
// 绝不静默放行；命中保护路径的写操作返回 *model.Error{Code:"path_protected"}。
func (g *pathGuard) Resolve(ctx context.Context, path string, access Access) (Resolved, error) {
	if err := ctx.Err(); err != nil {
		return Resolved{}, err
	}
	raw := strings.TrimSpace(path)
	if raw == "" {
		return Resolved{}, &model.Error{
			Class:   model.ErrToolFailed,
			Code:    "invalid_args",
			Message: "路径不能为空",
			Hint:    "提供 workspace 内的相对路径或已批准的绝对路径",
		}
	}
	abs := expandHome(raw)
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(g.workDir, abs)
	}
	abs = filepath.Clean(abs)
	resolved := resolveExisting(abs)

	rel, relErr := filepath.Rel(g.workDir, resolved)
	outside := relErr != nil || isOutsideRel(rel)
	if outside && containsRoot(g.allowRoots(access), resolved) {
		outside = false
	}

	if access == AccessWrite && !outside {
		if pat, hit := g.matchProtected(resolved); hit {
			return Resolved{}, &model.Error{
				Class:   model.ErrToolFailed,
				Code:    "path_protected",
				Message: fmt.Sprintf("路径 %s 命中保护路径 %s", displayPath(raw), pat),
				Hint:    "该路径属于受保护的元数据目录，不能写入；需要时请修改对应工具的允许范围",
				Details: map[string]any{"path": resolved, "protected": pat},
			}
		}
	}

	relOut := ""
	if relErr == nil {
		relOut = filepath.ToSlash(rel)
	}
	created := false
	if access == AccessWrite {
		parent := filepath.Dir(resolved)
		if _, err := os.Stat(parent); err != nil && os.IsNotExist(err) {
			created = true
		}
	}
	return Resolved{Abs: resolved, Rel: relOut, Outside: outside, Created: created}, nil
}

func (g *pathGuard) allowRoots(access Access) []string {
	if access == AccessWrite {
		return g.allowWrite
	}
	return g.allowRead
}

// matchProtected 判断路径是否落在保护目录内（workspace 内，任意深度）。
func (g *pathGuard) matchProtected(resolved string) (string, bool) {
	rel, err := filepath.Rel(g.workDir, resolved)
	if err != nil || isOutsideRel(rel) || rel == "." {
		return "", false
	}
	relSlash := filepath.ToSlash(rel)
	for _, pat := range g.protected {
		if pat == "" {
			continue
		}
		if relSlash == pat || strings.HasPrefix(relSlash, pat+"/") {
			return pat, true
		}
		if !strings.Contains(pat, "/") {
			for _, part := range strings.Split(relSlash, "/") {
				if part == pat {
					return pat, true
				}
			}
		}
	}
	return "", false
}

// realAbs 返回绝对路径，并对已存在的部分做符号链接解析。
func realAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	return resolveExisting(abs)
}

// resolveExisting 对「存在的最深父目录」做 EvalSymlinks，再拼回不存在的部分。
func resolveExisting(abs string) string {
	cur := filepath.Clean(abs)
	var rest []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			if real, err := filepath.EvalSymlinks(cur); err == nil {
				cur = real
			}
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
	for i := len(rest) - 1; i >= 0; i-- {
		cur = filepath.Join(cur, rest[i])
	}
	return cur
}

func resolveRoots(workDir string, entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		p := expandHome(entry)
		if !filepath.IsAbs(p) {
			p = filepath.Join(workDir, p)
		}
		out = append(out, realAbs(filepath.Clean(p)))
	}
	return out
}

func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") || (runtime.GOOS == "windows" && strings.HasPrefix(p, `~\`)) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func isOutsideRel(rel string) bool {
	if rel == "" || rel == "." {
		return false
	}
	if filepath.IsAbs(rel) {
		return true
	}
	if rel == ".." {
		return true
	}
	return strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// containsRoot 判断 p 是否在任一允许根内（含分隔符前缀校验）。
func containsRoot(roots []string, p string) bool {
	for _, root := range roots {
		if hasPathPrefix(p, root) {
			return true
		}
	}
	return false
}

func hasPathPrefix(p, root string) bool {
	if root == "" {
		return false
	}
	pp := filepath.Clean(p)
	rr := filepath.Clean(root)
	if runtime.GOOS == "windows" {
		pp = strings.ToLower(pp)
		rr = strings.ToLower(rr)
	}
	if pp == rr {
		return true
	}
	if !strings.HasSuffix(rr, string(os.PathSeparator)) {
		rr += string(os.PathSeparator)
	}
	return strings.HasPrefix(pp, rr)
}

func normalizeProtected(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = filepath.ToSlash(filepath.Clean(p))
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return ""
	}
	p = strings.TrimPrefix(p, "./")
	return p
}

func displayPath(p string) string {
	if p == "" {
		return "."
	}
	return p
}
