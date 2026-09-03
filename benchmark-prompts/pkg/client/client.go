package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout    = 15 * time.Second
	defaultUserAgent  = "bench-sdk/1"
	defaultMaxAttempt = 3
	maxResponseBytes  = 8 << 20
	protocolVersion   = 1
)

// Options 构造 Client 所需参数。零值可用（除 BaseURL）。
type Options struct {
	BaseURL   string
	APIKey    string
	Secret    string // 非空则走 HMAC 签名，否则退回 Bearer
	Timeout   time.Duration
	UserAgent string

	// CachePath 本地缓存文件；为空用 DefaultCachePath()。
	CachePath string
	// NoCache=true 关闭本地缓存与 ETag 协商（一次性脚本场景）。
	NoCache bool
	// DeviceID 覆盖评分去重指纹；不覆盖时优先取 Options 里缓存中的值。
	DeviceID string

	HTTP       *http.Client
	Logger     *slog.Logger
	MaxAttempt int

	// Now / Backoff 便于测试注入。
	Now     func() time.Time
	Backoff func(attempt int) time.Duration
}

// Client 是并发安全的 API 客户端。
type Client struct {
	opt      Options
	base     string
	http     *http.Client
	cache    *Cache
	ownsCch  bool
	log      *slog.Logger
	now      func() time.Time
	backoff  func(int) time.Duration
	attempts int
	ua       string
}

// New 校验参数并（除非 NoCache）打开本地缓存。
func New(opt Options) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(opt.BaseURL), "/")
	if base == "" {
		return nil, &Error{Code: CodeBadRequest, Message: "BaseURL 不能为空"}
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, &Error{Code: CodeBadRequest, Message: fmt.Sprintf("BaseURL 非法: %q", base), Err: err}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, &Error{Code: CodeBadRequest, Message: fmt.Sprintf("BaseURL 协议必须是 http/https，得到 %q", u.Scheme)}
	}

	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	hc := opt.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	} else if hc.Timeout == 0 {
		hc.Timeout = timeout
	}

	attempts := opt.MaxAttempt
	if attempts <= 0 {
		attempts = defaultMaxAttempt
	}
	c := &Client{
		opt:      opt,
		base:     base,
		http:     hc,
		log:      opt.Logger,
		now:      opt.Now,
		backoff:  opt.Backoff,
		attempts: attempts,
		ua:       defaultUserAgent,
	}
	if c.log == nil {
		c.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.backoff == nil {
		c.backoff = defaultBackoff
	}
	if opt.UserAgent != "" {
		c.ua = opt.UserAgent
	}

	if !opt.NoCache {
		cc, err := OpenCache(opt.CachePath)
		if err != nil {
			return nil, err
		}
		c.cache = cc
		c.ownsCch = true
	}
	return c, nil
}

// Close 释放自有的本地缓存连接（外部注入的 HTTP client 不会被关闭）。
func (c *Client) Close() error {
	if c.ownsCch && c.cache != nil {
		return c.cache.Close()
	}
	return nil
}

// Cache 暴露本地缓存（同步、离线读取用）。NoCache 时返回 nil。
func (c *Client) Cache() *Cache { return c.cache }

