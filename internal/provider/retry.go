package provider

import (
	"context"
	"math/rand"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

type retrier struct {
	attempts int
	base     time.Duration
	max      time.Duration
	jitter   float64
	sleep    func(ctx context.Context, d time.Duration) error
	maxDelay time.Duration
}

// NewRetrier 按 settings.retry 构造退避重试器（§13.2）。
func NewRetrier(s config.RetrySettings) Retrier {
	attempts := s.Attempts
	if attempts < 1 {
		attempts = 1
	}
	base := time.Duration(s.BaseDelayMs) * time.Millisecond
	if base <= 0 {
		base = time.Second
	}
	maxDelay := time.Duration(s.MaxDelayMs) * time.Millisecond
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}
	jitter := s.JitterRatio
	if jitter < 0 {
		jitter = 0
	}
	if jitter > 1 {
		jitter = 1
	}
	return &retrier{
		attempts: attempts,
		base:     base,
		max:      maxDelay,
		jitter:   jitter,
		maxDelay: maxDelay,
		sleep:    sleepWithContext,
	}
}

// NewRetrierWithSleep 允许测试注入 sleep，不引入真实等待（§17.3“不允许 sleep 等锁”）。
func NewRetrierWithSleep(s config.RetrySettings, sleep func(ctx context.Context, d time.Duration) error) Retrier {
	r := NewRetrier(s).(*retrier)
	if sleep != nil {
		r.sleep = sleep
	}
	return r
}

func (r *retrier) Do(ctx context.Context, attempt func(ctx context.Context, n int) error) error {
	var last error
	for n := 1; n <= r.attempts; n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = attempt(ctx, n)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !model.IsRetryable(last) || n == r.attempts {
			return last
		}
		delay := r.delay(n)
		if err := r.sleep(ctx, delay); err != nil {
			return err
		}
	}
	return last
}

func (r *retrier) delay(attempt int) time.Duration {
	d := r.base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= r.max {
			d = r.max
			break
		}
	}
	if r.jitter > 0 {
		// ±jitter 比例抖动。
		factor := 1 + (rand.Float64()*2-1)*r.jitter
		d = time.Duration(float64(d) * factor)
	}
	if d > r.max {
		d = r.max
	}
	if d < 0 {
		d = 0
	}
	return d
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
