package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/example/benchmark-prompts/internal/ratelimit"
)

type ctxKey string

const (
	ctxRequestID ctxKey = "request-id"
	ctxIdentity  ctxKey = "identity"
	ctxBody      ctxKey = "body"
)

// maxBodyBytes 限制写请求体（评分/上传都是小包）。
const maxBodyBytes = 1 << 20

func requestIDOf(r *http.Request) string {
	v, _ := r.Context().Value(ctxRequestID).(string)
	return v
}

// identityOf 返回鉴权身份；匿名为空串。
func identityOf(r *http.Request) string {
	v, _ := r.Context().Value(ctxIdentity).(string)
	return v
}

// bodyBytes 取鉴权中间件已读取的请求体（HMAC 验签必须先读 body）。
func bodyBytes(r *http.Request) []byte {
	v, _ := r.Context().Value(ctxBody).([]byte)
	return v
}

// ---- ResponseWriter 包装 ----

// metricsWriter 记录最终写到连接上的状态码与字节数（gzip 之后的真实量）。
type metricsWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (m *metricsWriter) WriteHeader(code int) {
	if m.wroteHeader {
		return
	}
	m.status = code
	m.wroteHeader = true
	m.ResponseWriter.WriteHeader(code)
}

func (m *metricsWriter) Write(p []byte) (int, error) {
	if !m.wroteHeader {
		m.WriteHeader(http.StatusOK)
	}
	n, err := m.ResponseWriter.Write(p)
	m.bytes += n
	return n, err
}

// gzipWriter 把明文压进 gzip 后写入内层 writer，因此内层统计到的是压缩后字节。
type gzipWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	status      int
	wroteHeader bool
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.status = code
	g.wroteHeader = true
	if code != http.StatusNotModified {
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Del("Content-Length")
		g.Header().Add("Vary", "Accept-Encoding")
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.status == http.StatusNotModified {
		return len(p), nil // 304 不允许有 body
	}
	return g.gz.Write(p)
}

// finish 仅在确实写过压缩内容时收尾，否则会给 304 补上 gzip 尾块。
func (g *gzipWriter) finish() {
	if g.wroteHeader && g.status != http.StatusNotModified {
		_ = g.gz.Close()
	}
}

// ---- 中间件 ----

// recoverMW panic 兜底：单请求崩溃不影响进程。
func (s *Server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("请求 panic 已恢复", "panic", rec, "path", r.URL.Path, "id", requestIDOf(r))
				s.renderErr(w, ErrInternal)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requestIDMW 注入请求 ID（透传给 CDN/日志排查）。
func (s *Server) requestIDMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

// metricsMW 统计请求量/字节/304/5xx 并输出结构化访问日志。
//
// 它与访问日志合并：若拆成两层会各自包一次 ResponseWriter，
// 导致同一批字节被累加两遍。
func (s *Server) metricsMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		mw := &metricsWriter{ResponseWriter: w}
		next.ServeHTTP(mw, r)

		s.metrics.Requests.Add(1)
		s.metrics.BytesOut.Add(int64(mw.bytes))
		if mw.status == http.StatusNotModified {
			s.metrics.Hits304.Add(1)
		}
		s.log.Info("http",
			"id", requestIDOf(r),
			"method", r.Method,
			"path", r.URL.Path,
			"status", mw.status,
			"dur_ms", time.Since(start).Milliseconds(),
			"bytes", mw.bytes,
			"ip", clientIP(r),
			"who", identityOf(r),
		)
	})
}

// compressMW 对接受 gzip 的客户端压缩响应。
func (s *Server) compressMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w, gz: gzip.NewWriter(w)}
		defer gw.finish()
		next.ServeHTTP(gw, r)
	})
}

// securityHeadersMW 统一安全响应头。
func (s *Server) securityHeadersMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		h.Set("Referrer-Policy", "same-origin")
		s.applyCORS(w, r)
		next.ServeHTTP(w, r)
	})
}

// degradeMW 在带宽吃紧时对低优先级端点熔断，保住 get/random 核心体验。
func (s *Server) degradeMW(endpoint string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Bandwidth.WatchEnabled && s.degrade[endpoint] && s.metrics.Degraded() {
			s.renderErr(w, ErrUnavailable.WithRetry(5*time.Second))
			return
		}
		next(w, r)
	}
}

// authMW 校验写请求；成功后把身份与已读 body 放进 context。
func (s *Server) authMW(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			s.renderErr(w, ErrTooLarge)
			return
		}
		who, err := s.authn.Authenticate(r, body)
		if err != nil {
			s.renderErr(w, ErrUnauthorized)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		ctx := context.WithValue(context.WithValue(r.Context(), ctxIdentity, who), ctxBody, body)
		next(w, r.WithContext(ctx))
	}
}

// limitMW 按 (tier, endpoint, subject) 限流。必须在 authMW 之后，
// 否则已鉴权请求会被按匿名配额误限。
func (s *Server) limitMW(endpoint string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tier, subject := ratelimit.TierAnonymous, clientIP(r)
		if who := identityOf(r); who != "" {
			tier, subject = ratelimit.TierAuthed, who
		}
		if ok, wait := s.limiter.Allow(tier, endpoint, subject); !ok {
			s.metrics.Limited.Add(1)
			s.renderErr(w, ErrRateLimited.WithRetry(wait))
			return
		}
		next(w, r)
	}
}

// clientIP 优先取 X-Forwarded-For（源站在 CDN 之后）。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func newRequestID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}
