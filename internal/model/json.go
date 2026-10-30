package model

import "encoding/json"

// JSON 把任意值编码为 json.RawMessage。
func JSON(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// MustJSON 是 JSON 的 panic-free 版本：编码失败时返回 "null"。
// 只用于事件 data 等内部结构，调用方不得依赖失败时的语义。
func MustJSON(v any) json.RawMessage {
	b, err := JSON(v)
	if err != nil || len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}

// DecodeEventData 把 Event.Data 解码到目标结构。
func DecodeEventData(ev Event, out any) error {
	if len(ev.Data) == 0 {
		return nil
	}
	return json.Unmarshal(ev.Data, out)
}
