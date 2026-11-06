package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

func openAISpec(baseURL string, mutate ...func(*config.ProviderSpec)) config.ProviderSpec {
	spec := config.ProviderSpec{
		Key: "p1", Name: "p1", API: "openai-completions", BaseURL: baseURL,
		APIKey: config.NewSecret("secret-key"), TimeoutMS: 5000,
		Models: []config.ModelSpec{{
			ProviderKey: "p1", ID: "real-model", Name: "alias",
			ContextWindow: 100000, MaxOutputTokens: 4096,
			SupportsTools: true, SupportsStreaming: true, SupportsParallelToolCalls: true,
		}},
	}
	for _, fn := range mutate {
		fn(&spec)
	}
	return spec
}

func TestOpenAINonStreamRequestAndResponse(t *testing.T) {
	var gotAuth, gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"choices":[{"message":{"role":"assistant","content":"hello","tool_calls":[
				{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.go\"}"}}
			]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}
		}`)
	}))
	defer srv.Close()

	p, err := NewOpenAICompletionsAdapter(openAISpec(srv.URL + "/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	temperature := 0.1
	resp, err := p.Chat(context.Background(), ChatRequest{
		Model: "alias",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "hi"},
			{Role: model.RoleAssistant, Content: "", ToolCalls: []model.ToolCall{{ID: "old", Name: "read", Arguments: json.RawMessage(`{"path":"b.go"}`)}}},
			{Role: model.RoleTool, ToolCallID: "old", Content: `{"ok":true}`},
		},
		Temperature: &temperature,
		MaxTokens:   100,
		Tools:       []model.ToolSpec{{Name: "read", Description: "read", Schema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:  "auto",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if body["model"] != "alias" || body["max_tokens"] != float64(100) || body["tool_choice"] != "auto" {
		t.Fatalf("body = %v", body)
	}
	msgs := body["messages"].([]any)
	assistant := msgs[1].(map[string]any)
	if assistant["content"] != nil {
		t.Fatalf("assistant tool-call content 应为 null: %v", assistant["content"])
	}
	calls := assistant["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"path":"b.go"}` {
		t.Fatalf("arguments 未按字符串发送: %v", fn["arguments"])
	}
	if resp.Message.Content != "hello" || resp.FinishReason != "tool_calls" {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].ID != "c1" {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	if resp.Usage.Input != 11 || resp.Usage.Output != 7 || resp.Usage.Cached != 3 || resp.Usage.Reasoning != 2 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestOpenAIStreamAccumulatesToolCalls(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`{"choices":[{"delta":{"role":"assistant","content":"你"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"想"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read","arguments":"{\"pa"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.go\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
		}
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p, _ := NewOpenAICompletionsAdapter(openAISpec(srv.URL))
	var deltas []Delta
	resp, err := p.Chat(context.Background(), ChatRequest{
		Model: "alias", Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}}, Stream: true,
	}, func(d Delta) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if body["stream"] != true {
		t.Fatalf("stream flag 未发送: %v", body)
	}
	if resp.Message.Content != "你" || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if got := string(resp.Message.ToolCalls[0].Arguments); got != `{"path":"a.go"}` {
		t.Fatalf("args = %q", got)
	}
	if resp.Usage.Input != 5 || resp.Usage.Output != 3 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if len(deltas) < 3 {
		t.Fatalf("deltas = %+v", deltas)
	}
}

func TestOpenAIErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		class  model.ErrorClass
		retry  bool
	}{
		{401, `{"error":{"message":"bad key"}}`, model.ErrProviderAuth, false},
		{429, `{"error":{"message":"slow"}}`, model.ErrProviderRateLimit, true},
		{500, `{"error":{"message":"boom"}}`, model.ErrProviderUnavailable, true},
		{400, `{"error":{"message":"maximum context length exceeded"}}`, model.ErrContextOverflow, false},
		{400, `{"error":{"message":"bad field"}}`, model.ErrProviderInvalidReq, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			p, _ := NewOpenAICompletionsAdapter(openAISpec(srv.URL))
			_, err := p.Chat(context.Background(), ChatRequest{Model: "alias"}, nil)
			if err == nil {
				t.Fatal("expected error")
			}
			if model.ClassOf(err) != tc.class {
				t.Fatalf("class = %q want %q (err=%v)", model.ClassOf(err), tc.class, err)
			}
			if model.IsRetryable(err) != tc.retry {
				t.Fatalf("retryable = %v want %v", model.IsRetryable(err), tc.retry)
			}
		})
	}
}

func TestOpenAIReasoningRequestShape(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	p, _ := NewOpenAICompletionsAdapter(openAISpec(srv.URL, func(s *config.ProviderSpec) {
		s.Models[0].Reasoning = true
	}))
	temp := 0.7
	_, err := p.Chat(context.Background(), ChatRequest{Model: "alias", MaxTokens: 50, Temperature: &temp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := body["max_tokens"]; ok {
		t.Fatalf("reasoning 模型不应使用 max_tokens: %v", body)
	}
	if body["max_completion_tokens"] != float64(50) {
		t.Fatalf("max_completion_tokens = %v", body["max_completion_tokens"])
	}
	if _, ok := body["temperature"]; ok {
		t.Fatalf("reasoning 模型不应发送 temperature: %v", body)
	}
}

func TestOpenAIContextCancelStopsRequest(t *testing.T) {
	var started atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Store(true)
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"late"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	p, _ := NewOpenAICompletionsAdapter(openAISpec(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for !started.Load() {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err := p.Chat(ctx, ChatRequest{Model: "alias"}, nil)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenAIStreamIncompleteToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	p, _ := NewOpenAICompletionsAdapter(openAISpec(srv.URL))
	_, err := p.Chat(context.Background(), ChatRequest{Model: "alias", Stream: true}, nil)
	if err == nil {
		t.Fatal("缺少 id/name 的 tool_call 必须失败")
	}
	if model.CodeOf(err) != "stream_tool_call_incomplete" || !strings.Contains(err.Error(), "id") {
		t.Fatalf("err = %v", err)
	}
}
