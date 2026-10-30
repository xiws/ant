package projectctx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

// fakeView 实现 config.View + config.Paths，避免测试依赖 Loader 与真实配置文件。
type fakeView struct {
	workDir  string
	settings config.Settings
}

func (f fakeView) Settings() config.Settings        { return f.settings }
func (f fakeView) Models() []config.ModelSpec       { return nil }
func (f fakeView) Providers() []config.ProviderSpec { return nil }
func (f fakeView) Origin(string) (config.Origin, bool) {
	return "", false
}
func (f fakeView) Warnings() []string { return nil }
func (f fakeView) WorkDir() string    { return f.workDir }
func (f fakeView) HomeDir() string    { return filepath.Join(f.workDir, ".home") }

// viewOnly 只实现 config.View，不实现 config.Paths（用于 config_invalid 断言）。
type viewOnly struct {
	settings config.Settings
}

func (v viewOnly) Settings() config.Settings        { return v.settings }
func (v viewOnly) Models() []config.ModelSpec       { return nil }
func (v viewOnly) Providers() []config.ProviderSpec { return nil }
func (v viewOnly) Origin(string) (config.Origin, bool) {
	return "", false
}
func (v viewOnly) Warnings() []string { return nil }

func testSettings() config.Settings {
	return config.Settings{
		Project: config.ProjectSettings{
			AutoInit:       true,
			MaxFiles:       500,
			MaxBytes:       32768,
			MaxOutputBytes: 8192,
			Depth:          3,
		},
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(raw)
}

// newWorkspace 构造一个包含 Go 清单、入口、README、.git 与各类应被排除文件的 workspace。
func newWorkspace(t *testing.T) (string, fakeView) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/demo\n\ngo 1.26\n\nrequire github.com/stretchr/testify v1.9.0\n")
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(root, "cmd", "app", "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(root, "src", "app.go"), "package demo\n\nfunc Run() {}\n")
	writeFile(t, filepath.Join(root, "README.md"), "# Demo 项目\n\n演示项目。\n")
	writeFile(t, filepath.Join(root, ".gitignore"), "*.log\nbuild/\n")
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(root, ".git", "refs", "heads", "main"), "1234567890abcdef1234567890abcdef12345678\n")
	writeFile(t, filepath.Join(root, "debug.log"), "ignore me\n")
	writeFile(t, filepath.Join(root, "build", "out.bin"), "ignore me\n")
	writeFile(t, filepath.Join(root, "node_modules", "left-pad", "index.js"), "module.exports = 1\n")
	writeFile(t, filepath.Join(root, "vendor", "x.go"), "package x\n")
	writeFile(t, filepath.Join(root, ".env"), "SECRET=1\n")
	return root, fakeView{workDir: root, settings: testSettings()}
}

