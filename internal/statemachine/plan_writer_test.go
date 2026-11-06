package statemachine

import (
	"context"
	"sync"
	"testing"

	"github.com/xiws/ant/internal/agent"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
)

type recordingPlanWriter struct {
	mu   sync.Mutex
	body string
	path string
}

func (w *recordingPlanWriter) WritePlan(_ context.Context, plan Plan) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.body = plan.Body
	w.path = "/tmp/session/plan.md"
	return w.path, nil
}

func (w *recordingPlanWriter) snapshot() (string, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body, w.path
}

func TestPlanGatePersistsPlan(t *testing.T) {
	ask := askNode("draft plan")
	deliberate := newDimNode(model.NodeDeliberate)
	releaseAfter(deliberate, 3)
	agg := aggregationNode("aggregation text", "merged plan")
	verifier := verdictNode(true, nil)
	gate := &fixedGate{accept: true}
	_, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModePlan),
		agent.StaticRegistry(ask, deliberate, agg, verifier), sinkFn, gate, logx.Nop())
	writer := &recordingPlanWriter{}
	setter, ok := eng.(interface{ SetPlanWriter(PlanWriter) })
	if !ok {
		t.Fatal("engine 未暴露 SetPlanWriter")
	}
	setter.SetPlanWriter(writer)

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "design it"})
	if err != nil {
		t.Fatal(err)
	}
	if res.PlanPath != "/tmp/session/plan.md" {
		t.Fatalf("PlanPath = %q", res.PlanPath)
	}
	body, _ := writer.snapshot()
	if body != "merged plan" {
		t.Fatalf("写入 plan = %q", body)
	}
	plans := gate.plans
	if len(plans) != 1 || plans[0].Path != "/tmp/session/plan.md" || plans[0].Body != "merged plan" {
		t.Fatalf("gate plans = %+v", plans)
	}
}
