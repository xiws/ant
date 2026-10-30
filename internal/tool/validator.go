package tool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/xiws/ant/internal/model"
)

// defaultValidator 做 JSON Schema（子集）校验与类型转换（§10.4）。
//
// 支持：对象根、properties、required、additionalProperties(bool/schema)、
// items、type（object/array/string/integer/number/boolean/null）、enum、
// minimum/maximum、minLength/maxLength、minItems/maxItems、pattern。
type defaultValidator struct{}

// NewValidator 返回生产校验器。
func NewValidator() Validator { return defaultValidator{} }

// schemaNode 是解析后的 JSON Schema 子集。
type schemaNode struct {
	Types             []string
	Properties        map[string]*schemaNode
	Required          []string
	AdditionalAllowed bool
	AdditionalSchema  *schemaNode
	Items             *schemaNode
	Enum              []any
	Minimum           *float64
	Maximum           *float64
	MinLength         *int
	MaxLength         *int
	MinItems          *int
	MaxItems          *int
	Pattern           *regexp.Regexp
}

// Validate 实现 Validator。
func (defaultValidator) Validate(spec model.ToolSpec, raw json.RawMessage) (map[string]any, *ToolError) {
	schema, err := parseSchema(spec.Schema)
	if err != nil {
		return nil, &ToolError{
			Code:    "internal",
			Message: fmt.Sprintf("工具 %s 的 schema 非法：%v", spec.Name, err),
		}
	}
	body := bytes.TrimSpace(raw)
	if len(body) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root any
	if derr := dec.Decode(&root); derr != nil {
		return nil, &ToolError{
			Code:    "invalid_args",
			Message: fmt.Sprintf("参数不是合法 JSON：%v", derr),
			Hint:    "按工具 JSON Schema 输出一个 JSON 对象，不要包含注释或解释文本",
			Details: map[string]any{"raw": snippet(body)},
		}
	}
	var extra any
	if derr := dec.Decode(&extra); derr == nil {
		return nil, &ToolError{
			Code:    "invalid_args",
			Message: "参数 JSON 之后还有多余内容",
			Hint:    "只输出一个 JSON 对象",
			Details: map[string]any{"raw": snippet(body)},
		}
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return nil, &ToolError{
			Code:    "invalid_args",
			Message: fmt.Sprintf("参数根必须是 JSON 对象，收到 %s", jsonTypeName(root)),
			Hint:    "用 {...} 包裹参数",
			Details: map[string]any{"actual": jsonTypeName(root)},
		}
	}
	out, terr := validateObject(obj, schema, "")
	if terr != nil {
		return nil, terr
	}
	return out, nil
}

// parseSchema 解析工具 schema；根必须是 object。
func parseSchema(raw json.RawMessage) (*schemaNode, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return &schemaNode{AdditionalAllowed: true}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	m, ok := root.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema 根必须是对象")
	}
	n, err := parseSchemaNode(m)
	if err != nil {
		return nil, err
	}
	if len(n.Types) > 0 && !containsString(n.Types, "object") {
		return nil, fmt.Errorf("schema 根类型必须是 object，收到 %q", strings.Join(n.Types, "|"))
	}
	return n, nil
}

