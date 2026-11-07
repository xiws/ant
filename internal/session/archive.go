package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/xiws/ant/internal/model"
)

// 只读归档的上限（§22.6.2、§22.10.3）。这些值只影响 Web 展示，
// 不改变 CLI 的读写行为。
const (
	// MaxArchiveEventsBytes 是单个 session 事件文件的可读上限。
	MaxArchiveEventsBytes int64 = 64 << 20
	// MaxArchiveEvents 是单个 session 可投影的事件条数上限。
	MaxArchiveEvents = 50000
	// MaxArchiveArtifactBytes 是单个 artifact 的读取上限。
	MaxArchiveArtifactBytes int64 = 8 << 20
)

// sessionIDPattern 是 session 目录名的完整格式（§9.1）。Web 使用完整 ID，
// 不做前缀歧义解析。
var sessionIDPattern = regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{6}$`)

// LockState 是 session 锁的只读投影。
type LockState struct {
	Present     bool      `json:"present"`
	Live        bool      `json:"live"`
	Stale       bool      `json:"stale"`
	PID         int       `json:"pid,omitempty"`
	Host        string    `json:"host,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	HeartbeatAt time.Time `json:"heartbeatAt,omitempty"`
}

// Snapshot 是一次只读读取的完整结果。
type Snapshot struct {
	Meta     Meta
	Events   []model.Event
	Warnings []string
	Lock     LockState
}

// Archive 是 Web/审计使用的只读视图；不取锁、不创建目录、不写文件。
type Archive interface {
	List(ctx context.Context, f ListFilter) ([]Summary, error)
	Snapshot(ctx context.Context, id string) (Snapshot, error)
	Artifact(ctx context.Context, id string, ref model.ArtifactRef) ([]byte, error)
}

// LockReporter 是 Archive 的可选扩展：批量返回锁状态，供列表展示。
type LockReporter interface {
	Locks(ctx context.Context, ids []string) (map[string]LockState, error)
}

// archive 是 Archive 的文件系统实现。
type archive struct {
	store            *store
	clock            func() time.Time
	staleAfter       time.Duration
	maxEventsBytes   int64
	maxEvents        int
	maxArtifactBytes int64
}

// NewArchive 打开 <workspace>/.ant/sessions 下的只读归档。
func NewArchive(workDir string) Archive {
	return newArchive(workDir, time.Now)
}

func newArchive(workDir string, clock func() time.Time) *archive {
	if clock == nil {
		clock = time.Now
	}
	return &archive{
		store:            newStore(workDir, clock),
		clock:            clock,
		staleAfter:       staleAfter,
		maxEventsBytes:   MaxArchiveEventsBytes,
		maxEvents:        MaxArchiveEvents,
		maxArtifactBytes: MaxArchiveArtifactBytes,
	}
}

// List 复用只读 store.List；不会调用 Open，因此不创建任何目录。
func (a *archive) List(ctx context.Context, f ListFilter) ([]Summary, error) {
	return a.store.List(ctx, f)
}

// Locks 返回给定 session 的锁状态；缺失/损坏的锁按 stale 处理。
func (a *archive) Locks(ctx context.Context, ids []string) (map[string]LockState, error) {
	out := make(map[string]LockState, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, contextErr(ctx)
		}
		if !validArchiveID(id) {
			continue
		}
		dir := filepath.Join(a.store.root, id)
		if !withinDir(a.store.root, dir) {
			continue
		}
		out[id] = a.lockState(dir)
	}
	return out, nil
}

