package ctxmgr

import "github.com/xiws/ant/internal/model"

type costCalculator struct{}

// NewCostCalculator 返回按 models[].pricing 估算费用的计算器（§11.7）。
func NewCostCalculator() CostCalculator { return costCalculator{} }

func (costCalculator) Cost(p *model.Pricing, u model.Usage) (float64, bool) {
	if p == nil {
		return 0, false
	}
	cachedPrice := p.CachedInputPer1M
	if cachedPrice == 0 {
		cachedPrice = p.InputPer1M
	}
	uncached := u.Input - u.Cached
	if uncached < 0 {
		uncached = 0
	}
	amount := float64(uncached)/1e6*p.InputPer1M +
		float64(u.Cached)/1e6*cachedPrice +
		float64(u.Output)/1e6*p.OutputPer1M
	return amount, true
}
