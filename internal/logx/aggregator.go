package logx

import (
	"sort"
	"sync"

	"github.com/xiws/ant/internal/model"
)

// UsageAggregator 聚合 token 与费用（§15.4）。
type UsageAggregator interface {
	Add(node model.NodeName, providerName string, u model.Usage)
	Total() model.Usage
	ByNode() map[model.NodeName]model.Usage
	ByProvider() map[string]model.Usage
	Cost() (float64, bool)
}

type usageAggregator struct {
	mu         sync.Mutex
	total      model.Usage
	byNode     map[model.NodeName]model.Usage
	byProvider map[string]model.Usage
	pricing    func(model.NodeName) *model.Pricing
}

// NewUsageAggregator 构造聚合器。pricing 可为 nil；为空时 Cost 返回 known=false。
func NewUsageAggregator(pricing func(model.NodeName) *model.Pricing) UsageAggregator {
	return &usageAggregator{
		byNode:     map[model.NodeName]model.Usage{},
		byProvider: map[string]model.Usage{},
		pricing:    pricing,
	}
}

func (a *usageAggregator) Add(node model.NodeName, providerName string, u model.Usage) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.total = a.total.Add(u)
	a.byNode[node] = a.byNode[node].Add(u)
	if providerName != "" {
		a.byProvider[providerName] = a.byProvider[providerName].Add(u)
	}
}

func (a *usageAggregator) Total() model.Usage {
	if a == nil {
		return model.Usage{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total
}

func (a *usageAggregator) ByNode() map[model.NodeName]model.Usage {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[model.NodeName]model.Usage, len(a.byNode))
	for k, v := range a.byNode {
		out[k] = v
	}
	return out
}

func (a *usageAggregator) ByProvider() map[string]model.Usage {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]model.Usage, len(a.byProvider))
	for k, v := range a.byProvider {
		out[k] = v
	}
	return out
}

// Cost 按 pricing 估算总费用；任一使用过的节点缺少 pricing 时 known=false（§11.7）。
func (a *usageAggregator) Cost() (float64, bool) {
	if a == nil || a.pricing == nil {
		return 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.byNode) == 0 {
		return 0, false
	}
	var total float64
	known := true
	nodes := make([]string, 0, len(a.byNode))
	for n := range a.byNode {
		nodes = append(nodes, string(n))
	}
	sort.Strings(nodes)
	for _, raw := range nodes {
		node := model.NodeName(raw)
		p := a.pricing(node)
		if p == nil {
			known = false
			continue
		}
		u := a.byNode[node]
		cachedPrice := p.CachedInputPer1M
		if cachedPrice == 0 {
			cachedPrice = p.InputPer1M
		}
		uncached := u.Input - u.Cached
		if uncached < 0 {
			uncached = 0
		}
		total += float64(uncached) / 1e6 * p.InputPer1M
		total += float64(u.Cached) / 1e6 * cachedPrice
		total += float64(u.Output) / 1e6 * p.OutputPer1M
	}
	return total, known
}
