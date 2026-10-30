package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/xiws/ant/internal/model"
)

const (
	// schemaVersion 是当前 session 文件格式版本（docs/07-session.md §9.9）。
	schemaVersion = 1

	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600

	// staleAfter 是锁心跳过期阈值（§9.7）。
	staleAfter = 120 * time.Second
	// heartbeatEvery 是持锁期间的心跳间隔（§9.7）。
	heartbeatEvery = 30 * time.Second
)

// sessionDirName 是 session 在 sessions/ 下的目录名；ID 同时是目录名。
// ID 格式 YYYYMMDD-HHMMSS-<6hex>（UTC）。
func newSessionID(now time.Time) (string, error) {
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", &model.Error{
			Class:   model.ErrInternal,
			Code:    "session_id_generate_failed",
			Message: "无法生成 session ID 随机后缀",
			Hint:    "检查系统随机源是否可用",
			Cause:   err,
		}
	}
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(buf[:]), nil
}

// secureMkdirAll 创建目录并尽力收紧权限（Windows 上为尽力而为）。
func secureMkdirAll(dir string) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	_ = os.Chmod(dir, dirPerm)
	return nil
}

// writeFileAtomic 写临时文件 → fsync → rename（§9.7）。
// Windows 上 rename 覆盖失败时先删除目标再重试，接受短暂窗口。
func writeFileAtomic(dst string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()

	if err = tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}

	if err = os.Rename(tmpName, dst); err != nil {
		if runtime.GOOS == "windows" {
			if rmErr := os.Remove(dst); rmErr == nil || os.IsNotExist(rmErr) {
				if retryErr := os.Rename(tmpName, dst); retryErr == nil {
					renamed = true
					err = nil
				}
			}
		}
		if !renamed {
			return err
		}
	}
	renamed = true
	_ = os.Chmod(dst, perm)
	// 目录 fsync 让 rename 在崩溃后可见；部分平台（Windows）不支持，忽略错误。
	if d, openErr := os.Open(dir); openErr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// writeJSONAtomic 以可读缩进 JSON 原子落盘。
func writeJSONAtomic(dst string, v any, perm os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return writeFileAtomic(dst, b, perm)
}

// hashOf 返回与仓库其他模块一致的 sha256:hex 形式。
func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// withinDir 报告 target 是否位于 root 内（两者都应为绝对路径）。
func withinDir(root, target string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// cleanRelativeName 校验并规范化相对路径，拒绝绝对路径、卷名与 .. 逃逸。
func cleanRelativeName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", artifactNameError("artifact 名称不能为空")
	}
	if strings.ContainsRune(trimmed, '\x00') {
		return "", artifactNameError("artifact 名称包含非法字符")
	}
	// 在把分隔符统一为 / 之前检查 Windows 卷名。
	if filepath.IsAbs(trimmed) || filepath.VolumeName(trimmed) != "" ||
		strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, `\`) {
		return "", artifactNameError("artifact 名称不能是绝对路径")
	}
	slashed := filepath.ToSlash(trimmed)
	for _, part := range strings.Split(slashed, "/") {
		if part == ".." {
			return "", artifactNameError("artifact 名称不能包含 .. 逃逸")
		}
	}
	cleaned := path.Clean(slashed)
	if cleaned == "." || cleaned == "" || cleaned == "/" {
		return "", artifactNameError("artifact 名称无效")
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", artifactNameError("artifact 名称不能逃逸 session 目录")
	}
	return cleaned, nil
}

// normalizeArtifactName 把 artifact 名称固定到 artifacts/ 下。
func normalizeArtifactName(name string) (string, error) {
	cleaned, err := cleanRelativeName(name)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(cleaned, "artifacts/") {
		cleaned = path.Join("artifacts", cleaned)
	}
	return cleaned, nil
}

func artifactNameError(message string) *model.Error {
	return &model.Error{
		Class:   model.ErrConfigInvalid,
		Code:    "artifact_name_invalid",
		Message: message,
		Hint:    "artifact 名称必须是 session 目录内的相对路径，通常以 artifacts/ 开头",
	}
}

func sessionErr(class model.ErrorClass, code, message, hint string, cause error) *model.Error {
	return &model.Error{Class: class, Code: code, Message: message, Hint: hint, Cause: cause}
}

func contextErr(ctx context.Context) *model.Error {
	return &model.Error{
		Class:   model.ErrInterrupted,
		Code:    "context_canceled",
		Message: "session 操作被取消",
		Cause:   ctx.Err(),
	}
}

// readLockOwner 读取锁文件；第二个返回值报告文件是否存在。
func readLockOwner(path string) (lockOwner, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return lockOwner{}, false, nil
		}
		return lockOwner{}, false, err
	}
	var owner lockOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return lockOwner{}, true, err
	}
	return owner, true, nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// maxEventSeq 返回事件中最大的 seq；没有事件时返回 0。
func maxEventSeq(events []model.Event) int64 {
	var max int64
	for _, ev := range events {
		if ev.Seq > max {
			max = ev.Seq
		}
	}
	return max
}

// readEventsFile 逐行解析 events.jsonl；损坏/不完整行记录 warning 后跳过，不回写。
func readEventsFile(path string) ([]model.Event, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if len(data) == 0 {
		return nil, nil, nil
	}
	lines := strings.Split(string(data), "\n")
	events := make([]model.Event, 0, len(lines))
	var warnings []string
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev model.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			if i == len(lines)-1 {
				warnings = append(warnings, fmt.Sprintf(
					"events.jsonl: 最后一行不完整（incomplete），已忽略: %v", err))
			} else {
				warnings = append(warnings, fmt.Sprintf(
					"events.jsonl: 第 %d 行不是合法 JSON，已忽略: %v", i+1, err))
			}
			continue
		}
		events = append(events, ev)
	}
	return events, warnings, nil
}

// lastEventType 返回文件顺序上的最后一条事件类型。
func lastEventType(events []model.Event) model.EventType {
	if len(events) == 0 {
		return ""
	}
	return events[len(events)-1].Type
}
