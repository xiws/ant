package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiws/ant/internal/model"
)

// textContract 用于 code/ask：保留自由文本，code 要求固定小节（§7.4.4）。
type textContract struct {
	node     model.NodeName
	required []string
}

func (c textContract) Name() string { return "text:" + string(c.node) }

func (c textContract) Parse(ctx context.Context, text string) (Parsed, error) {
	if err := ctx.Err(); err != nil {
		return Parsed{}, err
	}
	if len(c.required) > 0 {
		var missing []string
		for _, section := range c.required {
			if !strings.Contains(text, section) {
				missing = append(missing, section)
			}
		}
		if len(missing) > 0 {
			return Parsed{}, &model.Error{
				Class:   model.ErrVerificationFailed,
				Code:    "missing_section",
				Message: fmt.Sprintf("%s 输出缺少固定小节：%s", c.node, strings.Join(missing, "、")),
				Hint:    "按节点 prompt 的格式要求补齐小节",
			}
		}
	}
	return Parsed{Raw: json.RawMessage(model.MustJSON(map[string]string{"text": text}))}, nil
}

// verifierContract 解析 §7.4.1 的 JSON verdict。
type verifierContract struct{}

func (verifierContract) Name() string { return "verifier.v1" }

func (verifierContract) Parse(ctx context.Context, text string) (Parsed, error) {
	raw, err := extractJSONObject(text)
	if err != nil {
		return Parsed{}, contractErr("verifier", err)
	}
	var v model.Verdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return Parsed{}, contractErr("verifier", err)
	}
	for _, f := range v.Failures {
		if !validSeverity(f.Severity) {
			return Parsed{}, contractErr("verifier", fmt.Errorf("severity %q 非法", f.Severity))
		}
		if strings.TrimSpace(f.Evidence) == "" {
			return Parsed{}, contractErr("verifier", fmt.Errorf("failure 缺少 evidence"))
		}
	}
	if v.Pass && len(v.Failures) > 0 {
		return Parsed{}, contractErr("verifier", fmt.Errorf("pass=true 时 failures 必须为空"))
	}
	if !v.Pass && len(v.Failures) == 0 {
		return Parsed{}, contractErr("verifier", fmt.Errorf("pass=false 时至少一条 failure"))
	}
	return Parsed{Raw: raw, Verdict: &v}, nil
}

// deliberateContract 解析 §7.4.2 的 JSON。
type deliberateContract struct{}

func (deliberateContract) Name() string { return "deliberate.v1" }

func (deliberateContract) Parse(ctx context.Context, text string) (Parsed, error) {
	raw, err := extractJSONObject(text)
	if err != nil {
		return Parsed{}, contractErr("deliberate", err)
	}
	var d model.Deliberation
	if err := json.Unmarshal(raw, &d); err != nil {
		return Parsed{}, contractErr("deliberate", err)
	}
	if !validDimension(d.Dimension) {
		return Parsed{}, contractErr("deliberate", fmt.Errorf("dimension %q 非法", d.Dimension))
	}
	for i := range d.Issues {
		if !validSeverity(d.Issues[i].Severity) {
			return Parsed{}, contractErr("deliberate", fmt.Errorf("issue[%d] severity %q 非法", i, d.Issues[i].Severity))
		}
		if d.Issues[i].Description == "" {
			d.Issues[i].Description = d.Issues[i].Evidence
		}
	}
	return Parsed{Raw: raw, Issues: d.Issues}, nil
}

// aggregationContract 解析 §7.4.3 的 JSON。
type aggregationContract struct{}

func (aggregationContract) Name() string { return "aggregation.v1" }

func (aggregationContract) Parse(ctx context.Context, text string) (Parsed, error) {
	raw, err := extractJSONObject(text)
	if err != nil {
		return Parsed{}, contractErr("aggregation", err)
	}
	var a model.Aggregation
	if err := json.Unmarshal(raw, &a); err != nil {
		return Parsed{}, contractErr("aggregation", err)
	}
	if strings.TrimSpace(a.Plan) == "" {
		return Parsed{}, contractErr("aggregation", fmt.Errorf("plan 不能为空"))
	}
	for i := range a.Issues {
		if !validSeverity(a.Issues[i].Severity) {
			return Parsed{}, contractErr("aggregation", fmt.Errorf("issue[%d] severity %q 非法", i, a.Issues[i].Severity))
		}
	}
	return Parsed{Raw: raw, Issues: a.Issues, Plan: &a}, nil
}

func contractErr(node string, err error) error {
	return &model.Error{
		Class:   model.ErrVerificationFailed,
		Code:    "contract_parse",
		Message: fmt.Sprintf("%s 输出契约解析失败：%v", node, err),
		Hint:    "输出必须是约定 JSON；verifier 失败时会按 pass=false 处理",
		Cause:   err,
	}
}

// extractJSONObject 做有限修复：去 Markdown 围栏、提取首个平衡对象（§13.4）。
func extractJSONObject(text string) (json.RawMessage, error) {
	s := strings.TrimSpace(text)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```JSON")
	s = strings.TrimPrefix(s, "```")
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return nil, fmt.Errorf("未找到 JSON 对象")
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
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
				return json.RawMessage(s[start : i+1]), nil
			}
		}
	}
	return nil, fmt.Errorf("JSON 对象未闭合")
}

func validSeverity(s model.Severity) bool {
	switch s {
	case model.SeverityBlocker, model.SeverityMajor, model.SeverityMinor:
		return true
	}
	return false
}

func validDimension(d model.Dimension) bool {
	switch d {
	case model.DimCorrectness, model.DimRisk, model.DimExecution:
		return true
	}
	return false
}
