package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

// openAIChatPath 是 OpenAI 兼容协议的唯一端点（§5.4）。
const openAIChatPath = "/chat/completions"

// maxErrorBody 限制错误响应读取量，避免恶意/异常响应挤爆内存。
const maxErrorBody = 64 << 10

// openAIAdapter 实现 Provider：只做协议映射，不重试（§5.3）。
type openAIAdapter struct {
	spec   config.ProviderSpec
	client *http.Client
	url    string
}

// NewOpenAICompletionsAdapter 构造 OpenAI 兼容协议适配器。
func NewOpenAICompletionsAdapter(spec config.ProviderSpec) (Provider, error) {
	base := strings.TrimSpace(spec.BaseURL)
	if base == "" {
		return nil, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "provider_base_url",
			Message: fmt.Sprintf("provider %s 缺少 baseUrl", spec.DisplayName()),
			Hint:    "在 models.json 中为 openai-completions provider 配置 baseUrl",
		}
	}
	url := strings.TrimRight(base, "/")
	if !strings.HasSuffix(url, "/chat/completions") {
		url += openAIChatPath
	}
	timeout := spec.Timeout()
	return &openAIAdapter{
		spec:   spec,
		client: &http.Client{Timeout: timeout},
		url:    url,
	}, nil
}

// Name 实现 Provider。
func (p *openAIAdapter) Name() string { return p.spec.Key }

// Capabilities 实现 Provider：能力来自配置，不在协议里探测（§5.2）。
func (p *openAIAdapter) Capabilities(alias string) (model.Capabilities, error) {
	for _, m := range p.spec.Models {
		if m.Name == alias || m.ID == alias {
			return m.Capabilities(), nil
		}
	}
	if len(p.spec.Models) == 1 {
		return p.spec.Models[0].Capabilities(), nil
	}
	return model.Capabilities{}, &model.Error{
		Class:   model.ErrConfigInvalid,
		Code:    "unknown_model",
		Message: fmt.Sprintf("provider %s 下没有模型 %q", p.spec.DisplayName(), alias),
	}
}

// Chat 实现 Provider。
func (p *openAIAdapter) Chat(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return ChatResponse{}, err
	}
	body, err := p.buildBody(req)
	if err != nil {
		return ChatResponse{}, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, &model.Error{
			Class:   model.ErrInternal,
			Code:    "request_marshal",
			Message: "provider 请求无法序列化：" + err.Error(),
		}
	}

	httpCtx := ctx
	// spec.Timeout 是单次 HTTP 请求超时；父上下文取消优先。
	httpCtx, cancel := context.WithTimeout(ctx, p.spec.Timeout())
	defer cancel()

	httpReq, err := http.NewRequestWithContext(httpCtx, http.MethodPost, p.url, bytes.NewReader(raw))
	if err != nil {
		return ChatResponse{}, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "provider_request",
			Message: "无法构造 provider 请求：" + err.Error(),
			Cause:   err,
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	for k, v := range p.spec.Headers {
		if k == "" {
			continue
		}
		httpReq.Header.Set(k, v)
	}
	if p.spec.APIKey.IsSet() {
		httpReq.Header.Set("Authorization", "Bearer "+p.spec.APIKey.Reveal())
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, classifyTransportError(ctx, err, p.spec.DisplayName())
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return ChatResponse{}, classifyHTTPError(resp.StatusCode, body, resp.Header, p.spec.DisplayName())
	}
	if req.Stream {
		return p.readStream(resp.Body, onDelta)
	}
	return p.readResponse(resp.Body)
}