func TestEnsureGeneratesProjectDocAndMeta(t *testing.T) {
	ctx := context.Background()
	root, cfg := newWorkspace(t)
	b := NewBuilder(cfg, nil, nil)

	var stages []string
	doc, err := b.Ensure(ctx, EnsureOptions{Progress: func(stage string) { stages = append(stages, stage) }})
	if err != nil {
		t.Fatalf("Ensure 首次生成失败: %v", err)
	}
	if doc.Path != filepath.Join(root, ".ant", projectDocName) {
		t.Fatalf("Doc.Path = %q, 期望 %q", doc.Path, filepath.Join(root, ".ant", projectDocName))
	}
	if doc.Body == "" {
		t.Fatal("Doc.Body 为空")
	}
	if doc.FromModel {
		t.Fatal("确定性骨架的 FromModel 应为 false")
	}
	if !strings.HasPrefix(doc.Hash, "sha256:") {
		t.Fatalf("Doc.Hash = %q, 期望 sha256: 前缀", doc.Hash)
	}
	if doc.GeneratedAt.IsZero() {
		t.Fatal("Doc.GeneratedAt 为零值")
	}

	// 磁盘内容与 Doc 一致。
	if disk := readFile(t, doc.Path); disk != doc.Body {
		t.Fatalf("磁盘 project.md 与 Doc.Body 不一致")
	}

	// 9 个小节齐全且标记成对。
	for _, section := range generatedSections {
		if !strings.Contains(doc.Body, section) {
			t.Errorf("project.md 缺少小节 %q", section)
		}
	}
	if !strings.Contains(doc.Body, generatedStartMarker) || !strings.Contains(doc.Body, generatedEndMarker) {
		t.Fatal("project.md 缺少 ant:generated 标记")
	}
	// Go 默认命令。
	for _, want := range []string{"go build ./...", "go test ./...", "go vet ./..."} {
		if !strings.Contains(doc.Body, want) {
			t.Errorf("project.md 缺少默认命令 %q", want)
		}
	}
	// 项目类型、module、入口、关键文件。
	for _, want := range []string{"Go", "example.com/demo", "cmd/app", "README.md", "go.mod", "Demo 项目"} {
		if !strings.Contains(doc.Body, want) {
			t.Errorf("project.md 缺少 %q", want)
		}
	}
	// 内置排除、.gitignore 与敏感文件不得出现在目录树中。
	for _, banned := range []string{"node_modules", "vendor/", ".git/", ".ant/", "debug.log", "build/", ".env"} {
		if strings.Contains(doc.Body, banned) {
			t.Errorf("project.md 不应包含被排除内容 %q", banned)
		}
	}

	// project.meta.json 字段与文档一致。
	var meta metaFile
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(root, ".ant", projectMetaName))), &meta); err != nil {
		t.Fatalf("解析 project.meta.json: %v", err)
	}
	if meta.SchemaVersion != metaSchemaVersion {
		t.Errorf("schemaVersion = %d, 期望 %d", meta.SchemaVersion, metaSchemaVersion)
	}
	if meta.Hash != doc.Hash {
		t.Errorf("meta.hash = %q, Doc.Hash = %q", meta.Hash, doc.Hash)
	}
	if meta.Model != deterministicModel {
		t.Errorf("meta.model = %q, 期望 %q", meta.Model, deterministicModel)
	}
	if meta.GitCommit != "1234567" {
		t.Errorf("meta.gitCommit = %q, 期望 1234567", meta.GitCommit)
	}
	if meta.FileCount <= 0 {
		t.Errorf("meta.fileCount = %d, 期望 > 0", meta.FileCount)
	}
	if meta.BytesSent <= 0 {
		t.Errorf("meta.bytesSent = %d, 期望 > 0", meta.BytesSent)
	}
	if meta.GeneratedAt != doc.GeneratedAt.Format(metaTimeLayout) {
		t.Errorf("meta.generatedAt = %q, Doc.GeneratedAt = %q", meta.GeneratedAt, doc.GeneratedAt.Format(metaTimeLayout))
	}

	// 进度阶段。
	for _, want := range []string{"scan", "hash", "generate", "write"} {
		found := false
		for _, got := range stages {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Progress 缺少阶段 %q（实际 %v）", want, stages)
		}
	}
}

