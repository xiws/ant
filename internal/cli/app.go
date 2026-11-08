package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xiws/ant/internal/agent"
	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/config"
	ctxmgr "github.com/xiws/ant/internal/context"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/projectctx"
	"github.com/xiws/ant/internal/prompt"
	"github.com/xiws/ant/internal/provider"
	"github.com/xiws/ant/internal/readcode"
	"github.com/xiws/ant/internal/session"
	"github.com/xiws/ant/internal/statemachine"
	"github.com/xiws/ant/internal/tool"
	"github.com/xiws/ant/internal/version"
)

// App 是一次进程的装配结果（组合根产物）。
type App struct {
	opts  options
	stdio Stdio

	cfg      config.View
	store    session.Store
	sess     session.Session
	engine   statemachine.Engine
	sink     model.EventSink
	render   Renderer
	log      logx.Logger
	resumed  *session.State
	initDone <-chan struct{}

	clock      func() time.Time
	reader     *bufio.Reader
	prompter   *terminalPrompter
	tracker    *usageTracker
	builder    projectctx.Builder
	registry   provider.Registry
	provider   provider.Provider // 有效 provider（Router 或 Overrides.Provider）
	ovProvider provider.Provider
	history    []model.Message
	summary    string
	logFile    *os.File
	extraSink  model.EventSink

	projectMu  sync.RWMutex
	projectDoc prompt.ProjectDoc

	fallbackWorkDir string
	workDirOverride string
}

// Run 是唯一进程入口。
func Run(ctx context.Context, args []string, stdio Stdio) int {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	stdio = normalizeStdio(stdio)

	app, err := New(ctx, args, stdio, Overrides{})
	if err != nil {
		fmt.Fprintln(stdio.Err, "[ant] "+formatError(err))
		return exitCodeForError(err)
	}
	defer app.Close()

	code := app.Run(ctx)
	if ctx.Err() != nil && code == 0 {
		code = 3
	}
	return code
}

// normalizeStdio 初始化输出
func normalizeStdio(s Stdio) Stdio {
	if s.In == nil {
		s.In = os.Stdin
	}
	if s.Out == nil {
		s.Out = os.Stdout
	}
	if s.Err == nil {
		s.Err = os.Stderr
	}
	if s.Env == nil {
		s.Env = os.Environ()
	}
	return s
}

func exitCodeForError(err error) int {
	if err == nil {
		return 0
	}
	if isUsageError(err) {
		return 2
	}
	switch model.ClassOf(err) {
	case model.ErrConfigInvalid:
		return 2
	case model.ErrApprovalRequired:
		return 4
	case model.ErrInterrupted:
		return 3
	case model.ErrProviderAuth, model.ErrProviderRateLimit, model.ErrProviderUnavailable,
		model.ErrProviderInvalidReq, model.ErrAdapterNotImplemented:
		return 5
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 3
	}
	return 1
}

func formatError(err error) string {
	var me *model.Error
	if errors.As(err, &me) {
		out := me.Message
		if out == "" {
			out = me.Error()
		}
		if me.Hint != "" {
			out += "\n      提示：" + me.Hint
		}
		if me.Class != "" {
			out += "（" + string(me.Class) + "）"
		}
		return out
	}
	return err.Error()
}