// DefaultBackoff 是 1s、2s、4s 的指数退避。
func defaultBackoff(attempt int) time.Duration {
	if attempt < 1 {
		return 0
	}
	if attempt > 5 {
		attempt = 5
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

// ---- 传输 ----

type envelopeBody struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	RetryAfter int64  `json:"retry_after"`
}

// envelope 与 docs/api.md §1.1 一致；四个字段恒定出现。
type envelope struct {
	OK     bool            `json:"ok"`
	Data   json.RawMessage `json:"data"`
	Error  *envelopeBody   `json:"error"`
	Cursor *string         `json:"cursor"`
	V      int             `json:"v"`
}

type request struct {
	method      string
	path        string
	query       url.Values
	body        []byte
	idempotent  bool
	ifNoneMatch string
}

type response struct {
	status      int
	etag        string
	notModified bool
	data        json.RawMessage
	cursor      *string
}

func (c *Client) send(ctx context.Context, req *request) (*response, error) {
	var lastErr error
	for attempt := 1; attempt <= c.attempts; attempt++ {
		if attempt > 1 {
			wait := c.backoff(attempt - 1)
			if wait > 0 {
				if err := sleepCtx(ctx, wait); err != nil {
					return nil, err
				}
			}
		}
		resp, err := c.attempt(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		apiErr, ok := AsError(err)
		if !ok || !req.idempotent || !shouldRetry(apiErr) {
			return nil, err
		}
		// 被限流时优先按服务端给的时长等待，而不是盲目退避
		if apiErr.Code == CodeRateLimited && apiErr.RetryAfter > 0 {
			if err := sleepCtx(ctx, apiErr.RetryAfter); err != nil {
				return nil, err
			}
		}
		c.log.Debug("重试幂等请求", "attempt", attempt, "path", req.path, "code", apiErr.Code)
	}
	return nil, lastErr
}

// shouldRetry 只放行网络层错误与 5xx；4xx 一律不重试。
func shouldRetry(e *Error) bool {
	if e == nil {
		return false
	}
	switch e.Code {
	case CodeNetwork:
		return true
	case CodeRateLimited, CodeInternal, CodeUnavailable, CodeBadResponse:
		return e.HTTP == 0 || retryableStatus(e.HTTP)
	default:
		return false
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
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

func (c *Client) attempt(ctx context.Context, req *request) (*response, error) {
	full := c.base + req.path
	if len(req.query) > 0 {
		full += "?" + req.query.Encode()
	}

	var bodyReader io.Reader
	if req.body != nil {
		bodyReader = bytes.NewReader(req.body)
	}
	hreq, err := http.NewRequestWithContext(ctx, req.method, full, bodyReader)
	if err != nil {
		return nil, &Error{Code: CodeBadResponse, Message: "构造请求失败", Err: err}
	}

	hreq.Header.Set("Accept", "application/json")
	// 不手工设置 Accept-Encoding：net/http 默认 Transport 会自动加 "Accept-Encoding: gzip"
	// 并透明解压。自己设置就得自己解压，还会关掉 transport 的自动 gzip 优化。
	if c.ua != "" {
		hreq.Header.Set("User-Agent", c.ua)
	}
	if req.body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	if req.ifNoneMatch != "" {
		hreq.Header.Set("If-None-Match", req.ifNoneMatch)
	}
	// 签名只覆盖 path，不含 query（与服务端 internal/auth 一致）
	applyAuth(hreq.Header, c.opt.APIKey, c.opt.Secret, req.method, req.path, req.body, c.now())

	hresp, err := c.http.Do(hreq)
	if err != nil {
		return nil, &Error{Code: CodeNetwork, Message: "网络请求失败", Err: err}
	}
	defer hresp.Body.Close()

	if hresp.StatusCode == http.StatusNotModified {
		return &response{status: hresp.StatusCode, etag: hresp.Header.Get("ETag"), notModified: true}, nil
	}

	raw, rerr := io.ReadAll(io.LimitReader(hresp.Body, maxResponseBytes))
	if rerr != nil {
		return nil, &Error{Code: CodeNetwork, Message: "读取响应失败", HTTP: hresp.StatusCode, Err: rerr}
	}

	var env envelope
	if perr := json.Unmarshal(raw, &env); perr != nil {
		return nil, &Error{
			Code: CodeBadResponse, HTTP: hresp.StatusCode, Err: perr,
			Message: fmt.Sprintf("响应不是合法信封（HTTP %d）", hresp.StatusCode),
		}
	}

	if !env.OK {
		e := &Error{HTTP: hresp.StatusCode}
		if env.Error != nil {
			e.Code = env.Error.Code
			e.Message = env.Error.Message
			if env.Error.RetryAfter > 0 {
				e.RetryAfter = time.Duration(env.Error.RetryAfter) * time.Second
			}
		}
		if e.Code == "" {
			e.Code = codeForStatus(hresp.StatusCode)
		}
		if e.Message == "" {
			e.Message = http.StatusText(hresp.StatusCode)
		}
		if e.RetryAfter == 0 {
			if ra := hresp.Header.Get("Retry-After"); ra != "" {
				if secs, convErr := strconv.ParseInt(ra, 10, 64); convErr == nil {
					e.RetryAfter = time.Duration(secs) * time.Second
				}
			}
		}
		return nil, e
	}

	if env.V != 0 && env.V != protocolVersion {
		return nil, &Error{
			Code: CodeBadResponse, HTTP: hresp.StatusCode,
			Message: fmt.Sprintf("协议版本不符：服务端 v%d，客户端 v%d", env.V, protocolVersion),
		}
	}
	return &response{
		status: hresp.StatusCode,
		etag:   hresp.Header.Get("ETag"),
		data:   env.Data,
		cursor: env.Cursor,
	}, nil
}

func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return CodeBadRequest
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeConflict
	case http.StatusRequestEntityTooLarge:
		return CodeTooLarge
	case http.StatusUnprocessableEntity:
		return CodeValidation
	case http.StatusTooManyRequests:
		return CodeRateLimited
	case http.StatusInternalServerError:
		return CodeInternal
	case http.StatusServiceUnavailable:
		return CodeUnavailable
	default:
		return CodeBadResponse
	}
}

func decodeData[T any](resp *response) (*T, error) {
	if len(resp.data) == 0 {
		return nil, &Error{Code: CodeBadResponse, Message: "响应缺少 data 字段", HTTP: resp.status}
	}
	var out T
	if err := json.Unmarshal(resp.data, &out); err != nil {
		return nil, &Error{Code: CodeBadResponse, Message: "解析 data 失败", HTTP: resp.status, Err: err}
	}
	return &out, nil
}

// ---- 只读端点 ----

// Meta 拉取目录元信息。启用缓存时自动做 If-None-Match 协商（几乎零流量）。
func (c *Client) Meta(ctx context.Context) (*Meta, error) {
	const etagKeyMeta = "meta"
	req := &request{method: http.MethodGet, path: "/v1/meta", idempotent: true}
	if c.cache != nil {
		if et, err := c.cache.GetETag(ctx, etagKeyMeta); err == nil {
			req.ifNoneMatch = et
		}
	}

	resp, err := c.send(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.notModified && c.cache != nil {
		if raw, err := c.cache.GetKV(ctx, "meta_json"); err == nil && raw != "" {
			var m Meta
			if json.Unmarshal([]byte(raw), &m) == nil {
				return &m, nil
			}
		}
		// 缓存里没留下副本 → 强制再拉一次（不带条件头）
		req.ifNoneMatch = ""
		if resp, err = c.send(ctx, req); err != nil {
			return nil, err
		}
	}

	m, err := decodeData[Meta](resp)
	if err != nil {
		return nil, err
	}
	if c.cache != nil && resp.etag != "" {
		if err := c.cache.SetETag(ctx, etagKeyMeta, resp.etag); err != nil {
			c.log.Warn("写入 meta ETag 失败", "err", err)
		}
		if err := c.cache.SetKV(ctx, "meta_json", string(resp.data)); err != nil {
			c.log.Warn("写入 meta 副本失败", "err", err)
		}
	}
	return m, nil
}

// Get 取一条完整提示词。304 时直接回读本地缓存。
func (c *Client) Get(ctx context.Context, id string) (*Prompt, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, &Error{Code: CodeBadRequest, Message: "id 不能为空"}
	}
	req := &request{method: http.MethodGet, path: "/v1/prompts/" + url.PathEscape(id), idempotent: true}
	if c.cache != nil {
		if et, err := c.cache.GetETag(ctx, id); err == nil {
			req.ifNoneMatch = et
		}
	}

	resp, err := c.send(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.notModified {
		p, cerr := c.cache.GetPrompt(ctx, id)
		if cerr == nil {
			return p, nil
		}
		// ETag 命中但本地没有副本：去掉条件头重发一次
		req.ifNoneMatch = ""
		if resp, err = c.send(ctx, req); err != nil {
			return nil, err
		}
	}

	p, err := decodeData[Prompt](resp)
	if err != nil {
		return nil, err
	}
	if c.cache != nil {
		if resp.etag != "" {
			_ = c.cache.SetETag(ctx, p.ID, resp.etag)
		}
		_, _ = c.cache.UpsertPrompt(ctx, p)
	}
	return p, nil
}

// Random 随机取一条。exclude 最多 100 个 id（服务端会截断）。
func (c *Client) Random(ctx context.Context, tag string, exclude []string) (*Prompt, error) {
	q := url.Values{}
	if tag != "" {
		q.Set("tag", tag)
	}
	if len(exclude) > 0 {
		q.Set("exclude", strings.Join(exclude, ","))
	}
	// 随机结果不做 ETag 协商：每次内容本就不同
	resp, err := c.send(ctx, &request{
		method: http.MethodGet, path: "/v1/prompts/random", query: q, idempotent: true,
	})
	if err != nil {
		return nil, err
	}
	p, err := decodeData[Prompt](resp)
	if err != nil {
		return nil, err
	}
	if c.cache != nil {
		_, _ = c.cache.UpsertPrompt(ctx, p)
		// 记入“最近抽过”窗口，供 random --fresh 去重
		_ = c.cache.PushRecent(ctx, p.ID)
	}
	return p, nil
}

// List 拉一页摘要。cursor 传上一页返回的 ListPage.Cursor。
func (c *Client) List(ctx context.Context, tag string, limit int, cursor string) (*ListPage, error) {
	q := url.Values{}
	if tag != "" {
		q.Set("tag", tag)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	resp, err := c.send(ctx, &request{
		method: http.MethodGet, path: "/v1/prompts", query: q, idempotent: true,
	})
	if err != nil {
		return nil, err
	}
	page, err := decodeData[ListPage](resp)
	if err != nil {
		return nil, err
	}
	if resp.cursor != nil {
		page.Cursor = *resp.cursor
	}
	return page, nil
}

// ---- 写端点 ----

// Score 打分（1-5）。deviceId 由本地缓存持久化，保证同设备重复打分被服务端覆盖。
func (c *Client) Score(ctx context.Context, id string, value int) (*ScoreResult, error) {
	if strings.TrimSpace(id) == "" {
		return nil, &Error{Code: CodeBadRequest, Message: "id 不能为空"}
	}
	if value < 1 || value > 5 {
		return nil, &Error{Code: CodeValidation, Message: "value 必须是 1-5"}
	}
	dev, err := c.resolveDeviceID(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"id": id, "value": value, "deviceId": dev})
	if err != nil {
		return nil, &Error{Code: CodeBadResponse, Message: "序列化请求失败", Err: err}
	}

	resp, err := c.send(ctx, &request{method: http.MethodPost, path: "/v1/scores", body: body})
	if err != nil {
		return nil, err
	}
	return decodeData[ScoreResult](resp)
}

// Upload 上传一条提示词进审核队列，自动生成幂等 clientId。
func (c *Client) Upload(ctx context.Context, content string, tags []string) (*UploadResult, error) {
	return c.UploadWithClientID(ctx, content, tags, "c_"+newID(8))
}

// UploadWithClientID 允许调用方自带 clientId，从而在超时/断网后安全重放同一请求。
// 不写显式 clientId 时，重试可能产生重复条目（服务端只按 clientId 幂等）。
func (c *Client) UploadWithClientID(ctx context.Context, content string, tags []string, clientID string) (*UploadResult, error) {
	if strings.TrimSpace(content) == "" {
		return nil, &Error{Code: CodeValidation, Message: "正文不能为空"}
	}
	body, err := json.Marshal(map[string]any{"p": content, "t": tags, "clientId": clientID})
	if err != nil {
		return nil, &Error{Code: CodeBadResponse, Message: "序列化请求失败", Err: err}
	}
	resp, err := c.send(ctx, &request{method: http.MethodPost, path: "/v1/prompts", body: body})
	if err != nil {
		return nil, err
	}
	return decodeData[UploadResult](resp)
}

func (c *Client) resolveDeviceID(ctx context.Context) (string, error) {
	if c.opt.DeviceID != "" {
		return c.opt.DeviceID, nil
	}
	if c.cache != nil {
		return c.cache.DeviceID(ctx)
	}
	// 没有缓存就没有稳定指纹；用 Options.DeviceID，否则每次都是新设备。
	dev := "d_" + newID(8)
	c.log.Warn("未启用本地缓存且未设置 DeviceID，本次评分无法被去重", "deviceId", dev)
	return dev, nil
}
