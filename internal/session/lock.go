package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xiws/ant/internal/model"
)

// lockOwner 是 lock 文件的持久化内容（§9.7）。
type lockOwner struct {
	PID         int       `json:"pid"`
	Host        string    `json:"host"`
	StartedAt   time.Time `json:"startedAt"`
	HeartbeatAt time.Time `json:"heartbeatAt"`
}

// fileLocker 用心跳 JSON 文件实现跨进程互斥。
type fileLocker struct {
	root       string
	clock      func() time.Time
	staleAfter time.Duration
	interval   time.Duration
}

// NewLocker 返回基于 <workspace>/.ant/sessions/<id>/lock 的 Locker。
func NewLocker(workDir string) Locker {
	return NewLockerWithClock(workDir, time.Now)
}

// NewLockerWithClock 与 NewLocker 相同，但允许注入时钟（测试用）。
func NewLockerWithClock(workDir string, clock func() time.Time) Locker {
	if clock == nil {
		clock = time.Now
	}
	abs, err := filepath.Abs(workDir)
	if err != nil || abs == "" {
		abs = workDir
	}
	root := filepath.Join(filepath.Clean(abs), ".ant", "sessions")
	return newFileLocker(root, clock)
}

func newFileLocker(root string, clock func() time.Time) *fileLocker {
	return &fileLocker{
		root:       root,
		clock:      clock,
		staleAfter: staleAfter,
		interval:   heartbeatEvery,
	}
}

// Acquire 实现 Locker。
func (l *fileLocker) Acquire(ctx context.Context, id string) (Lease, error) {
	return l.acquire(ctx, id)
}