func (p *openAIAdapter) buildBody(req ChatRequest) (map[string]any, error) {
	modelID := req.Model
	if modelID == "" {
		return nil, &model.Error{
			Class:   model.ErrProviderInvalidReq,
			Code:    "model_required",
			Message: "provider 请求缺少真实模型 id",
		}
	}
	messages := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		messages = append(messages, encodeMessage(m))
	}
	out := map[string]any{
		"model":    modelID,
		"messages": messages,
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  json.RawMessage(t.Schema),
				},
			})
		}
		out["tools"] = tools
	}
	if req.ToolChoice != "" {
		out["tool_choice"] = req.ToolChoice
	}
	reasoning := false
	var defaultTemperature float64
	for _, m := range p.spec.Models {
		if m.Name == req.Model || m.ID == req.Model {
			reasoning = m.Reasoning
			defaultTemperature = m.DefaultTemperature
			break
		}
	}
	if !reasoning {
		if req.Temperature != nil {
			out["temperature"] = *req.Temperature
		} else if defaultTemperature > 0 {
			out["temperature"] = defaultTemperature
		}
	}
	if req.MaxTokens > 0 {
		if reasoning {
			out["max_completion_tokens"] = req.MaxTokens
		} else {
			out["max_tokens"] = req.MaxTokens
		}
	}
	if reasoning {
		out["parallel_tool_calls"] = false
	} else if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if req.Stream {
		out["stream"] = true
		// 仅使用 OpenAI 公共字段；include_usage 已是兼容实现的普遍行为。
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return out, nil
}

func encodeMessage(m model.Message) map[string]any {
	out := map[string]any{"role": string(m.Role)}
	if m.Role == model.RoleAssistant && m.Content == "" && len(m.ToolCalls) > 0 {
		out["content"] = nil
	} else {
		out["content"] = m.Content
	}
	if m.Name != "" {
		out["name"] = m.Name
	}
	if m.ToolCallID != "" {
		out["tool_call_id"] = m.ToolCallID
	}
	if len(m.ToolCalls) > 0 {
		calls := make([]any, 0, len(m.ToolCalls))
		for _, call := range m.ToolCalls {
			calls = append(calls, map[string]any{
				"id":   call.ID,
				"type": "function",
				"function": map[string]any{
					"name":      call.Name,
					"arguments": argumentsString(call.Arguments),
				},
			})
		}
		out["tool_calls"] = calls
	}
	return out
}

// argumentsString 把保留的原始参数转成协议要求的 JSON 字符串。
// 非法 JSON 仍然保留原文（外层是 JSON 字符串，不会破坏请求体），
// 执行阶段会再次交给修复器（§5.7.5、§13.4）。
func argumentsString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "{}"
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s
		}
	}
	return string(trimmed)
}

type openAIUsage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails   *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	TotalTokens int `json:"total_tokens"`
}