func parseSchemaNode(m map[string]any) (*schemaNode, error) {
	n := &schemaNode{AdditionalAllowed: true}
	switch t := m["type"].(type) {
	case string:
		if t != "" {
			n.Types = []string{t}
		}
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && s != "" {
				n.Types = append(n.Types, s)
			}
		}
	}
	if props, ok := m["properties"].(map[string]any); ok {
		n.Properties = make(map[string]*schemaNode, len(props))
		for name, v := range props {
			pm, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("properties.%s 必须是对象", name)
			}
			child, err := parseSchemaNode(pm)
			if err != nil {
				return nil, err
			}
			n.Properties[name] = child
		}
	}
	if req, ok := m["required"].([]any); ok {
		for _, item := range req {
			if s, ok := item.(string); ok {
				n.Required = append(n.Required, s)
			}
		}
	}
	if ap, ok := m["additionalProperties"]; ok {
		switch v := ap.(type) {
		case bool:
			n.AdditionalAllowed = v
		case map[string]any:
			child, err := parseSchemaNode(v)
			if err != nil {
				return nil, err
			}
			n.AdditionalSchema = child
			n.AdditionalAllowed = true
		}
	}
	if items, ok := m["items"].(map[string]any); ok {
		child, err := parseSchemaNode(items)
		if err != nil {
			return nil, err
		}
		n.Items = child
	}
	if enum, ok := m["enum"].([]any); ok {
		n.Enum = enum
	}
	if f, ok := numberOf(m["minimum"]); ok {
		n.Minimum = &f
	}
	if f, ok := numberOf(m["maximum"]); ok {
		n.Maximum = &f
	}
	if i, ok := intOf(m["minLength"]); ok {
		n.MinLength = &i
	}
	if i, ok := intOf(m["maxLength"]); ok {
		n.MaxLength = &i
	}
	if i, ok := intOf(m["minItems"]); ok {
		n.MinItems = &i
	}
	if i, ok := intOf(m["maxItems"]); ok {
		n.MaxItems = &i
	}
	if pat, ok := m["pattern"].(string); ok && pat != "" {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("pattern %q 非法：%w", pat, err)
		}
		n.Pattern = re
	}
	return n, nil
}

// validateObject 校验对象：required、未知字段、逐项递归转换。
func validateObject(obj map[string]any, s *schemaNode, path string) (map[string]any, *ToolError) {
	for _, name := range s.Required {
		val, ok := obj[name]
		if !ok || val == nil {
			field := fieldPath(path, name)
			return nil, &ToolError{
				Code:    "invalid_args",
				Message: fmt.Sprintf("缺少必填参数 %s", field),
				Hint:    "必填参数：" + strings.Join(s.Required, ", "),
				Details: map[string]any{"field": field, "required": s.Required},
			}
		}
	}
	out := make(map[string]any, len(obj))
	for name, val := range obj {
		field := fieldPath(path, name)
		prop, known := s.Properties[name]
		if !known {
			if s.AdditionalSchema != nil {
				conv, terr := convertValue(val, s.AdditionalSchema, field)
				if terr != nil {
					return nil, terr
				}
				out[name] = conv
				continue
			}
			if !s.AdditionalAllowed {
				return nil, &ToolError{
					Code:    "schema_mismatch",
					Message: fmt.Sprintf("未知参数 %s", field),
					Hint:    "允许的参数：" + strings.Join(sortedKeys(s.Properties), ", "),
					Details: map[string]any{"field": field, "allowed": sortedKeys(s.Properties)},
				}
			}
			out[name] = val
			continue
		}
		conv, terr := convertValue(val, prop, field)
		if terr != nil {
			return nil, terr
		}
		out[name] = conv
	}
	return out, nil
}

