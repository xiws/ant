package tool

import (
	"time"

	"github.com/xiws/ant/internal/config"
)

const (
	defaultToolTimeoutMs  = 5000
	defaultBashTimeoutMs  = 120000
	defaultMaxOutputBytes = 16384
	// maxParallelReads 是只读工具一批内的并发上限（docs/08-tools.md §10.2）。
	maxParallelReads = 4
)

// settingsOf 返回配置视图的只读设置；nil 视图按零值处理。
func settingsOf(cfg config.View) config.Settings {
	if cfg == nil {
		return config.Settings{}
	}
	return cfg.Settings()
}

// workDirOf 取 workspace 绝对路径；View 未实现 config.Paths 时返回空串。
func workDirOf(cfg config.View) string {
	if cfg == nil {
		return ""
	}
	if p, ok := cfg.(config.Paths); ok {
		return p.WorkDir()
	}
	return ""
}

// msDuration 把毫秒配置转成 Duration；非法值用 fallback。
func msDuration(ms, fallback int) time.Duration {
	if ms <= 0 {
		ms = fallback
	}
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// maxOutputBytesOf 取回传模型的输出上限。
func maxOutputBytesOf(cfg config.View) int {
	if n := settingsOf(cfg).Limits.MaxToolOutputBytes; n > 0 {
		return n
	}
	return defaultMaxOutputBytes
}

// snippet 截取用于错误消息的原文片段。
func snippet(b []byte) string {
	const max = 200
	s := string(b)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
