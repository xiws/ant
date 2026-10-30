package tool

import (
	"sort"
	"sync"

	"github.com/xiws/ant/internal/model"
)

// Catalog 是可注册的 Registry 实现：注册、查找与按节点权限过滤。
type Catalog struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string
}

// NewCatalog 构造空注册表。
func NewCatalog() *Catalog {
	return &Catalog{tools: map[string]Tool{}}
}

// Register 注册工具；同名覆盖。
func (c *Catalog) Register(t Tool) {
	if t == nil {
		return
	}
	spec := t.Spec()
	if spec.Name == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.tools[spec.Name]; !exists {
		c.order = append(c.order, spec.Name)
	}
	c.tools[spec.Name] = t
}

// Lookup 实现 Registry。
func (c *Catalog) Lookup(name string) (Tool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tools[name]
	return t, ok
}

// Specs 实现 Registry：按 §7.3 权限矩阵过滤并按 name 排序。
func (c *Catalog) Specs(node model.NodeName) []model.ToolSpec {
	allowed := Permissions[node]
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]model.ToolSpec, 0, len(c.order))
	for _, name := range c.order {
		t := c.tools[name]
		if t == nil || !allowName(allowed, name) {
			continue
		}
		out = append(out, t.Spec())
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names 返回注册顺序下的工具名（不属于 Registry 契约，供组合根/测试使用）。
func (c *Catalog) Names() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, len(c.order))
	copy(out, c.order)
	return out
}

// Permissions 是 §7.3 的节点权限矩阵（引擎注入，不依赖 prompt 自律）。
var Permissions = map[model.NodeName][]string{
	model.NodeCode: {
		string(model.ToolRead), string(model.ToolReadCode), string(model.ToolSearch),
		string(model.ToolEdit), string(model.ToolWrite), string(model.ToolBash), string(model.ToolAsk),
	},
	model.NodeAsk: {
		string(model.ToolRead), string(model.ToolReadCode), string(model.ToolSearch),
		string(model.ToolBash), string(model.ToolAsk),
	},
	model.NodeVerifier: {
		string(model.ToolRead), string(model.ToolReadCode), string(model.ToolSearch),
		string(model.ToolBash), string(model.ToolAsk),
	},
	model.NodeDeliberate: {
		string(model.ToolRead), string(model.ToolReadCode), string(model.ToolSearch),
		string(model.ToolBash), string(model.ToolAsk),
	},
	model.NodeAggregation: {
		string(model.ToolRead), string(model.ToolReadCode), string(model.ToolSearch),
		string(model.ToolBash), string(model.ToolAsk),
	},
}

func allowName(allowed []string, name string) bool {
	for _, a := range allowed {
		if a == name {
			return true
		}
	}
	return false
}
