package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// protocolVersion 是信封的 v 字段（docs/api.md §1.1）。
const protocolVersion = 1

type errorObject struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	RetryAfter int64  `json:"retry_after,omitempty"`
}

// envelope 是统一响应信封。四个字段恒定出现（值为 null 也序列化），
// 这样客户端可以无条件读 env.ok / env.data / env.error / env.cursor。
type envelope struct {
	OK     bool         `json:"ok"`
	Data   any          `json:"data"`
	Error  *errorObject `json:"error"`
	Cursor *string      `json:"cursor"`
	V      int          `json:"v"`
}

// renderOK 输出成功信封。
func (s *Server) renderOK(w http.ResponseWriter, status int, data any, cursor *string) {
	s.writeEnvelope(w, status, envelope{OK: true, Data: data, Cursor: cursor, V: protocolVersion})
}

// renderErr 输出失败信封；接受任意 error，内部归一化。
func (s *Server) renderErr(w http.ResponseWriter, err error) {
	ae := asAPIError(err)
	if ae == nil {
		ae = ErrInternal
	}
	obj := &errorObject{Code: ae.Code, Message: ae.Message}
	if ae.RetryAfter > 0 {
		secs := int64(ae.RetryAfter / time.Second)
		if ae.RetryAfter%time.Second != 0 || secs < 1 {
			secs++
		}
		obj.RetryAfter = secs
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	}
	if ae.HTTP >= 500 {
		s.metrics.Errors5xx.Add(1)
	}
	s.writeEnvelope(w, ae.HTTP, envelope{OK: false, Error: obj, V: protocolVersion})
}

func (s *Server) writeEnvelope(w http.ResponseWriter, status int, env envelope) {
	buf, err := json.Marshal(env)
	if err != nil {
		s.log.Error("序列化响应信封失败", "err", err)
		buf = []byte(`{"ok":false,"data":null,"error":{"code":"internal","message":"服务器内部错误"},"cursor":null,"v":1}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		s.log.Warn("写出响应失败", "err", err)
	}
}

// writeNotModified 输出 304（无 body）。304 计数在 metrics 中间件统一做，避免重复累加。
func (s *Server) writeNotModified(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotModified)
}

// quoteETag 给裸 hash 加上双引号，符合 HTTP 规范。
func quoteETag(v string) string {
	if strings.HasPrefix(v, `"`) {
		return v
	}
	return `"` + v + `"`
}

// matchETag 处理 If-None-Match，支持多值与弱标签。
func matchETag(r *http.Request, etag string) bool {
	in := strings.TrimSpace(r.Header.Get("If-None-Match"))
	if in == "" {
		return false
	}
	if in == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, cand := range strings.Split(in, ",") {
		c := strings.TrimSpace(strings.TrimPrefix(cand, "W/"))
		if c == want {
			return true
		}
	}
	return false
}
