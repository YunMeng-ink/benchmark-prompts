package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/example/benchmark-prompts/internal/config"
)

// TestIPResolver 逐条钉住“什么时候才采信转发头”。
func TestIPResolver(t *testing.T) {
	// 默认值：仅回环可信。
	def, err := newIPResolver(nil)
	if err != nil {
		t.Fatalf("默认解析器构建失败: %v", err)
	}
	// 显式配了公网段 —— 语义是整体替换，回环不再可信。
	onlyPub, err := newIPResolver([]string{"198.51.100.0/24"})
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}

	req := func(peer, xff, real string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/prompts/random", nil)
		r.RemoteAddr = peer
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		if real != "" {
			r.Header.Set("X-Real-IP", real)
		}
		return r
	}

	cases := []struct {
		name string
		res  *ipResolver
		req  *http.Request
		want string
	}{
		{"对端不可信 + 伪造单跳 → 忽略", def,
			req("203.0.113.7:40000", "1.1.1.1", ""), "203.0.113.7"},
		{"对端不可信 + 伪造 X-Real-IP → 忽略", def,
			req("203.0.113.7:40000", "", "1.1.1.1"), "203.0.113.7"},
		{"对端是回环 + 单跳 → 采信", def,
			req("127.0.0.1:50000", "203.0.113.9", ""), "203.0.113.9"},
		{"链：取最右的非可信跳（左侧是客户端自带的伪造值）", def,
			req("127.0.0.1:50000", "1.1.1.1, 203.0.113.10", ""), "203.0.113.10"},
		{"链尾还有一层可信代理 → 继续左移", mustResolver(t, "127.0.0.0/8", "10.0.0.0/8"),
			req("127.0.0.1:50000", "1.1.1.1, 10.0.0.5, 203.0.113.11", ""), "203.0.113.11"},
		{"整条链都可信 → 取最左，不落回环", def,
			req("127.0.0.1:50000", "127.0.0.2, 127.0.0.3", ""), "127.0.0.2"},
		{"可信对端但无转发头 → 用直连", def,
			req("127.0.0.1:50000", "", ""), "127.0.0.1"},
		{"X-Real-IP 兜底（nginx 常见单值配置）", def,
			req("127.0.0.1:50000", "", "203.0.113.12"), "203.0.113.12"},
		{"转发头是垃圾 → 退回直连", def,
			req("127.0.0.1:50000", "not-an-ip, also-bad", ""), "127.0.0.1"},
		{"IPv6 回环直连 + 转发", def,
			req("[::1]:50000", "2001:db8::5", ""), "2001:db8::5"},
		{"配了公网段之后，回环反而不可信（整体替换语义）", onlyPub,
			req("127.0.0.1:50000", "203.0.113.13", ""), "127.0.0.1"},
		{"公网段内的对端 → 采信", onlyPub,
			req("198.51.100.9:50000", "203.0.113.14", ""), "203.0.113.14"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.res.ip(c.req); got != c.want {
				t.Fatalf("客户端地址 = %q，期望 %q", got, c.want)
			}
		})
	}
}

func mustResolver(t *testing.T, cidrs ...string) *ipResolver {
	t.Helper()
	r, err := newIPResolver(cidrs)
	if err != nil {
		t.Fatalf("构建解析器失败: %v", err)
	}
	return r
}

// TestIPResolverRejectsBadCIDR：配错必须报错，不能静默变成“什么都信”。
func TestIPResolverRejectsBadCIDR(t *testing.T) {
	if _, err := newIPResolver([]string{"10.0.0.0/33"}); err == nil {
		t.Fatal("非法网段 10.0.0.0/33 竟然通过")
	}
	if _, err := newIPResolver([]string{"nginx"}); err == nil {
		t.Fatal("非地址值竟然通过")
	}
}

// TestForgedForwardedHeaderCannotEvadeLimit 是这条修复的攻防测试：
// 同一个不可信对端轮换伪造 X-Forwarded-For，必须共用同一个限流桶。
// 修复前每次请求都会换一个限流身份，本测试会全绿放行。
func TestForgedForwardedHeaderCannotEvadeLimit(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) {
		c.RateLimit.Anonymous["random"] = 3 // 不把测试绑在生产配额上
	})
	h.publish("伪造头绕限流测试正文", []string{"sec"})

	handler := h.srv.Routes()
	var gotLimited bool
	for i := range 6 {
		r := httptest.NewRequest(http.MethodGet, "/v1/prompts/random", nil)
		r.RemoteAddr = "203.0.113.77:40000" // 直连对端不可信
		r.Header.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(i))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			gotLimited = true
		}
	}
	if !gotLimited {
		t.Fatal("伪造 X-Forwarded-For 绕开了逐 IP 限流：不可信对端的转发头必须被忽略")
	}
}

// TestRealClientsBehindTrustedProxyAreSeparate 是反方向的钉：
// 可信代理后面不同真实客户端不能被合并成同一个桶。
func TestRealClientsBehindTrustedProxyAreSeparate(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) {
		c.RateLimit.Anonymous["random"] = 3
	})
	h.publish("同机代理后面的真实客户端正文", []string{"sec"})

	handler := h.srv.Routes()
	for i := range 6 {
		r := httptest.NewRequest(http.MethodGet, "/v1/prompts/random", nil)
		r.RemoteAddr = "127.0.0.1:50000" // 回环 = nginx 同机
		r.Header.Set("X-Forwarded-For", "198.51.100.5"+strconv.Itoa(i))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次就限流了：可信代理后面的不同客户端被并成了同一个桶", i+1)
		}
	}
}
