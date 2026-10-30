package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/xiws/ant/internal/model"
)

// fsSession 是 Session 的文件系统实现。
type fsSession struct {
	store *store
	dir   string
	id    string

	mu          sync.Mutex
	meta        Meta
	lastSeq     int64
	currentTurn int
	events      *os.File
	lease       Lease
	closed      bool

	warnings []string
	warnSeen map[string]struct{}
}

// Meta 返回 meta 的副本。
func (s *fsSession) Meta() Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneMeta(s.meta)
}

// UpdateMeta 通过「临时文件 → fsync → rename」原子更新 meta.json。
func (s *fsSession) UpdateMeta(ctx context.Context, mutate func(*Meta) error) error {
	if err := ctx.Err(); err != nil {
		return contextErr(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sessionClosedError(s.id)
	}
	meta := cloneMeta(s.meta)
	if mutate != nil {
		if err := mutate(&meta); err != nil {
			return err
		}
	}
	meta.ID = s.id
	if meta.SchemaVersion == 0 {
		meta.SchemaVersion = schemaVersion
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = s.meta.CreatedAt
	}
	meta.LastSeq = s.lastSeq
	meta.UpdatedAt = s.store.clock().UTC()
	if err := writeMetaFile(s.dir, meta); err != nil {
		return sessionErr(model.ErrInternal, "session_meta_write_failed",
			"无法写入 meta.json", "检查 session 目录写权限", err)
	}
	s.meta = meta
	return nil
}

// Emit 分配 seq/ts，整行追加写入并 Sync；失败必须由调用方视为致命。
func (s *fsSession) Emit(ctx context.Context, ev *model.Event) error {
	if err := ctx.Err(); err != nil {
		return contextErr(ctx)
	}
	if ev == nil {
		return sessionErr(model.ErrInternal, "event_nil", "不能写入 nil 事件", "", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sessionClosedError(s.id)
	}

	seq := s.lastSeq + 1
	ts := s.store.clock().UTC()
	ev.Seq = seq
	ev.TS = ts
	// 节点/模型事件不携带 turn；由 session 用最近一个 turn_started 补齐，
	// 保证 events.jsonl 中同一 turn 的事件 turn 字段一致（§9.3）。
	if ev.Type == model.EventTurnStarted && ev.Turn > 0 {
		s.currentTurn = ev.Turn
	} else if ev.Turn == 0 && s.currentTurn > 0 {
		ev.Turn = s.currentTurn
	}

	line, err := json.Marshal(ev)
	if err != nil {
		return sessionErr(model.ErrInternal, "event_marshal_failed",
			fmt.Sprintf("事件 seq=%d 序列化失败", seq), "事件 data 必须是合法 JSON", err)
	}
	line = append(line, '\n')

	if s.events == nil {
		path := filepath.Join(s.dir, "events.jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, filePerm)
		if err != nil {
			return sessionErr(model.ErrInternal, "event_open_failed",
				"无法打开 events.jsonl", "检查 session 目录写权限", err)
		}
		_ = f.Chmod(filePerm)
		// 上一次崩溃可能留下没有换行的半行；不要回写旧内容，只在追加前补分隔换行，
		// 否则新事件会与半行拼接成一条新的坏行。
		if info, statErr := f.Stat(); statErr == nil && info.Size() > 0 {
			var last [1]byte
			if _, readErr := f.ReadAt(last[:], info.Size()-1); readErr == nil && last[0] != '\n' {
				if _, writeErr := f.Write([]byte{'\n'}); writeErr != nil {
					_ = f.Close()
					return sessionErr(model.ErrInternal, "event_write_failed",
						"无法为 events.jsonl 的不完整尾行补分隔符", "Emit 失败是致命的，中止当前 turn", writeErr)
				}
			}
		}
		s.events = f
	}
	n, err := s.events.Write(line)
	if err != nil {
		return sessionErr(model.ErrInternal, "event_write_failed",
			fmt.Sprintf("写入事件 seq=%d 失败", seq), "Emit 失败是致命的，中止当前 turn", err)
	}
	if n != len(line) {
		return sessionErr(model.ErrInternal, "event_write_failed",
			fmt.Sprintf("写入事件 seq=%d 不完整（%d/%d 字节）", seq, n, len(line)),
			"Emit 失败是致命的，中止当前 turn", nil)
	}
	if err := s.events.Sync(); err != nil {
		return sessionErr(model.ErrInternal, "event_sync_failed",
			fmt.Sprintf("同步事件 seq=%d 失败", seq), "Emit 失败是致命的，中止当前 turn", err)
	}

	s.lastSeq = seq
	s.meta.LastSeq = seq
	s.meta.UpdatedAt = ts
	return nil
}

// PutArtifact 以「临时文件 → fsync → rename」原子写入 session 目录内的 artifact。
func (s *fsSession) PutArtifact(ctx context.Context, name string, data []byte, mediaType string) (model.ArtifactRef, error) {
	_ = mediaType // v1 只按扩展名/调用方约定解释内容，不额外持久化 media type。
	if err := ctx.Err(); err != nil {
		return model.ArtifactRef{}, contextErr(ctx)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return model.ArtifactRef{}, sessionClosedError(s.id)
	}
	id := s.meta.ID
	s.mu.Unlock()

	rel, err := normalizeArtifactName(name)
	if err != nil {
		return model.ArtifactRef{}, err
	}
	full := filepath.Join(s.dir, filepath.FromSlash(rel))
	if !withinDir(s.dir, full) {
		return model.ArtifactRef{}, artifactNameError("artifact 名称逃逸 session 目录")
	}
	if err := secureMkdirAll(filepath.Dir(full)); err != nil {
		return model.ArtifactRef{}, sessionErr(model.ErrInternal, "artifact_dir_failed",
			fmt.Sprintf("无法创建 artifact 目录 %q", filepath.Dir(rel)), "检查 session 目录写权限", err)
	}
	if err := writeFileAtomic(full, data, filePerm); err != nil {
		return model.ArtifactRef{}, sessionErr(model.ErrInternal, "artifact_write_failed",
			fmt.Sprintf("写入 artifact %q 失败", rel), "检查磁盘空间与 session 目录权限", err)
	}
	return model.ArtifactRef{
		SessionID: id,
		Name:      rel,
		Bytes:     int64(len(data)),
		SHA256:    hashOf(data),
	}, nil
}

// Events 逐行读取 events.jsonl；不完整尾行丢弃并 warning，不回写。
func (s *fsSession) Events(ctx context.Context) ([]model.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextErr(ctx)
	}
	events, warnings, err := readEventsFile(filepath.Join(s.dir, "events.jsonl"))
	if err != nil {
		return nil, sessionErr(model.ErrInternal, "session_events_read_failed",
			"无法读取 events.jsonl", "检查 session 目录与文件权限", err)
	}
	for _, warning := range warnings {
		s.addWarning(warning)
	}
	return events, nil
}

// Artifact 读取 session 目录内的 artifact；路径与符号链接都不得逃逸。
func (s *fsSession) Artifact(ref model.ArtifactRef) ([]byte, error) {
	rel, err := cleanRelativeName(ref.Name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	id := s.meta.ID
	s.mu.Unlock()
	if ref.SessionID != "" && id != "" && ref.SessionID != id {
		return nil, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "artifact_session_mismatch",
			Message: fmt.Sprintf("artifact %q 属于 session %q，不能从 %q 读取", ref.Name, ref.SessionID, id),
			Hint:    "只读取当前 session 的 artifact",
		}
	}

	full := filepath.Join(s.dir, filepath.FromSlash(rel))
	if !withinDir(s.dir, full) {
		return nil, artifactNameError("artifact 路径逃逸 session 目录")
	}
	rootReal, err := filepath.EvalSymlinks(s.dir)
	if err != nil {
		return nil, sessionErr(model.ErrInternal, "artifact_read_failed",
			"无法解析 session 目录", "检查 session 目录权限", err)
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, artifactNotFound(s.id, rel)
		}
		return nil, sessionErr(model.ErrInternal, "artifact_read_failed",
			fmt.Sprintf("无法读取 artifact %q", rel), "检查文件权限", err)
	}
	if !withinDir(rootReal, real) {
		return nil, artifactNameError("artifact 符号链接逃逸 session 目录")
	}
	info, err := os.Stat(real)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, artifactNotFound(s.id, rel)
		}
		return nil, sessionErr(model.ErrInternal, "artifact_read_failed",
			fmt.Sprintf("无法读取 artifact %q", rel), "检查文件权限", err)
	}
	if info.IsDir() {
		return nil, sessionErr(model.ErrInternal, "artifact_is_dir",
			fmt.Sprintf("artifact %q 是目录", rel), "artifact 必须是文件", nil)
	}
	body, err := os.ReadFile(real)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, artifactNotFound(s.id, rel)
		}
		return nil, sessionErr(model.ErrInternal, "artifact_read_failed",
			fmt.Sprintf("无法读取 artifact %q", rel), "检查文件权限", err)
	}
	return body, nil
}

