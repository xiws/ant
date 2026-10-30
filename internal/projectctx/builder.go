package projectctx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/provider"
)

const (
	generatedStartMarker = "<!-- ant:generated:start -->"
	generatedEndMarker   = "<!-- ant:generated:end -->"

	projectDocName    = "project.md"
	projectMetaName   = "project.meta.json"
	projectLockName   = "project.lock"
	projectBackupName = "project.md.bak"

	metaSchemaVersion = 1
	// metaTimeLayout 与 docs 示例一致（毫秒精度 UTC，形如 2026-10-30T09:47:04.123Z）。
	metaTimeLayout = "2006-01-02T15:04:05.000Z07:00"

	defaultMaxOutputBytes  = 8192
	hardMaxOutputBytes     = 1 << 20
	defaultProjectMaxFiles = 500
	defaultProjectMaxBytes = 32768
	defaultProjectDepth    = 3

	// lockStaleAfter 之后视为陈旧锁：进程崩溃时锁文件会残留，需要允许清理，
	// 否则 project.md 将永远无法再生成。10 分钟内正常扫描/生成不可能完不成。
	lockStaleAfter = 10 * time.Minute
)

// deterministicModel 写入 project.meta.json 的 model 字段：确定性骨架没有调用模型。
const deterministicModel = "deterministic"

type builder struct {
	cfg      config.View
	provider provider.Provider
	log      logx.Logger

	newScanner func() Scanner
	newHasher  func(workDir string) Hasher
	generator  Generator
	now        func() time.Time
}

// NewBuilder 注入配置、模型与日志；扫描/哈希/生成器在内部构造，便于单测替换。
func NewBuilder(cfg config.View, p provider.Provider, log logx.Logger) Builder {
	if log == nil {
		log = logx.Nop()
	}
	return &builder{
		cfg:        cfg,
		provider:   p,
		log:        log,
		newScanner: func() Scanner { return NewScanner() },
		newHasher:  func(workDir string) Hasher { return NewHasher(workDir) },
		generator:  NewDeterministicGenerator(),
		now:        func() time.Time { return time.Now().UTC() },
	}
}

var _ Builder = (*builder)(nil)

type projectPaths struct {
	workDir string
	dir     string
	doc     string
	meta    string
	lock    string
	backup  string
}

func pathsFor(workDir string) projectPaths {
	dir := filepath.Join(workDir, ".ant")
	return projectPaths{
		workDir: workDir,
		dir:     dir,
		doc:     filepath.Join(dir, projectDocName),
		meta:    filepath.Join(dir, projectMetaName),
		lock:    filepath.Join(dir, projectLockName),
		backup:  filepath.Join(dir, projectBackupName),
	}
}