// New 是组合根：显式构造全部模块并串联；不做反射注入（D43）。
func New(ctx context.Context, args []string, stdio Stdio, ov Overrides) (*App, error) {
	opts, err := parseArgs(args)
	if err != nil {
		return nil, err
	}
	stdio = normalizeStdio(stdio)
	clock := ov.Clock
	if clock == nil {
		clock = time.Now
	}
	app := &App{
		opts:            opts,
		stdio:           stdio,
		clock:           clock,
		extraSink:       ov.EventSink,
		fallbackWorkDir: ov.WorkDir,
		workDirOverride: ov.WorkDir,
		ovProvider:      ov.Provider,
		reader:          bufio.NewReader(stdio.In),
	}
	if opts.help {
		opts.command = "help"
		app.opts.command = "help"
	}
	promptOut := stdio.Out
	if opts.jsonOut {
		promptOut = stdio.Err
	}
	app.prompter = newTerminalPrompter(app.reader, promptOut, stdio.IsTTY, opts.nonInteractive)
	if opts.sessionID != "" && (opts.newSession || opts.command == "new") {
		return nil, usageErrorf("-s/--session 与 --new/new 不能同时使用")
	}

	if opts.command == "version" || opts.command == "help" {
		return app, nil
	}

	cfg, err := loadConfig(ctx, stdio, ov, opts)
	if err != nil {
		return nil, err
	}
	app.cfg = cfg
	if app.workDirOverride == "" {
		app.workDirOverride = app.workDir()
	}

	// 日志：stderr + .ant/logs（§15.1）。
	app.log = app.newLogger(cfg.Settings().Log)
	app.render = app.pickRenderer()
	if opts.jsonOut && opts.verboseSet && opts.verbose {
		app.render.Notice("--json 模式下忽略 --verbose（stdout 保持 NDJSON）")
	}
	if opts.yes {
		app.render.Notice("--yes 已启用：除硬拒绝外自动批准（危险，仅建议受控环境）")
	}

	switch opts.command {
	case "models", "config", "sessions", "show", "export":
		app.store = session.NewStore(app.workDir())
		return app, nil
	case "init":
		reg, effective, buildErr := app.buildProviders(nil)
		if buildErr != nil {
			_ = app.Close()
			return nil, buildErr
		}
		app.registry = reg
		app.provider = effective
		app.builder = projectctx.NewBuilder(cfg, effective, app.log)
		return app, nil
	}

	if err := app.openAndWire(ctx); err != nil {
		_ = app.Close()
		return nil, err
	}
	return app, nil
}

