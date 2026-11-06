package cli

import (
	"testing"

	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/session"
)

func TestShouldResumeRespectsTurnStatus(t *testing.T) {
	cases := []struct {
		name string
		st   *session.State
		want bool
	}{
		{"成功 turn 不恢复", &session.State{Last: model.EventTurnCompleted, TurnStatus: "ok", Turn: 1}, false},
		{"失败 turn 从断点恢复", &session.State{Last: model.EventTurnCompleted, TurnStatus: "failed", Turn: 1}, true},
		{"中断 turn 从断点恢复", &session.State{Last: model.EventTurnCompleted, TurnStatus: "interrupted", Turn: 1}, true},
		{"崩溃在模型请求", &session.State{Last: model.EventModelRequest, Turn: 1}, true},
		{"空状态", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldResume(tc.st); got != tc.want {
				t.Fatalf("shouldResume = %v want %v", got, tc.want)
			}
		})
	}
}
