package projectctx

import (
	"os"
	"path/filepath"
	"strings"
)

// readGitCommit 读取 workspace 的 git HEAD，返回完整 commit（无法读取时返回空串）。
// 支持普通分支、分离 HEAD、packed-refs 与 worktree 的 .git 文件。
func readGitCommit(workDir string) string {
	gitDir := resolveGitDir(workDir)
	if gitDir == "" {
		return ""
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(head))
	if !strings.HasPrefix(ref, "ref: ") {
		return ref
	}
	refName := strings.TrimSpace(strings.TrimPrefix(ref, "ref: "))
	if refName == "" {
		return ""
	}
	if commit, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(refName))); err == nil {
		return strings.TrimSpace(string(commit))
	}
	// 打包引用：refs 可能只在 packed-refs 中。
	packed, err := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(packed), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == refName {
			return fields[0]
		}
	}
	return ""
}

// resolveGitDir 返回 .git 目录的绝对路径；.git 为文件（worktree）时解析 gitdir。
func resolveGitDir(workDir string) string {
	if workDir == "" {
		return ""
	}
	dotGit := filepath.Join(workDir, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return dotGit
	}
	raw, err := os.ReadFile(dotGit)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, "gitdir: ") {
		return ""
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir: "))
	if target == "" {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(workDir, target)
	}
	if st, err := os.Stat(target); err != nil || !st.IsDir() {
		return ""
	}
	return target
}

// shortCommit 返回 meta 用的短 commit（与 docs 示例的 7 位一致）。
func shortCommit(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
