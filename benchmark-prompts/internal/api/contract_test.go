package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/example/benchmark-prompts/internal/auth"
	"github.com/example/benchmark-prompts/internal/cache"
	"github.com/example/benchmark-prompts/internal/config"
	"github.com/example/benchmark-prompts/internal/metrics"
	"github.com/example/benchmark-prompts/internal/model"
	"github.com/example/benchmark-prompts/internal/moderation"
	"github.com/example/benchmark-prompts/internal/ratelimit"
	"github.com/example/benchmark-prompts/internal/store"
)

// testMasterKey 是确定性的 32 字节主密钥（仅测试）。
var testMasterKey = func() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(200 - i)
	}
	return k
}()

type harness struct {
	t   *testing.T
	srv *Server
	st  *store.Store
}

func newHarness(t *testing.T) *harness {
	return newHarnessWith(t, nil)
}

func newHarnessWith(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()

	cfg := config.Default()
	cfg.Bandwidth.WatchEnabled = false
	if mutate != nil {
		mutate(cfg)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	mod, err := moderation.New(false, "")
	if err != nil {
		t.Fatalf("moderation: %v", err)
	}

	s := New(Deps{
		Config: cfg,
		Store:  st,
		Cache:  cache.New(64),
		Auth:   auth.New(st, testMasterKey, 5*time.Minute),
		Limiter: ratelimit.New(time.Minute, map[string]map[string]int{
			ratelimit.TierAnonymous: cfg.RateLimit.Anonymous,
			ratelimit.TierAuthed:    cfg.RateLimit.Authed,
		}),
		Metrics: &metrics.Registry{},
		Mod:     mod,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return &harness{t: t, srv: s, st: st}
}

// publish 造一条公开提示词。
func (h *harness) publish(content string, tags []string) string {
	h.t.Helper()
	res, err := h.st.CreatePendingPrompt(context.Background(), content, tags, "")
	if err != nil {
		h.t.Fatalf("seed 失败: %v", err)
	}
	if err := h.st.SetStatus(context.Background(), res.PromptID, model.StatusApproved); err != nil {
		h.t.Fatalf("审核失败: %v", err)
	}
	return res.PromptID
}

func (h *harness) seedKey(plainKey, secret string) {
	h.t.Helper()
	if err := h.st.PutAPIKey(context.Background(), plainKey, "tester", secret, testMasterKey); err != nil {
		h.t.Fatalf("登记 key 失败: %v", err)
	}
}

func (h *harness) do(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.srv.Routes().ServeHTTP(rec, req)
	return rec
}

// envShape 用于断言 docs/api.md §1.1 的信封结构。
type envShape struct {
	OK     bool            `json:"ok"`
	Data   json.RawMessage `json:"data"`
	Error  *errorShape     `json:"error"`
	Cursor *string         `json:"cursor"`
	V      int             `json:"v"`
}

type errorShape struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	RetryAfter int64  `json:"retry_after"`
}

func decodeEnv(t *testing.T, body []byte) envShape {
	t.Helper()

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatalf("响应不是合法 JSON: %v / %s", err, body)
	}
	// 契约要求四个字段恒定出现（值可为 null）
	for _, k := range []string{"ok", "data", "error", "cursor", "v"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("信封缺少字段 %q，实际键=%v", k, keys)
		}
	}
	var e envShape
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("信封解析失败: %v", err)
	}
	return e
}

func TestContractMetaShapeAndETag(t *testing.T) {
	h := newHarness(t)
	h.publish("meta probe prompt", []string{"probe"})

	rec := h.do(http.MethodGet, "/v1/meta", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("meta 必须返回 ETag")
	}

	e := decodeEnv(t, rec.Body.Bytes())
	if !e.OK || e.Error != nil || e.V != protocolVersion {
		t.Fatalf("信封不符: %+v", e)
	}
	var meta model.Meta
	if err := json.Unmarshal(e.Data, &meta); err != nil {
		t.Fatalf("meta 载荷不符: %v", err)
	}
	if meta.Total != 1 || meta.CatalogHash == "" || meta.SchemaVersion != store.SchemaVersion {
		t.Fatalf("meta 值不符: %+v", meta)
	}

	rec2 := h.do(http.MethodGet, "/v1/meta", nil, map[string]string{"If-None-Match": etag})
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("命中 If-None-Match 应 304，得到 %d", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("304 不得携带 body，得到 %d 字节", rec2.Body.Len())
	}
}

