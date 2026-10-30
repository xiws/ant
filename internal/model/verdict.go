package model

// Severity 是问题的严重度。
type Severity string

const (
	SeverityBlocker Severity = "blocker"
	SeverityMajor   Severity = "major"
	SeverityMinor   Severity = "minor"
)

// Failure 是 verifier 报告的一条失败。
type Failure struct {
	Severity   Severity `json:"severity"`
	File       string   `json:"file,omitempty"`
	Evidence   string   `json:"evidence,omitempty"`
	Suggestion string   `json:"suggestion,omitempty"`
}

// Check 是 verifier 实际执行过的一条检查。
type Check struct {
	Cmd       string      `json:"cmd"`
	ExitCode  int         `json:"exitCode"`
	OutputRef ArtifactRef `json:"outputRef,omitempty"`
}

// Verdict 是 verifier 节点的结构化输出（§7.4.1）。
type Verdict struct {
	Pass     bool      `json:"pass"`
	Summary  string    `json:"summary"`
	Failures []Failure `json:"failures,omitempty"`
	Checks   []Check   `json:"checks,omitempty"`
}

// Dimension 是 deliberate 的评审维度。
type Dimension string

const (
	DimCorrectness Dimension = "correctness"
	DimRisk        Dimension = "risk"
	DimExecution   Dimension = "execution"
)

// AllDimensions 返回固定的 join 顺序（§7.5）：correctness → risk → execution。
func AllDimensions() []Dimension {
	return []Dimension{DimCorrectness, DimRisk, DimExecution}
}

// Issue 是 deliberate / aggregation 报告的一条问题。
type Issue struct {
	ID          string   `json:"id,omitempty"`
	Severity    Severity `json:"severity"`
	File        string   `json:"file,omitempty"`
	Description string   `json:"description,omitempty"`
	// Evidence 是 deliberate 契约中的证据字段（§7.4.2）；
	// aggregation 使用 Description。
	Evidence   string      `json:"evidence,omitempty"`
	Suggestion string      `json:"suggestion,omitempty"`
	From       []Dimension `json:"from,omitempty"`
}

// Deliberation 是 deliberate 节点的输出（§7.4.2）。
type Deliberation struct {
	Dimension  Dimension `json:"dimension"`
	Issues     []Issue   `json:"issues,omitempty"`
	Questions  []string  `json:"questions,omitempty"`
	Confidence string    `json:"confidence,omitempty"`
}

// Aggregation 是 aggregation 节点的输出（§7.4.3）。
type Aggregation struct {
	Plan       string                 `json:"plan"`
	Issues     []Issue                `json:"issues,omitempty"`
	Missing    []Dimension            `json:"missing_dimensions,omitempty"`
	Provenance map[string][]Dimension `json:"provenance,omitempty"`
}
