package model

// Usage 是 token 用量。Estimated 为 true 表示客户端估算，非 provider 实测。
type Usage struct {
	Input     int  `json:"input"`
	Output    int  `json:"output"`
	Cached    int  `json:"cached"`
	Reasoning int  `json:"reasoning"`
	Estimated bool `json:"estimated"`
}

// Add 返回两者之和；任意一侧为估算时结果标记为估算。
func (u Usage) Add(o Usage) Usage {
	return Usage{
		Input:     u.Input + o.Input,
		Output:    u.Output + o.Output,
		Cached:    u.Cached + o.Cached,
		Reasoning: u.Reasoning + o.Reasoning,
		Estimated: u.Estimated || o.Estimated,
	}
}

// IsZero 报告用量是否全部为 0。
func (u Usage) IsZero() bool {
	return u.Input == 0 && u.Output == 0 && u.Cached == 0 && u.Reasoning == 0
}
