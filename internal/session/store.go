package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xiws/ant/internal/model"
)

// store 是文件系统实现（v1，不引入数据库）。
type store struct {
	workDir string
	root    string
	clock   func() time.Time
	locker  *fileLocker
}

// NewStore 打开 <workspace>/.ant/sessions 下的文件系统 store。
func NewStore(workDir string) Store {
	return NewStoreWithClock(workDir, time.Now)
}

// NewStoreWithClock 与 NewStore 相同，但允许注入时钟（测试用）。
func NewStoreWithClock(workDir string, clock func() time.Time) Store {
	if clock == nil {
		clock = time.Now
	}
	abs, err := filepath.Abs(workDir)
	if err != nil || abs == "" {
		abs = workDir
	}
	abs = filepath.Clean(abs)
	root := filepath.Join(abs, ".ant", "sessions")
	return &store{
		workDir: abs,
		root:    root,
		clock:   clock,
		locker:  newFileLocker(root, clock),
	}
}

// Acquire 让 store 本身也可作为 Locker 使用。
func (s *store) Acquire(ctx context.Context, id string) (Lease, error) {
	return s.locker.acquire(ctx, id)
}

// Open 打开或新建一个 session；取锁发生在任何事件写入之前。
func (s *store) Open(ctx context.Context, opts OpenOptions) (Session, Opened, error) {
	if err := ctx.Err(); err != nil {
		return nil, Opened{}, contextErr(ctx)
	}
	if err := secureMkdirAll(s.root); err != nil {
		return nil, Opened{}, sessionErr(model.ErrInternal, "session_root_failed",
			"无法创建 sessions 目录", "检查 workspace 写权限", err)
	}

	id := strings.TrimSpace(opts.ID)
	created := false
	var dir string

	switch {
	case id == "":
		if !opts.Create {
			return nil, Opened{}, sessionErr(model.ErrConfigInvalid, "session_id_required",
				"未提供 session ID 且 Create=false", "新建会话请传 Create=true，续跑请传唯一 ID 前缀", nil)
		}
		var err error
		id, dir, err = s.createSessionDir()
		if err != nil {
			return nil, Opened{}, err
		}
		created = true
	default:
		// 显式 ID 的续跑，不要求 Create；仍按唯一前缀解析。
		var err error
		id, dir, err = s.resolveSessionID(id)
		if err != nil {
			return nil, Opened{}, err
		}
	}

	artifactsDir := filepath.Join(dir, "artifacts")
	if err := secureMkdirAll(artifactsDir); err != nil {
		if created {
			_ = os.RemoveAll(dir)
		}
		return nil, Opened{}, sessionErr(model.ErrInternal, "session_artifacts_dir_failed",
			"无法创建 artifacts 目录", "检查 session 目录写权限", err)
	}

	var lease Lease
	if opts.Lock {
		l, err := s.locker.acquire(ctx, id)
		if err != nil {
			if created {
				_ = os.RemoveAll(dir)
			}
			return nil, Opened{}, err
		}
		lease = l
	}

	meta, _, err := s.loadOrCreateMeta(dir, id, opts, created)
	if err != nil {
		if lease != nil {
			_ = lease.Release()
		}
		if created {
			_ = os.RemoveAll(dir)
		}
		return nil, Opened{}, err
	}

	eventsPath := filepath.Join(dir, "events.jsonl")
	events, warns, err := readEventsFile(eventsPath)
	if err != nil {
		if lease != nil {
			_ = lease.Release()
		}
		if created {
			_ = os.RemoveAll(dir)
		}
		return nil, Opened{}, sessionErr(model.ErrInternal, "session_events_read_failed",
			"无法读取 events.jsonl", "检查 session 目录与文件权限", err)
	}

	// 事件文件是唯一事实来源：lastSeq 严格取完整事件的最大 seq。
	lastSeq := maxEventSeq(events)
	meta.LastSeq = lastSeq

	sess := &fsSession{
		store:       s,
		dir:         dir,
		id:          id,
		meta:        meta,
		lastSeq:     lastSeq,
		currentTurn: meta.TurnCount,
		lease:       lease,
		warnings:    warns,
	}
	// lock 接管等 warning 通过 Lease 的扩展方法传给恢复层。
	if w, ok := lease.(interface{ Warnings() []string }); ok {
		for _, msg := range w.Warnings() {
			sess.addWarning(msg)
		}
	}

	return sess, Opened{ID: id, Created: created, Resumed: !created}, nil
}

// createSessionDir 生成不冲突的 ID 并创建 0700 目录。
func (s *store) createSessionDir() (string, string, error) {
	var lastErr error
	for attempt := 0; attempt < 32; attempt++ {
		id, err := newSessionID(s.clock())
		if err != nil {
			return "", "", err
		}
		dir := filepath.Join(s.root, id)
		if err := os.Mkdir(dir, dirPerm); err != nil {
			if os.IsExist(err) {
				continue
			}
			lastErr = err
			break
		}
		_ = os.Chmod(dir, dirPerm)
		return id, dir, nil
	}
	return "", "", sessionErr(model.ErrInternal, "session_create_failed",
		"无法创建 session 目录", "检查 .ant/sessions 的写权限", lastErr)
}

