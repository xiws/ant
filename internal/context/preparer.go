package ctxmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

type preparer struct {
	cfg  config.View
	est  Estimator
	comp Compactor
	sum  Summarizer
	sink model.EventSink
}

// NewPreparer 构造预算/压缩入口。sum 可为 nil（只用确定性摘要）。
func NewPreparer(cfg config.View, sum Summarizer, sink model.EventSink) Preparer {
	return &preparer{cfg: cfg, est: NewEstimator(), comp: NewCompactor(), sum: sum, sink: sink}
}

// NewPreparerWith 允许测试替换估算器与压缩器。
func NewPreparerWith(cfg config.View, est Estimator, comp Compactor, sum Summarizer, sink model.EventSink) Preparer {
	if est == nil {
		est = NewEstimator()
	}
	if comp == nil {
		comp = NewCompactor()
	}
	return &preparer{cfg: cfg, est: est, comp: comp, sum: sum, sink: sink}
}

func (p *preparer) Prepare(ctx context.Context, in PrepareInput) (Prepared, error) {
	if err := ctx.Err(); err != nil {
		return Prepared{}, err
	}
	if in.Cap.ContextWindow <= 0 {
		return Prepared{}, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "context_window",
			Message: fmt.Sprintf("节点 %s 的模型没有有效 contextWindow", in.Node),
			Hint:    "检查 models[].contextWindow",
		}
	}
	s := p.cfg.Settings()
	budget := ComputeBudget(in.Cap, s.Context.SafetyMarginRatio)
	history := append([]model.Message(nil), in.History...)
	pinned := append([]model.Message(nil), in.Pinned...)
	summary := in.Summary

	build := func() []model.Message {
		msgs := make([]model.Message, 0, len(history)+len(pinned)+1)
		if summary != "" {
			msgs = append(msgs, model.Message{Role: model.RoleSystem, Content: "## 会话摘要\n" + summary})
		}
		msgs = append(msgs, history...)
		msgs = append(msgs, pinned...)
		return msgs
	}

	before := p.est.Estimate(build(), in.Tools)
	budget.Used = before
	if before > budget.InputBudget && !s.Context.AutoCompact {
		return Prepared{}, overflowError(in.Node, before, budget)
	}

	compacted := false
	var removed []model.Message
	var ref model.ArtifactRef
	threshold := int(float64(budget.InputBudget) * s.Context.CompactAt)
	if before > threshold && s.Context.AutoCompact {
		out, err := p.comp.Compact(ctx, CompactInput{
			History:         history,
			Pinned:          pinned,
			Budget:          budget,
			KeepRecentTurns: s.Context.KeepRecentTurns,
			Summary:         summary,
		})
		if err != nil {
			return Prepared{}, err
		}
		if out.Compacted {
			history = out.History
			removed = out.Removed
			compacted = true
			summary = out.Summary
			if len(removed) > 0 {
				summary = p.withModelSummary(ctx, summary, removed, s)
			}
		}
	}
	after := p.est.Estimate(build(), in.Tools)
	budget.Used = after
	if after > budget.InputBudget {
		return Prepared{}, overflowError(in.Node, after, budget)
	}

	if compacted {
		if store, ok := p.sink.(model.ArtifactStore); ok && summary != "" {
			body, _ := json.Marshal(map[string]any{"summary": summary, "turns": len(removed)})
			if r, err := store.PutArtifact(ctx, "context/summary.txt", body, "text/plain"); err == nil {
				ref = r
			}
		}
		if p.sink != nil {
			_ = p.sink.Emit(ctx, &model.Event{
				Type: model.EventContextCompacted,
				Node: in.Node,
				Data: model.MustJSON(map[string]any{
					"beforeTokens": before,
					"afterTokens":  after,
					"summaryRef":   ref,
					"replacedRange": map[string]int{
						"removed": len(removed),
						"kept":    len(history),
					},
				}),
			})
		}
	}

	return Prepared{
		Messages:  build(),
		Budget:    budget,
		Compacted: compacted,
		Summary:   summary,
		Ref:       ref,
	}, nil
}

// withModelSummary 调用摘要模型；失败时退回确定性摘要（§11.3 兜底）。
func (p *preparer) withModelSummary(ctx context.Context, previous string, removed []model.Message, s config.Settings) string {
	if p.sum == nil {
		return mergeSummary(previous, deterministicSummary(removed))
	}
	alias := s.Context.SummaryModel
	if alias == "" {
		alias = s.DefaultModel
	}
	text, _, err := p.sum.Summarize(ctx, SummaryInput{
		Model:      alias,
		Messages:   removed,
		Previous:   previous,
		MaxTokens:  s.Context.SummaryMaxTokens,
		TurnsRange: fmt.Sprintf("removed=%d", len(removed)),
	})
	if err != nil || strings.TrimSpace(text) == "" {
		return mergeSummary(previous, deterministicSummary(removed))
	}
	return text
}

func mergeSummary(previous, next string) string {
	switch {
	case strings.TrimSpace(previous) == "":
		return next
	case strings.TrimSpace(next) == "":
		return previous
	default:
		return previous + "\n" + next
	}
}

func overflowError(node model.NodeName, used int, b Budget) error {
	return &model.Error{
		Class:   model.ErrContextOverflow,
		Code:    "context_overflow",
		Message: fmt.Sprintf("节点 %s 输入估算 %d tokens 超过预算 %d", node, used, b.InputBudget),
		Hint:    "缩小任务、开启新 session，或在 models.json 调大 contextWindow",
		Details: map[string]any{"node": string(node), "used": used, "inputBudget": b.InputBudget, "window": b.Window},
	}
}

// ComputeBudget 实现 §11.1 的预算公式。
func ComputeBudget(cap model.Capabilities, safetyRatio float64) Budget {
	window := cap.ContextWindow
	reserve := cap.MaxOutputTokens
	if reserve <= 0 {
		reserve = 4096
	}
	if safetyRatio < 0 {
		safetyRatio = 0
	}
	margin := int(float64(window) * safetyRatio)
	input := window - reserve - margin
	if input < 0 {
		input = 0
	}
	return Budget{Window: window, InputBudget: input, OutputReserve: reserve}
}