func TestContractListExcludesPromptContent(t *testing.T) {
	h := newHarness(t)
	id := h.publish("this body must not leak into the list", []string{"coding"})

	rec := h.do(http.MethodGet, "/v1/prompts?limit=1&tag=coding", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	e := decodeEnv(t, rec.Body.Bytes())

	var out struct {
		Items   []map[string]any `json:"items"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.Unmarshal(e.Data, &out); err != nil {
		t.Fatalf("list 载荷不符: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("应有 1 条摘要，得到 %d", len(out.Items))
	}
	if _, ok := out.Items[0]["p"]; ok {
		t.Fatalf("列表摘要绝不能包含正文 p（2M 带宽硬约束）")
	}
	if out.Items[0]["id"] != id {
		t.Fatalf("id 不符，期望 %s 得到 %v", id, out.Items[0]["id"])
	}
	if _, ok := out.Items[0]["h"]; !ok {
		t.Fatalf("摘要应含 h 字段")
	}
	if out.HasMore {
		t.Fatalf("只有 1 条时 has_more 应为 false")
	}
}

func TestContractGetReturnsFullPromptAnd404Code(t *testing.T) {
	h := newHarness(t)
	id := h.publish("full body here", []string{"a", "b"})

	rec := h.do(http.MethodGet, "/v1/prompts/"+id, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	e := decodeEnv(t, rec.Body.Bytes())
	var p model.Prompt
	if err := json.Unmarshal(e.Data, &p); err != nil {
		t.Fatalf("prompt 载荷不符: %v", err)
	}
	if p.Content != "full body here" || p.ID != id || len(p.Tags) != 2 {
		t.Fatalf("内容不符: %+v", p)
	}

	rec2 := h.do(http.MethodGet, "/v1/prompts/p_missing", nil, nil)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("不存在的 id 应 404，得到 %d", rec2.Code)
	}
	e2 := decodeEnv(t, rec2.Body.Bytes())
	if e2.OK || e2.Error == nil || e2.Error.Code != "not_found" {
		t.Fatalf("错误码不符: %+v", e2.Error)
	}
}

func TestContractRandomOnEmptyCatalog(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/v1/prompts/random", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("空库随机应 404，得到 %d", rec.Code)
	}
	e := decodeEnv(t, rec.Body.Bytes())
	if e.Error == nil || e.Error.Code != "not_found" {
		t.Fatalf("错误码应为 not_found，得到 %+v", e.Error)
	}
}

func TestContractRandomServesFullPrompt(t *testing.T) {
	h := newHarness(t)
	id := h.publish("random candidate body", nil)

	rec := h.do(http.MethodGet, "/v1/prompts/random?exclude="+id, nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("exclude 掉唯一条目后应 404，得到 %d body=%s", rec.Code, rec.Body)
	}
}

func TestContractWriteEndpointsRequireAuth(t *testing.T) {
	h := newHarness(t)
	id := h.publish("needs auth", nil)

	body, err := json.Marshal(map[string]any{"id": id, "value": 5, "deviceId": "dev-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := h.do(http.MethodPost, "/v1/scores", body, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权打分应 401，得到 %d", rec.Code)
	}
	e := decodeEnv(t, rec.Body.Bytes())
	if e.Error == nil || e.Error.Code != "unauthorized" {
		t.Fatalf("错误码应为 unauthorized，得到 %+v", e.Error)
	}
}

func TestContractScoreIsIdempotentPerDevice(t *testing.T) {
	h := newHarness(t)
	h.seedKey("score-key", "score-secret")
	id := h.publish("score me", nil)

	call := func(v int) (float64, int64) {
		h.t.Helper()
		body, err := json.Marshal(map[string]any{"id": id, "value": v, "deviceId": "dev-1"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rec := h.do(http.MethodPost, "/v1/scores", body, map[string]string{"Authorization": "Bearer score-key"})
		if rec.Code != http.StatusOK {
			t.Fatalf("打分应 200，得到 %d body=%s", rec.Code, rec.Body)
		}
		e := decodeEnv(t, rec.Body.Bytes())
		var out struct {
			Avg   float64 `json:"avg"`
			Count int64   `json:"count"`
		}
		if err := json.Unmarshal(e.Data, &out); err != nil {
			t.Fatalf("打分载荷不符: %v", err)
		}
		return out.Avg, out.Count
	}

	if avg, count := call(5); count != 1 || avg != 5 {
		t.Fatalf("首次打分期望 avg=5 count=1，得到 avg=%v count=%d", avg, count)
	}
	if avg, count := call(3); count != 1 || avg != 3 {
		t.Fatalf("同设备重复打分应覆盖，期望 avg=3 count=1，得到 avg=%v count=%d", avg, count)
	}

	bad, err := json.Marshal(map[string]any{"id": id, "value": 9, "deviceId": "dev-2"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := h.do(http.MethodPost, "/v1/scores", bad, map[string]string{"Authorization": "Bearer score-key"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("value 越界应 422，得到 %d", rec.Code)
	}
	e := decodeEnv(t, rec.Body.Bytes())
	if e.Error == nil || e.Error.Code != "validation_failed" {
		t.Fatalf("错误码应为 validation_failed，得到 %+v", e.Error)
	}
}

func TestContractUploadGoesToModerationQueue(t *testing.T) {
	h := newHarness(t)
	h.seedKey("up-key", "up-secret")

	empty, err := json.Marshal(map[string]any{"p": "   ", "clientId": "c-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := h.do(http.MethodPost, "/v1/prompts", empty, map[string]string{"Authorization": "Bearer up-key"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("空正文应 422，得到 %d body=%s", rec.Code, rec.Body)
	}

	good, err := json.Marshal(map[string]any{"p": "新上传的提示词正文", "t": []string{"coding"}, "clientId": "c-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec2 := h.do(http.MethodPost, "/v1/prompts", good, map[string]string{"Authorization": "Bearer up-key"})
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("新建上传应 202，得到 %d body=%s", rec2.Code, rec2.Body)
	}
	e := decodeEnv(t, rec2.Body.Bytes())
	var out struct {
		ID     string `json:"id"`
		Status string `json:"s"`
	}
	if err := json.Unmarshal(e.Data, &out); err != nil {
		t.Fatalf("上传载荷不符: %v", err)
	}
	if out.Status != model.StatusPending || out.ID == "" {
		t.Fatalf("应为 pending 且带 id，得到 %+v", out)
	}

	// 上传未过审，不得出现在公开列表
	rec3 := h.do(http.MethodGet, "/v1/prompts", nil, nil)
	e3 := decodeEnv(t, rec3.Body.Bytes())
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(e3.Data, &list); err != nil {
		t.Fatalf("list 载荷不符: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("pending 内容不得对外可见，得到 %d 条", len(list.Items))
	}

	// 同 clientId 重放 → 幂等 200
	rec4 := h.do(http.MethodPost, "/v1/prompts", good, map[string]string{"Authorization": "Bearer up-key"})
	if rec4.Code != http.StatusOK {
		t.Fatalf("幂等重放应 200，得到 %d", rec4.Code)
	}
}

func TestContractHMACSignature(t *testing.T) {
	h := newHarness(t)
	h.seedKey("hmac-key", "hmac-secret")
	id := h.publish("hmac target", nil)

	body := []byte(`{"id":"` + id + `","value":4,"deviceId":"dev-h"}`)
	ts := time.Now().Unix()
	sig := auth.Sign("hmac-secret", http.MethodPost, "/v1/scores", ts, body)

	rec := h.do(http.MethodPost, "/v1/scores", body, map[string]string{
		"X-Api-Key":   "hmac-key",
		"X-Timestamp": strconv.FormatInt(ts, 10),
		"X-Signature": sig,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("合法签名应通过，得到 %d body=%s", rec.Code, rec.Body)
	}

	// 篡改 body 必须验签失败
	tampered := []byte(`{"id":"` + id + `","value":1,"deviceId":"dev-h"}`)
	rec2 := h.do(http.MethodPost, "/v1/scores", tampered, map[string]string{
		"X-Api-Key":   "hmac-key",
		"X-Timestamp": strconv.FormatInt(ts, 10),
		"X-Signature": sig,
	})
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("篡改 body 应 401，得到 %d", rec2.Code)
	}
}

func TestContractRateLimitReturns429WithCode(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) {
		c.RateLimit.Anonymous["meta"] = 2
	})

	for i := 0; i < 2; i++ {
		if rec := h.do(http.MethodGet, "/v1/meta", nil, nil); rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次应成功，得到 %d", i+1, rec.Code)
		}
	}
	rec := h.do(http.MethodGet, "/v1/meta", nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超限应 429，得到 %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("429 必须带 Retry-After")
	}
	e := decodeEnv(t, rec.Body.Bytes())
	if e.Error == nil || e.Error.Code != "rate_limited" {
		t.Fatalf("错误码应为 rate_limited，得到 %+v", e.Error)
	}
	if e.Error.RetryAfter < 1 {
		t.Fatalf("retry_after 应为正秒数，得到 %d", e.Error.RetryAfter)
	}
}

func TestContractHealthz(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/-/healthz", nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("healthz 不符: %d %q", rec.Code, rec.Body.String())
	}
}

func TestContractBadCursor(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/v1/prompts?cursor=%25%25not-base64%25%25", nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法游标应 400，得到 %d body=%s", rec.Code, rec.Body)
	}
	e := decodeEnv(t, rec.Body.Bytes())
	if e.Error == nil || e.Error.Code != "bad_request" {
		t.Fatalf("错误码应为 bad_request，得到 %+v", e.Error)
	}
}

// TestContractGzipRoundTrip 走真实 HTTP 服务器，验证压缩与解压闭环。
func TestContractGzipRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.publish("gzip payload content", []string{"gz"})

	ts := httptest.NewServer(h.srv.Routes())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/meta", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")

	// 关闭 transport 自动解压，才能看到服务端真实压缩行为
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("应返回 gzip，得到 %q", got)
	}
	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("构造 gzip reader 失败: %v", err)
	}
	raw, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	e := decodeEnv(t, raw)
	if !e.OK {
		t.Fatalf("解压后信封不符: %+v", e)
	}
}

func TestContractDeltaServesChangesAndFallback(t *testing.T) {
	h := newHarness(t)
	id := h.publish("delta seeded prompt", []string{"sync"})

	meta := decodeEnv(t, h.do(http.MethodGet, "/v1/meta", nil, nil).Body.Bytes())
	var m model.Meta
	if err := json.Unmarshal(meta.Data, &m); err != nil {
		t.Fatalf("meta 解析失败: %v", err)
	}

	// since 已是最新 → 空集
	rec := h.do(http.MethodGet, "/v1/prompts/delta?since="+m.CatalogHash, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	e := decodeEnv(t, rec.Body.Bytes())
	var d struct {
		Changes []model.Prompt `json:"changes"`
		Deleted []string       `json:"deleted"`
		Since   string         `json:"since"`
		HasMore bool           `json:"has_more"`
	}
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatalf("delta 载荷不符: %v", err)
	}
	if len(d.Changes) != 0 || d.HasMore {
		t.Fatalf("无变更时应为空集，得到 changes=%d hasMore=%v", len(d.Changes), d.HasMore)
	}
	if d.Since == "" {
		t.Fatalf("delta 必须回带新的 since")
	}
	if d.Deleted == nil {
		t.Fatalf("deleted 必须是数组而不是 null")
	}

	// 未知 since → 回退全量，且必须含正文
	rec2 := h.do(http.MethodGet, "/v1/prompts/delta?since=ffffffffffffffff", nil, nil)
	e2 := decodeEnv(t, rec2.Body.Bytes())
	if !e2.OK {
		t.Fatalf("回退全量应成功: %+v", e2.Error)
	}
	if !bytes.Contains(rec2.Body.Bytes(), []byte(id)) {
		t.Fatalf("全量回退应含已有条目 %s，得到 %s", id, rec2.Body)
	}
	if !bytes.Contains(rec2.Body.Bytes(), []byte(`"p":`)) {
		t.Fatalf("delta 必须返回正文（客户端要落本地缓存）")
	}
}

func TestContractCORSPreflight(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) {
		c.CORS.AllowedOrigins = []string{"https://cdn.example.com"}
	})

	rec := h.do(http.MethodOptions, "/v1/meta", nil, map[string]string{
		"Origin":                        "https://cdn.example.com",
		"Access-Control-Request-Method": "GET",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("预检应返回 204，得到 %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://cdn.example.com" {
		t.Fatalf("白名单内源应被放行，得到 %q", got)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "X-Signature") {
		t.Fatalf("必须放行签名相关请求头，得到 %q", rec.Header().Get("Access-Control-Allow-Headers"))
	}

	bad := h.do(http.MethodOptions, "/v1/meta", nil, map[string]string{"Origin": "https://evil.example"})
	if got := bad.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("白名单外源不得被放行，得到 %q", got)
	}

	noOrigin := h.do(http.MethodGet, "/v1/meta", nil, nil)
	if got := noOrigin.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("无 Origin（CLI 调用）不应写 CORS 头，得到 %q", got)
	}
}

func TestContractMetricsRequiresAuth(t *testing.T) {
	h := newHarness(t)
	h.seedKey("metric-key", "metric-secret")

	if rec := h.do(http.MethodGet, "/-/metrics", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权访问指标应 401，得到 %d", rec.Code)
	}

	rec := h.do(http.MethodGet, "/-/metrics", nil, map[string]string{"Authorization": "Bearer metric-key"})
	if rec.Code != http.StatusOK {
		t.Fatalf("带 Key 应 200，得到 %d", rec.Code)
	}
	var snap map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("指标输出应为 JSON: %v / %s", err, rec.Body)
	}
	for _, k := range []string{"requests", "bytes_out", "egress_mbps", "bandwidth_degraded"} {
		if _, ok := snap[k]; !ok {
			t.Fatalf("指标缺少字段 %q，实际 %v", k, snap)
		}
	}
	if _, ok := snap["ok"]; ok {
		t.Fatalf("/-/metrics 是运维面，不得套业务信封")
	}
}

func TestAPIErrorShape(t *testing.T) {
	if got := ErrNotFound.Error(); !strings.Contains(got, "not_found") {
		t.Fatalf("Error() 应含机器码，得到 %q", got)
	}

	orig := ErrValidation.Message
	derived := ErrValidation.WithMessage("字段 %s 非法", "value")
	if derived.Message != "字段 value 非法" {
		t.Fatalf("WithMessage 未生效: %q", derived.Message)
	}
	if ErrValidation.Message != orig {
		t.Fatalf("WithMessage 不得污染预定义错误")
	}
	if derived.Code != ErrValidation.Code || derived.HTTP != ErrValidation.HTTP {
		t.Fatalf("副本应保留 code 与 HTTP 语义")
	}

	if retry := ErrRateLimited.WithRetry(3 * time.Second); retry.RetryAfter != 3*time.Second {
		t.Fatalf("WithRetry 未生效")
	}

	if got := asAPIError(errors.New("boom")); got != ErrInternal {
		t.Fatalf("未知错误必须映射为 internal 且不回显细节，得到 %+v", got)
	}
	if got := asAPIError(nil); got != nil {
		t.Fatalf("nil 应返回 nil，得到 %+v", got)
	}
	if got := asAPIError(store.ErrNotFound); got != ErrNotFound {
		t.Fatalf("store.ErrNotFound 应映射为 404，得到 %+v", got)
	}
	if got := asAPIError(auth.ErrUnauthorized); got != ErrUnauthorized {
		t.Fatalf("auth 错误应映射为 401，得到 %+v", got)
	}
}

// TestSignatureHelperIsUsableBySDK 保证 CLI/SDK 能自行算出与服务端一致的签名。
func TestSignatureHelperIsUsableBySDK(t *testing.T) {
	h := newHarness(t)
	h.seedKey("sig-key", "sig-secret")
	id := h.publish("signature target", nil)

	body := []byte(`{"id":"` + id + `","value":2,"deviceId":"dev-sig"}`)
	ts := time.Now().Unix()

	rec := h.do(http.MethodPost, "/v1/scores", body, map[string]string{
		"X-Api-Key":   "sig-key",
		"X-Timestamp": strconv.FormatInt(ts, 10),
		"X-Signature": auth.Sign("sig-secret", http.MethodPost, "/v1/scores", ts, body),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("SDK 自算签名应被接受，得到 %d body=%s", rec.Code, rec.Body)
	}
}