func TestEnsureSkipsWhenHashUnchanged(t *testing.T) {
	ctx := context.Background()
	_, cfg := newWorkspace(t)
	b := NewBuilder(cfg, nil, nil)

	doc1, err := b.Ensure(ctx, EnsureOptions{})
	if err != nil {
		t.Fatalf("首次 Ensure 失败: %v", err)
	}
	firstDoc, err := os.Stat(doc1.Path)
	if err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(filepath.Dir(doc1.Path), projectMetaName)
	firstMeta, err := os.Stat(metaPath)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(20 * time.Millisecond)

	// SkipIfFresh=false（默认）：hash 未变时不重写。
	doc2, err := b.Ensure(ctx, EnsureOptions{SkipIfFresh: false})
	if err != nil {
		t.Fatalf("二次 Ensure 失败: %v", err)
	}
	if doc2.Body != doc1.Body || doc2.Hash != doc1.Hash {
		t.Fatal("hash 未变化时内容发生了改变")
	}
	secondDoc, err := os.Stat(doc1.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !secondDoc.ModTime().Equal(firstDoc.ModTime()) {
		t.Fatalf("hash 未变化却重写了 project.md（mtime %v → %v）", firstDoc.ModTime(), secondDoc.ModTime())
	}
	secondMeta, err := os.Stat(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !secondMeta.ModTime().Equal(firstMeta.ModTime()) {
		t.Fatal("hash 未变化却重写了 project.meta.json")
	}

	// SkipIfFresh=true：行为一致，同样不重写。
	doc3, err := b.Ensure(ctx, EnsureOptions{SkipIfFresh: true})
	if err != nil {
		t.Fatalf("SkipIfFresh=true Ensure 失败: %v", err)
	}
	if doc3.Body != doc1.Body || doc3.Hash != doc1.Hash {
		t.Fatal("SkipIfFresh=true 时内容发生了改变")
	}
	thirdDoc, err := os.Stat(doc1.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !thirdDoc.ModTime().Equal(firstDoc.ModTime()) {
		t.Fatal("SkipIfFresh=true 且 hash 未变化却重写了 project.md")
	}
}

func TestEnsureRegeneratesAndPreservesManualEdits(t *testing.T) {
	ctx := context.Background()
	root, cfg := newWorkspace(t)
	b := NewBuilder(cfg, nil, nil)

	doc1, err := b.Ensure(ctx, EnsureOptions{})
	if err != nil {
		t.Fatalf("首次 Ensure 失败: %v", err)
	}

	// 标记外手写内容：前后都要保留。
	manualHead := "# 团队补充\n\n这段文字在标记之外，必须保留。\n\n"
	manualTail := "\n## 团队验收\n\n- 必须通过内部 CI。\n"
	writeFile(t, doc1.Path, manualHead+doc1.Body+manualTail)

	// 修改关键清单（README）触发 hash 变化。
	writeFile(t, filepath.Join(root, "README.md"), "# Demo 项目 v2\n\n更新后的说明。\n")

	doc2, err := b.Ensure(ctx, EnsureOptions{})
	if err != nil {
		t.Fatalf("hash 变化后 Ensure 失败: %v", err)
	}
	if doc2.Hash == doc1.Hash {
		t.Fatal("关键文件变化后 hash 未变化")
	}
	if !strings.Contains(doc2.Body, "这段文字在标记之外，必须保留。") {
		t.Error("标记前的用户内容未被保留")
	}
	if !strings.Contains(doc2.Body, "必须通过内部 CI。") {
		t.Error("标记后的用户内容未被保留")
	}
	if !strings.Contains(doc2.Body, "Demo 项目 v2") {
		t.Error("标记块内内容未按新的关键文件重新生成")
	}
	if disk := readFile(t, doc1.Path); disk != doc2.Body {
		t.Error("磁盘 project.md 与 Doc.Body 不一致")
	}
	// 重新生成后 hash 写入 meta。
	var meta metaFile
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(root, ".ant", projectMetaName))), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Hash != doc2.Hash {
		t.Errorf("meta.hash = %q, Doc.Hash = %q", meta.Hash, doc2.Hash)
	}
}

func TestEnsureForceOverwritesWithoutMarkers(t *testing.T) {
	ctx := context.Background()
	root, cfg := newWorkspace(t)
	b := NewBuilder(cfg, nil, nil)

	doc1, err := b.Ensure(ctx, EnsureOptions{})
	if err != nil {
		t.Fatalf("首次 Ensure 失败: %v", err)
	}
	manual := "# 我的笔记\n\n完全手写的 project.md，没有任何生成标记。\n"
	writeFile(t, doc1.Path, manual)

	// 无标记 + 非 Force：视为用户接管，不覆盖。
	doc2, err := b.Ensure(ctx, EnsureOptions{})
	if err != nil {
		t.Fatalf("无标记 Ensure 失败: %v", err)
	}
	if !strings.Contains(doc2.Body, "完全手写的 project.md") {
		t.Fatal("无标记时 Doc.Body 应返回现有手写内容")
	}
	if disk := readFile(t, doc1.Path); disk != manual {
		t.Fatalf("无标记时不应覆盖文件，实际内容: %q", disk)
	}

	// 无标记 + Force：整体覆盖，并先备份原内容。
	doc3, err := b.Ensure(ctx, EnsureOptions{Force: true})
	if err != nil {
		t.Fatalf("Force Ensure 失败: %v", err)
	}
	if doc3.Body == manual {
		t.Fatal("Force 未覆盖无标记的 project.md")
	}
	if !strings.Contains(doc3.Body, generatedStartMarker) {
		t.Fatal("Force 覆盖后缺少生成标记")
	}
	if disk := readFile(t, doc1.Path); disk != doc3.Body {
		t.Fatal("Force 覆盖后磁盘内容与 Doc.Body 不一致")
	}
	backup := readFile(t, filepath.Join(root, ".ant", projectBackupName))
	if !strings.Contains(backup, "完全手写的 project.md") {
		t.Fatalf("project.md.bak 未保留原内容: %q", backup)
	}
}

func TestEnsureAutoInitDisabled(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	settings := testSettings()
	settings.Project.AutoInit = false
	cfg := fakeView{workDir: root, settings: settings}
	b := NewBuilder(cfg, nil, nil)

	// 无现有文件：返回空 Doc，不报错、不生成。
	doc, err := b.Ensure(ctx, EnsureOptions{})
	if err != nil {
		t.Fatalf("autoInit=false Ensure 失败: %v", err)
	}
	if doc.Path != "" || doc.Body != "" {
		t.Fatalf("autoInit=false 无现有文件时应返回空 Doc，实际 %+v", doc)
	}
	if _, err := os.Stat(filepath.Join(root, ".ant", projectDocName)); !os.IsNotExist(err) {
		t.Fatal("autoInit=false 不应生成 project.md")
	}

	// 有现有文件：只返回现有内容，不写 meta。
	existing := "# 现有项目上下文\n\n手工维护。\n"
	writeFile(t, filepath.Join(root, ".ant", projectDocName), existing)
	doc, err = b.Ensure(ctx, EnsureOptions{})
	if err != nil {
		t.Fatalf("autoInit=false 有现有文件时 Ensure 失败: %v", err)
	}
	if doc.Body != existing {
		t.Fatalf("autoInit=false 应返回现有 project.md，实际 %q", doc.Body)
	}
	if _, err := os.Stat(filepath.Join(root, ".ant", projectMetaName)); !os.IsNotExist(err) {
		t.Fatal("autoInit=false 不应写 project.meta.json")
	}

	// Force 仍然生成。
	doc, err = b.Ensure(ctx, EnsureOptions{Force: true})
	if err != nil {
		t.Fatalf("autoInit=false + Force Ensure 失败: %v", err)
	}
	if !strings.Contains(doc.Body, generatedStartMarker) {
		t.Fatal("Force 应跳过 autoInit=false 并生成标记块")
	}
}

func TestEnsureSkipsWhenLockHeld(t *testing.T) {
	ctx := context.Background()
	root, cfg := newWorkspace(t)
	lockPath := filepath.Join(root, ".ant", projectLockName)
	writeFile(t, lockPath, "pid=1\n")

	b := NewBuilder(cfg, nil, nil)
	doc, err := b.Ensure(ctx, EnsureOptions{})
	if err != nil {
		t.Fatalf("锁被占用时应返回 nil error，实际: %v", err)
	}
	if doc.Body != "" {
		t.Fatalf("锁被占用且无现有文件时应返回空 Doc，实际 %q", doc.Body)
	}
	if _, err := os.Stat(filepath.Join(root, ".ant", projectDocName)); !os.IsNotExist(err) {
		t.Fatal("锁被占用时不应写入 project.md")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatal("Ensure 不应删除其它进程持有的锁")
	}

	// 释放锁后可以正常生成，并在完成后删除锁。
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Ensure(ctx, EnsureOptions{}); err != nil {
		t.Fatalf("释放锁后 Ensure 失败: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatal("生成完成后应删除 project.lock")
	}
}

func TestEnsureScanFailureReturnsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/locked\n\ngo 1.26\n")
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	cfg := fakeView{workDir: root, settings: testSettings()}
	if _, err := NewBuilder(cfg, nil, nil).Ensure(context.Background(), EnsureOptions{}); err == nil {
		t.Fatal("扫描无权限目录时应返回 error")
	}
	if _, err := os.Stat(filepath.Join(root, ".ant", projectDocName)); !os.IsNotExist(err) {
		t.Fatal("扫描失败时不应写入 project.md")
	}
}

func TestEnsureWithoutWorkspaceReturnsConfigInvalid(t *testing.T) {
	b := NewBuilder(viewOnly{settings: testSettings()}, nil, nil)
	_, err := b.Ensure(context.Background(), EnsureOptions{})
	if err == nil {
		t.Fatal("缺少 workspace 时应返回 error")
	}
	if class := model.ClassOf(err); class != model.ErrConfigInvalid {
		t.Fatalf("错误分类 = %q, 期望 %q（err: %v）", class, model.ErrConfigInvalid, err)
	}
}

func TestScannerExcludesBuiltinsAndGitignore(t *testing.T) {
	ctx := context.Background()
	root, _ := newWorkspace(t)
	res, err := NewScanner().Scan(ctx, ScanOptions{WorkDir: root, MaxFiles: 500, MaxBytes: 32768, Depth: 3})
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	tree := strings.Join(res.Tree, "\n")
	for _, banned := range []string{"node_modules", "vendor/", ".git/", ".ant/", "debug.log", ".env", "build/"} {
		if strings.Contains(tree, banned) {
			t.Errorf("目录树不应包含 %q", banned)
		}
	}
	for _, want := range []string{"main.go", "go.mod", "src/app.go", "cmd/app/main.go"} {
		if !strings.Contains(tree, want) {
			t.Errorf("目录树缺少 %q（tree: %v）", want, res.Tree)
		}
	}
	if res.ProjectType != "Go" {
		t.Errorf("ProjectType = %q, 期望 Go", res.ProjectType)
	}
	if res.ModulePath != "example.com/demo" {
		t.Errorf("ModulePath = %q, 期望 example.com/demo", res.ModulePath)
	}
	if len(res.Commands.Build) == 0 || res.Commands.Build[0] != "go build ./..." {
		t.Errorf("Build 命令 = %v, 期望 go build ./...", res.Commands.Build)
	}
	if len(res.Commands.Test) == 0 || res.Commands.Test[0] != "go test ./..." {
		t.Errorf("Test 命令 = %v, 期望 go test ./...", res.Commands.Test)
	}
	if len(res.Commands.Lint) == 0 || res.Commands.Lint[0] != "go vet ./..." {
		t.Errorf("Lint 命令 = %v, 期望 go vet ./...", res.Commands.Lint)
	}
	entries := strings.Join(res.Entries, ",")
	if !strings.Contains(entries, "main.go") || !strings.Contains(entries, "cmd/app") {
		t.Errorf("Entries = %v, 期望包含 main.go 与 cmd/app", res.Entries)
	}
	keyPaths := make([]string, 0, len(res.KeyFiles))
	for _, kf := range res.KeyFiles {
		keyPaths = append(keyPaths, kf.Path)
	}
	keyJoined := strings.Join(keyPaths, ",")
	for _, want := range []string{"README.md", "go.mod"} {
		if !strings.Contains(keyJoined, want) {
			t.Errorf("KeyFiles 缺少 %q（实际 %v）", want, keyPaths)
		}
	}
	if res.FileCount <= 0 {
		t.Errorf("FileCount = %d, 期望 > 0", res.FileCount)
	}
}

func TestScannerSkipsBinaryAndOversizeFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "ok.go"), "package main\n")
	writeFile(t, filepath.Join(root, "blob.bin"), "abc\x00def")
	writeFile(t, filepath.Join(root, "huge.txt"), strings.Repeat("a", 300*1024))
	writeFile(t, filepath.Join(root, "server.pem"), "-----BEGIN CERTIFICATE-----\n")
	writeFile(t, filepath.Join(root, "id_rsa"), "secret\n")

	res, err := NewScanner().Scan(context.Background(), ScanOptions{WorkDir: root})
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	tree := strings.Join(res.Tree, "\n")
	for _, banned := range []string{"blob.bin", "huge.txt", "server.pem", "id_rsa"} {
		if strings.Contains(tree, banned) {
			t.Errorf("目录树不应包含 %q", banned)
		}
	}
	if !strings.Contains(tree, "ok.go") {
		t.Errorf("目录树缺少 ok.go（tree: %v）", res.Tree)
	}
	if res.FileCount != 1 {
		t.Errorf("FileCount = %d, 期望 1", res.FileCount)
	}
}

