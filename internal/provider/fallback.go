package provider

import (
	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

type fallbackChain struct {
	aliases []string
	reg     Registry
}

// NewFallback 按 settings.fallbackModels 构造回退链；默认空列表 = 永不回退。
func NewFallback(cfg config.View, reg Registry) Fallback {
	return &fallbackChain{aliases: append([]string(nil), cfg.Settings().FallbackModels...), reg: reg}
}

// Next 只在 provider 不可用/限流时切换，并跳过当前别名（§13.6）。
func (f *fallbackChain) Next(cur Resolved, class model.ErrorClass) (Resolved, bool) {
	if f == nil || len(f.aliases) == 0 {
		return Resolved{}, false
	}
	switch class {
	case model.ErrProviderUnavailable, model.ErrProviderRateLimit:
	default:
		return Resolved{}, false
	}
	start := 0
	for i, a := range f.aliases {
		if a == cur.Alias || a == cur.ProviderName+":"+cur.Alias {
			start = i + 1
			break
		}
	}
	for _, alias := range f.aliases[start:] {
		if alias == cur.Alias || alias == cur.ProviderName+":"+cur.Alias {
			continue
		}
		next, err := f.reg.Resolve(alias)
		if err != nil {
			continue
		}
		return next, true
	}
	return Resolved{}, false
}
