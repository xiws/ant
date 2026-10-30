package approval

import (
	"context"
	"testing"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

type fakeView struct{ s config.Settings }

func (f fakeView) Settings() config.Settings           { return f.s }
func (f fakeView) Models() []config.ModelSpec          { return nil }
func (f fakeView) Providers() []config.ProviderSpec    { return nil }
func (f fakeView) Origin(string) (config.Origin, bool) { return "", false }
func (f fakeView) Warnings() []string                  { return nil }

type scriptPrompter struct {
	choice Choice
	err    error
	calls  int
}

func (p *scriptPrompter) Confirm(ctx context.Context, cp ConfirmPrompt) (Choice, error) {
	p.calls++
	return p.choice, p.err
}
func (p *scriptPrompter) Ask(ctx context.Context, q Question) (Answer, error) {
	p.calls++
	return Answer{}, p.err
}

func settings() config.Settings {
	return config.Settings{
		DefaultMode: model.ModeCode,
		Approval:    config.ApprovalSettings{Policy: "risk-based"},
		Sandbox:     config.SandboxSettings{Network: "ask"},
	}
}

func TestHardDenyWinsOverEverything(t *testing.T) {
	s := settings()
	s.Yes = true
	var types []model.EventType
	sink := model.EventSinkFunc(func(ctx context.Context, ev *model.Event) error {
		types = append(types, ev.Type)
		return nil
	})
	approver := New(fakeView{s: s}, &scriptPrompter{choice: ChoiceAll}, NewGrants(), sink)
	dec, err := approver.Decide(context.Background(), Request{
		Class: model.ApprovalBashRisk, Command: "rm -rf /", Node: model.NodeCode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Allow {
		t.Fatal("hard deny must not be allowed")
	}
	if len(types) != 2 || types[0] != model.EventApprovalRequested || types[1] != model.EventApprovalResolved {
		t.Fatalf("events = %v", types)
	}
}

func TestNonInteractiveRequiresApproval(t *testing.T) {
	s := settings()
	s.NonInteractive = true
	approver := New(fakeView{s: s}, nil, NewGrants(), nil)
	_, err := approver.Decide(context.Background(), Request{Class: model.ApprovalFSReadOutside, Action: "read"})
	if err == nil {
		t.Fatal("expected ErrNonInteractive")
	}
	if model.ClassOf(err) != model.ErrApprovalRequired {
		t.Fatalf("class = %q", model.ClassOf(err))
	}
	if err != ErrNonInteractive {
		t.Fatalf("should return the sentinel, got %v", err)
	}
}

func TestGrantedClassAndChoiceAll(t *testing.T) {
	s := settings()
	grants := NewGrants()
	grants.Grant(model.ApprovalFSReadOutside)
	approver := New(fakeView{s: s}, nil, grants, nil)
	dec, err := approver.Decide(context.Background(), Request{Class: model.ApprovalFSReadOutside})
	if err != nil || !dec.Allow || dec.Scope != ScopeSession {
		t.Fatalf("dec=%+v err=%v", dec, err)
	}

	grants = NewGrants()
	prompter := &scriptPrompter{choice: ChoiceAll}
	approver = New(fakeView{s: s}, prompter, grants, nil)
	req := Request{Class: model.ApprovalBashNetwork, Command: "curl https://example.com", Node: model.NodeCode}
	dec, err = approver.Decide(context.Background(), req)
	if err != nil || !dec.Allow || !grants.Granted(model.ApprovalBashNetwork) {
		t.Fatalf("choice all not applied: dec=%+v err=%v", dec, err)
	}
	// 第二次不再提示。
	dec, err = approver.Decide(context.Background(), req)
	if err != nil || !dec.Allow || dec.Scope != ScopeSession {
		t.Fatalf("second decide = %+v/%v", dec, err)
	}
	if prompter.calls != 1 {
		t.Fatalf("prompter calls = %d", prompter.calls)
	}
}

func TestPolicyAlwaysAndNever(t *testing.T) {
	s := settings()
	s.Approval.Policy = "always"
	approver := New(fakeView{s: s}, nil, NewGrants(), nil)
	dec, err := approver.Decide(context.Background(), Request{Class: model.ApprovalBashRisk, Command: "ls"})
	if err != nil || !dec.Allow {
		t.Fatalf("always policy: %+v %v", dec, err)
	}

	s.Approval.Policy = "never"
	approver = New(fakeView{s: s}, nil, NewGrants(), nil)
	dec, err = approver.Decide(context.Background(), Request{Class: model.ApprovalBashRisk, Command: "ls"})
	if err != nil || dec.Allow {
		t.Fatalf("never policy: %+v %v", dec, err)
	}
}

func TestAutoApprovalAndGlobs(t *testing.T) {
	s := settings()
	approver := New(fakeView{s: s}, nil, NewGrants(), nil)
	dec, err := approver.Decide(context.Background(), Request{})
	if err != nil || !dec.Allow || dec.Reason != "auto" {
		t.Fatalf("auto = %+v/%v", dec, err)
	}

	s.Approval.Deny = []string{"cat *"}
	approver = New(fakeView{s: s}, nil, NewGrants(), nil)
	dec, err = approver.Decide(context.Background(), Request{Class: model.ApprovalBashOutside, Command: "cat /etc/hosts"})
	if err != nil || dec.Allow {
		t.Fatalf("deny glob = %+v/%v", dec, err)
	}

	s.Approval.Deny = nil
	s.Approval.Allow = []string{"cat *"}
	approver = New(fakeView{s: s}, nil, NewGrants(), nil)
	dec, err = approver.Decide(context.Background(), Request{Class: model.ApprovalBashOutside, Command: "cat /etc/hosts"})
	if err != nil || !dec.Allow {
		t.Fatalf("allow glob = %+v/%v", dec, err)
	}
}
