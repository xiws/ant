package model

// NodeName 是内置节点名。
type NodeName string

const (
	NodeCode        NodeName = "code"
	NodeAsk         NodeName = "ask"
	NodeVerifier    NodeName = "verifier"
	NodeDeliberate  NodeName = "deliberate"
	NodeAggregation NodeName = "aggregation"
)

// AllNodes 返回内置节点顺序（仅用于遍历，不代表图顺序）。
func AllNodes() []NodeName {
	return []NodeName{NodeCode, NodeAsk, NodeVerifier, NodeDeliberate, NodeAggregation}
}

// Mode 是状态机模式。
type Mode string

const (
	ModeCode    Mode = "code"
	ModePlan    Mode = "plan"
	ModeAsk     Mode = "ask"
	ModeCouncil Mode = "council"
)

// Valid 报告模式是否合法。
func (m Mode) Valid() bool {
	switch m {
	case ModeCode, ModePlan, ModeAsk, ModeCouncil:
		return true
	}
	return false
}

// ApprovalClass 是审批类别（§12.3）；保护路径与硬拒绝不产生可放行类别。
type ApprovalClass string

const (
	ApprovalFSReadOutside  ApprovalClass = "fs_read_outside"
	ApprovalFSWriteOutside ApprovalClass = "fs_write_outside"
	ApprovalBashRisk       ApprovalClass = "bash_risk"
	ApprovalBashNetwork    ApprovalClass = "bash_network"
	ApprovalBashOutside    ApprovalClass = "bash_outside"
	ApprovalPlanToCode     ApprovalClass = "plan_to_code"
)

// AllApprovalClasses 返回全部可放行类别。
func AllApprovalClasses() []ApprovalClass {
	return []ApprovalClass{
		ApprovalFSReadOutside, ApprovalFSWriteOutside,
		ApprovalBashRisk, ApprovalBashNetwork, ApprovalBashOutside,
		ApprovalPlanToCode,
	}
}

// StepRef 是事件与 checkpoint 的定位符（§9.3 的 step 字段）。
type StepRef struct {
	Mode  Mode     `json:"mode"`
	Round int      `json:"round,omitempty"`
	Phase string   `json:"phase,omitempty"`
	Node  NodeName `json:"node,omitempty"`
}

// String 返回稳定的 step 标识，如 plan.round-1.verifier。
func (s StepRef) String() string {
	out := string(s.Mode)
	if s.Round > 0 {
		out += ".round-" + itoa(s.Round)
	}
	if s.Node != "" {
		out += "." + string(s.Node)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
