package session

import (
	"context"
	"testing"

	"github.com/xiws/ant/internal/model"
)

func TestRecoverDoneTracksCompletedSteps(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sess, _ := mustOpen(t, store, OpenOptions{Create: true, Mode: model.ModeCode, Workspace: dir, Lock: true})
	defer sess.Close()

	emitData(t, sess, model.EventTurnStarted, 1, "", "", map[string]any{"input": "do it"})
	emitData(t, sess, model.EventStepCompleted, 1, "code.code", model.NodeCode, map[string]any{"status": "ok"})
	emitData(t, sess, model.EventNodeCompleted, 1, "code.code", model.NodeCode, map[string]any{"status": "ok"})
	emitData(t, sess, model.EventStepCompleted, 1, "code.round-1.verifier", model.NodeVerifier, map[string]any{"status": "failed"})
	emitData(t, sess, model.EventNodeCompleted, 1, "code.round-1.verifier", model.NodeVerifier, map[string]any{
		"status": "ok", "verdict": map[string]any{"pass": false},
	})
	emitData(t, sess, model.EventSessionFailed, 1, "code.round-1.verifier", "", map[string]any{"reason": "verification_failed"})

	state, err := Recover(context.Background(), sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Done) != 1 || state.Done[0] != "code" {
		t.Fatalf("Done = %v", state.Done)
	}
	if state.Step.Node != model.NodeVerifier {
		t.Fatalf("Step = %+v", state.Step)
	}
}

func TestRecoverDoneFanoutNeedsAllBranches(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sess, _ := mustOpen(t, store, OpenOptions{Create: true, Mode: model.ModePlan, Workspace: dir, Lock: true})
	defer sess.Close()

	emitData(t, sess, model.EventTurnStarted, 1, "", "", map[string]any{"input": "plan"})
	emitData(t, sess, model.EventStepCompleted, 1, "plan.ask", model.NodeAsk, map[string]any{"status": "ok"})
	emitData(t, sess, model.EventNodeCompleted, 1, "plan.ask", model.NodeAsk, map[string]any{"status": "ok"})
	for i := 0; i < 3; i++ {
		emitData(t, sess, model.EventNodeCompleted, 1, "plan.round-1.fanout.deliberate", model.NodeDeliberate, map[string]any{"status": "ok"})
	}
	state, err := Recover(context.Background(), sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Done) != 2 {
		t.Fatalf("Done = %v, want ask+fanout", state.Done)
	}
	seen := map[string]bool{}
	for _, id := range state.Done {
		seen[id] = true
	}
	if !seen["ask"] || !seen["fanout"] {
		t.Fatalf("Done = %v", state.Done)
	}
}
