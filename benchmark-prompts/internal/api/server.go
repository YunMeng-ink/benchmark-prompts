package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/example/benchmark-prompts/internal/auth"
	"github.com/example/benchmark-prompts/internal/cache"
	"github.com/example/benchmark-prompts/internal/catalog"
	"github.com/example/benchmark-prompts/internal/config"
	"github.com/example/benchmark-prompts/internal/metrics"
	"github.com/example/benchmark-prompts/internal/moderation"
	"github.com/example/benchmark-prompts/internal/ratelimit"
	"github.com/example/benchmark-prompts/internal/store"
)

// Server 汇聚依赖并对外暴露 http.Handler。
type Server struct {
	cfg     *config.Config
	st      *store.Store
	cat     *catalog.Service
	cache   *cache.LRU
	authn   *auth.Authenticator
	limiter *ratelimit.Limiter
	metrics *metrics.Registry
	mod     *moderation.Checker
	degrade map[string]bool
	log     *slog.Logger
}

// Deps 是构造 Server 所需的依赖集合，装配全部在 cmd/server 完成。
type Deps struct {
	Config  *config.Config
	Store   *store.Store
	Cache   *cache.LRU
	Auth    *auth.Authenticator
	Limiter *ratelimit.Limiter
	Metrics *metrics.Registry
	Mod     *moderation.Checker
	Logger  *slog.Logger
}

// New 装配 Server。
func New(d Deps) *Server {
	s := &Server{
		cfg:     d.Config,
		st:      d.Store,
		cat:     catalog.New(d.Store),
		cache:   d.Cache,
		authn:   d.Auth,
		limiter: d.Limiter,
		metrics: d.Metrics,
		mod:     d.Mod,
		degrade: make(map[string]bool, len(d.Config.Bandwidth.Degrade)),
		log:     d.Logger,
	}
	for _, e := range d.Config.Bandwidth.Degrade {
		s.degrade[e] = true
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.metrics == nil {
		s.metrics = &metrics.Registry{}
	}
	return s
}

// Store 暴露底层存储（供审核脚本/维护子命令复用）。
func (s *Server) Store() *store.Store { return s.st }

// Routes 组装路由与全局中间件链（顺序见 docs/server.md §2）。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// 只读端点：降级 → 限流 → handler
	mux.HandleFunc("GET /v1/meta", s.route("meta", s.handleMeta))
	mux.HandleFunc("GET /v1/prompts", s.route("list", s.handleList))
	mux.HandleFunc("GET /v1/prompts/random", s.route("random", s.handleRandom))
	mux.HandleFunc("GET /v1/prompts/delta", s.route("delta", s.handleDelta))
	mux.HandleFunc("GET /v1/prompts/{id}", s.route("get", s.handleGet))
	mux.HandleFunc("GET /v1/prompts/{id}/score", s.route("get", s.handleScoreStats))

	// 写端点：鉴权 → 限流 → handler（限流必须在鉴权之后，才能按身份分桶）
	mux.HandleFunc("POST /v1/scores", s.writeRoute("scores", s.handleScore))
	mux.HandleFunc("POST /v1/prompts", s.writeRoute("upload", s.handleUpload))

	// 运维端点
	mux.HandleFunc("GET /-/healthz", s.handleHealthz)
	mux.HandleFunc("GET /-/metrics", s.writeRoute("metrics", s.handleMetrics))
	mux.HandleFunc("OPTIONS /", s.handlePreflight)

	// 由内向外依次包装（最外层最后赋值）。
	// compress 必须包在 metrics 之内：这样 gzip 压缩后的字节才流入 metricsWriter，
	// BytesOut 统计到的才是真实出站量（带宽看门狗的输入依据）。
	var h http.Handler = mux
	h = s.compressMW(h)
	h = s.metricsMW(h)
	h = s.securityHeadersMW(h)
	h = s.requestIDMW(h)
	h = s.recoverMW(h)
	return h
}

func (s *Server) route(endpoint string, h http.HandlerFunc) http.HandlerFunc {
	return s.degradeMW(endpoint, s.limitMW(endpoint, h))
}

func (s *Server) writeRoute(endpoint string, h http.HandlerFunc) http.HandlerFunc {
	return s.authMW(s.limitMW(endpoint, h))
}

// applyCORS 只在白名单命中时回写 CORS 头。
// 写端点用 Bearer/HMAC 而非 Cookie，因此不依赖 Allow-Credentials。
func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	w.Header().Add("Vary", "Origin")
	if !s.corsAllowed(origin) {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers",
		"Authorization, Content-Type, X-Api-Key, X-Timestamp, X-Signature, If-None-Match")
	w.Header().Set("Access-Control-Expose-Headers", "ETag, X-Request-Id, Retry-After")
	w.Header().Set("Access-Control-Max-Age", "600")
}

func (s *Server) corsAllowed(origin string) bool {
	for _, o := range s.cfg.CORS.AllowedOrigins {
		if o == "*" || strings.EqualFold(strings.TrimSpace(o), origin) {
			return true
		}
	}
	return false
}