// acquire 取锁；成功时启动心跳 goroutine。stale 锁会被接管并记录 warning。
func (l *fileLocker) acquire(ctx context.Context, id string) (*lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextErr(ctx)
	}
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id ||
		strings.ContainsAny(id, `/\`) {
		return nil, sessionErr(model.ErrConfigInvalid, "session_id_invalid",
			fmt.Sprintf("非法 session ID %q", id), "session ID 必须是不含路径分隔符的目录名", nil)
	}
	dir := filepath.Join(l.root, id)
	if !withinDir(l.root, dir) {
		return nil, sessionErr(model.ErrConfigInvalid, "session_id_invalid",
			"session 路径逃逸 sessions 目录", "检查 session ID", nil)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, sessionNotFound(id)
	}

	lockPath := filepath.Join(dir, "lock")
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}

	var warnings []string
	var owner lockOwner
	acquired := false
	var lastErr error

	for attempt := 0; attempt < 4 && !acquired; attempt++ {
		now := l.clock().UTC()
		owner = lockOwner{
			PID:         os.Getpid(),
			Host:        host,
			StartedAt:   now,
			HeartbeatAt: now,
		}

		current, exists, readErr := readLockOwner(lockPath)
		if readErr == nil && exists {
			stale, heartbeat := l.isStale(current, lockPath, now)
			if !stale {
				return nil, lockedByOther(id, dir, current, heartbeat)
			}
			warnings = append(warnings, fmt.Sprintf(
				"lock stale：已接管 session %s 的过期锁（pid=%d, host=%s, heartbeat=%s）",
				id, current.PID, current.Host, formatTime(heartbeat)))
			if err := writeJSONAtomic(lockPath, owner, filePerm); err != nil {
				return nil, sessionErr(model.ErrInternal, "session_lock_write_failed",
					fmt.Sprintf("无法接管 session %s 的锁", id), "检查 session 目录写权限", err)
			}
			acquired = true
			break
		}
		if readErr != nil {
			warnings = append(warnings, fmt.Sprintf(
				"session %s 的 lock 文件损坏，已接管：%v", id, readErr))
			if err := writeJSONAtomic(lockPath, owner, filePerm); err != nil {
				return nil, sessionErr(model.ErrInternal, "session_lock_write_failed",
					fmt.Sprintf("无法接管 session %s 的锁", id), "检查 session 目录写权限", err)
			}
			acquired = true
			break
		}

		// 文件不存在：用 O_EXCL 原子创建；若被其他进程抢先则进入下一轮重新判断。
		f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			lastErr = err
			break
		}
		body, _ := json.Marshal(owner)
		body = append(body, '\n')
		if _, err := f.Write(body); err != nil {
			_ = f.Close()
			_ = os.Remove(lockPath)
			lastErr = err
			break
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			_ = os.Remove(lockPath)
			lastErr = err
			break
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(lockPath)
			lastErr = err
			break
		}
		_ = os.Chmod(lockPath, filePerm)
		acquired = true
	}

	if !acquired {
		return nil, sessionErr(model.ErrInternal, "session_lock_failed",
			fmt.Sprintf("无法获取 session %s 的锁", id), "检查 lock 文件权限", lastErr)
	}

	lsl := &lease{
		path:       lockPath,
		owner:      owner,
		clock:      l.clock,
		staleAfter: l.staleAfter,
		interval:   l.interval,
		warnings:   warnings,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	go lsl.loop()
	return lsl, nil
}

// isStale 判断锁心跳是否超过阈值；缺失时间时退回 startedAt / 文件 mtime。
func (l *fileLocker) isStale(owner lockOwner, lockPath string, now time.Time) (bool, time.Time) {
	heartbeat := owner.HeartbeatAt
	if heartbeat.IsZero() {
		heartbeat = owner.StartedAt
	}
	if heartbeat.IsZero() {
		if info, err := os.Stat(lockPath); err == nil {
			heartbeat = info.ModTime()
		}
	}
	if heartbeat.IsZero() {
		return true, heartbeat
	}
	return now.Sub(heartbeat) > l.staleAfter, heartbeat
}

func lockedByOther(id, dir string, owner lockOwner, heartbeat time.Time) *model.Error {
	return &model.Error{
		Class: model.ErrConfigInvalid,
		Code:  "session_locked",
		Message: fmt.Sprintf("session %s 已被进程 %d 锁定（host %s，heartbeat %s）",
			id, owner.PID, owner.Host, formatTime(heartbeat)),
		Hint: "等待该进程退出后重试；若进程已崩溃，心跳超过 120s 后可自动接管",
		Details: map[string]any{
			"pid":           owner.PID,
			"host":          owner.Host,
			"heartbeatAt":   heartbeat,
			"sessionPath":   dir,
			"staleAfterSec": int(staleAfter / time.Second),
		},
	}
}

// lease 是持锁凭据，带 30s 心跳。
type lease struct {
	mu         sync.Mutex
	path       string
	owner      lockOwner
	clock      func() time.Time
	staleAfter time.Duration
	interval   time.Duration
	warnings   []string

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	released bool
	lost     bool
}

// Heartbeat 立即更新一次心跳。
func (l *lease) Heartbeat(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return contextErr(ctx)
	}
	return l.heartbeat()
}

// heartbeat 在写文件前确认仍持有锁，避免接管后旧进程覆盖新锁。
func (l *lease) heartbeat() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return sessionErr(model.ErrInternal, "session_lock_released",
			"锁已释放", "不要对已释放的 Lease 调用 Heartbeat", nil)
	}
	if l.lost {
		return lockLostError(l.path)
	}
	current, exists, err := readLockOwner(l.path)
	if err != nil || !exists || !sameOwner(current, l.owner) {
		l.lost = true
		return lockLostError(l.path)
	}
	next := l.owner
	next.HeartbeatAt = l.clock().UTC()
	if err := writeJSONAtomic(l.path, next, filePerm); err != nil {
		return sessionErr(model.ErrInternal, "session_lock_heartbeat_failed",
			"更新锁心跳失败", "检查 session 目录写权限", err)
	}
	l.owner = next
	return nil
}

// Release 仅在仍持有时删除锁文件；可重复调用。
func (l *lease) Release() error {
	l.stopHeartbeat()

	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	lost := l.lost
	owner := l.owner
	l.mu.Unlock()
	if lost {
		return nil
	}
	current, exists, err := readLockOwner(l.path)
	if err != nil || !exists {
		return nil
	}
	if !sameOwner(current, owner) {
		return nil
	}
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		return sessionErr(model.ErrInternal, "session_lock_release_failed",
			"删除 lock 文件失败", "检查 session 目录权限", err)
	}
	return nil
}

// Stale 报告本 Lease 的心跳是否已超过阈值。
func (l *lease) Stale() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return true
	}
	heartbeat := l.owner.HeartbeatAt
	if heartbeat.IsZero() {
		return true
	}
	return l.clock().UTC().Sub(heartbeat) > l.staleAfter
}

// Warnings 返回取锁过程中的 warning（如 stale 接管）。
func (l *lease) Warnings() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.warnings) == 0 {
		return nil
	}
	out := make([]string, len(l.warnings))
	copy(out, l.warnings)
	return out
}

func (l *lease) loop() {
	defer close(l.done)
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			if err := l.heartbeat(); err != nil {
				return
			}
		}
	}
}

func (l *lease) stopHeartbeat() {
	l.stopOnce.Do(func() { close(l.stop) })
	<-l.done
}

func sameOwner(a, b lockOwner) bool {
	return a.PID == b.PID && a.Host == b.Host && a.StartedAt.Equal(b.StartedAt)
}

func lockLostError(path string) *model.Error {
	return &model.Error{
		Class:   model.ErrInternal,
		Code:    "session_lock_lost",
		Message: "session 锁已被其他进程接管",
		Hint:    "停止当前进程的写入；不要与新的持锁进程并行修改同一 session",
		Details: map[string]any{"lockPath": path},
	}
}

var _ Locker = (*fileLocker)(nil)
var _ Lease = (*lease)(nil)
