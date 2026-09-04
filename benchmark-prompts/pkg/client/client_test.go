package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const metaJSON = `{"ok":true,"data":{"total":7,"catalog_hash":"h1","schema_version":1,"server_time":1700000000},"error":null,"cursor":null,"v":1}`

const promptJSON = `{"ok":true,"data":{"id":"p_1","p":"正文内容","t":["coding"],"v":3,"s":"approved","h":"abcdef12"},"error":null,"cursor":null,"v":1}`

// newTestClient 起一个进程内 HTTP 服务并接好临时缓存；退避置 0 以免测试真等。
func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c, err := New(Options{
		BaseURL:   srv.URL,
		CachePath: filepath.Join(t.TempDir(), "cache.db"),
		Backoff:   func(int) time.Duration { return 0 },
	})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestMetaCachesETagAndServes304FromCache(t *testing.T) {
	ctx := context.Background()
	var hits, withINM int32

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("If-None-Match") != "" {
			atomic.AddInt32(&withINM, 1)
		}
		if r.URL.Path != "/v1/meta" {
			http.Error(w, `{"ok":false,"data":null,"error":{"code":"not_found"},"cursor":null,"v":1}`, http.StatusNotFound)
			return
		}
		if r.Header.Get("If-None-Match") == `"h1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"h1"`)
		fmt.Fprint(w, metaJSON)
	}))

	m1, err := c.Meta(ctx)
	if err != nil {
		t.Fatalf("首次 Meta: %v", err)
	}
	if m1.Total != 7 || m1.CatalogHash != "h1" {
		t.Fatalf("meta 解析不符: %+v", m1)
	}

	m2, err := c.Meta(ctx)
	if err != nil {
		t.Fatalf("第二次 Meta 应命中 304 并回读缓存: %v", err)
	}
	if m2.Total != 7 {
		t.Fatalf("304 后应返回缓存副本，得到 %+v", m2)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("应正好发 2 次请求，得到 %d", got)
	}
	if got := atomic.LoadInt32(&withINM); got != 1 {
		t.Fatalf("第二次必须带 If-None-Match，实际带了几次=%d", got)
	}
}

func TestGetFallsBackToCacheOn304(t *testing.T) {
	ctx := context.Background()
	var hits int32

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("If-None-Match") == `"p_1:v3"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"p_1:v3"`)
		fmt.Fprint(w, promptJSON)
	}))

	p1, err := c.Get(ctx, "p_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p1.Content != "正文内容" || p1.Version != 3 {
		t.Fatalf("正文解析不符: %+v", p1)
	}

	p2, err := c.Get(ctx, "p_1")
	if err != nil {
		t.Fatalf("第二次 Get 应靠 304+缓存成功: %v", err)
	}
	if p2.Content != "正文内容" {
		t.Fatalf("应回读本地缓存正文，得到 %q", p2.Content)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("应正好 2 次请求，得到 %d", got)
	}
}

// TestRetryOnlyForIdempotent 锁住退避策略：读请求可重试，写请求绝不重试。
// 写请求重试会在服务端变慢时把延迟成倍放大，比偶发失败更糟。
func TestRetryOnlyForIdempotent(t *testing.T) {
	ctx := context.Background()
	var getHits, postHits int32

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/prompts/p_1":
			n := atomic.AddInt32(&getHits, 1)
			if n < 3 {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, promptJSON)
		case "/v1/scores":
			atomic.AddInt32(&postHits, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"ok":false,"data":null,"error":{"code":"unavailable","message":"过载"},"cursor":null,"v":1}`)
		default:
			t.Errorf("未预期路径 %s", r.URL.Path)
		}
	}))

	if _, err := c.Get(ctx, "p_1"); err != nil {
		t.Fatalf("读请求应在第三次成功: %v", err)
	}
	if got := atomic.LoadInt32(&getHits); got != 3 {
		t.Fatalf("读请求应重试到第 3 次，实际 %d", got)
	}

	if _, err := c.Score(ctx, "p_1", 5); err == nil {
		t.Fatalf("写请求应返回错误")
	} else if !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("应透传 unavailable，得到 %v", err)
	}
	if got := atomic.LoadInt32(&postHits); got != 1 {
		t.Fatalf("写请求不得重试，实际请求 %d 次", got)
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	ctx := context.Background()
	var hits int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"ok":false,"data":null,"error":{"code":"not_found","message":"没有这条"},"cursor":null,"v":1}`)
	}))

	_, err := c.Get(ctx, "p_x")
	if !IsNotFound(err) {
		t.Fatalf("应识别为 NotFound，得到 %v", err)
	}
	if e, ok := AsError(err); !ok || e.ExitCode() != ExitNotFound {
		t.Fatalf("退出码应为 %d，得到 %+v", ExitNotFound, err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("4xx 不该重试，实际 %d 次", got)
	}
}