func loadConfig(ctx context.Context, stdio Stdio, ov Overrides, opts options) (config.View, error) {
	if ov.Config != nil {
		return ov.Config, nil
	}
	loader := config.NewLoader()
	cfg, err := loader.Load(ctx, config.Options{
		WorkDir: ov.WorkDir,
		HomeDir: ov.HomeDir,
		Env:     stdio.Env,
		CLI: config.Overrides{
			Model:          opts.model,
			Mode:           opts.mode,
			Yes:            opts.yes,
			NonInteractive: opts.nonInteractive,
			LogLevel:       opts.logLevel,
			NoColor:        opts.noColor,
			Verbose:        opts.verbose,
			VerboseSet:     opts.verboseSet,
			NodeModels:     opts.nodeModels,
		},
	})
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func (a *App) workDir() string {
	if p, ok := a.cfg.(config.Paths); ok && p.WorkDir() != "" {
		return p.WorkDir()
	}
	if a.workDirOverride != "" {
		return a.workDirOverride
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// verbose 报告是否启用人类可读的详细链路输出。
// --json 模式承诺 stdout 只有 NDJSON，因此强制关闭。
func (a *App) verbose() bool {
	if a == nil || a.cfg == nil || a.opts.jsonOut {
		return false
	}
	return a.cfg.Settings().UI.Verbose
}

func (a *App) pickRenderer() Renderer {
	if a.opts.jsonOut {
		return &jsonRenderer{err: a.stdio.Err}
	}
	r := newTextRenderer(a.stdio.Out, a.stdio.Err)
	if a.cfg == nil {
		return r
	}
	settings := a.cfg.Settings()
	r.mode = settings.DefaultMode
	r.modelAlias = initialAlias(a.cfg, a.opts)
	r.verbose = a.verbose()
	if !colorEnabled(settings.UI.Color, a.stdio.IsTTY, a.stdio.Env) {
		return r
	}
	rich, err := newRichTextRenderer(a.stdio.Out, a.stdio.Err, terminalWidth(a.stdio.Out), markdownStyle(a.stdio.Env))
	if err != nil {
		if a.log != nil {
			a.log.Warn("Markdown 渲染不可用，回退纯文本: " + err.Error())
		}
		return r
	}
	rich.mode = r.mode
	rich.modelAlias = r.modelAlias
	rich.verbose = r.verbose
	return rich
}

func (a *App) newLogger(s config.LogSettings) logx.Logger {
	writer := a.stdio.Err
	path := s.File
	if path == "" {
		path = filepath.Join(a.workDir(), ".ant", "logs", "ant-"+a.clock().UTC().Format("20060102")+".log")
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(a.workDir(), path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			a.logFile = f
			writer = io.MultiWriter(a.stdio.Err, f)
		}
	}
	logger := logx.New(s, writer)
	if a.logFile == nil {
		logger.Warn("日志文件不可用，仅输出到 stderr")
	}
	return logger
}

func initialAlias(cfg config.View, opts options) string {
	if opts.model != "" {
		return opts.model
	}
	s := cfg.Settings()
	for _, alias := range []string{s.CLIModel, s.DefaultModel, s.DefaultProvider} {
		if alias != "" {
			return alias
		}
	}
	if models := cfg.Models(); len(models) == 1 {
		return models[0].Name
	}
	return ""
}

// openAndWire 打开 session、建事件扇出、恢复状态并装配全部模块。
func (a *App) openAndWire(ctx context.Context) error {
	wd := a.workDir()
	store := session.NewStore(wd)
	mode := a.opts.mode
	if mode == "" {
		mode = a.cfg.Settings().DefaultMode
	}
	alias := initialAlias(a.cfg, a.opts)

	openOpts := session.OpenOptions{
		Mode: mode, Model: alias, Workspace: wd, Lock: true,
	}
	var (
		sess   session.Session
		opened session.Opened
		err    error
	)
	switch {
	case a.opts.sessionID != "":
		openOpts.ID = a.opts.sessionID
		sess, opened, err = store.Open(ctx, openOpts)
	case a.opts.command == "new" || a.opts.newSession:
		openOpts.Create = true
		sess, opened, err = store.Open(ctx, openOpts)
	default:
		latest, lerr := store.Latest(ctx)
		if lerr == nil {
			openOpts.ID = latest.ID
			sess, opened, err = store.Open(ctx, openOpts)
		} else if model.CodeOf(lerr) == "no_sessions" {
			openOpts.Create = true
			sess, opened, err = store.Open(ctx, openOpts)
		} else {
			return lerr
		}
	}
	if err != nil {
		return err
	}
	a.store, a.sess = store, sess
	a.tracker = newUsageTracker(a.pricingFor)
	if r, ok := a.render.(*textRenderer); ok {
		r.sessionPath = filepath.Join(filepath.Join(wd, ".ant", "sessions"), opened.ID)
		r.mode = sess.Meta().Mode
		r.modelAlias = alias
	}

	// 4. 事件扇出：session 第一（分配 seq）。
	// renderer 在 logger 之前：模型文本结束补换行（EndMessage）后才能写日志行，
	// 否则日志会粘在最后一行正文后面。
	sinks := []model.EventSink{sess}
	if a.opts.jsonOut {
		sinks = append(sinks, logx.NewNDJSON(a.stdio.Out))
	}
	sinks = append(sinks, renderSink(a.render, sess, a.verbose()))
	sinks = append(sinks, logx.NewLoggerSink(a.log, a.cfg.Settings().Log.Level))
	if a.tracker != nil {
		sinks = append(sinks, usageSink(a.tracker))
	}
	if a.extraSink != nil {
		sinks = append(sinks, a.extraSink)
	}
	a.sink = logx.Multi(sinks...)

	// 5. 首个事件（新建时）。
	if opened.Created {
		ev := &model.Event{
			Type: model.EventSessionCreated,
			Data: model.MustJSON(map[string]any{
				"id": opened.ID, "workspace": wd, "mode": sess.Meta().Mode, "model": alias,
			}),
		}
		if err := a.sink.Emit(ctx, ev); err != nil {
			return err
		}
	}

	// 6. 恢复 + 灌回放行类别。
	grants := approval.NewGrants()
	decider := replayDecider{prompter: a.prompter, nonInteractive: a.opts.nonInteractive}
	if opened.Resumed {
		st, rerr := session.Recover(ctx, sess, decider)
		if rerr != nil {
			return rerr
		}
		grants.Restore(st.Grants)
		a.resumed = &st
		a.history = append([]model.Message(nil), st.Messages...)
		a.summary = st.Summary
	}

	// 7. provider、项目上下文、审批、工具、节点、状态机（§21.15 的显式装配）。
	reg, effective, err := a.buildProviders(a.sink)
	if err != nil {
		return err
	}
	a.registry = reg
	a.provider = effective

	a.loadProjectDoc()
	a.builder = projectctx.NewBuilder(a.cfg, effective, a.log)
	a.startProjectInit(ctx)

	approver := approval.New(a.cfg, a.prompter, grants, a.sink)
	toolkit := tool.NewToolkit(a.cfg, tool.Deps{
		Guard:    tool.NewPathGuard(a.cfg, wd),
		Versions: tool.NewVersionTable(),
		Approver: approver,
		Prompter: a.prompter,
		Index:    readcode.NewIndex(a.cfg),
		Sink:     a.sink,
		Store:    sess,
	})

	nodes := agent.NewRegistry(agent.Deps{
		Provider:     effective,
		Registry:     reg,
		Prompt:       prompt.NewAssembler(prompt.NewFileSource(a.cfg)),
		Context:      ctxmgr.NewPreparer(a.cfg, ctxmgr.NewSummarizer(effective, a.cfg), a.sink),
		Runtime:      toolkit.Runtime,
		ToolRegistry: toolkit.Registry,
		Recorder:     a.sink,
		Store:        sess,
		Loop:         agent.NewLoop(a.cfg),
		Clock:        a.clock,
		Config:       a.cfg,
		Project:      a.projectProjection,
		OnDelta: func(d provider.Delta) error {
			if d.Text != "" {
				a.render.Delta(d.Text)
			}
			if d.ReasoningText != "" {
				a.render.Reasoning(d.ReasoningText)
			}
			return nil
		},
		OnText: func(text string) { a.render.Delta(text) },
	})

	engine := statemachine.NewEngine(a.cfg, nodes, a.sink, statemachine.NewPlanGate(approver), a.log)
	if setter, ok := engine.(interface {
		SetPlanWriter(statemachine.PlanWriter)
	}); ok {
		setter.SetPlanWriter(&sessionPlanWriter{sess: sess, workDir: wd})
	}
	a.engine = engine

	target := mode
	if opened.Resumed {
		current := sess.Meta().Mode
		if a.opts.mode != "" && a.opts.mode != current {
			if !a.stdio.IsTTY || a.opts.nonInteractive {
				// 非交互下不允许隐式切换模式；用户需显式 --new 或交互确认（§3.2，退出码 2）。
				return usageErrorf("非交互模式不能切换 session 模式（当前 %s，请求 %s）；请使用 --new 或交互模式", current, a.opts.mode)
			}
			ok, cerr := a.confirmModeSwitch(ctx, current, a.opts.mode)
			if cerr != nil {
				return cerr
			}
			if !ok {
				return usageErrorf("未确认模式切换：当前 session 为 %s，请求 %s", current, a.opts.mode)
			}
			target = a.opts.mode
		} else {
			target = current
		}
	}
	if engine.Mode() != target {
		if err := engine.SwitchMode(ctx, target, "session_start", true); err != nil {
			return err
		}
	}
	if r, ok := a.render.(*textRenderer); ok {
		r.mode = target
	}
	return nil
}

// overrideFactory 把测试注入的 Provider 当成所有 provider 的适配器，
// 这样 Router/Client 仍然负责别名解析、重试与 model_request/model_response 事件。
type overrideFactory struct{ base provider.Provider }

func (f overrideFactory) New(config.ProviderSpec) (provider.Provider, error) { return f.base, nil }

func (a *App) buildProviders(sink model.EventSink) (provider.Registry, provider.Provider, error) {
	var factory provider.Factory
	if a.ovProvider != nil {
		factory = overrideFactory{base: a.ovProvider}
	} else {
		factory = provider.NewFactory(a.cfg, a.log)
	}
	reg := provider.NewRegistry(a.cfg, factory, a.log)
	settings := a.cfg.Settings()
	router := provider.NewRouter(reg,
		provider.NewRetrier(settings.Retry),
		provider.NewFallback(a.cfg, reg),
		sink,
	)
	return reg, router, nil
}

func (a *App) pricingFor(node model.NodeName) *model.Pricing {
	if a.registry == nil {
		return nil
	}
	res, err := a.registry.ResolveForNode(node)
	if err != nil {
		return nil
	}
	return res.Pricing
}

func (a *App) loadProjectDoc() {
	path := filepath.Join(a.workDir(), ".ant", "project.md")
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	hash := ""
	if raw, err := os.ReadFile(filepath.Join(a.workDir(), ".ant", "project.meta.json")); err == nil {
		var meta struct {
			Hash string `json:"hash"`
		}
		if json.Unmarshal(raw, &meta) == nil {
			hash = meta.Hash
		}
	}
	a.projectMu.Lock()
	a.projectDoc = prompt.ProjectDoc{Body: string(body), Hash: hash}
	a.projectMu.Unlock()
}

func (a *App) projectProjection() prompt.ProjectDoc {
	a.projectMu.RLock()
	defer a.projectMu.RUnlock()
	return a.projectDoc
}

func (a *App) startProjectInit(ctx context.Context) {
	if a.builder == nil || !a.cfg.Settings().Project.AutoInit {
		return
	}
	ch := make(chan struct{})
	a.initDone = ch
	sess := a.sess
	go func() {
		defer close(ch)
		doc, err := a.builder.Ensure(ctx, projectctx.EnsureOptions{
			Progress: func(stage string) { a.log.Debug("projectctx", "stage", stage) },
		})
		if err != nil {
			a.log.Warn("项目上下文初始化失败", "err", err)
			return
		}
		if doc.Path == "" {
			return
		}
		a.projectMu.Lock()
		a.projectDoc = prompt.ProjectDoc{Body: doc.Body, Hash: doc.Hash}
		a.projectMu.Unlock()
		if sess != nil && doc.Hash != "" {
			metaCtx := context.WithoutCancel(ctx)
			if err := sess.UpdateMeta(metaCtx, func(m *session.Meta) error {
				m.ProjectContextHash = doc.Hash
				return nil
			}); err != nil {
				a.log.Warn("写入 projectContextHash 失败", "err", err)
			}
		}
	}()
}

// Run 执行已装配的 App。
func (a *App) Run(ctx context.Context) int {
	switch a.opts.command {
	case "version":
		fmt.Fprintln(a.stdio.Out, "ant "+version.String())
		return 0
	case "help":
		a.printHelp()
		return 0
	case "models":
		return a.runModels(ctx)
	case "config":
		return a.runConfig(ctx)
	case "sessions":
		return a.runSessions(ctx)
	case "show":
		return a.runShow(ctx)
	case "export":
		return a.runExport(ctx)
	case "init":
		return a.runInit(ctx)
	}

	if a.engine == nil {
		return a.fail(usageErrorf("内部错误：engine 未装配"))
	}

	// 断点续传：先继续未完成的 turn，再处理新输入（§21.16.1）。
	if a.resumed != nil && shouldResume(a.resumed) {
		res, err := a.engine.ResumeTurn(ctx, resumeState(a.resumed))
		if err != nil {
			a.render.Notice("恢复失败：" + formatError(err))
			a.updateMeta(session.StatusFailed, model.Usage{})
			return exitCodeForError(err)
		}
		a.finishTurn(res)
		a.resumed = nil
		if a.opts.prompt == "" && !a.stdio.IsTTY {
			return res.ExitCode
		}
	}

	if a.opts.prompt != "" {
		return a.runTurn(ctx, a.opts.prompt)
	}
	if a.stdio.IsTTY {
		return a.repl(ctx)
	}
	return a.runPiped(ctx)
}

func (a *App) runPiped(ctx context.Context) int {
	data, err := io.ReadAll(a.stdio.In)
	if err != nil {
		return a.fail(err)
	}
	input := strings.TrimSpace(string(data))
	if input == "" {
		a.printHelp()
		return 2
	}
	return a.runTurn(ctx, input)
}

func (a *App) repl(ctx context.Context) int {
	if a.sess != nil {
		a.render.Notice(fmt.Sprintf("session %s · mode=%s · 输入 /help 查看命令", a.sess.Meta().ID, a.engine.Mode()))
	}
	for {
		fmt.Fprint(a.stdio.Out, "\nant> ")
		line, err := a.reader.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			fmt.Fprintln(a.stdio.Out)
			return 0
		}
		input := strings.TrimSpace(line)
		if input == "" {
			if err != nil {
				return 0
			}
			continue
		}
		if strings.HasPrefix(input, "/") {
			code, done := a.replCommand(ctx, input)
			if done {
				return code
			}
			continue
		}
		code := a.runTurn(ctx, input)
		if ctx.Err() != nil {
			return 3
		}
		if code == 2 {
			return code
		}
		if err != nil {
			return 0
		}
	}
}

func (a *App) replCommand(ctx context.Context, input string) (int, bool) {
	fields := strings.Fields(input)
	switch fields[0] {
	case "/exit", "/quit":
		return 0, true
	case "/help":
		fmt.Fprintln(a.stdio.Out, "命令：/exit /help /mode <code|plan|ask|council> /new")
		return 0, false
	case "/new":
		if err := a.newSession(ctx); err != nil {
			a.render.Notice(formatError(err))
			return 0, false
		}
		a.render.Notice(fmt.Sprintf("已新建 session %s · mode=%s", a.sess.Meta().ID, a.engine.Mode()))
		return 0, false
	case "/mode":
		if len(fields) != 2 {
			a.render.Notice("用法：/mode code|plan|ask|council")
			return 0, false
		}
		mode := model.Mode(fields[1])
		if !mode.Valid() {
			a.render.Notice("未知模式：" + fields[1])
			return 0, false
		}
		if err := a.switchMode(ctx, mode); err != nil {
			a.render.Notice(formatError(err))
			return 0, false
		}
		return 0, false
	default:
		a.render.Notice("未知命令 " + fields[0] + "；可用：/exit /help /mode /new")
		return 0, false
	}
}

// newSession 在 REPL 内关闭当前 session 并新建一个（§3.5 /new）。
func (a *App) newSession(ctx context.Context) error {
	if a.sess == nil || a.store == nil {
		return usageErrorf("内部错误：session 未装配")
	}
	if err := a.sess.Close(); err != nil {
		return err
	}
	a.sess = nil
	a.engine = nil
	a.resumed = nil
	a.history = nil
	a.summary = ""
	a.initDone = nil
	prevNew := a.opts.newSession
	prevID := a.opts.sessionID
	a.opts.newSession = true
	a.opts.sessionID = ""
	err := a.openAndWire(ctx)
	a.opts.newSession = prevNew
	a.opts.sessionID = prevID
	if err != nil {
		return err
	}
	return nil
}

func (a *App) switchMode(ctx context.Context, mode model.Mode) error {
	if a.engine.Mode() == mode {
		return nil
	}
	ok, err := a.confirmModeSwitch(ctx, a.engine.Mode(), mode)
	if err != nil {
		return err
	}
	if !ok {
		return usageErrorf("未确认模式切换")
	}
	if err := a.engine.SwitchMode(ctx, mode, "repl", true); err != nil {
		return err
	}
	if r, ok := a.render.(*textRenderer); ok {
		r.mode = mode
	}
	return nil
}

func (a *App) confirmModeSwitch(ctx context.Context, from, to model.Mode) (bool, error) {
	if !a.stdio.IsTTY || a.opts.nonInteractive {
		return false, approval.ErrNonInteractive
	}
	ans, err := a.prompter.Ask(ctx, approval.Question{
		Question: fmt.Sprintf("当前模式 %s，切换到 %s？", from, to),
		Options: []approval.Option{
			{ID: "yes", Label: "切换"},
			{ID: "no", Label: "保持当前模式"},
		},
	})
	if err != nil {
		return false, err
	}
	return ans.SelectedID == "yes", nil
}

func (a *App) runTurn(ctx context.Context, input string) int {
	if a.engine == nil || a.sess == nil {
		return a.fail(usageErrorf("内部错误：session 未装配"))
	}
	a.ensureTitle(ctx, input)
	if err := a.sess.UpdateMeta(ctx, func(m *session.Meta) error {
		m.Status = session.StatusRunning
		return nil
	}); err != nil {
		a.log.Warn("标记 session running 失败", "err", err)
	}
	if a.tracker != nil {
		a.tracker.Reset()
	}
	started := time.Now()
	res, err := a.engine.RunTurn(ctx, statemachine.TurnInput{UserInput: input, History: a.history, Summary: a.summary})
	if err != nil {
		a.render.Notice(formatError(err))
		code := exitCodeForError(err)
		status := statemachine.StatusFailed
		if ctx.Err() != nil || model.ClassOf(err) == model.ErrInterrupted {
			status = statemachine.StatusInterrupted
		}
		failure := statemachine.TurnResult{Status: status, Duration: time.Since(started), ExitCode: code}
		if a.tracker != nil {
			failure.Usage = a.tracker.Total()
		}
		if a.engine.Mode() == model.ModePlan && a.sess != nil {
			// plan gate 未确认/非交互时也告知 plan.md 位置（§8.3）。
			path := filepath.Join(a.workDir(), ".ant", "sessions", a.sess.Meta().ID, "plan.md")
			if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
				failure.PlanPath = path
			}
		}
		a.finishTurn(failure)
		return code
	}
	a.finishTurn(res)
	return res.ExitCode
}

func (a *App) ensureTitle(ctx context.Context, input string) {
	if a.sess == nil || strings.TrimSpace(input) == "" {
		return
	}
	if a.sess.Meta().Title != "" {
		return
	}
	title := truncateLine(strings.TrimSpace(input), 60)
	if err := a.sess.UpdateMeta(ctx, func(m *session.Meta) error {
		m.Title = title
		return nil
	}); err != nil {
		a.log.Warn("写入 session title 失败", "err", err)
	}
}

func (a *App) finishTurn(res statemachine.TurnResult) {
	if a.tracker != nil && res.Usage.IsZero() {
		res.Usage = a.tracker.Total()
	}
	a.render.TurnSummary(res, a.tracker)
	status := session.StatusIdle
	switch res.Status {
	case statemachine.StatusFailed:
		status = session.StatusFailed
	case statemachine.StatusInterrupted:
		status = session.StatusInterrupted
	}
	a.updateMeta(status, res.Usage)
	a.refreshHistory(context.Background())
}

func (a *App) updateMeta(status session.Status, delta model.Usage) {
	if a.sess == nil || a.engine == nil {
		return
	}
	mode := a.engine.Mode()
	err := a.sess.UpdateMeta(context.Background(), func(m *session.Meta) error {
		m.Status = status
		m.Mode = mode
		m.TurnCount++
		m.Usage = m.Usage.Add(delta)
		return nil
	})
	if err != nil {
		a.log.Warn("更新 meta 失败", "err", err)
	}
}

func (a *App) refreshHistory(ctx context.Context) {
	if a.sess == nil {
		return
	}
	st, err := session.Recover(ctx, a.sess, replayDecider{prompter: a.prompter, nonInteractive: true})
	if err != nil {
		a.log.Warn("折叠 session 历史失败", "err", err)
		return
	}
	a.history = st.Messages
	a.summary = st.Summary
	a.log.Debug("session 历史已折叠", "messages", len(a.history), "turn", st.Turn, "last", st.Last)
}

func shouldResume(st *session.State) bool {
	if st == nil {
		return false
	}
	if st.Last == model.EventTurnCompleted {
		// 失败/中断的 turn 也会写 turn_completed；只有成功才不恢复（§9.6 第 4 步）。
		return st.TurnStatus == "failed" || st.TurnStatus == "interrupted"
	}
	return st.Turn > 0 || len(st.Pending) > 0 || st.Last == model.EventModelRequest ||
		st.Last == model.EventSessionInterrupted || st.Last == model.EventSessionFailed
}

func resumeState(st *session.State) statemachine.ResumeState {
	return statemachine.ResumeState{
		Turn:    st.Turn,
		Step:    st.Step,
		Round:   st.Round,
		Done:    st.Done,
		History: st.Messages,
		Summary: st.Summary,
	}
}

func (a *App) fail(err error) int {
	if a.render != nil {
		a.render.Notice(formatError(err))
	} else {
		fmt.Fprintln(a.stdio.Err, "[ant] "+formatError(err))
	}
	return exitCodeForError(err)
}

// Close 释放锁、落盘 meta、等待异步初始化完成。
func (a *App) Close() error {
	if a.initDone != nil {
		select {
		case <-a.initDone:
		case <-time.After(5 * time.Second):
			a.log.Warn("等待项目上下文初始化超时")
		}
	}
	var firstErr error
	if a.sess != nil {
		if err := a.sess.Close(); err != nil {
			firstErr = err
		}
	}
	if a.logFile != nil {
		_ = a.logFile.Close()
	}
	return firstErr
}

// usageTracker 是 per-turn 的用量聚合器（实现 logx.UsageAggregator）。
type usageTracker struct {
	mu         sync.Mutex
	pricing    func(model.NodeName) *model.Pricing
	total      model.Usage
	byNode     map[model.NodeName]model.Usage
	byProvider map[string]model.Usage
}

func newUsageTracker(pricing func(model.NodeName) *model.Pricing) *usageTracker {
	return &usageTracker{pricing: pricing, byNode: map[model.NodeName]model.Usage{}, byProvider: map[string]model.Usage{}}
}

// Reset 在每次 turn 开始时清空。
func (t *usageTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total = model.Usage{}
	t.byNode = map[model.NodeName]model.Usage{}
	t.byProvider = map[string]model.Usage{}
}

func (t *usageTracker) Add(node model.NodeName, providerName string, u model.Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total = t.total.Add(u)
	t.byNode[node] = t.byNode[node].Add(u)
	if providerName != "" {
		t.byProvider[providerName] = t.byProvider[providerName].Add(u)
	}
}

func (t *usageTracker) Total() model.Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.total
}

