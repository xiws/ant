package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/xiws/ant/internal/model"
)

const maxFallbackHops = 8

// Client 是单适配器装饰器：重试、回退、写事件。由 Router 按别名构造。
type Client struct {
	base     Provider
	retrier  Retrier
	fallback Fallback
	sink     model.EventSink

	res    Resolved
	lookup func(Resolved) (Provider, error)
}

// NewClient 构造装饰器；fallback 可为 nil。
func NewClient(base Provider, retrier Retrier, fallback Fallback, sink model.EventSink) *Client {
	return &Client{base: base, retrier: retrier, fallback: fallback, sink: sink}
}

// Bind 绑定本次请求的解析结果与回退时的适配器查找函数。
// Client 复用时每次 Chat 前都必须重新 Bind（Router 每次构造新 Client）。
func (c *Client) Bind(res Resolved, lookup func(Resolved) (Provider, error)) {
	c.res = res
	c.lookup = lookup
}

// Name 返回底层适配器名。
func (c *Client) Name() string {
	if c.base == nil {
		return "client"
	}
	return c.base.Name()
}

// Capabilities 优先使用解析缓存。
func (c *Client) Capabilities(alias string) (model.Capabilities, error) {
	if c.res.Capabilities.ContextWindow > 0 && (alias == c.res.Alias || alias == c.res.ModelID) {
		return c.res.Capabilities, nil
	}
	return c.base.Capabilities(alias)
}

// Chat 实现 Provider：重试 → 回退 → 事件。
func (c *Client) Chat(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (ChatResponse, error) {
	cur := c.res
	base := c.base
	if cur.ModelID == "" {
		cur.ModelID = req.Model
	}
	var lastResp ChatResponse
	var lastErr error

	for hop := 0; hop < maxFallbackHops; hop++ {
		attemptReq := req
		attemptReq.Model = cur.ModelID
		if err := c.emit(ctx, model.EventModelRequest, model.MustJSON(map[string]any{
			"model":                cur.Alias,
			"provider":             cur.ProviderName,
			"messageCount":         len(req.Messages),
			"estimatedInputTokens": estimateMessagesTokens(req.Messages) + estimateToolsTokens(req.Tools),
			"tools":                toolNames(req.Tools),
			"stream":               req.Stream,
			"attempt":              1,
		})); err != nil {
			return ChatResponse{}, err
		}

		attempts := 0
		start := time.Now()
		lastResp, lastErr = ChatResponse{}, nil
		err := c.retrier.Do(ctx, func(ctx context.Context, n int) error {
			attempts = n
			resp, e := base.Chat(ctx, attemptReq, onDelta)
			if e != nil {
				return e
			}
			lastResp = resp
			return nil
		})
		duration := time.Since(start)

		if err == nil {
			if emitErr := c.emit(ctx, model.EventModelResponse, model.MustJSON(map[string]any{
				"finishReason":  lastResp.FinishReason,
				"usage":         lastResp.Usage,
				"textHash":      sha256Text(lastResp.Message.Content),
				"toolCallCount": len(lastResp.Message.ToolCalls),
				"durationMs":    duration.Milliseconds(),
				"attempt":       attempts,
				"model":         cur.Alias,
			})); emitErr != nil {
				return lastResp, emitErr
			}
			return lastResp, nil
		}
		lastErr = err
		_ = c.emit(ctx, model.EventModelResponse, model.MustJSON(map[string]any{
			"errorClass": model.ClassOf(err),
			"code":       model.CodeOf(err),
			"message":    err.Error(),
			"durationMs": duration.Milliseconds(),
			"attempt":    attempts,
			"model":      cur.Alias,
		}))
		if ctx.Err() != nil {
			return lastResp, ctx.Err()
		}
		if c.fallback == nil || c.lookup == nil {
			return lastResp, err
		}
		next, ok := c.fallback.Next(cur, model.ClassOf(err))
		if !ok {
			return lastResp, err
		}
		nextBase, lookupErr := c.lookup(next)
		if lookupErr != nil {
			return lastResp, err
		}
		_ = c.emit(ctx, model.EventModelFallback, model.MustJSON(map[string]any{
			"from":    cur.Alias,
			"to":      next.Alias,
			"reason":  model.ClassOf(err),
			"attempt": attempts,
		}))
		cur, base = next, nextBase
	}
	if lastErr == nil {
		lastErr = &model.Error{
			Class:   model.ErrProviderUnavailable,
			Code:    "fallback_exhausted",
			Message: fmt.Sprintf("回退链超过 %d 次仍未成功", maxFallbackHops),
		}
	}
	return lastResp, lastErr
}

func (c *Client) emit(ctx context.Context, t model.EventType, data []byte) error {
	if c.sink == nil {
		return nil
	}
	ev := &model.Event{Type: t, Data: data, Node: model.NodeFromContext(ctx)}
	return c.sink.Emit(ctx, ev)
}

func sha256Text(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func toolNames(tools []model.ToolSpec) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

func estimateMessagesTokens(msgs []model.Message) int {
	total := 0
	for _, m := range msgs {
		total += estimateTextTokens(m.Content)
		for _, tc := range m.ToolCalls {
			total += estimateTextTokens(tc.Name) + estimateTextTokens(string(tc.Arguments))
		}
	}
	return total
}

func estimateToolsTokens(tools []model.ToolSpec) int {
	total := 0
	for _, t := range tools {
		total += estimateTextTokens(t.Name) + estimateTextTokens(t.Description) + estimateTextTokens(string(t.Schema))
	}
	return total
}

// estimateTextTokens 是保守估算：ASCII 约 4 字符/token，CJK 约 1 字符/token（§11.2）。
func estimateTextTokens(s string) int {
	ascii, nonASCII := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			nonASCII++
		}
	}
	return (ascii+3)/4 + nonASCII
}
