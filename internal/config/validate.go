package config

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/xiws/ant/internal/model"
)

func (c *Config) validate() error {
	cfgErr := func(code, format string, args ...any) error {
		return &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    code,
			Message: fmt.Sprintf(format, args...),
			Hint:    "运行 `ant config` 查看合并后的配置",
		}
	}

	s := &c.settings
	if !s.DefaultMode.Valid() {
		return cfgErr("default_mode", "defaultMode=%q 不是合法模式（code|plan|ask|council）", s.DefaultMode)
	}
	switch s.ModelPolicy {
	case "strict", "degraded":
	default:
		return cfgErr("model_policy", "modelPolicy=%q 不是 strict|degraded", s.ModelPolicy)
	}
	switch s.Log.Level {
	case "error", "warn", "info", "debug":
	default:
		return cfgErr("log_level", "log.level=%q 不是 error|warn|info|debug", s.Log.Level)
	}
	switch s.Log.Format {
	case "text", "json":
	default:
		return cfgErr("log_format", "log.format=%q 不是 text|json", s.Log.Format)
	}
	switch s.UI.Color {
	case "auto", "always", "never":
	default:
		return cfgErr("ui_color", "ui.color=%q 不是 auto|always|never", s.UI.Color)
	}
	switch s.Approval.Policy {
	case "risk-based", "always", "never":
	default:
		return cfgErr("approval_policy", "approval.policy=%q 不是 risk-based|always|never", s.Approval.Policy)
	}
	switch s.Sandbox.Network {
	case "ask", "allow", "deny":
	default:
		return cfgErr("sandbox_network", "sandbox.network=%q 不是 ask|allow|deny", s.Sandbox.Network)
	}
	if s.Limits.MaxToolTurns <= 0 {
		return cfgErr("limits", "limits.maxToolTurns 必须 > 0")
	}
	if s.Limits.MaxToolOutputBytes <= 0 {
		return cfgErr("limits", "limits.maxToolOutputBytes 必须 > 0")
	}
	for mode, n := range s.Limits.MaxRounds {
		if n <= 0 {
			return cfgErr("limits", "limits.maxRounds.%s 必须 > 0", mode)
		}
		if !model.Mode(mode).Valid() {
			c.addWarning("limits.maxRounds 引用了未知模式 %s", mode)
		}
	}
	if s.Context.CompactAt <= 0 || s.Context.CompactAt >= 1 {
		return cfgErr("context", "context.compactAt 必须在 (0,1) 内")
	}
	if s.Context.SafetyMarginRatio < 0 || s.Context.SafetyMarginRatio >= 0.5 {
		return cfgErr("context", "context.safetyMarginRatio 必须在 [0,0.5) 内")
	}
	if s.Context.KeepRecentTurns < 0 {
		return cfgErr("context", "context.keepRecentTurns 不能为负")
	}
	if s.Retry.Attempts < 1 {
		return cfgErr("retry", "retry.attempts 必须 >= 1")
	}
	if s.Retry.BaseDelayMs < 0 || s.Retry.MaxDelayMs < 0 {
		return cfgErr("retry", "retry.baseDelayMs/maxDelayMs 不能为负")
	}
	if s.Retry.JitterRatio < 0 || s.Retry.JitterRatio > 1 {
		return cfgErr("retry", "retry.jitterRatio 必须在 [0,1] 内")
	}

	// providers / models 校验（§4.4）。
	seenNames := map[string]string{}
	for _, p := range c.providers {
		switch p.API {
		case "openai-completions":
			if p.BaseURL == "" {
				return cfgErr("provider_base_url", "provider %s: api=openai-completions 必须提供 baseUrl", p.DisplayName())
			}
		case "otter":
			// 实验性适配器：协议未公开，选中时报 adapter_not_implemented（§5.6）。
		default:
			return cfgErr("provider_api", "provider %s: 未知 api %q（可选 openai-completions|otter）", p.DisplayName(), p.API)
		}
		if p.API == "openai-completions" {
			u, err := url.Parse(p.BaseURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return cfgErr("provider_base_url", "provider %s: baseUrl=%q 不是合法 URL", p.DisplayName(), p.BaseURL)
			}
		}
		if p.MaxConcurrency < 1 {
			return cfgErr("provider_concurrency", "provider %s: maxConcurrency 必须 >= 1", p.DisplayName())
		}
		if len(p.Models) == 0 {
			return cfgErr("provider_models", "provider %s: models 至少一个", p.DisplayName())
		}
		seenIDs := map[string]bool{}
		for _, m := range p.Models {
			if strings.TrimSpace(m.Name) == "" {
				return cfgErr("model_name", "provider %s: 存在空模型别名（models[].name）", p.DisplayName())
			}
			if _, dup := seenNames[m.Name]; dup {
				return cfgErr("model_name_unique", "模型别名 %q 重复；别名必须全局唯一", m.Name)
			}
			seenNames[m.Name] = p.Key
			if strings.TrimSpace(m.ID) == "" {
				return cfgErr("model_id", "模型 %q: id 不能为空", m.Name)
			}
			if seenIDs[m.ID] {
				return cfgErr("model_id_unique", "provider %s: 模型 id %q 重复", p.DisplayName(), m.ID)
			}
			seenIDs[m.ID] = true
			if m.ContextWindow <= 0 {
				return cfgErr("context_window", "模型 %q: contextWindow 必须 > 0", m.Name)
			}
			if m.MaxOutputTokens <= 0 || m.MaxOutputTokens >= m.ContextWindow {
				return cfgErr("max_output", "模型 %q: maxOutputTokens 必须 > 0 且小于 contextWindow", m.Name)
			}
		}
	}

	// 引用校验。
	refs := []struct {
		key   string
		value string
	}{
		{"defaultModel", s.DefaultModel},
		{"context.summaryModel", s.Context.SummaryModel},
		{"project.projectModel", s.Project.ProjectModel},
	}
	for _, ref := range refs {
		if ref.value != "" && !c.aliasExists(ref.value) {
			return cfgErr("model_reference", "%s 引用了不存在的模型 %q；可用别名：%s", ref.key, ref.value, strings.Join(c.modelNames(), ", "))
		}
	}
	for mode, alias := range s.NodeModels {
		if !isKnownNode(mode) {
			c.addWarning("nodeModels 引用了未知节点 %q（内置节点：code/ask/verifier/deliberate/aggregation）", mode)
		}
		if alias != "" && !c.aliasExists(alias) {
			return cfgErr("model_reference", "nodeModels.%s 引用了不存在的模型 %q", mode, alias)
		}
	}
	for node, alias := range s.CLINodeModels {
		if !isKnownNode(node) {
			c.addWarning("--node-model 引用了未知节点 %q", node)
		}
		if !c.aliasExists(alias) {
			return cfgErr("model_reference", "--node-model %s=%s 引用了不存在的模型", node, alias)
		}
	}
	for _, alias := range s.FallbackModels {
		if !c.aliasExists(alias) {
			return cfgErr("model_reference", "fallbackModels 引用了不存在的模型 %q", alias)
		}
	}
	if s.CLIModel != "" && !c.aliasExists(s.CLIModel) {
		return cfgErr("model_reference", "-p/--model 引用了不存在的模型 %q", s.CLIModel)
	}
	return nil
}

func (c *Config) aliasExists(alias string) bool {
	if alias == "" {
		return false
	}
	if i := strings.IndexByte(alias, ':'); i > 0 {
		provider, name := alias[:i], alias[i+1:]
		for _, p := range c.providers {
			if p.Key != provider && p.Name != provider {
				continue
			}
			for _, m := range p.Models {
				if m.Name == name || m.ID == name {
					return true
				}
			}
		}
		return false
	}
	for _, m := range c.models {
		if m.Name == alias || m.ID == alias {
			return true
		}
	}
	return false
}

func (c *Config) modelNames() []string {
	out := make([]string, 0, len(c.models))
	for _, m := range c.models {
		out = append(out, m.Name)
	}
	return out
}

func isKnownNode(name string) bool {
	for _, n := range model.AllNodes() {
		if string(n) == name {
			return true
		}
	}
	return false
}
