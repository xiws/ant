package provider

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
)

type registry struct {
	cfg     config.View
	factory Factory
	log     logx.Logger

	mu       sync.Mutex
	adapters map[string]Provider
	warned   map[string]bool
}

// NewRegistry 构造别名解析与适配器缓存。log 可为 nil。
func NewRegistry(cfg config.View, f Factory, log logx.Logger) Registry {
	if log == nil {
		log = logx.Nop()
	}
	return &registry{cfg: cfg, factory: f, log: log, adapters: map[string]Provider{}, warned: map[string]bool{}}
}

func (r *registry) Resolve(alias string) (Resolved, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return Resolved{}, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "model_required",
			Message: "未指定模型",
			Hint:    "使用 -p/--model，或在 settings.json 设置 defaultModel",
			Details: map[string]any{"candidates": r.modelNames()},
		}
	}

	// 1. provider:name（显式限定 provider）。
	if i := strings.IndexByte(alias, ':'); i > 0 {
		providerName, modelName := alias[:i], alias[i+1:]
		p, ok := r.findProvider(providerName)
		if !ok {
			return Resolved{}, r.unknown(alias, fmt.Sprintf("provider %q 不存在", providerName))
		}
		m, ok := findModel(p.Models, modelName)
		if !ok {
			return Resolved{}, r.unknown(alias, fmt.Sprintf("provider %q 下没有模型 %q", providerName, modelName))
		}
		return resolved(p, m), nil
	}

	var byName, byID []Resolved
	for _, p := range r.cfg.Providers() {
		for _, m := range p.Models {
			if m.Name == alias {
				byName = append(byName, resolved(p, m))
			}
			if m.ID == alias {
				byID = append(byID, resolved(p, m))
			}
		}
	}
	if len(byName) == 1 {
		return byName[0], nil
	}
	if len(byName) > 1 {
		return Resolved{}, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "ambiguous_model",
			Message: fmt.Sprintf("模型别名 %q 出现在多个 provider 中", alias),
			Hint:    "使用 provider:name 形式消除歧义",
			Details: map[string]any{"candidates": aliasesOf(byName)},
		}
	}
	// 3. 全局唯一的 id（兼容 defaultProvider 填 id 的写法，warning）。
	if len(byID) == 1 {
		r.warnOnce("id:"+alias, "模型 %q 按 id 解析到别名 %q；建议改用 name", alias, byID[0].Alias)
		return byID[0], nil
	}
	if len(byID) > 1 {
		return Resolved{}, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "ambiguous_model",
			Message: fmt.Sprintf("模型 id %q 出现在多个 provider 中", alias),
			Hint:    "使用 provider:name 形式消除歧义",
			Details: map[string]any{"candidates": aliasesOf(byID)},
		}
	}
	return Resolved{}, r.unknown(alias, "")
}

func (r *registry) ResolveForNode(node model.NodeName) (Resolved, error) {
	s := r.cfg.Settings()
	alias := ""
	switch {
	case s.CLINodeModels[string(node)] != "":
		alias = s.CLINodeModels[string(node)]
	case s.CLIModel != "":
		alias = s.CLIModel
	case s.NodeModels[string(node)] != "":
		alias = s.NodeModels[string(node)]
	case s.DefaultModel != "":
		alias = s.DefaultModel
	case s.DefaultProvider != "":
		alias = s.DefaultProvider
	default:
		models := r.cfg.Models()
		if len(models) == 1 {
			alias = models[0].Name
			r.warnOnce("single-model", "未配置 defaultModel，使用唯一模型 %q", alias)
		}
	}
	if alias == "" {
		return Resolved{}, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "model_required",
			Message: fmt.Sprintf("节点 %s 没有可用模型，且不存在唯一模型可回落", node),
			Hint:    "在 settings.json 设置 defaultModel，或使用 -p/--model",
			Details: map[string]any{"candidates": r.modelNames()},
		}
	}
	res, err := r.Resolve(alias)
	if err != nil {
		return Resolved{}, err
	}
	if !res.Capabilities.SupportsTools {
		if s.ModelPolicy == "degraded" && node == model.NodeAsk {
			r.warnOnce("degraded:"+res.Alias, "模型 %q 不支持工具调用；degraded 策略下仅 ask 节点可用", res.Alias)
			return res, nil
		}
		return Resolved{}, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "model_capability",
			Message: fmt.Sprintf("节点 %s 需要工具调用，但模型 %q 不支持（modelPolicy=%s）", node, res.Alias, s.ModelPolicy),
			Hint:    "为该节点选择 supportsTools=true 的模型，或只使用 ask 模式",
			Details: map[string]any{"node": string(node), "model": res.Alias},
		}
	}
	return res, nil
}

func (r *registry) Capabilities(alias string) (model.Capabilities, error) {
	res, err := r.Resolve(alias)
	if err != nil {
		return model.Capabilities{}, err
	}
	return res.Capabilities, nil
}

func (r *registry) Adapter(res Resolved) (Provider, error) {
	key := res.ProviderName
	r.mu.Lock()
	if p, ok := r.adapters[key]; ok {
		r.mu.Unlock()
		return p, nil
	}
	r.mu.Unlock()

	spec, ok := r.findProvider(key)
	if !ok {
		return nil, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "provider_missing",
			Message: fmt.Sprintf("provider %q 不存在", key),
		}
	}
	p, err := r.factory.New(spec)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.adapters[key]; ok {
		return existing, nil
	}
	r.adapters[key] = p
	return p, nil
}

func (r *registry) unknown(alias, reason string) error {
	msg := fmt.Sprintf("无法解析模型 %q", alias)
	if reason != "" {
		msg = fmt.Sprintf("无法解析模型 %q：%s", alias, reason)
	}
	return &model.Error{
		Class:   model.ErrConfigInvalid,
		Code:    "unknown_model",
		Message: msg,
		Hint:    "可用模型别名： " + strings.Join(r.modelNames(), ", "),
		Details: map[string]any{"alias": alias, "candidates": r.modelNames()},
	}
}

func (r *registry) findProvider(nameOrKey string) (config.ProviderSpec, bool) {
	for _, p := range r.cfg.Providers() {
		if p.Key == nameOrKey || p.Name == nameOrKey {
			return p, true
		}
	}
	return config.ProviderSpec{}, false
}

func (r *registry) modelNames() []string {
	models := r.cfg.Models()
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.Name)
	}
	sort.Strings(out)
	return out
}

func (r *registry) warnOnce(key, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.warned[key] {
		return
	}
	r.warned[key] = true
	r.log.Warn(fmt.Sprintf(format, args...))
}

func findModel(specs []config.ModelSpec, nameOrID string) (config.ModelSpec, bool) {
	for _, m := range specs {
		if m.Name == nameOrID || m.ID == nameOrID {
			return m, true
		}
	}
	return config.ModelSpec{}, false
}

func resolved(p config.ProviderSpec, m config.ModelSpec) Resolved {
	return Resolved{
		Alias:        m.Name,
		ProviderName: p.Key,
		ModelID:      m.ID,
		Capabilities: m.Capabilities(),
		Pricing:      m.Pricing,
	}
}

func aliasesOf(list []Resolved) []string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r.ProviderName+":"+r.Alias)
	}
	sort.Strings(out)
	return out
}
