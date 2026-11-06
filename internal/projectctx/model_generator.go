package projectctx

import (
	"context"
	"fmt"
	"strings"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/provider"
)

const projectInitPrompt = `你是项目上下文生成器。输入是 ant 对仓库的确定性扫描骨架（目录树、清单、入口、命令、关键文件）。
请把骨架整理成面向编码 agent 的 Markdown 项目上下文，必须且只能包含以下小节（顺序固定）：

## 项目用途
## 技术栈
## 目录结构
## 入口
## 构建 / 测试 / Lint
## 约定与风格
## 关键文件
## 已知坑
## 验收规则

要求：
- 只使用输入中出现的信息，不得编造不存在的文件、依赖或命令；
- 引用文件时使用 workspace 相对路径；
- 目录结构用 text 代码块，其他小节用简洁列表；
- 不确定的内容写“未验证/未知”，不要补全；
- 不要输出额外解释、前言或 Markdown 围栏包裹整份文档。`

// NewModelGenerator 用模型把扫描骨架摘要成 project.md；cfg/provider 不足时返回 (nil, "")。
func NewModelGenerator(cfg config.View, p provider.Provider) (Generator, string) {
	if p == nil || cfg == nil {
		return nil, ""
	}
	settings := cfg.Settings()
	alias := firstNonEmpty(
		settings.Project.ProjectModel,
		settings.DefaultModel,
		settings.CLIModel,
		settings.DefaultProvider,
	)
	if alias == "" {
		if models := cfg.Models(); len(models) == 1 {
			alias = models[0].Name
		}
	}
	if alias == "" {
		return nil, ""
	}
	maxTokens := settings.Project.MaxOutputBytes / 4
	if maxTokens < 512 {
		maxTokens = 512
	}
	if maxTokens > 8192 {
		maxTokens = 8192
	}
	maxBytes := settings.Project.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultProjectMaxBytes
	}
	return &modelGenerator{p: p, alias: alias, maxTokens: maxTokens, maxBytes: maxBytes}, alias
}

type modelGenerator struct {
	p         provider.Provider
	alias     string
	maxTokens int
	maxBytes  int
}

// Generate 实现 Generator。
func (g *modelGenerator) Generate(ctx context.Context, s Skeleton) (string, model.Usage, error) {
	if err := ctx.Err(); err != nil {
		return "", model.Usage{}, err
	}
	prompt := skeletonPrompt(s, g.maxBytes)
	resp, err := g.p.Chat(ctx, provider.ChatRequest{
		Model: g.alias,
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: projectInitPrompt},
			{Role: model.RoleUser, Content: prompt},
		},
		Stream:    false,
		MaxTokens: g.maxTokens,
	}, nil)
	if err != nil {
		return "", resp.Usage, err
	}
	body := strings.TrimSpace(resp.Message.Content)
	if body == "" {
		return "", resp.Usage, fmt.Errorf("projectctx: 模型返回空内容")
	}
	body = stripDocumentFences(body)
	body = stripLeadingTitle(body)
	md := wrapGenerated(body)
	if err := validateMarkdown(md); err != nil {
		return "", resp.Usage, fmt.Errorf("projectctx: 模型输出不合规: %w", err)
	}
	return md, resp.Usage, nil
}

func stripDocumentFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func stripLeadingTitle(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "# 项目上下文") {
		lines = lines[1:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func wrapGenerated(body string) string {
	body = strings.TrimSpace(body)
	if start, end, ok := markerRange(body); ok {
		body = strings.TrimSpace(body[start:end])
		body = strings.TrimPrefix(body, generatedStartMarker)
		body = strings.TrimSuffix(body, generatedEndMarker)
		body = strings.TrimSpace(body)
	}
	return "# 项目上下文\n\n" + generatedStartMarker + "\n" + body + "\n" + generatedEndMarker + "\n"
}

// skeletonPrompt 构造模型输入：确定性骨架 + 关键文件正文，受 maxBytes 限制。
func skeletonPrompt(s Skeleton, maxBytes int) string {
	var b strings.Builder
	b.WriteString("以下是确定性扫描骨架：\n\n")
	base := renderDeterministic(s)
	if start, end, ok := markerRange(base); ok {
		base = base[start:end]
		base = strings.TrimPrefix(base, generatedStartMarker)
		base = strings.TrimSuffix(base, generatedEndMarker)
	}
	b.WriteString(strings.TrimSpace(base))
	b.WriteString("\n\n关键文件内容（截断后）：\n")
	used := b.Len()
	for _, kf := range s.KeyFiles {
		body := strings.TrimRight(kf.Body, "\n")
		if body == "" {
			continue
		}
		entry := "\n### " + kf.Path + "\n```\n" + body + "\n```\n"
		if maxBytes > 0 && used+len(entry) > maxBytes {
			remain := maxBytes - used
			if remain > 200 {
				b.WriteString(entry[:remain])
			}
			break
		}
		b.WriteString(entry)
		used += len(entry)
	}
	return b.String()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