// Ensure 幂等地生成/刷新 project.md：hash 未变化时复用现有文件，失败返回 error
// 但不 panic（组合根只 warning，§14.7）。并发由 `.ant/project.lock` 保护。
func (b *builder) Ensure(ctx context.Context, opts EnsureOptions) (Doc, error) {
	if b == nil {
		return Doc{}, model.NewError(model.ErrInternal, "projectctx.nil_builder", "builder 未初始化")
	}
	if err := ctx.Err(); err != nil {
		return Doc{}, err
	}
	workDir, err := b.workDir()
	if err != nil {
		return Doc{}, err
	}
	settings := b.settings()
	paths := pathsFor(workDir)

	// autoInit=false 且非显式 init/Force：只读现有文件，绝不生成（§14.1）。
	if !settings.Project.AutoInit && !opts.Force && !opts.Explicit {
		report(opts.Progress, "skip")
		doc := b.existingDoc(paths)
		b.log.Info("projectctx: project.autoInit=false，跳过自动生成", "workdir", workDir)
		return doc, nil
	}

	report(opts.Progress, "scan")
	scanRes, err := b.newScanner().Scan(ctx, ScanOptions{
		WorkDir:  workDir,
		MaxFiles: settings.Project.MaxFiles,
		MaxBytes: settings.Project.MaxBytes,
		Depth:    settings.Project.Depth,
		Exclude:  settings.Project.Exclude,
		Progress: opts.Progress,
	})
	if err != nil {
		b.log.Warn("projectctx: 扫描 workspace 失败", "workdir", workDir, "error", err)
		return Doc{}, fmt.Errorf("projectctx: 扫描 workspace 失败: %w", err)
	}

	report(opts.Progress, "hash")
	hash, err := b.newHasher(workDir).Hash(ctx, scanRes)
	if err != nil {
		b.log.Warn("projectctx: 计算失效 hash 失败", "workdir", workDir, "error", err)
		return Doc{}, fmt.Errorf("projectctx: 计算失效 hash 失败: %w", err)
	}

	meta, hasMeta, metaErr := loadMeta(paths.meta)
	if metaErr != nil {
		b.log.Warn("projectctx: project.meta.json 不可用，将重新生成", "path", paths.meta, "error", metaErr)
	}
	existing, hasDoc, docErr := loadFile(paths.doc)
	if docErr != nil {
		b.log.Warn("projectctx: 读取现有 project.md 失败", "path", paths.doc, "error", docErr)
		return Doc{}, fmt.Errorf("projectctx: 读取 %s 失败: %w", paths.doc, docErr)
	}

	// hash 未变化且已有内容：不重写（SkipIfFresh 真假行为一致，§14.2）。
	if !opts.Force && hasMeta && meta.Hash == hash && hasDoc && strings.TrimSpace(existing) != "" {
		report(opts.Progress, "skip")
		b.log.Debug("projectctx: hash 未变化，复用现有 project.md", "path", paths.doc, "hash", hash)
		return docFromDisk(paths, existing, &meta), nil
	}

	// 需要写盘：先抢 `.ant/project.lock`；后到进程直接跳过，不覆盖正在写入的文件。
	release, acquired, lockErr := acquireLock(paths.lock)
	if lockErr != nil {
		b.log.Warn("projectctx: 获取 project.lock 失败", "path", paths.lock, "error", lockErr)
		return Doc{}, fmt.Errorf("projectctx: 获取 %s 失败: %w", paths.lock, lockErr)
	}
	if !acquired {
		report(opts.Progress, "skip")
		b.log.Warn("projectctx: 另一个进程正在生成 project.md，跳过本次生成", "lock", paths.lock)
		return b.existingDoc(paths), nil
	}
	defer release()

	// 持锁后复查：期间可能有其它进程已完成同 hash 的生成。
	if !opts.Force {
		if meta2, ok, _ := loadMeta(paths.meta); ok && meta2.Hash == hash {
			if current, curOK, curErr := loadFile(paths.doc); curErr == nil && curOK && strings.TrimSpace(current) != "" {
				report(opts.Progress, "skip")
				b.log.Debug("projectctx: 持锁复查发现内容已刷新，跳过写入", "hash", hash)
				return docFromDisk(paths, current, &meta2), nil
			}
		}
	}

	report(opts.Progress, "generate")
	body, bytesSent, takeover, genErr := b.buildBody(ctx, paths, scanRes, settings, existing, hasDoc, opts.Force)
	if genErr != nil {
		b.log.Warn("projectctx: 生成 project.md 失败", "workdir", workDir, "error", genErr)
		return Doc{}, genErr
	}
	if takeover {
		// 用户接管：不写盘也不更新 meta；hash 继续保持失效，下次仍提示。
		report(opts.Progress, "skip")
		return docFromDisk(paths, existing, metaPtr(hasMeta, meta)), nil
	}

	generatedAt := b.now().UTC().Truncate(time.Millisecond)
	out := metaFile{
		SchemaVersion: metaSchemaVersion,
		GeneratedAt:   generatedAt.Format(metaTimeLayout),
		Model:         deterministicModel,
		GitCommit:     shortCommit(readGitCommit(workDir)),
		Hash:          hash,
		FileCount:     scanRes.FileCount,
		BytesSent:     bytesSent,
	}
	rawMeta, marshalErr := json.MarshalIndent(out, "", "  ")
	if marshalErr != nil {
		b.log.Warn("projectctx: 序列化 project.meta.json 失败", "error", marshalErr)
		return Doc{}, fmt.Errorf("projectctx: 序列化 %s 失败: %w", paths.meta, marshalErr)
	}
	rawMeta = append(rawMeta, '\n')

	report(opts.Progress, "write")
	// 用户删除标记后 Force 会整体覆盖：先把原内容备份为 project.md.bak，
	// 避免 `ant init --force` 静默丢弃手写内容（§14.5 允许二选一）。
	if hasDoc && opts.Force && !hasGeneratedMarkers(existing) {
		if err := atomicWriteFile(paths.backup, []byte(existing), 0o644); err != nil {
			b.log.Warn("projectctx: 备份 project.md 失败", "path", paths.backup, "error", err)
			return Doc{}, fmt.Errorf("projectctx: 备份 %s 失败: %w", paths.backup, err)
		}
	}
	if err := atomicWriteFile(paths.doc, []byte(body), 0o644); err != nil {
		b.log.Warn("projectctx: 写入 project.md 失败", "path", paths.doc, "error", err)
		return Doc{}, fmt.Errorf("projectctx: 写入 %s 失败: %w", paths.doc, err)
	}
	if err := atomicWriteFile(paths.meta, rawMeta, 0o644); err != nil {
		b.log.Warn("projectctx: 写入 project.meta.json 失败", "path", paths.meta, "error", err)
		return Doc{}, fmt.Errorf("projectctx: 写入 %s 失败: %w", paths.meta, err)
	}

	b.log.Debug("projectctx: 已更新 project.md", "path", paths.doc, "hash", hash, "files", scanRes.FileCount)
	return Doc{Path: paths.doc, Body: body, Hash: hash, GeneratedAt: generatedAt, FromModel: false}, nil
}

