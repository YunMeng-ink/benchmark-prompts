package client

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// 服务端错误码，与 docs/api.md §1.2 一一对应。
// 调用方应据此分支；Message 是给人看的，可能随版本变化。
const (
	CodeBadRequest   = "bad_request"
	CodeUnauthorized = "unauthorized"
	CodeForbidden    = "forbidden"
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeTooLarge     = "too_large"
	CodeValidation   = "validation_failed"
	CodeRateLimited  = "rate_limited"
	CodeInternal     = "internal"
	CodeUnavailable  = "unavailable"
	CodeBadResponse  = "bad_response" // 本地：响应无法解析/契约不符
	CodeNetwork      = "network"      // 本地：连接层失败
)

// 进程退出码约定（docs/client.md §11）。
// bench CLI 用它作 exit code，插件与 shell 脚本可据此判断失败类别。
const (
	ExitOK          = 0
	ExitNetwork     = 1
	ExitRateLimited = 2
	ExitAuth        = 3
	ExitNotFound    = 4
	ExitBadInput    = 5
)

// Error 是一次可判定的失败：可能来自服务端，也可能是本地解析/网络错误。
type Error struct {
	Code       string
	Message    string
	HTTP       int
	RetryAfter time.Duration
	Err        error // 底层原因（网络错误等），可为 nil
}

// Error 实现 error。
func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s (%v)", e.Code, e.Message, e.Err)
	}
	if e.HTTP != 0 {
		return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Message, e.HTTP)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap 暴露底层错误，支持 errors.Is(err, context.DeadlineExceeded) 之类。
func (e *Error) Unwrap() error { return e.Err }

// ExitCode 返回约定退出码。
func (e *Error) ExitCode() int { return ExitCodeFor(e.Code) }

// ExitCodeFor 把错误码映射为退出码，未知码归到通用失败。
//
// 未知码必须落到非 0：否则新增的服务端错误码会被旧版插件当成成功。
func ExitCodeFor(code string) int {
	switch code {
	case CodeNetwork, CodeInternal, CodeUnavailable:
		return ExitNetwork
	case CodeRateLimited:
		return ExitRateLimited
	case CodeUnauthorized, CodeForbidden:
		return ExitAuth
	case CodeNotFound:
		return ExitNotFound
	case CodeBadRequest, CodeValidation, CodeTooLarge, CodeConflict, CodeBadResponse:
		return ExitBadInput
	default:
		return ExitBadInput
	}
}

// AsError 取出 *Error；不是 *Error 时返回 false。
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// IsNotFound 判断是否是"资源不存在"。CLI 用它把随机/查询空结果当正常情况处理。
func IsNotFound(err error) bool {
	if e, ok := AsError(err); ok {
		return e.Code == CodeNotFound
	}
	return false
}

// IsRateLimited 判断是否被限流（通常该稍后重试而不是报错给用户）。
func IsRateLimited(err error) bool {
	if e, ok := AsError(err); ok {
		return e.Code == CodeRateLimited
	}
	return false
}

// retryable 判断该 HTTP 状态是否值得重试。
//
// 只对**幂等的只读请求**开放重试；写请求即使带 clientId 也不重试，
// 避免在服务端慢的时候把延迟放大成 3 倍。
func retryableStatus(status int) bool {
	switch status {
	case http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