// convertValue 按 schema 转换一个值。
func convertValue(val any, s *schemaNode, path string) (any, *ToolError) {
	if s == nil {
		return val, nil
	}
	types := s.Types
	if len(types) == 0 {
		if len(s.Properties) > 0 || len(s.Required) > 0 || s.AdditionalSchema != nil {
			types = []string{"object"}
		} else if s.Items != nil {
			types = []string{"array"}
		}
	}
	if len(types) == 0 {
		return val, checkEnum(val, s.Enum, path)
	}
	var firstErr *ToolError
	for _, typ := range types {
		conv, terr := convertTyped(val, typ, s, path)
		if terr != nil {
			if firstErr == nil {
				firstErr = terr
			}
			continue
		}
		if eerr := checkEnum(conv, s.Enum, path); eerr != nil {
			return nil, eerr
		}
		return conv, nil
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, typeError(path, strings.Join(types, "|"), val)
}

func convertTyped(val any, typ string, s *schemaNode, path string) (any, *ToolError) {
	switch typ {
	case "object":
		m, ok := val.(map[string]any)
		if !ok {
			return nil, typeError(path, "object", val)
		}
		return validateObject(m, s, path)
	case "array":
		arr, ok := val.([]any)
		if !ok {
			return nil, typeError(path, "array", val)
		}
		if s.MinItems != nil && len(arr) < *s.MinItems {
			return nil, schemaError(path, fmt.Sprintf("数组至少 %d 项，收到 %d 项", *s.MinItems, len(arr)))
		}
		if s.MaxItems != nil && len(arr) > *s.MaxItems {
			return nil, schemaError(path, fmt.Sprintf("数组最多 %d 项，收到 %d 项", *s.MaxItems, len(arr)))
		}
		out := make([]any, 0, len(arr))
		for i, item := range arr {
			conv, terr := convertValue(item, s.Items, fmt.Sprintf("%s[%d]", path, i))
			if terr != nil {
				return nil, terr
			}
			out = append(out, conv)
		}
		return out, nil
	case "string":
		str, terr := toString(val, path)
		if terr != nil {
			return nil, terr
		}
		if s.MinLength != nil || s.MaxLength != nil {
			n := utf8.RuneCountInString(str)
			if s.MinLength != nil && n < *s.MinLength {
				return nil, schemaError(path, fmt.Sprintf("字符串长度至少 %d，收到 %d", *s.MinLength, n))
			}
			if s.MaxLength != nil && n > *s.MaxLength {
				return nil, schemaError(path, fmt.Sprintf("字符串长度最多 %d，收到 %d", *s.MaxLength, n))
			}
		}
		if s.Pattern != nil && !s.Pattern.MatchString(str) {
			return nil, schemaError(path, "字符串不匹配 pattern "+s.Pattern.String())
		}
		return str, nil
	case "integer":
		i, terr := toInt(val, path)
		if terr != nil {
			return nil, terr
		}
		if s.Minimum != nil && float64(i) < *s.Minimum {
			return nil, schemaError(path, fmt.Sprintf("值 %d 小于最小值 %v", i, *s.Minimum))
		}
		if s.Maximum != nil && float64(i) > *s.Maximum {
			return nil, schemaError(path, fmt.Sprintf("值 %d 大于最大值 %v", i, *s.Maximum))
		}
		return i, nil
	case "number":
		f, terr := toFloat(val, path)
		if terr != nil {
			return nil, terr
		}
		if s.Minimum != nil && f < *s.Minimum {
			return nil, schemaError(path, fmt.Sprintf("值 %v 小于最小值 %v", f, *s.Minimum))
		}
		if s.Maximum != nil && f > *s.Maximum {
			return nil, schemaError(path, fmt.Sprintf("值 %v 大于最大值 %v", f, *s.Maximum))
		}
		return f, nil
	case "boolean":
		return toBool(val, path)
	case "null":
		if val == nil {
			return nil, nil
		}
		return nil, typeError(path, "null", val)
	case "":
		return val, nil
	default:
		return nil, &ToolError{
			Code:    "internal",
			Message: fmt.Sprintf("字段 %s：schema 类型 %q 不受支持", displayField(path), typ),
		}
	}
}

func toString(val any, path string) (string, *ToolError) {
	switch v := val.(type) {
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	}
	return "", typeError(path, "string", val)
}

func toInt(val any, path string) (int, *ToolError) {
	switch v := val.(type) {
	case json.Number:
		s := v.String()
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return intFromInt64(i, path)
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || !isIntegral(f) {
			return 0, typeError(path, "integer", val)
		}
		return intFromFloat(f, path)
	case string:
		s := strings.TrimSpace(v)
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return intFromInt64(i, path)
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || !isIntegral(f) {
			return 0, typeError(path, "integer", val)
		}
		return intFromFloat(f, path)
	case int:
		return v, nil
	case int64:
		return intFromInt64(v, path)
	case float64:
		if !isIntegral(v) {
			return 0, typeError(path, "integer", val)
		}
		return intFromFloat(v, path)
	}
	return 0, typeError(path, "integer", val)
}

func toFloat(val any, path string) (float64, *ToolError) {
	var f float64
	switch v := val.(type) {
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return 0, typeError(path, "number", val)
		}
		f = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, typeError(path, "number", val)
		}
		f = parsed
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	case float64:
		f = v
	default:
		return 0, typeError(path, "number", val)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, schemaError(path, "数值不能是 NaN/Inf")
	}
	return f, nil
}

