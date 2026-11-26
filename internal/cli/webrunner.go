package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/provider"
	"github.com/xiws/ant/internal/web"
)

// webExecutor 复用 CLI 的完整组合根执行 Web 提交的 turn。
//
// 每次提交创建一个短生命周期 App：它负责打开 session、取锁、恢复断点、
// 装配 provider/工具/审批并执行 turn，结束后释放锁。Web 层只通过
// web.Executor 接口调用它，不直接依赖 agent/tool 实现。
type webExecutor struct {
	workDir  string
	homeDir  string
	env      []string
	cfg      config.View
	provider provider.Provider // 测试注入；生产为 nil
	clock    func() time.Time
}

func (e *webExecutor) Run(ctx context.Context, req web.RunRequest, hooks web.RunHooks) (web.RunOutcome, error) {
	if e.cfg == nil {
		return web.RunOutcome{}, fmt.Errorf("配置不可用，无法执行：请先修复 workspace/home 配置")
	}
	if strings.TrimSpace(req.Input) == "" {
		return web.RunOutcome{}, fmt.Errorf("输入不能为空")
	}

	args := make([]string, 0, 8)
	if req.SessionID == "" {
		args = append(args, "new")
	} else {
		args = append(args, "-s", req.SessionID)
	}
	if req.Mode != "" {
		args = append(args, "-m", string(req.Mode))
	}
	if req.Model != "" {
		args = append(args, "-p", req.Model)
	}
	// 用 -- 终止 flag 解析，保证用户输入即使以 '-' 开头也原样作为 prompt。
	args = append(args, "--", req.Input)

	var (
		idOnce    sync.Once
		foundID   string
		onSession = func(id string) {
			if id == "" || hooks.OnSessionID == nil {
				return
			}
			idOnce.Do(func() { hooks.OnSessionID(id) })
		}
	)
	sink := model.EventSinkFunc(func(ctx context.Context, ev *model.Event) error {
		if ev != nil && ev.Type == model.EventSessionCreated {
			var payload struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(ev.Data, &payload) == nil && payload.ID != "" {
				foundID = payload.ID
				onSession(payload.ID)
			}
		}
		if hooks.OnEvent != nil && ev != nil {
			hooks.OnEvent(ev)
		}
		return nil
	})

	app, err := New(ctx, args, Stdio{
		In:    strings.NewReader(""),
		Out:   io.Discard,
		Err:   io.Discard,
		IsTTY: true,
		Env:   e.env,
	}, Overrides{
		Config:     e.cfg,
		WorkDir:    e.workDir,
		HomeDir:    e.homeDir,
		Clock:      e.clock,
		Prompter:   hooks.Prompter,
		Provider:   e.provider,
		EventSink:  sink,
		SkipResume: true,
		OnDelta: func(text, reasoning string) {
			if hooks.OnDelta != nil {
				hooks.OnDelta(text, reasoning)
			}
		},
	})
	if err != nil {
		return web.RunOutcome{}, err
	}

	// 新建 session 时 session_created 已在 New 中触发；续跑时 ID 已知。
	if foundID == "" {
		foundID = req.SessionID
	}
	onSession(foundID)

	code := app.Run(ctx)
	closeErr := app.Close()
	if closeErr != nil && code == 0 {
		return web.RunOutcome{ExitCode: 1}, closeErr
	}
	return web.RunOutcome{ExitCode: code}, nil
}