// resolveSessionID 按唯一前缀匹配已有 session；完全相等优先于前缀。
func (s *store) resolveSessionID(prefix string) (string, string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", "", sessionErr(model.ErrConfigInvalid, "session_id_required",
			"session ID 前缀不能为空", "使用 ant sessions 查看可用 ID", nil)
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", sessionNotFound(prefix)
		}
		return "", "", sessionErr(model.ErrInternal, "session_list_failed",
			"无法读取 sessions 目录", "检查 .ant/sessions 权限", err)
	}
	matches := make([]string, 0, 2)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == prefix {
			return name, filepath.Join(s.root, name), nil
		}
		if strings.HasPrefix(name, prefix) {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return "", "", sessionNotFound(prefix)
	case 1:
		return matches[0], filepath.Join(s.root, matches[0]), nil
	default:
		shown := matches
		if len(shown) > 5 {
			shown = shown[:5]
		}
		return "", "", &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "session_ambiguous",
			Message: fmt.Sprintf("session ID 前缀 %q 匹配到 %d 个会话", prefix, len(matches)),
			Hint:    "提供更长的唯一前缀，或使用 ant sessions 查看完整 ID",
			Details: map[string]any{"matches": shown, "count": len(matches)},
		}
	}
}

func sessionNotFound(prefix string) *model.Error {
	return &model.Error{
		Class:   model.ErrConfigInvalid,
		Code:    "session_not_found",
		Message: fmt.Sprintf("找不到匹配 %q 的 session", prefix),
		Hint:    "使用 ant sessions 查看当前 workspace 的会话",
	}
}

// loadOrCreateMeta 读取 meta.json；缺失时创建并落盘。
func (s *store) loadOrCreateMeta(dir, id string, opts OpenOptions, created bool) (Meta, bool, error) {
	metaPath := filepath.Join(dir, "meta.json")
	meta, exists, err := readMetaFile(metaPath)
	if err != nil {
		return Meta{}, false, sessionErr(model.ErrInternal, "session_meta_corrupt",
			"meta.json 无法解析", "检查或从备份恢复 meta.json", err)
	}
	if exists && meta.ID != "" && meta.ID != id {
		return Meta{}, false, sessionErr(model.ErrConfigInvalid, "session_meta_mismatch",
			fmt.Sprintf("meta.json 的 id=%q 与目录名 %q 不一致", meta.ID, id),
			"确认 session 未被手工移动或修改", nil)
	}

	now := s.clock().UTC()
	needWrite := !exists
	if !exists {
		meta = Meta{
			SchemaVersion: schemaVersion,
			ID:            id,
			Workspace:     opts.Workspace,
			CreatedAt:     now,
			UpdatedAt:     now,
			Status:        StatusIdle,
			Mode:          opts.Mode,
			Model:         opts.Model,
		}
	}
	if meta.SchemaVersion == 0 {
		meta.SchemaVersion = schemaVersion
		needWrite = true
	}
	meta.ID = id
	if meta.Workspace == "" {
		if opts.Workspace != "" {
			meta.Workspace = opts.Workspace
		} else {
			meta.Workspace = s.workDir
		}
		needWrite = true
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
		needWrite = true
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = meta.CreatedAt
		needWrite = true
	}
	if meta.Status == "" {
		meta.Status = StatusIdle
		needWrite = true
	}
	if meta.Mode == "" && opts.Mode != "" {
		meta.Mode = opts.Mode
		needWrite = true
	}
	if meta.Model == "" && opts.Model != "" {
		meta.Model = opts.Model
		needWrite = true
	}
	if needWrite || created {
		if err := writeMetaFile(dir, meta); err != nil {
			return Meta{}, false, sessionErr(model.ErrInternal, "session_meta_write_failed",
				"无法写入 meta.json", "检查 session 目录写权限", err)
		}
	}
	return meta, !exists, nil
}

// readMetaFile 读取 meta.json；返回 exists=false 表示文件不存在。
func readMetaFile(path string) (Meta, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Meta{}, false, nil
		}
		return Meta{}, false, err
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return Meta{}, true, err
	}
	return meta, true, nil
}

func writeMetaFile(dir string, meta Meta) error {
	return writeJSONAtomic(filepath.Join(dir, "meta.json"), meta, filePerm)
}