// buildBody 生成最终文件内容并实现手改保护（§14.5）。
// 返回 (正文, 发送给生成器的关键文件字节数, 是否用户接管, error)。
func (b *builder) buildBody(ctx context.Context, paths projectPaths, res ScanResult, settings config.Settings, existing string, hasDoc, force bool) (string, int, bool, error) {
	maxOutput := settings.Project.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = defaultMaxOutputBytes
	}
	if maxOutput > hardMaxOutputBytes {
		maxOutput = hardMaxOutputBytes
	}
	skeleton := skeletonFrom(res)
	md, bytesSent, err := generateWithin(ctx, b.generator, skeleton, maxOutput)
	if err != nil {
		return "", 0, false, err
	}
	if !hasDoc || strings.TrimSpace(existing) == "" {
		return md, bytesSent, false, nil
	}
	block, ok := generatedBlock(md)
	if !ok {
		return "", 0, false, model.NewError(model.ErrInternal, "projectctx.markers", "生成内容缺少 ant:generated 标记")
	}
	if start, end, ok := markerRange(existing); ok {
		// 只替换标记块，标记外的用户内容原样保留。
		return replaceGeneratedBlock(existing, start, end, block), bytesSent, false, nil
	}
	if !force {
		b.log.Warn("projectctx: project.md 已被手动修改（缺少 ant:generated 标记），不再自动覆盖；`ant init --force` 可重建", "path", paths.doc)
		return existing, 0, true, nil
	}
	b.log.Warn("projectctx: project.md 缺少 ant:generated 标记，--force 将整体覆盖（原内容备份为 project.md.bak）", "path", paths.doc)
	return md, bytesSent, false, nil
}

