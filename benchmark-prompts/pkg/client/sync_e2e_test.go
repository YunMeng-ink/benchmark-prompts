package client

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/benchmark-prompts/internal/api"
	"github.com/example/benchmark-prompts/internal/auth"
	"github.com/example/benchmark-prompts/internal/cache"
	"github.com/example/benchmark-prompts/internal/config"
	"github.com/example/benchmark-prompts/internal/metrics"
	"github.com/example/benchmark-prompts/internal/model"
	"github.com/example/benchmark-prompts/internal/moderation"
	"github.com/example/benchmark-prompts/internal/ratelimit"
	"github.com/example/benchmark-prompts/internal/store"
)

// backend 是**真实运行的源站**（httptest + api.Server + SQLite），不是 mock。
// SDK 的增量同步、鉴权、幂等语义必须对着真实现验证，否则契约测试毫无意义。
type backend struct {
	srv    *httptest.Server
	st     *store.Store
	master []byte
}

func newBackend(t *testing.T) *backend {
	t.Helper()

	cfg := config.Default()
	cfg.Bandwidth.WatchEnabled = false
	// 测试会从一个 IP 密集打接口，放开限流以免测到的是限流而不是逻辑
	for k := range cfg.RateLimit.Anonymous {
		cfg.RateLimit.Anonymous[k] = 1 << 20
	}
	for k := range cfg.RateLimit.Authed {
		cfg.RateLimit.Authed[k] = 1 << 20
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("打开源站库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	mod, err := moderation.New(false, "")
	if err != nil {
		t.Fatalf("moderation: %v", err)
	}
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 11)
	}

	s := api.New(api.Deps{
		Config: cfg,
		Store:  st,
		Cache:  cache.New(64),
		Auth:   auth.New(st, master, 5*time.Minute),
		Limiter: ratelimit.New(time.Minute, map[string]map[string]int{
			ratelimit.TierAnonymous: cfg.RateLimit.Anonymous,
			ratelimit.TierAuthed:    cfg.RateLimit.Authed,
		}),
		Metrics: &metrics.Registry{},
		Mod:     mod,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return &backend{srv: ts, st: st, master: master}
}

// publish 直接落库并过审（绕过 HTTP，等价于运维执行 -approve）。
func (b *backend) publish(t *testing.T, content string, tags []string) string {
	t.Helper()
	res, err := b.st.CreatePendingPrompt(context.Background(), content, tags, "")
	if err != nil {
		t.Fatalf("seed 失败: %v", err)
	}
	if err := b.st.SetStatus(context.Background(), res.PromptID, model.StatusApproved); err != nil {
		t.Fatalf("过审失败: %v", err)
	}
	return res.PromptID
}

func (b *backend) reject(t *testing.T, id string) {
	t.Helper()
	if err := b.st.SetStatus(context.Background(), id, model.StatusRejected); err != nil {
		t.Fatalf("打回失败: %v", err)
	}
}

func (b *backend) seedKey(t *testing.T, plain, secret string) {
	t.Helper()
	if err := b.st.PutAPIKey(context.Background(), plain, "sdk-test", secret, b.master); err != nil {
		t.Fatalf("登记 key 失败: %v", err)
	}
}

// clientFor 构造指向真实源站的 SDK 客户端。
func (b *backend) clientFor(t *testing.T, secret string) *Client {
	t.Helper()
	c, err := New(Options{
		BaseURL:   b.srv.URL,
		APIKey:    "sdk-key",
		Secret:    secret,
		CachePath: filepath.Join(t.TempDir(), "cache.db"),
		Backoff:   func(int) time.Duration { return 0 },
	})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestEndToEndFirstSyncIsFullThenIncremental(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	a := b.publish(t, "第一条提示词正文", []string{"a"})
	_ = b.publish(t, "第二条提示词正文", []string{"b"})
	c := b.clientFor(t, "")

	rep, err := c.Sync(ctx)
	if err != nil {
		t.Fatalf("首次同步失败: %v", err)
	}
	if !rep.FullSync {
		t.Fatalf("本地无快照时必须是全量")
	}
	if rep.Upserted != 2 {
		t.Fatalf("首次同步应落 2 条，得到 %d", rep.Upserted)
	}
	if rep.Since == "" {
		t.Fatalf("同步后必须推进 catalog_hash")
	}

	// 无变化时第二次同步应零写入
	rep2, err := c.Sync(ctx)
	if err != nil {
		t.Fatalf("第二次同步失败: %v", err)
	}
	if rep2.Upserted != 0 || rep2.Deleted != 0 {
		t.Fatalf("无变化时增量应为零，得到 %+v", rep2)
	}

	// 新增 + 下架，再同步应精确反映
	newID := b.publish(t, "第三条提示词正文", []string{"c"})
	b.reject(t, a)

	rep3, err := c.Sync(ctx)
	if err != nil {
		t.Fatalf("第三次同步失败: %v", err)
	}
	if rep3.Upserted != 1 {
		t.Fatalf("应只新增 1 条，得到 %d（changed=%v）", rep3.Upserted, rep3.Changed)
	}
	if rep3.Deleted != 1 {
		t.Fatalf("应有 1 条被下架，得到 %d", rep3.Deleted)
	}

	if _, err := c.Cached(ctx, newID); err != nil {
		t.Fatalf("新条目应已落本地: %v", err)
	}
	if _, err := c.Cached(ctx, a); err == nil {
		t.Fatalf("被下架的条目必须从本地移除")
	}
	n, err := c.LocalCount(ctx)
	if err != nil || n != 2 {
		t.Fatalf("本地应有 2 条，得到 %d err=%v", n, err)
	}
}

// TestEndToEndSyncKeepsSinceFixedAcrossPages 盯住一个很容易写错的地方：
// 翻页时若把服务端返回的新 hash 回喂给下一页，服务端会认为客户端已最新
// 而返回空集，导致只同步到第一页。这里用超过一页的量来验证。
func TestEndToEndSyncKeepsSinceFixedAcrossPages(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)

	const total = DefaultPageSize + 7
	want := make([]string, 0, total)
	for i := 0; i < total; i++ {
		want = append(want, b.publish(t, uniqueContent(i), []string{"bulk"}))
	}

	c := b.clientFor(t, "")
	rep, err := c.Sync(ctx)
	if err != nil {
		t.Fatalf("跨页同步失败: %v", err)
	}
	if rep.Pages < 2 {
		t.Fatalf("应至少翻 2 页，实际 %d 页", rep.Pages)
	}
	if rep.Upserted != total {
		t.Fatalf("跨页同步必须落全部 %d 条，实际 %d", total, rep.Upserted)
	}

	n, err := c.LocalCount(ctx)
	if err != nil || n != total {
		t.Fatalf("本地应共 %d 条，得到 %d err=%v", total, n, err)
	}
	for _, id := range want {
		if _, err := c.Cached(ctx, id); err != nil {
			t.Fatalf("末页条目丢失：%s (%v)", id, err)
		}
	}
}

// uniqueContent 生成互不相同的正文，避免被 content_hash 去重。
func uniqueContent(i int) string {
	return "第 " + itoaTest(i) + " 条基准提示词，内容唯一以保证不被去重。"
}

func itoaTest(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

func TestEndToEndCheckStatus(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	_ = b.publish(t, "status probe one", nil)
	c := b.clientFor(t, "")

	st, err := c.CheckStatus(ctx)
	if err != nil {
		t.Fatalf("CheckStatus: %v", err)
	}
	if st.UpToDate {
		t.Fatalf("本地还没同步过，不应判定为已最新")
	}
	if st.ServerTotal != 1 {
		t.Fatalf("服务端总数应为 1，得到 %d", st.ServerTotal)
	}

	if _, err := c.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st2, err := c.CheckStatus(ctx)
	if err != nil {
		t.Fatalf("CheckStatus 2: %v", err)
	}
	if !st2.UpToDate {
		t.Fatalf("同步后应判定为已最新，本地=%s 服务端=%s", st2.CatalogHash, st2.ServerHash)
	}
	if st2.LocalCount != 1 {
		t.Fatalf("本地计数应为 1，得到 %d", st2.LocalCount)
	}
}

func TestEndToEndGetUsesServerETagForReal(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	id := b.publish(t, "etag probe", []string{"x"})
	c := b.clientFor(t, "")

	p, err := c.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p.Content != "etag probe" {
		t.Fatalf("正文不符: %q", p.Content)
	}

	// 第二次应命中真实服务端的 ETag → 304 → 回读本地
	p2, err := c.Get(ctx, id)
	if err != nil {
		t.Fatalf("第二次 Get: %v", err)
	}
	if p2.Content != p.Content || p2.Version != p.Version {
		t.Fatalf("304 回读结果不一致: %+v vs %+v", p2, p)
	}
}

func TestEndToEndWriteFlowWithRealServer(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	b.seedKey(t, "sdk-key", "sdk-secret")
	target := b.publish(t, "scoring target prompt", nil)

	// 用 HMAC 凭据写入：验证 SDK 签名被真实服务端接受
	c := b.clientFor(t, "sdk-secret")

	up, err := c.Upload(ctx, "新上传的提示词正文，带唯一标记 "+b.srv.URL, []string{"new"})
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if up.Status != model.StatusPending {
		t.Fatalf("上传后应为 pending，得到 %s", up.Status)
	}

	// pending 内容不得被随机/列表暴露
	if _, err := c.Get(ctx, up.ID); err == nil {
		t.Fatalf("未过审内容不应可取")
	} else if !IsNotFound(err) {
		t.Fatalf("应返回 not_found，得到 %v", err)
	}

	// 幂等重放同一 clientId
	up2, err := c.UploadWithClientID(ctx, "另一个正文", nil, "replay-cid")
	if err != nil {
		t.Fatalf("首次带 clientId 上传失败: %v", err)
	}
	up3, err := c.UploadWithClientID(ctx, "又一个正文", nil, "replay-cid")
	if err != nil {
		t.Fatalf("重放上传失败: %v", err)
	}
	if up2.ID != up3.ID {
		t.Fatalf("同 clientId 必须幂等返回同一 id：%s vs %s", up2.ID, up3.ID)
	}

	// 打分：同设备覆盖
	r1, err := c.Score(ctx, target, 5)
	if err != nil {
		t.Fatalf("打分失败: %v", err)
	}
	if r1.Count != 1 || r1.Avg != 5 {
		t.Fatalf("首次打分期望 5/1，得到 %+v", r1)
	}
	r2, err := c.Score(ctx, target, 3)
	if err != nil {
		t.Fatalf("第二次打分失败: %v", err)
	}
	if r2.Count != 1 || r2.Avg != 3 {
		t.Fatalf("同设备应覆盖为 3 且仍算 1 票，得到 %+v", r2)
	}
}

func TestEndToEndUnauthorizedWrite(t *testing.T) {
	b := newBackend(t)
	c := b.clientFor(t, "") // 指向真实源站但未登记 key

	_, err := c.Score(context.Background(), "p_any", 5)
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("应返回 *Error，得到 %T (%v)", err, err)
	}
	if e.Code != CodeUnauthorized || e.ExitCode() != ExitAuth {
		t.Fatalf("未登记凭据的写请求应 401，得到 %+v", e)
	}
}

func TestEndToEndWrongSecretIsRejected(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	b.seedKey(t, "sdk-key", "correct-secret")
	id := b.publish(t, "wrong secret target", nil)

	c := b.clientFor(t, "wrong-secret")
	if _, err := c.Score(ctx, id, 5); err == nil {
		t.Fatalf("错误 secret 必须被真实服务端拒绝")
	} else if !strings.Contains(err.Error(), CodeUnauthorized) {
		t.Fatalf("应返回 unauthorized，得到 %v", err)
	}
}

func TestEndToEndListAndRandom(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	one := b.publish(t, "listable one", []string{"keep"})
	two := b.publish(t, "listable two", []string{"keep"})
	c := b.clientFor(t, "")

	page, err := c.List(ctx, "keep", 10, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("应有 2 条摘要，得到 %d", len(page.Items))
	}
	for _, it := range page.Items {
		if it.ID != one && it.ID != two {
			t.Fatalf("出现了意外条目 %s", it.ID)
		}
	}

	p, err := c.Random(ctx, "", []string{one})
	if err != nil {
		t.Fatalf("Random: %v", err)
	}
	if p.ID != two {
		t.Fatalf("exclude 后只可能抽到 %s，得到 %s", two, p.ID)
	}
}