// List 读取各 meta.json 并投影 Summary，按 UpdatedAt 倒序。
func (s *store) List(ctx context.Context, f ListFilter) ([]Summary, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextErr(ctx)
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return []Summary{}, nil
		}
		return nil, sessionErr(model.ErrInternal, "session_list_failed",
			"无法读取 sessions 目录", "检查 .ant/sessions 权限", err)
	}
	summaries := make([]Summary, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, contextErr(ctx)
		}
		dir := filepath.Join(s.root, entry.Name())
		meta, exists, err := readMetaFile(filepath.Join(dir, "meta.json"))
		if err != nil || !exists {
			// 正在进行中的新会话可能尚未落 meta；列表跳过而不是失败。
			continue
		}
		if meta.ID == "" {
			meta.ID = entry.Name()
		}
		if f.Mode != "" && meta.Mode != f.Mode {
			continue
		}
		if f.Status != "" && meta.Status != f.Status {
			continue
		}
		summary := Summary{Meta: meta, Tokens: meta.Usage.Input + meta.Usage.Output}
		if events, _, err := readEventsFile(filepath.Join(dir, "events.jsonl")); err == nil {
			summary.LastEvent = lastEventType(events)
		}
		summaries = append(summaries, summary)
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		ti, tj := summaries[i].UpdatedAt, summaries[j].UpdatedAt
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return summaries[i].ID > summaries[j].ID
	})
	if f.Limit > 0 && len(summaries) > f.Limit {
		summaries = summaries[:f.Limit]
	}
	return summaries, nil
}

// Latest 返回最近更新的 session；没有会话时返回明确的 no_sessions 错误。
func (s *store) Latest(ctx context.Context) (Summary, error) {
	list, err := s.List(ctx, ListFilter{})
	if err != nil {
		return Summary{}, err
	}
	if len(list) == 0 {
		return Summary{}, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "no_sessions",
			Message: "当前 workspace 还没有任何 session",
			Hint:    "运行 ant 开始一个新会话",
		}
	}
	return list[0], nil
}

// Show 返回单个 session 的只读详情。
func (s *store) Show(ctx context.Context, idPrefix string) (Detail, error) {
	if err := ctx.Err(); err != nil {
		return Detail{}, contextErr(ctx)
	}
	id, dir, err := s.resolveSessionID(idPrefix)
	if err != nil {
		return Detail{}, err
	}
	meta, exists, err := readMetaFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return Detail{}, sessionErr(model.ErrInternal, "session_meta_corrupt",
			"meta.json 无法解析", "检查或从备份恢复 meta.json", err)
	}
	if !exists {
		return Detail{}, sessionNotFound(idPrefix)
	}
	if meta.ID == "" {
		meta.ID = id
	}
	events, _, err := readEventsFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return Detail{}, sessionErr(model.ErrInternal, "session_events_read_failed",
			"无法读取 events.jsonl", "检查 session 目录与文件权限", err)
	}
	detail := Detail{
		Summary: Summary{
			Meta:      meta,
			Tokens:    meta.Usage.Input + meta.Usage.Output,
			LastEvent: lastEventType(events),
		},
	}
	for _, ev := range events {
		if ev.Type != model.EventApprovalResolved {
			continue
		}
		var data struct {
			Class    string `json:"class"`
			Decision string `json:"decision"`
			Scope    string `json:"scope"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			continue
		}
		detail.Approvals = append(detail.Approvals, ApprovalRecord{
			Class:    model.ApprovalClass(data.Class),
			Decision: data.Decision,
			Scope:    data.Scope,
			At:       ev.TS,
		})
	}
	arts, err := listArtifacts(dir, id)
	if err != nil {
		return Detail{}, sessionErr(model.ErrInternal, "session_artifacts_read_failed",
			"无法列出 artifacts 目录", "检查 artifacts/ 权限", err)
	}
	detail.Artifacts = arts
	detail.PlanPath = findPlanPath(dir)
	return detail, nil
}

// listArtifacts 列出 artifacts/ 下的常规文件（不跟随符号链接）。
func listArtifacts(dir, id string) ([]model.ArtifactRef, error) {
	root := filepath.Join(dir, "artifacts")
	refs := make([]model.ArtifactRef, 0)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		body, readErr := os.ReadFile(path)
		sha := ""
		if readErr == nil {
			sha = hashOf(body)
		}
		refs = append(refs, model.ArtifactRef{
			SessionID: id,
			Name:      filepath.ToSlash(rel),
			Bytes:     info.Size(),
			SHA256:    sha,
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// findPlanPath 返回 plan.md 的绝对路径；不存在时返回空串。
func findPlanPath(dir string) string {
	candidates := []string{
		filepath.Join(dir, "plan.md"),
		filepath.Join(dir, "artifacts", "plan.md"),
	}
	for _, candidate := range candidates {
		if info, err := os.Lstat(candidate); err == nil && info.Mode().IsRegular() {
			abs, absErr := filepath.Abs(candidate)
			if absErr == nil {
				return abs
			}
			return candidate
		}
	}
	return ""
}

// ensure interface conformance.
var _ Store = (*store)(nil)
var _ Locker = (*store)(nil)