func TestRateLimitedCarriesRetryAfterAndExitCode(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"ok":false,"data":null,"error":{"code":"rate_limited","message":"慢一点","retry_after":7},"cursor":null,"v":1}`)
	}))

	_, err := c.Score(context.Background(), "p_1", 5)
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("应返回 *Error，得到 %T", err)
	}
	if e.Code != CodeRateLimited || !IsRateLimited(err) {
		t.Fatalf("错误码不符: %+v", e)
	}
	if e.RetryAfter != 7*time.Second {
		t.Fatalf("应解析 retry_after，得到 %v", e.RetryAfter)
	}
	if e.ExitCode() != ExitRateLimited {
		t.Fatalf("限流退出码应为 %d，得到 %d", ExitRateLimited, e.ExitCode())
	}
}

func TestProtocolVersionMismatchIsRejected(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"data":{"total":1,"catalog_hash":"h","schema_version":9,"server_time":1},"error":null,"cursor":null,"v":2}`)
	}))

	_, err := c.Meta(context.Background())
	e, ok := AsError(err)
	if !ok || e.Code != CodeBadResponse {
		t.Fatalf("主版本不符必须显式失败，得到 %v", err)
	}
	if !strings.Contains(e.Message, "协议版本") {
		t.Fatalf("错误信息应说明版本不符，得到 %q", e.Message)
	}
}

func TestGarbageResponseIsRejected(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>网关错误页</html>")
	}))

	_, err := c.Meta(context.Background())
	if e, ok := AsError(err); !ok || e.Code != CodeBadResponse {
		t.Fatalf("非信封响应应报 bad_response，得到 %v", err)
	}
}

func TestErrorWithoutCodeFallsBackToStatus(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"ok":false,"data":null,"error":null,"cursor":null,"v":1}`)
	}))

	_, err := c.Score(context.Background(), "p_1", 5)
	e, ok := AsError(err)
	if !ok || e.Code != CodeForbidden {
		t.Fatalf("应按 HTTP 状态推断错误码，得到 %v", err)
	}
	if e.ExitCode() != ExitAuth {
		t.Fatalf("forbidden 退出码应为 %d", ExitAuth)
	}
}

func TestDeviceIDStableAcrossCalls(t *testing.T) {
	ctx := context.Background()
	var seen []string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, string(body))
		fmt.Fprint(w, `{"ok":true,"data":{"avg":4.5,"count":2},"error":null,"cursor":null,"v":1}`)
	}))

	if _, err := c.Score(ctx, "p_1", 5); err != nil {
		t.Fatalf("第一次打分失败: %v", err)
	}
	if _, err := c.Score(ctx, "p_1", 4); err != nil {
		t.Fatalf("第二次打分失败: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("应有 2 次请求，得到 %d", len(seen))
	}

	d1, d2 := deviceOf(t, seen[0]), deviceOf(t, seen[1])
	if d1 == "" {
		t.Fatalf("请求必须携带 deviceId，得到 %s", seen[0])
	}
	if d1 != d2 {
		t.Fatalf("同一设备的 deviceId 必须稳定（否则无法去重）：%s vs %s", d1, d2)
	}
	// 持久化在缓存里
	stored, err := c.Cache().DeviceID(ctx)
	if err != nil || stored != d1 {
		t.Fatalf("deviceId 应已持久化，得到 %q err=%v", stored, err)
	}
}

func TestUploadHonoursExplicitClientID(t *testing.T) {
	var body string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"ok":true,"data":{"id":"p_new","s":"pending"},"error":null,"cursor":null,"v":1}`)
	}))

	res, err := c.UploadWithClientID(context.Background(), "正文", []string{"a"}, "fixed-cid")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.ID != "p_new" || res.Status != "pending" {
		t.Fatalf("结果不符: %+v", res)
	}
	if !strings.Contains(body, `"clientId":"fixed-cid"`) {
		t.Fatalf("应透传 clientId 以获得幂等重放，得到 %s", body)
	}
}

