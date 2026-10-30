package projectctx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"

	"github.com/xiws/ant/internal/model"
)

// hasher 计算失效 hash（§14.2）：文件树（相对路径 + 大小，排序）、关键清单文件
// 内容、git HEAD 与生成器版本。不做全量文件内容 hash，避免无关改动频繁失效。
type hasher struct {
	workDir string
	version string
}

// NewHasher 返回基于 workspace 的 Hasher；workDir 只用于读取 git HEAD，
// 为空时跳过 git 部分（测试或非 git 目录）。
func NewHasher(workDir string) Hasher {
	return &hasher{workDir: workDir, version: generatorVersion}
}

// Hash 返回 `sha256:<hex>`。字段带类型与长度前缀，避免拼接歧义。
func (h *hasher) Hash(ctx context.Context, r ScanResult) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if h == nil {
		return "", model.NewError(model.ErrInternal, "projectctx.nil_hasher", "hasher 未初始化")
	}
	sum := sha256.New()
	write := func(kind, value string) {
		fmt.Fprintf(sum, "%s %d %s\n", kind, len(value), value)
	}

	write("generator", h.version)

	// 文件树：排序后逐条写入（Scanner 用 "\t<size>" 携带大小）。
	tree := append([]string(nil), r.Tree...)
	sort.Strings(tree)
	for _, entry := range tree {
		write("tree", entry)
	}

	write("type", r.ProjectType)
	write("module", r.ModulePath)
	write("fileCount", strconv.Itoa(r.FileCount))

	// 关键清单文件：只 hash 内容摘要，正文可能因预算未全部进入 KeyFiles。
	keyFiles := append([]KeyFile(nil), r.KeyFiles...)
	sort.SliceStable(keyFiles, func(i, j int) bool { return keyFiles[i].Path < keyFiles[j].Path })
	for _, kf := range keyFiles {
		bodySum := sha256.Sum256([]byte(kf.Body))
		write("keyfile", kf.Path)
		write("keyfile-sha256", hex.EncodeToString(bodySum[:]))
	}

	deps := append([]string(nil), r.Deps...)
	sort.Strings(deps)
	for _, dep := range deps {
		write("dep", dep)
	}

	// git HEAD：分支引用会被解析为 commit；读不到时为空串（不影响 hash 有效性）。
	write("git", readGitCommit(h.workDir))

	return "sha256:" + hex.EncodeToString(sum.Sum(nil)), nil
}
