package api

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
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

// BenchmarkIPResolverTrustedScan 量出“把 CDN 网段整体塞进 trusted_proxies”的代价。
// 每请求要查 2 次（直连对端 + 链上每一跳），所以这里跑 2 次查找。
func BenchmarkIPResolverTrustedScan(b *testing.B) {
	cidrs := make([]string, 0, 232)
	for i := 0; i < 144; i++ {
		cidrs = append(cidrs, netipCIDRv4(i))
	}
	for i := 0; i < 88; i++ {
		cidrs = append(cidrs, netipCIDRv6(i))
	}
	r, err := newIPResolver(cidrs)
	if err != nil {
		b.Fatalf("构建失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/prompts/random", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Forwarded-For", "2001:db8:1::5") // 末跳非可信，触发整表扫描
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.ip(req)
		_ = r.ip(req)
	}
}

// 基线：默认仅回环两项。
func BenchmarkIPResolverDefaultTwoEntries(b *testing.B) {
	r, err := newIPResolver(nil)
	if err != nil {
		b.Fatalf("构建失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/prompts/random", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.ip(req)
		_ = r.ip(req)
	}
}

func netipCIDRv4(i int) string {
	return fmt.Sprintf("10.%d.%d.0/24", i%256, (i*7)%256)
}

func netipCIDRv6(i int) string {
	return fmt.Sprintf("2001:db8:%x::/64", i)
}

// TestTrustedProxiesCDNFragment 钉住交付的 CDN 网段片段：
// 文件必须能被解析，且链上行为符合“继续左移到真实客户端”的目的。
func TestTrustedProxiesCDNFragment(t *testing.T) {
	const path = "../../deploy/trusted-proxies.cdn.yaml"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败（该文件是交付件，缺失即失败而不是跳过）: %v", path, err)
	}

	var cidrs []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `- "`) {
			continue
		}
		rest := line[3:]
		q := strings.IndexByte(rest, '"') // 行尾可能还有注释，必须到闭合引号为止
		if q < 0 {
			t.Fatalf("片段里有未闭合引号的条目: %q", line)
		}
		if v := rest[:q]; v != "" {
			cidrs = append(cidrs, v)
		}
	}
	if len(cidrs) < 100 {
		t.Fatalf("片段只解析出 %d 条，远少于预期的 234 条（解析逻辑或文件被截断？）", len(cidrs))
	}
	r, err := newIPResolver(cidrs)
	if err != nil {
		t.Fatalf("片段里有非法网段: %v", err)
	}

	// 回环两项必须在：少了它，同机 nginx 那一跳就不被信任，整表等于白配。
	if !r.isTrusted(net.ParseIP("127.0.0.1")) || !r.isTrusted(net.ParseIP("::1")) {
		t.Fatal("片段丢了回环项 —— 同机 nginx 会不被信任")
	}

	req := func(peer, xff string) *http.Request {
		q := httptest.NewRequest(http.MethodGet, "/v1/prompts/random", nil)
		q.RemoteAddr = peer
		q.Header.Set("X-Forwarded-For", xff)
		return q
	}

	// 178.236.38.0/23 来自清单里两个满 /24（178.236.38.0/24 与 178.236.39.0/24）。
	chain := req("127.0.0.1:50000", "203.0.113.200, 178.236.38.77")
	if got := r.ip(chain); got != "203.0.113.200" {
		t.Fatalf("CDN 节点在链上时应继续左移取真实客户端，实际取到 %q", got)
	}

	// 未列出的中间跳不是可信网段，必须就地停住 —— 这证明没有过度信任。
	unknown := req("127.0.0.1:50000", "203.0.113.200, 203.0.113.5")
	if got := r.ip(unknown); got != "203.0.113.5" {
		t.Fatalf("未列出的中间跳被当成可信了？实际取到 %q", got)
	}

	// 精确覆盖语义的抽查：清单里出现过的地址一定在网段内。
	for _, s := range []string{"178.236.39.199", "103.115.48.192", "2406:da14:1443:3500:50a1:cd01:7ebb:4377"} {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("测试数据里的地址 %q 本身非法", s)
		}
		if !r.isTrusted(ip) {
			t.Fatalf("清单里的节点 %q 不在任何交付网段内（聚合漏了？）", s)
		}
	}
}