// Snapshot 精确读取一个 session 的 meta、events 与锁状态。
func (a *archive) Snapshot(ctx context.Context, id string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, contextErr(ctx)
	}
	id = strings.TrimSpace(id)
	if !validArchiveID(id) {
		return Snapshot{}, invalidSessionIDError(id)
	}
	dir := filepath.Join(a.store.root, id)
	if !withinDir(a.store.root, dir) {
		return Snapshot{}, invalidSessionIDError(id)
	}

	rootReal, err := filepath.EvalSymlinks(a.store.root)
	if err != nil {
		if os.IsNotExist(err) {
			return Snapshot{}, sessionNotFound(id)
		}
		return Snapshot{}, sessionErr(model.ErrInternal, "session_root_failed",
			"无法解析 sessions 目录", "检查 .ant/sessions 权限", err)
	}
	dirReal, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return Snapshot{}, sessionNotFound(id)
		}
		return Snapshot{}, sessionErr(model.ErrInternal, "session_read_failed",
			fmt.Sprintf("无法解析 session %s 目录", id), "检查 session 目录权限", err)
	}
	if !withinDir(rootReal, dirReal) {
		return Snapshot{}, invalidSessionIDError(id)
	}
	info, err := os.Stat(dirReal)
	if err != nil || !info.IsDir() {
		return Snapshot{}, sessionNotFound(id)
	}

	var warnings []string
	meta, exists, err := readMetaFile(filepath.Join(dirReal, "meta.json"))
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("meta.json 无法解析，已按目录名兜底：%v", err))
		meta = Meta{}
		exists = false
	}
	if !exists {
		warnings = append(warnings, "meta.json 缺失，已按目录名与会话文件兜底")
	}
	if meta.ID != "" && meta.ID != id {
		warnings = append(warnings, fmt.Sprintf("meta.json 的 id=%q 与目录名 %q 不一致，以目录名为准", meta.ID, id))
	}
	// 目录名是路径解析的唯一依据；避免 meta 中的旧 ID 破坏 artifact URL。
	meta.ID = id
	if meta.SchemaVersion == 0 {
		meta.SchemaVersion = schemaVersion
	}
	if meta.Workspace == "" {
		meta.Workspace = a.store.workDir
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = info.ModTime().UTC()
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = info.ModTime().UTC()
	}
	if meta.Status == "" {
		meta.Status = StatusIdle
	}

	eventsPath := filepath.Join(dirReal, "events.jsonl")
	if evInfo, statErr := os.Stat(eventsPath); statErr == nil && evInfo.Size() > a.maxEventsBytes {
		return Snapshot{}, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "session_too_large",
			Message: fmt.Sprintf("session %s 的事件文件 %d 字节，超过 Web 读取上限 %d", id, evInfo.Size(), a.maxEventsBytes),
			Hint:    "使用 ant show --events 或 CLI 排查；Web 一期不加载超大 session",
		}
	}
	events, readWarnings, err := readEventsFile(eventsPath)
	if err != nil {
		return Snapshot{}, sessionErr(model.ErrInternal, "session_events_read_failed",
			"无法读取 events.jsonl", "检查 session 目录与文件权限", err)
	}
	warnings = append(warnings, readWarnings...)
	if len(events) > a.maxEvents {
		return Snapshot{}, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "session_too_large",
			Message: fmt.Sprintf("session %s 有 %d 条事件，超过 Web 读取上限 %d", id, len(events), a.maxEvents),
			Hint:    "使用 ant show --events 或 CLI 排查；Web 一期不加载超大 session",
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })

	if meta.LastSeq < maxEventSeq(events) {
		meta.LastSeq = maxEventSeq(events)
	}
	if meta.Mode == "" {
		if m := modeFromEvents(events); m != "" {
			meta.Mode = m
		}
	}
	if meta.TurnCount == 0 {
		meta.TurnCount = turnCountFromEvents(events)
	}

	return Snapshot{
		Meta:     meta,
		Events:   events,
		Warnings: warnings,
		Lock:     a.lockState(dirReal),
	}, nil
}