func (b *builder) workDir() (string, error) {
	if b.cfg == nil {
		return "", model.NewError(model.ErrConfigInvalid, "projectctx.config", "配置视图为空，无法定位 workspace")
	}
	paths, ok := b.cfg.(config.Paths)
	if !ok {
		return "", model.NewError(model.ErrConfigInvalid, "projectctx.workspace", "配置视图未提供 workspace 路径（config.Paths）")
	}
	raw := strings.TrimSpace(paths.WorkDir())
	if raw == "" {
		return "", model.NewError(model.ErrConfigInvalid, "projectctx.workspace", "workspace 路径为空")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		e := model.NewError(model.ErrConfigInvalid, "projectctx.workspace", "workspace 路径不可用")
		e.Cause = err
		return "", e
	}
	return abs, nil
}

func (b *builder) settings() config.Settings {
	if b.cfg == nil {
		return config.Settings{}
	}
	return b.cfg.Settings()
}

func (b *builder) existingDoc(paths projectPaths) Doc {
	body, ok, err := loadFile(paths.doc)
	if err != nil || !ok || strings.TrimSpace(body) == "" {
		return Doc{}
	}
	if meta, hasMeta, metaErr := loadMeta(paths.meta); metaErr == nil && hasMeta {
		return docFromDisk(paths, body, &meta)
	}
	return Doc{Path: paths.doc, Body: body}
}

// --- project.meta.json ---

type metaFile struct {
	SchemaVersion int    `json:"schemaVersion"`
	GeneratedAt   string `json:"generatedAt"`
	Model         string `json:"model"`
	GitCommit     string `json:"gitCommit"`
	Hash          string `json:"hash"`
	FileCount     int    `json:"fileCount"`
	BytesSent     int    `json:"bytesSent"`
}

func loadMeta(path string) (metaFile, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return metaFile{}, false, nil
		}
		return metaFile{}, false, err
	}
	var m metaFile
	if err := json.Unmarshal(raw, &m); err != nil {
		return metaFile{}, false, err
	}
	if m.SchemaVersion != metaSchemaVersion {
		return metaFile{}, false, fmt.Errorf("不支持的 schemaVersion %d", m.SchemaVersion)
	}
	return m, true, nil
}

func metaPtr(ok bool, m metaFile) *metaFile {
	if !ok {
		return nil
	}
	return &m
}

func docFromDisk(paths projectPaths, body string, meta *metaFile) Doc {
	doc := Doc{Path: paths.doc, Body: body}
	if meta == nil {
		return doc
	}
	doc.Hash = meta.Hash
	doc.FromModel = meta.Model != "" && meta.Model != deterministicModel
	if t, err := time.Parse(metaTimeLayout, meta.GeneratedAt); err == nil {
		doc.GeneratedAt = t
	}
	return doc
}

// --- 手改保护 ---

func markerRange(body string) (start, end int, ok bool) {
	startIdx := strings.Index(body, generatedStartMarker)
	endIdx := strings.Index(body, generatedEndMarker)
	if startIdx < 0 || endIdx < 0 || endIdx < startIdx {
		return 0, 0, false
	}
	return startIdx, endIdx + len(generatedEndMarker), true
}

func hasGeneratedMarkers(body string) bool {
	_, _, ok := markerRange(body)
	return ok
}

func generatedBlock(md string) (string, bool) {
	start, end, ok := markerRange(md)
	if !ok {
		return "", false
	}
	return md[start:end], true
}

func replaceGeneratedBlock(existing string, start, end int, block string) string {
	prefix := existing[:start]
	suffix := existing[end:]
	// 只在必要时补换行（纯空白调整），其余用户内容逐字节保留。
	if prefix != "" && !strings.HasSuffix(prefix, "\n") {
		prefix += "\n"
	}
	if suffix != "" && !strings.HasPrefix(suffix, "\n") {
		suffix = "\n" + suffix
	}
	return ensureTrailingNewline(prefix + block + suffix)
}

