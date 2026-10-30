package model

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorClass 是错误分类（§13.1）。
type ErrorClass string

const (
	ErrConfigInvalid         ErrorClass = "config_invalid"
	ErrProviderAuth          ErrorClass = "provider_auth"
	ErrProviderRateLimit     ErrorClass = "provider_rate_limit"
	ErrProviderUnavailable   ErrorClass = "provider_unavailable"
	ErrProviderInvalidReq    ErrorClass = "provider_invalid_request"
	ErrContextOverflow       ErrorClass = "context_overflow"
	ErrToolInvalidArgs       ErrorClass = "tool_invalid_args"
	ErrToolTimeout           ErrorClass = "tool_timeout"
	ErrToolFailed            ErrorClass = "tool_failed"
	ErrApprovalDenied        ErrorClass = "approval_denied"
	ErrApprovalRequired      ErrorClass = "approval_required" // 非交互缺授权（退出码 4）
	ErrVerificationFailed    ErrorClass = "verification_failed"
	ErrLoopDetected          ErrorClass = "loop_detected"
	ErrInterrupted           ErrorClass = "interrupted"
	ErrInternal              ErrorClass = "internal"
	ErrAdapterNotImplemented ErrorClass = "adapter_not_implemented"
)

// Error 是跨模块统一错误。失败走返回值，不 panic（R5）。
type Error struct {
	Class     ErrorClass     `json:"class"`
	Code      string         `json:"code,omitempty"`
	Message   string         `json:"message,omitempty"`
	Hint      string         `json:"hint,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	Retryable bool           `json:"retryable,omitempty"`
	Cause     error          `json:"-"`
}

// Error 实现 error。
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	if e.Class != "" {
		b.WriteString(string(e.Class))
	} else {
		b.WriteString("error")
	}
	if e.Code != "" && e.Code != string(e.Class) {
		b.WriteString("/")
		b.WriteString(e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Hint != "" {
		b.WriteString("（hint: ")
		b.WriteString(e.Hint)
		b.WriteString("）")
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

// Unwrap 支持 errors.Is/As。
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NewError 构造一个分类错误。
func NewError(class ErrorClass, code, message string) *Error {
	return &Error{Class: class, Code: code, Message: message}
}

// Errorf 构造一个带格式化消息的分类错误。
func Errorf(class ErrorClass, code, format string, args ...any) *Error {
	return &Error{Class: class, Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithHint 返回带 hint 的副本（不修改原值）。
func (e *Error) WithHint(hint string) *Error {
	if e == nil {
		return nil
	}
	cp := *e
	cp.Hint = hint
	return &cp
}

// WithDetails 返回带 details 的副本（不修改原值）。
func (e *Error) WithDetails(kv map[string]any) *Error {
	if e == nil {
		return nil
	}
	cp := *e
	cp.Details = kv
	return &cp
}

// WithCode 返回带细码的副本（不修改原值）。
func (e *Error) WithCode(code string) *Error {
	if e == nil {
		return nil
	}
	cp := *e
	cp.Code = code
	return &cp
}

// ClassOf 返回错误的分类；未分类错误视为 ErrInternal（§13.1）。
func ClassOf(err error) ErrorClass {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) && e.Class != "" {
		return e.Class
	}
	return ErrInternal
}

// IsRetryable 报告错误是否可重试。显式 Retryable 优先，其次按类别默认（§13.2）；
// 包装错误会沿 Unwrap 链检查，避免把可重试的 provider 错误误判为不可重试。
func IsRetryable(err error) bool {
	for err != nil {
		var e *Error
		if errors.As(err, &e) {
			if e.Retryable {
				return true
			}
			switch e.Class {
			case ErrProviderRateLimit, ErrProviderUnavailable:
				return true
			}
		}
		err = errors.Unwrap(err)
	}
	return false
}

// CodeOf 返回错误的细码，没有时返回空串。
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
