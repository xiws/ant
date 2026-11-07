package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiws/ant/internal/model"
)

const archiveTestID = "20261030-094704-3f9a2c"
const archiveTestID2 = "20261030-094705-4a5b6c"

// writeArchiveSession 在 workDir 下构造一个只读 session fixture。
func writeArchiveSession(t *testing.T, workDir, id string, meta *Meta, events []string, artifacts map[string]string) string {
	t.Helper()
	dir := filepath.Join(workDir, ".ant", "sessions", id)
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if meta != nil {
		body, err := json.MarshalIndent(meta, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if len(events) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(strings.Join(events, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range artifacts {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func archiveMeta(id string, updated time.Time) *Meta {
	return &Meta{
		SchemaVersion: 1,
		ID:            id,
		Workspace:     "/tmp/workspace",
		CreatedAt:     updated.Add(-time.Hour),
		UpdatedAt:     updated,
		Status:        StatusIdle,
		Mode:          model.ModeAsk,
		Title:         "归档测试",
		Model:         "alpha",
		TurnCount:     1,
		Usage:         model.Usage{Input: 10, Output: 5},
	}
}

func archiveEvents() []string {
	return []string{
		`{"seq":1,"ts":"2026-10-30T09:47:04.000Z","type":"session_created","data":{"id":"20261030-094704-3f9a2c","mode":"ask","model":"alpha"}}`,
		`{"seq":2,"ts":"2026-10-30T09:47:05.000Z","type":"turn_started","turn":1,"data":{"input":"hello","mode":"ask","round":0}}`,
		`{"seq":3,"ts":"2026-10-30T09:47:06.000Z","type":"turn_completed","turn":1,"data":{"status":"ok","exitCode":0}}`,
	}
}

// treeSnapshot 记录目录树中每个文件的相对路径、大小与 mtime。
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return statErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out[rel] = info.Mode().String() + "|" + info.ModTime().UTC().Format(time.RFC3339Nano) + "|" + itoa64(info.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func TestArchiveSnapshotIsReadOnly(t *testing.T) {
	work := t.TempDir()
	dir := writeArchiveSession(t, work, archiveTestID, archiveMeta(archiveTestID, time.Now()), archiveEvents(), map[string]string{
		"artifacts/message-1.txt": "hello world",
	})
	before := treeSnapshot(t, dir)

	archive := NewArchive(work)
	snapshot, err := archive.Snapshot(context.Background(), archiveTestID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(snapshot.Events))
	}
	if snapshot.Meta.Title != "归档测试" {
		t.Fatalf("meta title = %q", snapshot.Meta.Title)
	}
	if _, err := archive.Artifact(context.Background(), archiveTestID, model.ArtifactRef{Name: "artifacts/message-1.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.List(context.Background(), ListFilter{}); err != nil {
		t.Fatal(err)
	}
	after := treeSnapshot(t, dir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("Snapshot/Artifact/List must not write files\nbefore=%v\nafter=%v", before, after)
	}
	if _, statErr := os.Stat(filepath.Join(work, ".ant", "sessions", archiveTestID, "plan.md")); !os.IsNotExist(statErr) {
		t.Fatalf("archive must not create plan.md, stat err = %v", statErr)
	}
}

func TestArchiveSnapshotBadLineWarning(t *testing.T) {
	work := t.TempDir()
	events := append(archiveEvents(), `{"seq":4,"ts":"2026-10-30T09:47:07.000Z","type":"bogus"`)
	writeArchiveSession(t, work, archiveTestID, archiveMeta(archiveTestID, time.Now()), events, nil)

	snapshot, err := NewArchive(work).Snapshot(context.Background(), archiveTestID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Warnings) == 0 {
		t.Fatal("bad trailing line should produce a warning")
	}
	if len(snapshot.Events) != 3 {
		t.Fatalf("bad line must be skipped, got %d events", len(snapshot.Events))
	}
}

func TestArchiveSnapshotMissingMeta(t *testing.T) {
	work := t.TempDir()
	writeArchiveSession(t, work, archiveTestID, nil, archiveEvents(), nil)

	snapshot, err := NewArchive(work).Snapshot(context.Background(), archiveTestID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Meta.ID != archiveTestID {
		t.Fatalf("fallback ID = %q", snapshot.Meta.ID)
	}
	if snapshot.Meta.TurnCount != 1 {
		t.Fatalf("fallback turn count = %d, want 1", snapshot.Meta.TurnCount)
	}
	if len(snapshot.Warnings) == 0 || !strings.Contains(strings.Join(snapshot.Warnings, "\n"), "meta.json") {
		t.Fatalf("expected meta warning, got %v", snapshot.Warnings)
	}
}

func TestArchiveSnapshotNotFoundAndInvalidID(t *testing.T) {
	work := t.TempDir()
	archive := NewArchive(work)
	if _, err := archive.Snapshot(context.Background(), "20261030-094704-ffffff"); model.CodeOf(err) != "session_not_found" {
		t.Fatalf("missing session error = %v", err)
	}
	for _, id := range []string{"", "..", "../../etc", "20261030-094704-3F9A2C", "prefix"} {
		if _, err := archive.Snapshot(context.Background(), id); model.CodeOf(err) != "session_id_invalid" {
			t.Fatalf("id %q error = %v, want session_id_invalid", id, err)
		}
	}
}

func TestArchiveLockStates(t *testing.T) {
	work := t.TempDir()
	dir := writeArchiveSession(t, work, archiveTestID, archiveMeta(archiveTestID, time.Now()), archiveEvents(), nil)
	now := time.Now().UTC()
	live := lockOwner{PID: 42, Host: "test", StartedAt: now, HeartbeatAt: now}
	body, _ := json.Marshal(live)
	if err := os.WriteFile(filepath.Join(dir, "lock"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	archive := newArchive(work, func() time.Time { return now })
	snapshot, err := archive.Snapshot(context.Background(), archiveTestID)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Lock.Present || !snapshot.Lock.Live || snapshot.Lock.Stale {
		t.Fatalf("live lock state = %+v", snapshot.Lock)
	}
	locks, err := archive.Locks(context.Background(), []string{archiveTestID})
	if err != nil {
		t.Fatal(err)
	}
	if !locks[archiveTestID].Live {
		t.Fatalf("Locks live = %+v", locks[archiveTestID])
	}

	stale := lockOwner{PID: 43, Host: "test", StartedAt: now.Add(-10 * time.Minute), HeartbeatAt: now.Add(-10 * time.Minute)}
	body, _ = json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(dir, "lock"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err = archive.Snapshot(context.Background(), archiveTestID)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Lock.Present || snapshot.Lock.Live || !snapshot.Lock.Stale {
		t.Fatalf("stale lock state = %+v", snapshot.Lock)
	}
	if snapshot.Lock.PID != 43 {
		t.Fatalf("stale lock pid = %d", snapshot.Lock.PID)
	}
}

func TestArchiveArtifactSafety(t *testing.T) {
	work := t.TempDir()
	outside := filepath.Join(work, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := writeArchiveSession(t, work, archiveTestID, archiveMeta(archiveTestID, time.Now()), archiveEvents(), map[string]string{
		"artifacts/ok.txt": "ok",
	})
	if err := os.Symlink(outside, filepath.Join(dir, "artifacts", "escape.txt")); err != nil {
		t.Logf("symlink unavailable: %v", err)
	}

	archive := NewArchive(work)
	cases := []struct {
		name string
		ref  model.ArtifactRef
		code string
	}{
		{"traversal", model.ArtifactRef{Name: "artifacts/../../outside.txt"}, "artifact_name_invalid"},
		{"absolute", model.ArtifactRef{Name: "/etc/passwd"}, "artifact_name_invalid"},
		{"not artifacts", model.ArtifactRef{Name: "meta.json"}, "artifact_name_invalid"},
		{"missing", model.ArtifactRef{Name: "artifacts/nope.txt"}, "artifact_not_found"},
		{"session mismatch", model.ArtifactRef{SessionID: "20261030-094705-4a5b6c", Name: "artifacts/ok.txt"}, "artifact_session_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := archive.Artifact(context.Background(), archiveTestID, tc.ref)
			if model.CodeOf(err) != tc.code {
				t.Fatalf("error = %v, want code %s", err, tc.code)
			}
		})
	}
	if _, err := archive.Artifact(context.Background(), archiveTestID, model.ArtifactRef{Name: "artifacts/ok.txt"}); err != nil {
		t.Fatalf("valid artifact error = %v", err)
	}
	if _, err := archive.Artifact(context.Background(), archiveTestID, model.ArtifactRef{Name: "artifacts/escape.txt"}); err == nil {
		t.Fatal("symlink escape must be rejected")
	}

	// 大小上限。
	original := newArchive(work, nil).maxArtifactBytes
	a := newArchive(work, nil)
	a.maxArtifactBytes = 1
	if _, err := a.Artifact(context.Background(), archiveTestID, model.ArtifactRef{Name: "artifacts/ok.txt"}); model.CodeOf(err) != "artifact_too_large" {
		t.Fatalf("too large error = %v (original limit %d)", err, original)
	}
}

func TestArchiveListAndLocks(t *testing.T) {
	work := t.TempDir()
	now := time.Now()
	writeArchiveSession(t, work, archiveTestID, archiveMeta(archiveTestID, now.Add(-time.Hour)), archiveEvents(), nil)
	writeArchiveSession(t, work, archiveTestID2, archiveMeta(archiveTestID2, now), archiveEvents(), nil)
	archive := NewArchive(work)
	list, err := archive.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d, want 2", len(list))
	}
	if list[0].ID != archiveTestID2 {
		t.Fatalf("list order: first = %s, want %s", list[0].ID, archiveTestID2)
	}
	if list[0].Tokens != 15 {
		t.Fatalf("tokens = %d, want 15", list[0].Tokens)
	}
	if list[0].LastEvent != model.EventTurnCompleted {
		t.Fatalf("last event = %q", list[0].LastEvent)
	}
	locks, err := archive.(LockReporter).Locks(context.Background(), []string{archiveTestID, archiveTestID2})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 2 || locks[archiveTestID].Present {
		t.Fatalf("locks = %+v", locks)
	}
}

func TestArchiveEventTooLarge(t *testing.T) {
	work := t.TempDir()
	writeArchiveSession(t, work, archiveTestID, archiveMeta(archiveTestID, time.Now()), archiveEvents(), nil)
	a := newArchive(work, nil)
	a.maxEventsBytes = 1
	if _, err := a.Snapshot(context.Background(), archiveTestID); model.CodeOf(err) != "session_too_large" {
		t.Fatalf("event size limit error = %v", err)
	}
	a = newArchive(work, nil)
	a.maxEvents = 1
	if _, err := a.Snapshot(context.Background(), archiveTestID); model.CodeOf(err) != "session_too_large" {
		t.Fatalf("event count limit error = %v", err)
	}
}
