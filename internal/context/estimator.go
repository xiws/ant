package ctxmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiws/ant/internal/model"
)

type estimator struct{}

// NewEstimator 返回保守 token 估算器（§11.2）。
func NewEstimator() Estimator { return estimator{} }

func (estimator) Estimate(messages []model.Message, tools []model.ToolSpec) int {
	total := 0
	for _, m := range messages {
		total += estimateText(m.Content)
		for _, tc := range m.ToolCalls {
			total += estimateText(tc.Name) + estimateText(string(tc.Arguments))
		}
	}
	for _, t := range tools {
		total += estimateText(t.Name) + estimateText(t.Description) + estimateText(string(t.Schema))
	}
	return total
}

// estimateText：ASCII 约 4 字符/token，CJK 约 1 字符/token（保守）。
func estimateText(s string) int {
	ascii, nonASCII := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			nonASCII++
		}
	}
	return (ascii+3)/4 + nonASCII
}

type compactor struct{}

// NewCompactor 返回配对安全的历史截断器。
func NewCompactor() Compactor { return compactor{} }

// Compact 以 user 消息为轮次边界丢弃最旧轮次，保证 assistant(tool_calls)
// 与其 tool 结果整体保留或整体替换（§11.4）。摘要由 Preparer/Summarizer 负责。
func (compactor) Compact(ctx context.Context, in CompactInput) (CompactOutput, error) {
	if err := ctx.Err(); err != nil {
		return CompactOutput{}, err
	}
	out := CompactOutput{History: append([]model.Message(nil), in.History...), Summary: in.Summary}
	out.BeforeTokens = estimateMessages(in.History)
	keepTurns := in.KeepRecentTurns
	if keepTurns <= 0 {
		keepTurns = 6
	}

	var system []model.Message
	var body []model.Message
	for _, m := range in.History {
		if m.Role == model.RoleSystem {
			system = append(system, m)
			continue
		}
		body = append(body, m)
	}
	groups := splitTurns(body)
	if len(groups) <= keepTurns {
		out.AfterTokens = out.BeforeTokens
		return out, nil
	}
	cut := len(groups) - keepTurns
	dropped := groups[:cut]
	keptGroups := groups[cut:]

	// 保留首个用户请求（§11.3 阶段 3）。
	firstUser := firstUserMessage(dropped)
	kept := flatten(keptGroups)
	removed := flatten(dropped)
	if firstUser != nil {
		kept = append([]model.Message{*firstUser}, kept...)
	}
	kept = append(append([]model.Message(nil), system...), kept...)

	// 仍然超预算时继续丢弃最旧轮次（至少保留最后一个）。
	for in.Budget.InputBudget > 0 && estimateMessages(kept) > in.Budget.InputBudget && len(kept) > len(system)+1 {
		idx := firstBodyIndex(kept, len(system))
		if idx < 0 || idx >= len(kept)-1 {
			break
		}
		removed = append(removed, kept[idx])
		kept = append(kept[:idx], kept[idx+1:]...)
	}

	out.History = kept
	out.Removed = removed
	out.Compacted = len(removed) > 0
	out.AfterTokens = estimateMessages(kept)
	return out, nil
}

func splitTurns(msgs []model.Message) [][]model.Message {
	var groups [][]model.Message
	var cur []model.Message
	for _, m := range msgs {
		if m.Role == model.RoleUser && len(cur) > 0 {
			groups = append(groups, cur)
			cur = nil
		}
		cur = append(cur, m)
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

func flatten(groups [][]model.Message) []model.Message {
	var out []model.Message
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func firstUserMessage(groups [][]model.Message) *model.Message {
	for _, g := range groups {
		for _, m := range g {
			if m.Role == model.RoleUser {
				cp := m
				return &cp
			}
		}
	}
	return nil
}

func firstBodyIndex(msgs []model.Message, skip int) int {
	for i := skip; i < len(msgs); i++ {
		if msgs[i].Role == model.RoleUser {
			return i
		}
	}
	return -1
}

func estimateMessages(msgs []model.Message) int {
	return estimator{}.Estimate(msgs, nil)
}

// deterministicSummary 是摘要模型失败时的兜底（§11.3）。
func deterministicSummary(removed []model.Message) string {
	type summary struct {
		Goal         string   `json:"goal"`
		Constraints  []string `json:"constraints"`
		Decisions    []string `json:"decisions"`
		FilesTouched []string `json:"files_touched"`
		CommandsRun  []string `json:"commands_run"`
		OpenIssues   []string `json:"open_issues"`
		NextSteps    []string `json:"next_steps"`
	}
	s := summary{
		Constraints: []string{}, Decisions: []string{}, FilesTouched: []string{},
		CommandsRun: []string{}, OpenIssues: []string{}, NextSteps: []string{},
	}
	for _, m := range removed {
		switch m.Role {
		case model.RoleUser:
			if s.Goal == "" {
				s.Goal = truncate(m.Content, 500)
			} else {
				s.Constraints = append(s.Constraints, truncate(m.Content, 200))
			}
		case model.RoleAssistant:
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					s.Decisions = append(s.Decisions, fmt.Sprintf("调用工具 %s", tc.Name))
				}
			} else if m.Content != "" {
				s.Decisions = append(s.Decisions, truncate(m.Content, 200))
			}
		case model.RoleTool:
			if strings.Contains(m.Content, `"ok":false`) || strings.Contains(m.Content, `"ok": false`) {
				s.OpenIssues = append(s.OpenIssues, truncate(m.Content, 200))
			}
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "（历史摘要不可用）"
	}
	return "```json\n" + string(b) + "\n```"
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