func toBool(val any, path string) (any, *ToolError) {
	switch v := val.(type) {
	case bool:
		return v, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return nil, typeError(path, "boolean", val)
}

func isIntegral(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f)
}

func intFromFloat(f float64, path string) (int, *ToolError) {
	// float64 无法精确表示 2^63，用半开区间判断 int64 范围。
	if f < -9223372036854775808.0 || f >= 9223372036854775808.0 {
		return 0, schemaError(path, "整数超出 int64 范围")
	}
	i := int64(f)
	return intFromInt64(i, path)
}

func intFromInt64(i int64, path string) (int, *ToolError) {
	if int64(int(i)) != i {
		return 0, schemaError(path, fmt.Sprintf("整数 %d 超出平台 int 范围", i))
	}
	return int(i), nil
}

func checkEnum(val any, enum []any, path string) *ToolError {
	if len(enum) == 0 {
		return nil
	}
	for _, want := range enum {
		if enumEqual(val, want) {
			return nil
		}
	}
	return &ToolError{
		Code:    "schema_mismatch",
		Message: fmt.Sprintf("字段 %s 的取值 %v 不在允许范围内", displayField(path), val),
		Hint:    "允许的值：" + enumPreview(enum),
		Details: map[string]any{"field": displayField(path), "allowed": enum, "got": val},
	}
}

func enumEqual(val, want any) bool {
	switch v := val.(type) {
	case string:
		w, ok := want.(string)
		return ok && v == w
	case bool:
		w, ok := want.(bool)
		return ok && v == w
	case int:
		return numberEquals(want, float64(v))
	case float64:
		return numberEquals(want, v)
	case nil:
		return want == nil
	}
	return reflect.DeepEqual(val, want)
}

func numberEquals(want any, f float64) bool {
	switch w := want.(type) {
	case json.Number:
		parsed, err := w.Float64()
		return err == nil && parsed == f
	case float64:
		return w == f
	case int:
		return float64(w) == f
	case int64:
		return float64(w) == f
	}
	return false
}

func typeError(path, want string, got any) *ToolError {
	actual := jsonTypeName(got)
	return &ToolError{
		Code:    "schema_mismatch",
		Message: fmt.Sprintf("字段 %s 期望 %s，收到 %s", displayField(path), want, actual),
		Hint:    "按 schema 修正类型；数字/布尔字符串与数字字符串会被自动转换",
		Details: map[string]any{"field": displayField(path), "expected": want, "actual": actual},
	}
}

func schemaError(path, message string) *ToolError {
	return &ToolError{
		Code:    "schema_mismatch",
		Message: fmt.Sprintf("字段 %s：%s", displayField(path), message),
		Hint:    "检查字段的取值、范围、长度与格式是否符合 schema",
		Details: map[string]any{"field": displayField(path)},
	}
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number, int, int64, float64:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	}
	return fmt.Sprintf("%T", v)
}

func fieldPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func displayField(path string) string {
	if path == "" {
		return "参数"
	}
	return path
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]*schemaNode) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func enumPreview(enum []any) string {
	parts := make([]string, 0, len(enum))
	for _, e := range enum {
		parts = append(parts, fmt.Sprint(e))
	}
	return strings.Join(parts, ", ")
}

func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func intOf(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case float64:
		if !isIntegral(n) {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}