func (u *openAIUsage) toUsage() model.Usage {
	if u == nil {
		return model.Usage{Estimated: true}
	}
	usage := model.Usage{Input: u.PromptTokens, Output: u.CompletionTokens}
	if u.PromptCacheHitTokens > 0 {
		usage.Cached = u.PromptCacheHitTokens
	} else if u.PromptTokensDetails != nil {
		usage.Cached = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		usage.Reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return usage
}

type openAIToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type openAIMessage struct {
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	Reasoning        string           `json:"reasoning"`
	ToolCalls        []openAIToolCall `json:"tool_calls"`
}

type openAIChoice struct {
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	Delta        openAIMessage `json:"delta"`
	FinishReason *string       `json:"finish_reason"`
}

type openAIErrorBody struct {
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

type openAIResponseBody struct {
	Choices []openAIChoice   `json:"choices"`
	Usage   *openAIUsage     `json:"usage"`
	Error   *openAIErrorBody `json:"error"`
}

func (p *openAIAdapter) readResponse(body io.Reader) (ChatResponse, error) {
	raw, err := io.ReadAll(io.LimitReader(body, 64<<20))
	if err != nil {
		return ChatResponse{}, &model.Error{
			Class: model.ErrProviderUnavailable, Code: "response_read",
			Message: "读取 provider 响应失败：" + err.Error(), Retryable: true, Cause: err,
		}
	}
	var parsed openAIResponseBody
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ChatResponse{}, &model.Error{
			Class: model.ErrProviderInvalidReq, Code: "response_invalid_json",
			Message: "provider 响应不是合法 JSON：" + err.Error(),
			Details: map[string]any{"body": truncateString(string(raw), 500)},
			Cause:   err,
		}
	}
	if parsed.Error != nil && parsed.Error.Error != nil {
		return ChatResponse{}, errorFromBody(parsed.Error.Error.Message, parsed.Error.Error.Type, 0, nil)
	}
	if len(parsed.Choices) == 0 {
		return ChatResponse{}, &model.Error{
			Class: model.ErrProviderInvalidReq, Code: "response_no_choices",
			Message: "provider 响应没有任何 choice",
			Details: map[string]any{"body": truncateString(string(raw), 500)},
		}
	}
	choice := parsed.Choices[0]
	content, err := decodeContent(choice.Message.Content)
	if err != nil {
		return ChatResponse{}, err
	}
	toolCalls, err := normalizeToolCalls(choice.Message.ToolCalls)
	if err != nil {
		return ChatResponse{}, err
	}
	finish := "stop"
	if choice.FinishReason != nil && *choice.FinishReason != "" {
		finish = *choice.FinishReason
	} else if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	reasoning := choice.Message.ReasoningContent
	if reasoning == "" {
		reasoning = choice.Message.Reasoning
	}
	return ChatResponse{
		Message: model.Message{
			Role:      model.RoleAssistant,
			Content:   content,
			ToolCalls: toolCalls,
		},
		Usage:        parsed.Usage.toUsage(),
		FinishReason: finish,
	}, nil
}

func (p *openAIAdapter) readStream(body io.Reader, onDelta func(Delta) error) (ChatResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)

	var (
		content   strings.Builder
		reasoning strings.Builder
		calls     = map[int]*streamCall{}
		finish    string
		usage     *openAIUsage
		sawDone   bool
	)

	emit := func(d Delta) error {
		if onDelta == nil {
			return nil
		}
		if d.Text == "" && d.ReasoningText == "" && len(d.ToolCalls) == 0 {
			return nil
		}
		return onDelta(d)
	}

	handleData := func(data string) error {
		data = strings.TrimSpace(data)
		if data == "" {
			return nil
		}
		if data == "[DONE]" {
			sawDone = true
			return nil
		}
		var chunk openAIResponseBody
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return &model.Error{
				Class: model.ErrProviderUnavailable, Code: "stream_invalid_chunk",
				Message: "流式响应包含非法 JSON：" + err.Error(), Retryable: true,
				Details: map[string]any{"chunk": truncateString(data, 300)}, Cause: err,
			}
		}
		if chunk.Error != nil && chunk.Error.Error != nil {
			return errorFromBody(chunk.Error.Error.Message, chunk.Error.Error.Type, 0, nil)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = *choice.FinishReason
			}
			delta := choice.Delta
			text, err := decodeContent(delta.Content)
			if err != nil {
				return err
			}
			reason := delta.ReasoningContent
			if reason == "" {
				reason = delta.Reasoning
			}
			if text != "" {
				content.WriteString(text)
			}
			if reason != "" {
				reasoning.WriteString(reason)
			}
			var toolDeltas []model.ToolCallDelta
			for _, tc := range delta.ToolCalls {
				acc := calls[tc.Index]
				if acc == nil {
					acc = &streamCall{index: tc.Index}
					calls[tc.Index] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					acc.args.WriteString(tc.Function.Arguments)
				}
				toolDeltas = append(toolDeltas, model.ToolCallDelta{
					Index:     tc.Index,
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
			if err := emit(Delta{Text: text, ReasoningText: reason, ToolCalls: toolDeltas}); err != nil {
				return err
			}
		}
		return nil
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE 注释/心跳
		}
		if strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		data = strings.TrimPrefix(data, " ")
		if err := handleData(data); err != nil {
			return ChatResponse{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		if ctxErr := contextError(err); ctxErr != nil {
			return ChatResponse{}, ctxErr
		}
		return ChatResponse{}, &model.Error{
			Class: model.ErrProviderUnavailable, Code: "stream_read",
			Message: "读取流式响应失败：" + err.Error(), Retryable: true, Cause: err,
		}
	}
	if !sawDone && finish == "" {
		return ChatResponse{}, &model.Error{
			Class: model.ErrProviderUnavailable, Code: "stream_interrupted",
			Message: "流式响应在 finish_reason 之前中断", Retryable: true,
		}
	}

	ordered := make([]*streamCall, 0, len(calls))
	for _, c := range calls {
		ordered = append(ordered, c)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })
	toolCalls := make([]model.ToolCall, 0, len(ordered))
	for _, c := range ordered {
		call, err := finalizeStreamCall(c)
		if err != nil {
			return ChatResponse{}, err
		}
		toolCalls = append(toolCalls, call)
	}
	if finish == "" {
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	text := content.String()
	return ChatResponse{
		Message: model.Message{
			Role:      model.RoleAssistant,
			Content:   text,
			ToolCalls: toolCalls,
		},
		Usage:        usage.toUsage(),
		FinishReason: finish,
	}, nil
}

type streamCall struct {
	index int
	id    string
	name  string
	args  strings.Builder
}

func finalizeStreamCall(c *streamCall) (model.ToolCall, error) {
	if c.id == "" || c.name == "" {
		return model.ToolCall{}, &model.Error{
			Class: model.ErrProviderInvalidReq, Code: "stream_tool_call_incomplete",
			Message:   fmt.Sprintf("流式 tool_call[index=%d] 缺少 id 或 name，整批丢弃", c.index),
			Hint:      "provider 必须按 OpenAI 流式协议先发送 id/name，再增量发送 arguments",
			Retryable: true,
			Details:   map[string]any{"index": c.index, "id": c.id, "name": c.name},
		}
	}
	args := c.args.String()
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	return model.ToolCall{
		ID:        c.id,
		Name:      c.name,
		Arguments: normalizeArguments(json.RawMessage(args)),
	}, nil
}

func normalizeToolCalls(in []openAIToolCall) ([]model.ToolCall, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]model.ToolCall, 0, len(in))
	for _, tc := range in {
		if tc.ID == "" || tc.Function.Name == "" {
			return nil, &model.Error{
				Class: model.ErrProviderInvalidReq, Code: "tool_call_incomplete",
				Message:   "provider 返回的 tool_call 缺少 id 或 name",
				Retryable: true,
				Details:   map[string]any{"id": tc.ID, "name": tc.Function.Name},
			}
		}
		args := tc.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out, model.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: normalizeArguments(json.RawMessage(args)),
		})
	}
	return out, nil
}