func TestScannerHonorsMaxFilesAndDepth(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"), "package a\n")
	writeFile(t, filepath.Join(root, "b.go"), "package b\n")
	writeFile(t, filepath.Join(root, "deep", "a", "b", "c.go"), "package c\n")

	res, err := NewScanner().Scan(context.Background(), ScanOptions{WorkDir: root, MaxFiles: 1, Depth: 2})
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	tree := strings.Join(res.Tree, "\n")
	if res.FileCount != 1 {
		t.Errorf("FileCount = %d, 期望受 MaxFiles=1 限制", res.FileCount)
	}
	if strings.Contains(tree, "c.go") || strings.Contains(tree, "deep/a/") {
		t.Errorf("depth=2 时不应包含更深的目录/文件（tree: %v）", res.Tree)
	}
	if !strings.Contains(tree, "a.go") {
		t.Errorf("目录树缺少 a.go（tree: %v）", res.Tree)
	}
}

func TestScannerUsesMakefileCommands(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/make\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "Makefile"), "build:\n\tgo build ./...\n\ntest:\n\tgo test ./...\n\nlint:\n\tgolangci-lint run\n\n.PHONY: build test lint\n")

	res, err := NewScanner().Scan(context.Background(), ScanOptions{WorkDir: root})
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	if len(res.Commands.Build) != 1 || res.Commands.Build[0] != "make build" {
		t.Errorf("Build = %v, 期望 make build", res.Commands.Build)
	}
	if len(res.Commands.Test) != 1 || res.Commands.Test[0] != "make test" {
		t.Errorf("Test = %v, 期望 make test", res.Commands.Test)
	}
	if len(res.Commands.Lint) != 1 || res.Commands.Lint[0] != "make lint" {
		t.Errorf("Lint = %v, 期望 make lint", res.Commands.Lint)
	}
}

