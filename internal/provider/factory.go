package provider

import (
	"fmt"
	"sync"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
)

// AdapterBuilder 按 provider 配置构造适配器；新协议通过它注册（§21.19）。
type AdapterBuilder func(spec config.ProviderSpec) (Provider, error)

type factory struct {
	cfg      config.View
	log      logx.Logger
	mu       sync.RWMutex
	builders map[string]AdapterBuilder
}

// NewFactory 按 provider 配置造适配器。
//
// 抽象层阶段只冻结装配面：`openai-completions` 与 `otter` 均返回
// adapter_not_implemented；后续实现适配器时用 RegisterAdapter 注册即可，
// 不影响 Registry/Router/节点与测试替身。
func NewFactory(cfg config.View, log logx.Logger) Factory {
	if log == nil {
		log = logx.Nop()
	}
	f := &factory{cfg: cfg, log: log, builders: map[string]AdapterBuilder{}}
	f.builders[string(API("openai-completions"))] = notImplemented("openai-completions", "OpenAI 兼容协议适配器将在 provider 实现阶段接入")
	f.builders[string(API("otter"))] = notImplemented("otter", "otter 缺少公开协议，按实验性处理（§5.6）")
	return f
}

// API 是 provider API 标识；单独定义以便扩展点复用。
type API string

// RegisterAdapter 注册某协议的适配器构造器。f 必须是 NewFactory 的返回值。
func RegisterAdapter(f Factory, api string, b AdapterBuilder) error {
	ff, ok := f.(*factory)
	if !ok {
		return fmt.Errorf("provider: RegisterAdapter 需要 *factory，实际 %T", f)
	}
	ff.mu.Lock()
	defer ff.mu.Unlock()
	ff.builders[api] = b
	return nil
}

func (f *factory) New(spec config.ProviderSpec) (Provider, error) {
	f.mu.RLock()
	b, ok := f.builders[spec.API]
	f.mu.RUnlock()
	if !ok {
		return nil, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "unknown_api",
			Message: fmt.Sprintf("provider %s: 未知 api %q", spec.DisplayName(), spec.API),
			Hint:    "可选 openai-completions 或 otter",
		}
	}
	f.log.Debug("construct provider adapter", "provider", spec.Key, "api", spec.API, "baseUrl", spec.BaseURL, "models", len(spec.Models))
	return b(spec)
}

func notImplemented(api, hint string) AdapterBuilder {
	return func(spec config.ProviderSpec) (Provider, error) {
		return nil, &model.Error{
			Class:   model.ErrAdapterNotImplemented,
			Code:    "adapter_not_implemented",
			Message: fmt.Sprintf("provider %s 的适配器 %s 尚未实现（抽象层阶段）", spec.DisplayName(), api),
			Hint:    hint,
			Details: map[string]any{"provider": spec.Key, "api": api},
		}
	}
}
