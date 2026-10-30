package config

import (
	"encoding/json"

	"github.com/xiws/ant/internal/model"
)

// defaultSettings 返回 docs/02-config.md §4.5 的内置默认值。
func defaultSettings() Settings {
	return Settings{
		SchemaVersion:  1,
		DefaultMode:    model.ModeCode,
		ModelPolicy:    "strict",
		FallbackModels: []string{},
		NodeModels:     map[string]string{},
		Stream:         true,
		Log:            LogSettings{Level: "info", Format: "text"},
		UI:             UISettings{Color: "auto"},
		Approval:       ApprovalSettings{Policy: "risk-based", Allow: []string{}, Deny: []string{}},
		Limits: Limits{
			MaxToolTurns:          12,
			MaxRounds:             map[string]int{"code": 3, "plan": 3, "council": 3},
			ToolTimeoutMs:         5000,
			BashTimeoutMs:         120000,
			BashMaxTimeoutMs:      600000,
			MaxToolOutputBytes:    16384,
			MaxFileReadBytes:      262144,
			MaxFileReadLines:      2000,
			MaxWriteBytes:         2097152,
			MaxIdenticalToolCalls: 3,
			NodeTimeoutMs:         900000,
			MaxSessionTokens:      0,
		},
		Context: ContextSettings{
			AutoCompact:       true,
			CompactAt:         0.8,
			KeepRecentTurns:   6,
			SummaryMaxTokens:  4096,
			SafetyMarginRatio: 0.05,
		},
		Retry: RetrySettings{
			Attempts:    3,
			BaseDelayMs: 1000,
			MaxDelayMs:  30000,
			JitterRatio: 0.2,
			RetryOn:     []any{408, 429, 500, 502, 503, 504, "network", "stream"},
		},
		Sandbox: SandboxSettings{
			WorkspaceOnly:   true,
			AllowReadPaths:  []string{},
			AllowWritePaths: []string{},
			ProtectedPaths:  []string{".git", ".ant"},
			Network:         "ask",
			HardDenyExtra:   []string{},
		},
		Project: ProjectSettings{
			AutoInit:       true,
			MaxFiles:       500,
			MaxBytes:       32768,
			MaxOutputBytes: 8192,
			Depth:          3,
			Exclude:        []string{},
		},
	}
}

func defaultSettingsMap() map[string]any {
	b, err := json.Marshal(defaultSettings())
	if err != nil {
		// 默认值都是可编码的基础类型；失败意味着代码错误，直接返回空表。
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// 已知键集合用于「未知字段只 warning」。
var knownSettingsKeys = map[string]map[string]bool{
	"": {
		"schemaVersion": true, "defaultModel": true, "defaultProvider": true,
		"nodeModels": true, "modelPolicy": true, "fallbackModels": true,
		"defaultMode": true, "stream": true, "log": true, "ui": true,
		"approval": true, "limits": true, "context": true, "retry": true,
		"sandbox": true, "project": true,
	},
	"log":      {"level": true, "format": true, "file": true},
	"ui":       {"color": true},
	"approval": {"policy": true, "allow": true, "deny": true},
	"limits": {
		"maxToolTurns": true, "maxRounds": true, "toolTimeoutMs": true,
		"bashTimeoutMs": true, "bashMaxTimeoutMs": true, "maxToolOutputBytes": true,
		"maxFileReadBytes": true, "maxFileReadLines": true, "maxWriteBytes": true,
		"maxIdenticalToolCalls": true, "nodeTimeoutMs": true, "maxSessionTokens": true,
	},
	"context": {
		"autoCompact": true, "compactAt": true, "keepRecentTurns": true,
		"summaryMaxTokens": true, "summaryModel": true, "safetyMarginRatio": true,
	},
	"retry": {"attempts": true, "baseDelayMs": true, "maxDelayMs": true, "jitterRatio": true, "retryOn": true},
	"sandbox": {
		"workspaceOnly": true, "allowReadPaths": true, "allowWritePaths": true,
		"protectedPaths": true, "network": true, "shell": true, "hardDenyExtra": true,
	},
	"project": {
		"autoInit": true, "projectModel": true, "maxFiles": true, "maxBytes": true,
		"maxOutputBytes": true, "depth": true, "exclude": true,
	},
}

var knownProviderKeys = map[string]bool{
	"name": true, "api": true, "baseUrl": true, "apiKey": true, "apiKeyEnv": true,
	"headers": true, "timeoutMs": true, "maxConcurrency": true, "retry": true, "models": true,
}

var knownModelKeys = map[string]bool{
	"id": true, "name": true, "contextWindow": true, "maxOutputTokens": true,
	"supportsTools": true, "supportsStreaming": true, "supportsParallelToolCalls": true,
	"reasoning": true, "defaultTemperature": true, "pricing": true, "notes": true,
}