func TestHasherDeterministicAndSensitive(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(root, ".git", "refs", "heads", "main"), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")

	res := ScanResult{
		ProjectType: "Go",
		ModulePath:  "example.com/demo",
		Tree:        []string{"main.go\t10", "src/app.go\t20"},
		FileCount:   2,
		Deps:        []string{"github.com/stretchr/testify"},
		KeyFiles:    []KeyFile{{Path: "README.md", Body: "# Demo\n"}},
	}
	h := NewHasher(root)
	h1, err := h.Hash(ctx, res)
	if err != nil {
		t.Fatalf("Hash 失败: %v", err)
	}
	h2, err := h.Hash(ctx, res)
	if err != nil {
		t.Fatalf("Hash 失败: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("相同输入 hash 不稳定: %q vs %q", h1, h2)
	}
	if !strings.HasPrefix(h1, "sha256:") || len(h1) != len("sha256:")+64 {
		t.Fatalf("hash 格式不正确: %q", h1)
	}

	// 文件树大小变化。
	changed := res
	changed.Tree = []string{"main.go\t10", "src/app.go\t21"}
	h3, err := h.Hash(ctx, changed)
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h1 {
		t.Fatal("文件树大小变化后 hash 未变化")
	}

	// git HEAD 变化。
	writeFile(t, filepath.Join(root, ".git", "refs", "heads", "main"), "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n")
	h4, err := h.Hash(ctx, res)
	if err != nil {
		t.Fatal(err)
	}
	if h4 == h1 {
		t.Fatal("git HEAD 变化后 hash 未变化")
	}

	// 无 git 目录时仍可用。
	if _, err := NewHasher("").Hash(ctx, res); err != nil {
		t.Fatalf("无 workDir 的 Hash 失败: %v", err)
	}
}