func TestLocalValidationAvoidsRoundTrip(t *testing.T) {
	var hits int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))

	if _, err := c.Score(context.Background(), "p_1", 6); err == nil {
		t.Fatalf("越界分值应在本地就被拒")
	}
	if _, err := c.Score(context.Background(), "  ", 5); err == nil {
		t.Fatalf("空 id 应在本地就被拒")
	}
	if _, err := c.Get(context.Background(), "   "); err == nil {
		t.Fatalf("空 id 的 Get 应被拒")
	}
	if _, err := c.Upload(context.Background(), "   \n ", nil); err == nil {
		t.Fatalf("空正文应在本地就被拒")
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("本地校验失败时不应发出任何请求，实际 %d 次", got)
	}
}

func TestListFollowsCursor(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			fmt.Fprint(w, `{"ok":true,"data":{"items":[{"id":"p_1","t":[],"v":1,"h":"a"}],"has_more":true},"error":null,"cursor":"Y3Vyc29yLTE=","v":1}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"data":{"items":[{"id":"p_2","t":[],"v":1,"h":"b"}],"has_more":false},"error":null,"cursor":null,"v":1}`)
	}))

	page, err := c.List(context.Background(), "", 1, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 1 || !page.HasMore || page.Cursor != "Y3Vyc29yLTE=" {
		t.Fatalf("首页不符: %+v", page)
	}
	next, err := c.List(context.Background(), "", 1, page.Cursor)
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if next.HasMore || next.Cursor != "" || len(next.Items) != 1 {
		t.Fatalf("次页不符: %+v", next)
	}
}

func TestNoCacheModeStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, metaJSON)
	}))
	t.Cleanup(srv.Close)

	c, err := New(Options{BaseURL: srv.URL, NoCache: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if c.Cache() != nil {
		t.Fatalf("NoCache 模式下不应有缓存")
	}
	m, err := c.Meta(context.Background())
	if err != nil || m.Total != 7 {
		t.Fatalf("NoCache 仍应可读: %v", err)
	}
	if _, err := c.Sync(context.Background()); err == nil {
		t.Fatalf("NoCache 下 Sync 必须明确报错而不是静默失效")
	}
}

func TestNewValidatesOptions(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatalf("空 BaseURL 必须报错")
	}
	if _, err := New(Options{BaseURL: "not a url"}); err == nil {
		t.Fatalf("非法 URL 必须报错")
	}
	if _, err := New(Options{BaseURL: "ftp://example.com"}); err == nil {
		t.Fatalf("非 http(s) 协议必须报错")
	}
	c, err := New(Options{BaseURL: "https://example.com/", NoCache: true})
	if err != nil {
		t.Fatalf("合法参数应通过: %v", err)
	}
	defer c.Close()
	if c.base != "https://example.com" {
		t.Fatalf("尾部斜杠应被去掉，得到 %q", c.base)
	}
}