func artifactNotFound(sessionID, name string) *model.Error {
	return &model.Error{
		Class:   model.ErrInternal,
		Code:    "artifact_not_found",
		Message: fmt.Sprintf("session %s 缺少 artifact %q", sessionID, name),
		Hint:    "artifact 可能被手工删除；恢复会记录 warning 并用占位内容继续",
	}
}

// Close 停止心跳、释放锁、落盘 meta 并关闭事件文件；可重复调用。
func (s *fsSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	meta := cloneMeta(s.meta)
	meta.LastSeq = s.lastSeq
	meta.UpdatedAt = s.store.clock().UTC()
	if meta.Status == "" {
		meta.Status = StatusIdle
	}
	file := s.events
	s.events = nil
	lease := s.lease
	s.lease = nil
	s.mu.Unlock()

	var firstErr error
	if err := writeMetaFile(s.dir, meta); err != nil {
		firstErr = sessionErr(model.ErrInternal, "session_meta_write_failed",
			"关闭时无法写入 meta.json", "检查 session 目录写权限", err)
	} else {
		s.mu.Lock()
		s.meta = meta
		s.mu.Unlock()
	}
	if file != nil {
		_ = file.Sync()
		if err := file.Close(); err != nil && firstErr == nil {
			firstErr = sessionErr(model.ErrInternal, "event_close_failed",
				"关闭 events.jsonl 失败", "检查文件系统状态", err)
		}
	}
	if lease != nil {
		if err := lease.Release(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Warnings 返回打开/读取过程中累积的 warning（供 Recover 汇总）。
func (s *fsSession) Warnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.warnings) == 0 {
		return nil
	}
	out := make([]string, len(s.warnings))
	copy(out, s.warnings)
	return out
}

func (s *fsSession) addWarning(msg string) {
	if msg == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.warnSeen == nil {
		s.warnSeen = make(map[string]struct{})
	}
	if _, exists := s.warnSeen[msg]; exists {
		return
	}
	s.warnSeen[msg] = struct{}{}
	s.warnings = append(s.warnings, msg)
}

func sessionClosedError(id string) *model.Error {
	return &model.Error{
		Class:   model.ErrInternal,
		Code:    "session_closed",
		Message: fmt.Sprintf("session %s 已关闭", id),
		Hint:    "关闭后的 session 不可再写入事件或 artifact",
	}
}

func cloneMeta(meta Meta) Meta {
	cp := meta
	if meta.PromptVersions != nil {
		cp.PromptVersions = make(map[string]string, len(meta.PromptVersions))
		for k, v := range meta.PromptVersions {
			cp.PromptVersions[k] = v
		}
	}
	return cp
}

var _ Session = (*fsSession)(nil)
var _ Reader = (*fsSession)(nil)
var _ Recorder = (*fsSession)(nil)
