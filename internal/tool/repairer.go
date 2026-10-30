package tool

import (
	"bytes"
	"encoding/json"
)

// defaultRepairer 做有限 JSON 修复（docs/11-fault-tolerance.md §13.4）：
//  1. 去掉 Markdown 代码围栏；
//  2. 提取首个平衡的 {...}；
//  3. 删除尾随逗号（字符串内部不处理）。
//
// 不做语义猜测，也不补全缺失的括号/引号。
type defaultRepairer struct{}

// NewRepairer 返回生产修复器。
func NewRepairer() Repairer { return defaultRepairer{} }

// Repair 返回修复后的字节与「是否发生了有效修复」。
func (defaultRepairer) Repair(raw []byte) ([]byte, bool) {
	original := raw
	out := bytes.TrimSpace(raw)
	if len(out) == 0 {
		return raw, false
	}
	if fenced, ok := stripFences(out); ok {
		out = fenced
	}
	if obj, ok := firstBalancedObject(out); ok {
		out = obj
	}
	out = removeTrailingCommas(out)
	out = bytes.TrimSpace(out)
	if bytes.Equal(out, original) {
		return original, false
	}
	return out, json.Valid(out)
}

// stripFences 去掉 ```json ... ``` 围栏；找不到围栏时原样返回。
func stripFences(b []byte) ([]byte, bool) {
	i := bytes.Index(b, []byte("```"))
	if i < 0 {
		return b, false
	}
	rest := b[i+3:]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		return b, false
	}
	rest = rest[nl+1:]
	if j := bytes.Index(rest, []byte("```")); j >= 0 {
		rest = rest[:j]
	}
	return bytes.TrimSpace(rest), true
}

// firstBalancedObject 返回第一个平衡的 {...}；字符串内的括号不计数。
func firstBalancedObject(b []byte) ([]byte, bool) {
	start := bytes.IndexByte(b, '{')
	if start < 0 {
		return b, false
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(b); i++ {
		c := b[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return bytes.TrimSpace(b[start : i+1]), true
			}
		}
	}
	return b, false
}

// removeTrailingCommas 删除对象/数组闭合前的逗号，字符串内部保持原样。
func removeTrailingCommas(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inString := false
	escaped := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			out = append(out, c)
		case ',':
			j := i + 1
			for j < len(b) && isJSONSpace(b[j]) {
				j++
			}
			if j < len(b) && (b[j] == '}' || b[j] == ']') {
				continue
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