func TestRecentWindowRollsAndDedups(t *testing.T) {
	ctx := context.Background()
	cc, err := OpenCache(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	defer cc.Close()

	if got, err := cc.RecentIDs(ctx); err != nil || len(got) != 0 {
		t.Fatalf("初始应为空，得到 %v err=%v", got, err)
	}

	for _, id := range []string{"a", "b", "c"} {
		if err := cc.PushRecent(ctx, id); err != nil {
			t.Fatalf("PushRecent(%s): %v", id, err)
		}
	}
	got, err := cc.RecentIDs(ctx)
	if err != nil || len(got) != 3 || got[0] != "c" || got[2] != "a" {
		t.Fatalf("应为最新在前 [c b a]，得到 %v err=%v", got, err)
	}

	// 重复抽到同一项不应产生重复条目，而是提到最前
	if err := cc.PushRecent(ctx, "a"); err != nil {
		t.Fatalf("PushRecent: %v", err)
	}
	got, _ = cc.RecentIDs(ctx)
	if len(got) != 3 || got[0] != "a" {
		t.Fatalf("去重并提到最前失效，得到 %v", got)
	}

	// 窗口有界：避免 exclude 参数无限增长撞服端 100 个上限
	for i := 0; i < 200; i++ {
		if err := cc.PushRecent(ctx, fmt.Sprintf("p_%03d", i)); err != nil {
			t.Fatalf("PushRecent: %v", err)
		}
	}
	got, _ = cc.RecentIDs(ctx)
	if len(got) > 50 {
		t.Fatalf("最近窗口应被限在 50 以内，得到 %d", len(got))
	}

	if err := cc.PushRecent(ctx, "  "); err != nil {
		t.Fatalf("空白 id 应被忽略: %v", err)
	}

	if err := cc.ClearRecent(ctx); err != nil {
		t.Fatalf("ClearRecent: %v", err)
	}
	if got, _ := cc.RecentIDs(ctx); len(got) != 0 {
		t.Fatalf("清空后应为空，得到 %v", got)
	}
}

// TestRandomRecordsRecent 验证 random 会自动把抽到的 id 记入最近窗口。
func TestRandomRecordsRecent(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "exclude=") {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"ok":false,"data":null,"error":{"code":"not_found","message":"当前没有可用的提示词"},"cursor":null,"v":1}`)
			return
		}
		fmt.Fprint(w, promptJSON)
	}))
	ctx := context.Background()

	if _, err := c.Random(ctx, "", nil); err != nil {
		t.Fatalf("Random: %v", err)
	}
	recent, err := c.Cache().RecentIDs(ctx)
	if err != nil || len(recent) != 1 || recent[0] != "p_1" {
		t.Fatalf("抽到的 id 应被记入最近窗口，得到 %v err=%v", recent, err)
	}

	// 再抽时带 exclude 会 404，证明 --fresh 语义链路贯通
	if _, err := c.Random(ctx, "", recent); !IsNotFound(err) {
		t.Fatalf("排除已抽过的 id 应 404，得到 %v", err)
	}
}

func TestCacheRoundTripAndPurge(t *testing.T) {
	ctx := context.Background()
	cc, err := OpenCache(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	defer cc.Close()

	changed, err := cc.UpsertPrompt(ctx, &Prompt{ID: "p_1", Content: "x", Tags: []string{"a", "b"}, Version: 2, Hash: "h"})
	if err != nil || !changed {
		t.Fatalf("首次写入应视为变化: changed=%v err=%v", changed, err)
	}
	again, err := cc.UpsertPrompt(ctx, &Prompt{ID: "p_1", Content: "x", Tags: []string{"a", "b"}, Version: 2, Hash: "h"})
	if err != nil {
		t.Fatalf("重复写入失败: %v", err)
	}
	if again {
		t.Fatalf("同版本同 hash 不应算作变化")
	}

	got, err := cc.GetPrompt(ctx, "p_1")
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if len(got.Tags) != 2 || got.Tags[1] != "b" {
		t.Fatalf("标签往返不符: %v", got.Tags)
	}

	if _, err := cc.GetPrompt(ctx, "p_missing"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("未命中应返回 ErrCacheMiss，得到 %v", err)
	}

	n, err := cc.DeletePrompts(ctx, []string{"p_1", "p_ghost"})
	if err != nil || n != 1 {
		t.Fatalf("删除计数应为 1，得到 %d err=%v", n, err)
	}

	if _, err := cc.UpsertPrompt(ctx, &Prompt{ID: "p_2", Content: "y", Version: 1}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := cc.SetETag(ctx, "p_2", `"v"`); err != nil {
		t.Fatalf("SetETag: %v", err)
	}
	if err := cc.Purge(ctx); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if got, err := cc.GetPrompt(ctx, "p_2"); err == nil {
		t.Fatalf("Purge 后不应还能读到，得到 %+v", got)
	}
	if et, err := cc.GetETag(ctx, "p_2"); err != nil || et != "" {
		t.Fatalf("Purge 应同时清掉 ETag，得到 %q", et)
	}
	dev1, err := cc.DeviceID(ctx)
	if err != nil || dev1 == "" {
		t.Fatalf("Purge 必须保留 device_id，否则评分去重失效：%v", err)
	}
	dev2, _ := cc.DeviceID(ctx)
	if dev1 != dev2 {
		t.Fatalf("device_id 必须稳定：%s vs %s", dev1, dev2)
	}
}

func TestCacheRejectsEmptyPrompt(t *testing.T) {
	cc, err := OpenCache(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	defer cc.Close()

	if _, err := cc.UpsertPrompt(context.Background(), nil); err == nil {
		t.Fatalf("nil 提示词必须被拒")
	}
	if _, err := cc.UpsertPrompt(context.Background(), &Prompt{}); err == nil {
		t.Fatalf("无 id 的提示词必须被拒")
	}
}

func TestConfigRoundTripAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")

	cfg := &Config{Endpoint: "bench.example.com", APIKey: "k", Secret: "s"}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Endpoint != "bench.example.com" || got.APIKey != "k" || got.Secret != "s" {
		t.Fatalf("往返不一致: %+v", got)
	}

	// 缺失文件不算错误（首次运行）
	absent, err := LoadConfig(filepath.Join(dir, "nope"))
	if err != nil || absent == nil || absent.Endpoint != "" {
		t.Fatalf("缺失配置应返回空值，得到 %+v err=%v", absent, err)
	}

	t.Setenv("BENCH_ENDPOINT", "https://from-env.example")
	t.Setenv("BENCH_API_KEY", "env-key")
	merged, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	merged.ApplyEnv()
	if merged.Endpoint != "https://from-env.example" || merged.APIKey != "env-key" {
		t.Fatalf("环境变量应覆盖文件值: %+v", merged)
	}

	if err := (&Config{}).Save(filepath.Join(dir, "sub", "deeper", "config")); err != nil {
		t.Fatalf("Save 应自动建目录: %v", err)
	}
}

// TestSaveTightensExistingFile 盯住一个很阴的坑：os.WriteFile 的 perm
// 只在创建时生效，文件已存在时不会收紧权限。
func TestSaveTightensExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不支持 POSIX 权限位")
	}
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("预置文件失败: %v", err)
	}
	if err := (&Config{Endpoint: "https://a.b"}).Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if m := st.Mode().Perm(); m != 0o600 {
		t.Fatalf("已存在的凭据文件必须被收紧到 0600，得到 %v", m)
	}
}

// TestCacheJournalModeIsDelete 钉住本地缓存的 journal 模式这一**配置选择**。
//
// 它只断言"没有偷偷开回 WAL"，不声称能防住任何 Windows 清理问题——
// 那个偶发抖动至今未定位，别拿这个测试当它的回归防线。
func TestCacheJournalModeIsDelete(t *testing.T) {
	c, err := OpenCache(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("打开缓存失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	var mode string
	if err := c.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("读 journal_mode 失败: %v", err)
	}
	if strings.ToLower(mode) != "delete" {
		t.Fatalf("本地缓存应为 delete 模式（单连接场景 WAL 无收益），实际 %q", mode)
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	ok := map[string]string{
		"bench.example.com":         "https://bench.example.com",
		"https://bench.example.com": "https://bench.example.com",
		"http://127.0.0.1:8080/":    "http://127.0.0.1:8080",
		"  https://a.b  ":           "https://a.b",
		// docs/api.md 的 Base URL 带 /v1，用户会原样粘过来（SDK 自己会再拼 /v1）。
		"https://bench.example.com/v1": "https://bench.example.com",
		"http://127.0.0.1:8080/v1/":    "http://127.0.0.1:8080",
		"bench.example.com/v1":         "https://bench.example.com",
		"https://api.example.com/v1x":  "https://api.example.com/v1x", // 不是版本段，不得误剔
		"https://example.com/a/v1":     "https://example.com/a",
	}
	for in, want := range ok {
		got, err := NormalizeEndpoint(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q 应规范化为 %q，得到 %q", in, want, got)
		}
	}
	for _, bad := range []string{"", "   ", "ftp://a.b", "unix:///tmp/s"} {
		if _, err := NormalizeEndpoint(bad); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		}
	}
}

func TestExitCodeMappingCoversAllCodes(t *testing.T) {
	cases := map[string]int{
		CodeNetwork:      ExitNetwork,
		CodeInternal:     ExitNetwork,
		CodeUnavailable:  ExitNetwork,
		CodeRateLimited:  ExitRateLimited,
		CodeUnauthorized: ExitAuth,
		CodeForbidden:    ExitAuth,
		CodeNotFound:     ExitNotFound,
		CodeBadRequest:   ExitBadInput,
		CodeValidation:   ExitBadInput,
		CodeTooLarge:     ExitBadInput,
		CodeConflict:     ExitBadInput,
		CodeBadResponse:  ExitBadInput,
		"brand_new_code": ExitBadInput, // 未知码必须落到通用失败而不是 0
	}
	for code, want := range cases {
		if got := ExitCodeFor(code); got != want {
			t.Fatalf("%s 退出码应为 %d，得到 %d", code, want, got)
		}
	}
}

func TestErrorFormattingAndUnwrap(t *testing.T) {
	e := &Error{Code: CodeNotFound, Message: "没有", HTTP: 404}
	if !strings.Contains(e.Error(), "not_found") || !strings.Contains(e.Error(), "404") {
		t.Fatalf("Error() 应含 code 与状态，得到 %q", e.Error())
	}
	cause := fmt.Errorf("dial tcp: refused")
	wrapped := &Error{Code: CodeNetwork, Message: "网络请求失败", Err: cause}
	if !errors.Is(wrapped, cause) {
		t.Fatalf("应支持 Unwrap")
	}
	if !strings.Contains(wrapped.Error(), "refused") {
		t.Fatalf("Error() 应带出底层原因，得到 %q", wrapped.Error())
	}
}

func TestDeltaNormalizesNilArrays(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"data":{"changes":null,"deleted":null,"since":"h9","has_more":false},"error":null,"cursor":null,"v":1}`)
	}))

	d, err := c.Delta(context.Background(), "h1", 10, "")
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if d.Changes == nil || d.Deleted == nil {
		t.Fatalf("null 数组应归一化为空切片，方便调用方 range：%+v", d)
	}
	if d.Since != "h9" {
		t.Fatalf("since 不符: %q", d.Since)
	}
}

func TestDefaultBackoffGrowsAndCaps(t *testing.T) {
	if d := defaultBackoff(0); d != 0 {
		t.Fatalf("attempt 0 不应等待，得到 %v", d)
	}
	if d := defaultBackoff(1); d != time.Second {
		t.Fatalf("第一次应等 1s，得到 %v", d)
	}
	if d := defaultBackoff(2); d != 2*time.Second {
		t.Fatalf("第二次应等 2s，得到 %v", d)
	}
	if d := defaultBackoff(50); d > 30*time.Second {
		t.Fatalf("退避必须有上限，得到 %v", d)
	}
}

func deviceOf(t *testing.T, body string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("请求体不是 JSON: %v / %s", err, body)
	}
	if v, ok := m["deviceId"].(string); ok {
		return v
	}
	return ""
}
