package statemachine

import (
	"sync"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

// defaultMaxRounds 是 §06 §8 的轮次缺省值。
const defaultMaxRounds = 3

// turnBudget 是 turn 内轮次、工具轮数、token 与超时的唯一计数来源（§21.12 Budget）。
// 引擎只通过它读取轮次/剩余时间/已花 token，避免第二套计数逻辑。
type turnBudget struct {
	mu sync.Mutex

	maxToolTurns int
	toolTurns    int

	round int

	nodeTimeout time.Duration
	deadline    time.Time

	spentTokens int
}

// newTurnBudget 读取 limits 构造 turn 预算。
func newTurnBudget(cfg config.View, maxToolTurns int) *turnBudget {
	b := &turnBudget{maxToolTurns: maxToolTurns}
	if b.maxToolTurns <= 0 && cfg != nil {
		b.maxToolTurns = cfg.Settings().Limits.MaxToolTurns
	}
	if b.maxToolTurns <= 0 {
		// §7.7 默认值；缺配置时仍要有上限。
		b.maxToolTurns = 12
	}
	if cfg == nil {
		return b
	}
	limits := cfg.Settings().Limits
	if limits.NodeTimeoutMs > 0 {
		b.nodeTimeout = time.Duration(limits.NodeTimeoutMs) * time.Millisecond
	}
	if b.nodeTimeout > 0 {
		b.deadline = time.Now().Add(b.nodeTimeout)
	}
	return b
}

// TakeToolTurn 消耗一次工具轮；预算耗尽返回 false。
func (b *turnBudget) TakeToolTurn() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxToolTurns > 0 && b.toolTurns >= b.maxToolTurns {
		return false
	}
	b.toolTurns++
	return true
}

// timed 报告是否配置了节点/turn 超时。
func (b *turnBudget) timed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.deadline.IsZero()
}

// Round 返回当前轮次。
func (b *turnBudget) Round() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.round
}

// Remaining 返回本节点剩余可用时间；0 表示不设超时。
func (b *turnBudget) Remaining() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deadline.IsZero() {
		return 0
	}
	left := time.Until(b.deadline)
	if b.nodeTimeout > 0 && left > b.nodeTimeout {
		left = b.nodeTimeout
	}
	if left < 0 {
		return 0
	}
	return left
}

// SpentTokens 返回本 turn 已累计的 token。
func (b *turnBudget) SpentTokens() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spentTokens
}

// addUsage 累计 token 用量。
func (b *turnBudget) addUsage(u model.Usage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spentTokens += u.Input + u.Output + u.Cached + u.Reasoning
}

// setRound 由引擎在进入入口节点时调用（轮次按「回到入口节点」计数）。
func (b *turnBudget) setRound(round int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.round = round
}

// reset 在新 turn 开始时重置工具轮数、token 与超时；模式切换也复用它。
func (b *turnBudget) reset(round int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.toolTurns = 0
	b.spentTokens = 0
	b.round = round
	b.deadline = time.Time{}
	if b.nodeTimeout > 0 {
		b.deadline = time.Now().Add(b.nodeTimeout)
	}
}

// chargeToolTurns 按节点回报的工具调用数记账；只做统计，不改变节点结果。
func (b *turnBudget) chargeToolTurns(n int) {
	for i := 0; i < n; i++ {
		if !b.TakeToolTurn() {
			return
		}
	}
}