// normalizeArguments 保留参数原文；非法 JSON 用 JSON 字符串包起来，
// 保证消息历史仍可序列化，执行阶段再解开并修复。
func normalizeArguments(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage("{}")
	}
	if json.Valid(trimmed) {
		return trimmed
	}
	encoded, err := json.Marshal(string(trimmed))
	if err != nil {
		return json.RawMessage("{}")
	}
	return encoded
}

func decodeContent(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(trimmed), &parts); err == nil {
			var b strings.Builder
			for _, p := range parts {
				if p.Text != "" {
					b.WriteString(p.Text)
				}
			}
			return b.String(), nil
		}
	}
	if strings.HasPrefix(trimmed, "{") {
		var obj struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			return obj.Text, nil
		}
	}
	return raw, nil
}

// classifyHTTPError 把 HTTP 错误映射为 §13.1 的分类。
func classifyHTTPError(status int, body []byte, header http.Header, providerName string) error {
	message, code := parseErrorBody(body)
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if len(message) > 1000 {
		message = message[:1000] + "…"
	}
	retryAfter := parseRetryAfter(header.Get("Retry-After"))
	details := map[string]any{"status": status, "provider": providerName}
	if code != "" {
		details["providerCode"] = code
	}
	if retryAfter > 0 {
		details["retryAfterMs"] = retryAfter.Milliseconds()
	}
	if message != "" {
		details["body"] = message
	}
	base := &model.Error{Code: strconv.Itoa(status), Details: details}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		base.Class = model.ErrProviderAuth
		base.Message = fmt.Sprintf("provider %s 鉴权失败（HTTP %d）：%s", providerName, status, message)
		base.Hint = "检查 apiKey/apiKeyEnv 是否正确、是否有该模型权限"
	case status == http.StatusTooManyRequests:
		base.Class = model.ErrProviderRateLimit
		base.Message = fmt.Sprintf("provider %s 限流（HTTP 429）：%s", providerName, message)
		base.Hint = "稍后重试，或降低并发；已配置 fallbackModels 时会切换模型"
		base.Retryable = true
	case status == http.StatusRequestTimeout:
		base.Class = model.ErrProviderUnavailable
		base.Message = fmt.Sprintf("provider %s 请求超时（HTTP 408）：%s", providerName, message)
		base.Retryable = true
	case status >= 500:
		base.Class = model.ErrProviderUnavailable
		base.Message = fmt.Sprintf("provider %s 暂不可用（HTTP %d）：%s", providerName, status, message)
		base.Retryable = true
	case status == http.StatusBadRequest && (isContextOverflow(message) || isContextOverflow(code)):
		base.Class = model.ErrContextOverflow
		base.Code = "context_overflow"
		base.Message = fmt.Sprintf("provider %s 报告输入超出上下文窗口：%s", providerName, message)
		base.Hint = "压缩历史或减少工具输出后重试"
	case status == http.StatusNotFound:
		base.Class = model.ErrProviderInvalidReq
		base.Message = fmt.Sprintf("provider %s 端点不存在（HTTP 404）：%s", providerName, message)
		base.Hint = "检查 baseUrl 是否包含正确的 API 前缀（通常以 /v1 结尾）"
	default:
		base.Class = model.ErrProviderInvalidReq
		base.Message = fmt.Sprintf("provider %s 拒绝了请求（HTTP %d）：%s", providerName, status, message)
		base.Hint = "检查模型 id、请求字段与 maxTokens 是否符合 provider 要求"
	}
	return base
}

