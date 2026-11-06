package projectctx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/provider"
	"github.com/xiws/ant/internal/provider/fake"
)

func TestEnsureUsesModelSummary(t *testing.T) {
	root, cfg := newWorkspace(t)
	cfg.settings.DefaultModel = "demo-model"
	fp := fake.New().QueueText(`## 项目用途
演示项目。

## 技术栈
- Go

## 目录结构
` + "```text\ncmd/\n```" + `

## 入口
- cmd/app/main.go

## 构建 / 测试 / Lint
- go build ./...
- go test ./...

## 约定与风格
- 标准 Go 风格

## 关键文件
- README.md

## 已知坑
- 无

## 验收规则
- go test ./... 通过
`)
	b := NewBuilder(cfg, fp, nil)
	doc, err := b.Ensure(context.Background(), EnsureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !doc.FromModel {
		t.Fatal("应使用模型摘要")
	}
	if !strings.Contains(doc.Body, "演示项目") || !strings.Contains(doc.Body, generatedStartMarker) {
		t.Fatalf("模型内容未写入:\n%s", doc.Body)
	}
	var meta metaFile
	raw, err := os.ReadFile(filepath.Join(root, ".ant", projectMetaName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Model != "demo-model" {
		t.Fatalf("meta.model = %q", meta.Model)
	}
	reqs := fp.Requests()
	if len(reqs) != 1 || reqs[0].Model != "demo-model" {
		t.Fatalf("provider requests = %+v", reqs)
	}
}

func TestEnsureFallsBackWhenModelOutputInvalid(t *testing.T) {
	root, cfg := newWorkspace(t)
	cfg.settings.DefaultModel = "demo-model"
	fp := fake.New().QueueText("这不是合法的项目上下文")
	b := NewBuilder(cfg, fp, nil)
	doc, err := b.Ensure(context.Background(), EnsureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if doc.FromModel {
		t.Fatal("非法模型输出应退回确定性骨架")
	}
	var meta metaFile
	raw, _ := os.ReadFile(filepath.Join(root, ".ant", projectMetaName))
	_ = json.Unmarshal(raw, &meta)
	if meta.Model != deterministicModel {
		t.Fatalf("meta.model = %q", meta.Model)
	}
}

var _ provider.Provider = (*fake.Provider)(nil)

func TestModelGeneratorAliasPriority(t *testing.T) {
	settings := config.Settings{}
	settings.Project.ProjectModel = "project-alias"
	settings.DefaultModel = "default-alias"
	cfg := fakeView{settings: settings}
	if _, alias := NewModelGenerator(cfg, fake.New()); alias != "project-alias" {
		t.Fatalf("alias = %q", alias)
	}
	settings.Project.ProjectModel = ""
	cfg.settings = settings
	if _, alias := NewModelGenerator(cfg, fake.New()); alias != "default-alias" {
		t.Fatalf("alias = %q", alias)
	}
}
