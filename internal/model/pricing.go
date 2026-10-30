package model

// Pricing 来自 models[].pricing，用于成本估算（§11.7）。
type Pricing struct {
	InputPer1M       float64 `json:"inputPer1M"`
	OutputPer1M      float64 `json:"outputPer1M"`
	CachedInputPer1M float64 `json:"cachedInputPer1M,omitempty"`
}