func parseErrorBody(body []byte) (message, code string) {
	var top struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	}
	if json.Unmarshal(body, &top) == nil && top.Message != "" {
		switch c := top.Code.(type) {
		case string:
			code = c
		case float64:
			code = strconv.FormatFloat(c, 'f', -1, 64)
		}
		return top.Message, code
	}
	var parsed struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && len(parsed.Error) > 0 {
		var obj struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		}
		if json.Unmarshal(parsed.Error, &obj) == nil {
			message = obj.Message
			if obj.Type != "" && message == "" {
				message = obj.Type
			}
			switch c := obj.Code.(type) {
			case string:
				code = c
			case float64:
				code = strconv.FormatFloat(c, 'f', -1, 64)
			}
			if message != "" {
				return message, code
			}
		}
		var text string
		if json.Unmarshal(parsed.Error, &text) == nil {
			return text, ""
		}
	}
	return "", ""
}

func errorFromBody(message, typ string, status int, details map[string]any) error {
	if message == "" {
		message = typ
	}
	if message == "" {
		message = "provider 返回错误"
	}
	if isContextOverflow(message) {
		return &model.Error{
			Class: model.ErrContextOverflow, Code: "context_overflow",
			Message: message, Hint: "压缩历史后重试", Details: details,
		}
	}
	return &model.Error{
		Class: model.ErrProviderInvalidReq, Code: "provider_error",
		Message: message, Details: details, Retryable: true,
	}
}

var contextOverflowPattern = regexp.MustCompile(`(?i)(context_length_exceeded|maximum context length|context window|too many tokens|prompt is too long|reduce the length|context overflow|输入.*(超|长)|超出.*上下文)`)

func isContextOverflow(message string) bool {
	return contextOverflowPattern.MatchString(message)
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func classifyTransportError(parent context.Context, err error, providerName string) error {
	if parent != nil && parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	base := &model.Error{
		Class: model.ErrProviderUnavailable, Code: "network",
		Message:   fmt.Sprintf("连接 provider %s 失败：%v", providerName, err),
		Hint:      "检查网络、baseUrl 与代理设置",
		Retryable: true,
		Cause:     err,
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		base.Code = "timeout"
		base.Message = fmt.Sprintf("provider %s 请求超时：%v", providerName, err)
	}
	return base
}

func isTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	var t timeout
	return errors.As(err, &t) && t.Timeout()
}

func contextError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
