// Package api 是 HTTP 层：路由、中间件、handler 与统一响应信封。
// 对外契约由 docs/api.md 冻结，本包只做实现。
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/example/benchmark-prompts/internal/auth"
	"github.com/example/benchmark-prompts/internal/store"
)

// APIError 承载对外错误语义，与 docs/api.md §1.2 的错误码表一一对应。
type APIError struct {
	Code       string
	Message    string
	HTTP       int
	RetryAfter time.Duration
}

// Error 实现 error。
func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// WithMessage 返回替换了人类可读文案的副本（不改动预定义错误）。
func (e *APIError) WithMessage(format string, args ...any) *APIError {
	cp := *e
	cp.Message = fmt.Sprintf(format, args...)
	return &cp
}

// WithRetry 返回带建议等待时长的副本。
func (e *APIError) WithRetry(d time.Duration) *APIError {
	cp := *e
	cp.RetryAfter = d
	return &cp
}

// 预定义错误：code 是稳定机器码，客户端据此分支；message 只给人看。
var (
	ErrBadRequest   = &APIError{Code: "bad_request", Message: "请求参数缺失或非法", HTTP: http.StatusBadRequest}
	ErrUnauthorized = &APIError{Code: "unauthorized", Message: "缺少或非法的鉴权信息", HTTP: http.StatusUnauthorized}
	ErrForbidden    = &APIError{Code: "forbidden", Message: "无权限访问该资源", HTTP: http.StatusForbidden}
	ErrNotFound     = &APIError{Code: "not_found", Message: "资源不存在", HTTP: http.StatusNotFound}
	ErrConflict     = &APIError{Code: "conflict", Message: "资源状态冲突", HTTP: http.StatusConflict}
	ErrTooLarge     = &APIError{Code: "too_large", Message: "请求体超过上限", HTTP: http.StatusRequestEntityTooLarge}
	ErrValidation   = &APIError{Code: "validation_failed", Message: "字段校验失败", HTTP: http.StatusUnprocessableEntity}
	ErrRateLimited  = &APIError{Code: "rate_limited", Message: "请求过于频繁", HTTP: http.StatusTooManyRequests}
	ErrInternal     = &APIError{Code: "internal", Message: "服务器内部错误", HTTP: http.StatusInternalServerError}
	ErrUnavailable  = &APIError{Code: "unavailable", Message: "服务繁忙，请稍后重试", HTTP: http.StatusServiceUnavailable}
)

// asAPIError 把任意 error 归一化为 APIError；未知错误一律 500 且不回显细节。
func asAPIError(err error) *APIError {
	if err == nil {
		return nil
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, auth.ErrUnauthorized):
		return ErrUnauthorized
	case errors.Is(err, context.DeadlineExceeded):
		return ErrUnavailable.WithRetry(2 * time.Second)
	default:
		return ErrInternal
	}
}