// Artifact 只读 session 的 artifacts/ 下的文件；路径、符号链接与大小都有边界。
func (a *archive) Artifact(ctx context.Context, id string, ref model.ArtifactRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextErr(ctx)
	}
	id = strings.TrimSpace(id)
	if !validArchiveID(id) {
		return nil, invalidSessionIDError(id)
	}
	if ref.SessionID != "" && ref.SessionID != id {
		return nil, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "artifact_session_mismatch",
			Message: fmt.Sprintf("artifact %q 属于 session %q，不能从 %q 读取", ref.Name, ref.SessionID, id),
			Hint:    "只读取当前 session 的 artifact",
		}
	}
	cleaned, err := cleanRelativeName(ref.Name)
	if err != nil {
		return nil, err
	}
	if cleaned != "artifacts" && !strings.HasPrefix(cleaned, "artifacts/") {
		return nil, artifactNameError("artifact 名称必须以 artifacts/ 开头")
	}
	if cleaned == "artifacts" {
		return nil, artifactNameError("artifact 名称不能是目录")
	}

	dir := filepath.Join(a.store.root, id)
	if !withinDir(a.store.root, dir) {
		return nil, invalidSessionIDError(id)
	}
	full := filepath.Join(dir, filepath.FromSlash(cleaned))
	if !withinDir(dir, full) {
		return nil, artifactNameError("artifact 路径逃逸 session 目录")
	}

	rootReal, err := filepath.EvalSymlinks(a.store.root)
	if err != nil {
		return nil, sessionErr(model.ErrInternal, "artifact_read_failed",
			"无法解析 sessions 目录", "检查 .ant/sessions 权限", err)
	}
	dirReal, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, sessionNotFound(id)
		}
		return nil, sessionErr(model.ErrInternal, "artifact_read_failed",
			fmt.Sprintf("无法解析 session %s 目录", id), "检查目录权限", err)
	}
	if !withinDir(rootReal, dirReal) {
		return nil, invalidSessionIDError(id)
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, archiveArtifactNotFound(id, cleaned)
		}
		return nil, sessionErr(model.ErrInternal, "artifact_read_failed",
			fmt.Sprintf("无法读取 artifact %q", cleaned), "检查文件权限", err)
	}
	if !withinDir(dirReal, real) {
		return nil, artifactNameError("artifact 符号链接逃逸 session 目录")
	}
	info, err := os.Stat(real)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, archiveArtifactNotFound(id, cleaned)
		}
		return nil, sessionErr(model.ErrInternal, "artifact_read_failed",
			fmt.Sprintf("无法读取 artifact %q", cleaned), "检查文件权限", err)
	}
	if info.IsDir() {
		return nil, artifactNameError("artifact 必须是文件")
	}
	if info.Size() > a.maxArtifactBytes {
		return nil, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "artifact_too_large",
			Message: fmt.Sprintf("artifact %q 有 %d 字节，超过 Web 读取上限 %d", cleaned, info.Size(), a.maxArtifactBytes),
			Hint:    "用 CLI 或系统工具查看该文件",
			Details: map[string]any{"bytes": info.Size(), "limit": a.maxArtifactBytes},
		}
	}
	body, err := os.ReadFile(real)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, archiveArtifactNotFound(id, cleaned)
		}
		return nil, sessionErr(model.ErrInternal, "artifact_read_failed",
			fmt.Sprintf("无法读取 artifact %q", cleaned), "检查文件权限", err)
	}
	return body, nil
}

func (a *archive) lockState(dir string) LockState {
	owner, exists, err := readLockOwner(filepath.Join(dir, "lock"))
	if !exists {
		return LockState{}
	}
	state := LockState{
		Present:     true,
		PID:         owner.PID,
		Host:        owner.Host,
		StartedAt:   owner.StartedAt,
		HeartbeatAt: owner.HeartbeatAt,
	}
	if err != nil || owner.HeartbeatAt.IsZero() {
		state.Stale = true
		return state
	}
	now := a.clock().UTC()
	state.Live = now.Sub(owner.HeartbeatAt) <= a.staleAfter
	state.Stale = !state.Live
	return state
}

func validArchiveID(id string) bool {
	return sessionIDPattern.MatchString(id)
}

func invalidSessionIDError(id string) *model.Error {
	return &model.Error{
		Class:   model.ErrConfigInvalid,
		Code:    "session_id_invalid",
		Message: fmt.Sprintf("非法 session ID %q", id),
		Hint:    "session ID 必须形如 20261030-094704-3f9a2c",
	}
}

func archiveArtifactNotFound(sessionID, name string) *model.Error {
	return &model.Error{
		Class:   model.ErrConfigInvalid,
		Code:    "artifact_not_found",
		Message: fmt.Sprintf("session %s 缺少 artifact %q", sessionID, name),
		Hint:    "artifact 可能已被手工删除；页面会显示占位内容",
	}
}

func modeFromEvents(events []model.Event) model.Mode {
	for _, ev := range events {
		var data struct {
			Mode model.Mode `json:"mode"`
		}
		if err := unmarshalEventData(ev, &data); err != nil {
			continue
		}
		if data.Mode.Valid() {
			return data.Mode
		}
	}
	return ""
}

func turnCountFromEvents(events []model.Event) int {
	count := 0
	for _, ev := range events {
		if ev.Type == model.EventTurnStarted {
			count++
		}
	}
	return count
}

func unmarshalEventData(ev model.Event, dst any) error {
	if len(ev.Data) == 0 {
		return nil
	}
	return json.Unmarshal(ev.Data, dst)
}

var (
	_ Archive      = (*archive)(nil)
	_ LockReporter = (*archive)(nil)
)

// ValidID 报告 id 是否符合 session 目录名的完整格式（不解析前缀）。
func ValidID(id string) bool { return validArchiveID(id) }
