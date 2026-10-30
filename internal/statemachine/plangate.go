package statemachine

import (
	"context"

	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/model"
)

// planGate 包装 approval.Approver，走 plan_to_code 类别（§8.3、§12.3）。
type planGate struct {
	approver approval.Approver
}

// NewPlanGate 构造默认 PlanGate：把「是否进入 code 模式」交给审批器。
// 非交互场景由审批器返回 approval.ErrNonInteractive（退出码 4）。
func NewPlanGate(approver approval.Approver) PlanGate {
	return &planGate{approver: approver}
}

// Confirm 询问是否进入 code 模式执行计划。
func (g *planGate) Confirm(ctx context.Context, plan Plan) (bool, error) {
	if g == nil || g.approver == nil {
		return false, approval.ErrNonInteractive
	}
	reason := "plan 已通过验证，是否进入 code 模式执行"
	dec, err := g.approver.Decide(ctx, approval.Request{
		Class:  model.ApprovalPlanToCode,
		Node:   model.NodeVerifier,
		Action: "plan_to_code",
		Paths:  []string{plan.Path},
		Reason: reason,
	})
	if err != nil {
		return false, err
	}
	return dec.Allow, nil
}
