package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/session"
	"github.com/xiws/ant/internal/version"
	"github.com/xiws/ant/internal/web"
)

// runServe 是 `ant serve` 的组合根分支：只读归档 + Web 服务。
func (a *App) runServe(ctx context.Context) int {
	wd := a.workDir()
	if abs, err := filepath.Abs(wd); err == nil {
		wd = abs
	}

	// 1. 尽力加载配置；失败只告警，不阻塞历史浏览。
	cfg, cfgErr := a.loadConfigBestEffort(ctx, wd)
	if cfgErr != nil {
		fmt.Fprintf(a.stdio.Err, "[ant] 配置告警：%s\n", formatError(cfgErr))
	}

	// 2. 只读归档 + Web 服务。
	archive := session.NewArchive(wd)
	logger := a.serveLogger(cfg)
	srv, err := web.New(web.Deps{
		WorkDir: wd,
		HomeDir: a.homeDir(),
		Config:  cfg,
		Archive: archive,
		Clock:   a.clock,
		Logger:  logger,
		Version: version.Version,
	})
	if err != nil {
		fmt.Fprintln(a.stdio.Err, "[ant] serve 初始化失败："+formatError(err))
		return 1
	}

	// 3. 监听并校验回环。
	listen := a.opts.listen
	if listen == "" {
		listen = "127.0.0.1:7788"
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		fmt.Fprintf(a.stdio.Err, "[ant] 无法监听 %s：%v\n", listen, err)
		return 2
	}
	if !isLoopbackAddr(ln.Addr()) {
		_ = ln.Close()
		fmt.Fprintln(a.stdio.Err, "[ant] 拒绝非回环地址；ant serve 只允许 127.0.0.1/localhost/[::1]")
		return 2
	}

	url := "http://" + ln.Addr().String()
	fmt.Fprintf(a.stdio.Out, "[ant] serve: %s\n", url)
	fmt.Fprintf(a.stdio.Out, "      workspace: %s\n", wd)
	fmt.Fprintf(a.stdio.Out, "      home:      %s\n", a.homeDir())
	fmt.Fprintln(a.stdio.Out, "      只读模式 · 无需登录 · Ctrl-C 停止")

	if a.opts.open {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(a.stdio.Err, "[ant] 无法自动打开浏览器：%v\n", err)
		}
	}

	// 4. 阻塞到信号；Serve 内部负责优雅关闭。
	if err := srv.Serve(ctx, ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(a.stdio.Err, "[ant] serve 异常退出："+formatError(err))
		return 1
	}
	fmt.Fprintln(a.stdio.Out, "[ant] serve 已停止")
	return 0
}

// loadConfigBestEffort 加载配置但把错误返回给调用方，不吞掉 warning。
func (a *App) loadConfigBestEffort(ctx context.Context, wd string) (config.View, error) {
	if a.cfgOverride != nil {
		return a.cfgOverride, nil
	}
	loader := config.NewLoader()
	cfg, err := loader.Load(ctx, config.Options{
		WorkDir: wd,
		HomeDir: a.homeDir(),
		Env:     a.env,
		CLI: config.Overrides{
			LogLevel: a.opts.logLevel,
			NoColor:  a.opts.noColor,
		},
	})
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// serveLogger 只写 stderr，不写 .ant/logs（只读保证）。
func (a *App) serveLogger(cfg config.View) logx.Logger {
	settings := config.LogSettings{Level: "info", Format: "text"}
	if cfg != nil {
		settings = cfg.Settings().Log
		settings.File = ""
	}
	if a.opts.logLevel != "" {
		settings.Level = a.opts.logLevel
	}
	return logx.New(settings, a.stdio.Err)
}

// isLoopbackAddr 报告 listener 地址是否回环。
func isLoopbackAddr(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

// openBrowser 尝试用系统浏览器打开 URL；失败只告警，不影响服务。
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
