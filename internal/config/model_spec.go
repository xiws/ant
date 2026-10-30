package config

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/xiws/ant/internal/model"
)

// UnmarshalJSON 为 provider 的可选字段补默认值（§4.4）。
func (p *ProviderSpec) UnmarshalJSON(data []byte) error {
	type alias ProviderSpec
	aux := struct {
		*alias
		TimeoutMS      *int `json:"timeoutMs"`
		MaxConcurrency *int `json:"maxConcurrency"`
	}{alias: (*alias)(p)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.TimeoutMS != nil {
		p.TimeoutMS = *aux.TimeoutMS
	} else if p.TimeoutMS == 0 {
		p.TimeoutMS = 120000
	}
	if aux.MaxConcurrency != nil {
		p.MaxConcurrency = *aux.MaxConcurrency
	} else if p.MaxConcurrency == 0 {
		p.MaxConcurrency = 1
	}
	if p.Headers == nil {
		p.Headers = map[string]string{}
	}
	return nil
}

// UnmarshalJSON 为模型的 bool/number 可选字段补默认值（§4.4）。
func (m *ModelSpec) UnmarshalJSON(data []byte) error {
	type alias ModelSpec
	aux := struct {
		*alias
		MaxOutputTokens           *int     `json:"maxOutputTokens"`
		SupportsStreaming         *bool    `json:"supportsStreaming"`
		SupportsParallelToolCalls *bool    `json:"supportsParallelToolCalls"`
		DefaultTemperature        *float64 `json:"defaultTemperature"`
	}{alias: (*alias)(m)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.MaxOutputTokens != nil {
		m.MaxOutputTokens = *aux.MaxOutputTokens
	} else if m.MaxOutputTokens == 0 {
		m.MaxOutputTokens = 4096
	}
	if aux.SupportsStreaming != nil {
		m.SupportsStreaming = *aux.SupportsStreaming
	} else {
		m.SupportsStreaming = true
	}
	if aux.SupportsParallelToolCalls != nil {
		m.SupportsParallelToolCalls = *aux.SupportsParallelToolCalls
	} else {
		m.SupportsParallelToolCalls = m.SupportsTools
	}
	if aux.DefaultTemperature != nil {
		m.DefaultTemperature = *aux.DefaultTemperature
	} else if m.DefaultTemperature == 0 {
		m.DefaultTemperature = 0.2
	}
	return nil
}

func decodeModels(raw map[string]any) ([]ProviderSpec, []ModelSpec, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, &model.Error{Class: model.ErrConfigInvalid, Code: "models_json", Message: err.Error()}
	}
	var mf struct {
		SchemaVersion int                     `json:"schemaVersion"`
		Providers     map[string]ProviderSpec `json:"providers"`
	}
	if err := json.Unmarshal(b, &mf); err != nil {
		return nil, nil, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "models_type",
			Message: fmt.Sprintf("models.json 类型错误：%v", err),
			Hint:    "检查 models.providers[].api/baseUrl/models 的字段类型",
		}
	}
	keys := make([]string, 0, len(mf.Providers))
	for k := range mf.Providers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	providers := make([]ProviderSpec, 0, len(keys))
	var models []ModelSpec
	for _, key := range keys {
		p := mf.Providers[key]
		p.Key = key
		if p.Name == "" {
			p.Name = key
		}
		for i := range p.Models {
			p.Models[i].ProviderKey = key
		}
		providers = append(providers, p)
		models = append(models, p.Models...)
	}
	return providers, models, nil
}
