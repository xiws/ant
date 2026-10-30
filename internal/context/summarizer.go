package ctxmgr

import (
	"context"
	"fmt"
	"strings"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/provider"
)

type providerSummarizer struct {
	p       provider.Provider
	cfg     config.View
	aliases []string
}

// NewSummarizer 用模型压缩历史；失败时由 Preparer 退回确定性摘要。
func NewSummarizer(p provider.Provider, cfg config.View) Summarizer {
	s := &providerSummarizer{p: p, cfg: cfg}
	if cfg != nil {
		st := cfg.Settings()
		for _, a := range []string{st.Context.SummaryModel, st.DefaultModel, st.CLIModel, st.DefaultProvider} {
			if a != "" {
				s.aliases = append(s.aliases, a)
			}
		}
	}
	return s
}

const summaryPrompt = `你是上下文压缩器。把输入对话整理为 JSON 对象：
{"goal":"用户当前目标","constraints":[],"decisions":[],"files_touched":[],"commands_run":[],"open_issues":[],"next_steps":[]}
要求：只使用输入中出现的信息，不编造；引用文件路径；输出只有 JSON，不要 Markdown 围栏。`

func (s *providerSummarizer) Summarize(ctx context.Context, in SummaryInput) (string, model.Usage, error) {
	if s.p == nil {
		return "", model.Usage{}, fmt.Errorf("ctxmgr: 摘要 provider 为空")
	}
	alias := in.Model
	if alias == "" && len(s.aliases) > 0 {
		alias = s.aliases[0]
	}
	maxTokens := in.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	var b strings.Builder
	if in.TurnsRange != "" {
		b.WriteString("来源范围：" + in.TurnsRange + "\n")
	}
	if in.Previous != "" {
		b.WriteString("已有摘要：\n" + in.Previous + "\n\n")
	}
	b.WriteString("待压缩对话：\n")
	for _, m := range in.Messages {
		b.WriteString(string(m.Role))
		b.WriteString(": ")
		b.WriteString(truncate(m.Content, 2000))
		for _, tc := range m.ToolCalls {
			b.WriteString("\n  tool_call ")
			b.WriteString(tc.Name)
			b.WriteString(" ")
			b.WriteString(truncate(string(tc.Arguments), 500))
		}
		b.WriteString("\n")
	}

	resp, err := s.p.Chat(ctx, provider.ChatRequest{
		Model: alias,
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: summaryPrompt},
			{Role: model.RoleUser, Content: b.String()},
		},
		Stream:    false,
		MaxTokens: maxTokens,
	}, nil)
	if err != nil {
		return "", model.Usage{}, err
	}
	text := strings.TrimSpace(resp.Message.Content)
	if text == "" {
		return "", resp.Usage, fmt.Errorf("ctxmgr: 摘要模型返回空内容")
	}
	return text, resp.Usage, nil
}