func ensureTrailingNewline(s string) string {
	if s == "" {
		return s
	}
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// --- 生成与校验 ---

func skeletonFrom(r ScanResult) Skeleton {
	return Skeleton{
		ProjectType: r.ProjectType,
		ModulePath:  r.ModulePath,
		Tree:        append([]string(nil), r.Tree...),
		Entries:     append([]string(nil), r.Entries...),
		Deps:        append([]string(nil), r.Deps...),
		Commands:    r.Commands,
		KeyFiles:    append([]KeyFile(nil), r.KeyFiles...),
	}
}

// generateWithin 生成并校验 Markdown；超过 maxOutputBytes 时用裁剪后的骨架重试，
// 最后退化为最小骨架（§14.3 第 5 步：不合法则退回确定性骨架）。
func generateWithin(ctx context.Context, g Generator, s Skeleton, maxOutput int) (string, int, error) {
	attempts := []Skeleton{
		s,
		trimSkeleton(s, 60, 8, 6),
		trimSkeleton(s, 20, 4, 4),
		trimSkeleton(s, 0, 0, 0),
	}
	var lastErr error
	for i, sk := range attempts {
		md, _, err := g.Generate(ctx, sk)
		if err != nil {
			return "", 0, err
		}
		md = ensureTrailingNewline(strings.TrimRight(md, "\n"))
		if err := validateMarkdown(md); err != nil {
			// 结构错误与输入大小无关，裁剪重试没有意义。
			return "", 0, err
		}
		if maxOutput > 0 && len(md) > maxOutput {
			lastErr = model.NewError(model.ErrInternal, "projectctx.too_large",
				fmt.Sprintf("生成内容 %d 字节，超过上限 %d", len(md), maxOutput))
			if i < len(attempts)-1 {
				continue
			}
			return "", 0, lastErr
		}
		return md, keyFileBytes(sk), nil
	}
	if lastErr == nil {
		lastErr = model.NewError(model.ErrInternal, "projectctx.invalid_output", "生成内容未通过校验")
	}
	return "", 0, lastErr
}

func trimSkeleton(s Skeleton, trees, keyFiles, deps int) Skeleton {
	out := s
	if trees >= 0 && len(out.Tree) > trees {
		out.Tree = out.Tree[:trees]
	}
	if keyFiles >= 0 && len(out.KeyFiles) > keyFiles {
		out.KeyFiles = out.KeyFiles[:keyFiles]
	}
	if deps >= 0 && len(out.Deps) > deps {
		out.Deps = out.Deps[:deps]
	}
	return out
}

func keyFileBytes(s Skeleton) int {
	total := 0
	for _, kf := range s.KeyFiles {
		total += len(kf.Body)
	}
	return total
}

// --- 并发锁与原子写 ---

// acquireLock 用 O_CREATE|O_EXCL 抢占 `.ant/project.lock`。
// 已存在时返回 (nil, false, nil)，调用方跳过本次生成；成功后返回 release。
func acquireLock(path string) (func(), bool, error) {
	return acquireLockRetry(path, true)
}

func acquireLockRetry(path string, allowStaleRecovery bool) (func(), bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, false, err
		}
		if allowStaleRecovery {
			if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
				if rmErr := os.Remove(path); rmErr == nil {
					// 陈旧锁只清理并重试一次。
					return acquireLockRetry(path, false)
				}
			}
		}
		return nil, false, nil
	}
	_, _ = fmt.Fprintf(f, "pid=%d\ncreatedAt=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, false, err
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = os.Remove(path)
	}, true, nil
}

// atomicWriteFile 写临时文件后 rename，保证读者不会看到半截内容。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
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
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func loadFile(path string) (string, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(raw), true, nil
}

func report(progress func(stage string), stage string) {
	if progress == nil {
		return
	}
	progress(stage)
}