func (t *usageTracker) ByNode() map[model.NodeName]model.Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[model.NodeName]model.Usage, len(t.byNode))
	for k, v := range t.byNode {
		out[k] = v
	}
	return out
}

func (t *usageTracker) ByProvider() map[string]model.Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]model.Usage, len(t.byProvider))
	for k, v := range t.byProvider {
		out[k] = v
	}
	return out
}

func (t *usageTracker) Cost() (float64, bool) {
	calc := ctxmgr.NewCostCalculator()
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.byNode) == 0 || t.pricing == nil {
		return 0, false
	}
	names := make([]string, 0, len(t.byNode))
	for n := range t.byNode {
		names = append(names, string(n))
	}
	sort.Strings(names)
	var total float64
	known := true
	for _, raw := range names {
		node := model.NodeName(raw)
		p := t.pricing(node)
		amount, ok := calc.Cost(p, t.byNode[node])
		if !ok {
			known = false
			continue
		}
		total += amount
	}
	return total, known
}

func (a *App) printHelp() {
	fmt.Fprint(a.stdio.Out, usageText)
	if len(a.opts.rest) > 0 {
		fmt.Fprintln(a.stdio.Out)
		fmt.Fprintln(a.stdio.Out, commandHelp)
	}
}

// sessionPlanWriter 把 plan.md 写到当前 session 目录（§9.1、§8.3）。
type sessionPlanWriter struct {
	sess    session.Session
	workDir string
}

func (w *sessionPlanWriter) WritePlan(ctx context.Context, plan statemachine.Plan) (string, error) {
	body := plan.Body
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if w.sess != nil {
		if _, err := w.sess.PutArtifact(ctx, "plan.md", []byte(body), "text/markdown; charset=utf-8"); err != nil {
			return "", err
		}
		if plan.Round > 0 {
			_, _ = w.sess.PutArtifact(ctx, fmt.Sprintf("artifacts/plan/round-%d.md", plan.Round), []byte(body), "text/markdown; charset=utf-8")
		}
		return filepath.Join(w.workDir, ".ant", "sessions", w.sess.Meta().ID, "plan.md"), nil
	}
	path := plan.Path
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(w.workDir, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
