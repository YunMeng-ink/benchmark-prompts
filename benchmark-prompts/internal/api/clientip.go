package api

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// clientip.go 负责“这个请求真正的客户端地址是什么”。
//
// 为什么不能直接信 X-Forwarded-For：限流主体与审计日志的 ip 字段都取自它。
// 若对任意对端都采信该头，客户端自己加一个 X-Forwarded-For: 1.2.3.<随机>
// 就能每次都换一个新限流身份，逐 IP 限流形同虚设，审计也被洗白。
//
// 规则：只有**直连对端**落在可信代理网段内时才采信转发头；采信时从右往左
// 取第一个非可信跳（右侧是我们自己的代理追加的，左侧仍可能被伪造）。

// defaultTrustedProxies 是未配置时的可信网段：仅回环。
// 典型部署（nginx 与源站同机）下这就够用，而远端客户端无法伪造回环直连。
var defaultTrustedProxies = []string{"127.0.0.0/8", "::1/128"}

// ipResolver 按可信代理网段解析客户端地址。nil 等价于“什么都不采信”。
type ipResolver struct {
	trusted []*net.IPNet
}

// newIPResolver 解析 CIDR 列表；列表为空时使用默认值（仅回环）。
// 非法 CIDR 直接报错，不静默忽略——配错等于把限流敞开。
func newIPResolver(cidrs []string) (*ipResolver, error) {
	if len(cidrs) == 0 {
		cidrs = defaultTrustedProxies
	}
	r := &ipResolver{}
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(c); err == nil {
			r.trusted = append(r.trusted, n)
			continue
		}
		// 允许写单个 IP，按 /32 或 /128 处理。
		if ip := net.ParseIP(c); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			r.trusted = append(r.trusted, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits*8)})
			continue
		}
		return nil, fmt.Errorf("server.trusted_proxies 含非法网段 %q", c)
	}
	return r, nil
}

func (r *ipResolver) isTrusted(ip net.IP) bool {
	if r == nil || ip == nil {
		return false
	}
	for _, n := range r.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// hostOnlyIP 取 RemoteAddr 的 IP 部分；取不到返回 nil。
func hostOnlyIP(remoteAddr string) net.IP {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	} else if i := strings.LastIndexByte(remoteAddr, ':'); i >= 0 {
		host = remoteAddr[:i] // IPv6 无方括号时的退化情况
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}

// ip 返回该请求的客户端地址。对端不可信时，任何转发头都不采信。
func (r *ipResolver) ip(req *http.Request) string {
	direct := hostOnlyIP(req.RemoteAddr)
	if !r.isTrusted(direct) {
		if direct == nil {
			return strings.TrimSpace(req.RemoteAddr)
		}
		return direct.String()
	}

	// 直连对端可信：先看 XFF（链），再看 X-Real-IP（nginx 常见单值）。
	hops := splitForwarded(req.Header.Get("X-Forwarded-For"))
	if len(hops) == 0 {
		if v := parseIP(req.Header.Get("X-Real-IP")); v != nil {
			return v.String()
		}
		return direct.String()
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if !r.isTrusted(hops[i]) {
			return hops[i].String()
		}
	}
	// 整条链都是可信地址（内网探针、健康检查等）：取最左，避免全落成回环。
	return hops[0].String()
}

// splitForwarded 解析逗号分隔的转发链，逐项 ParseIP，非法项丢弃。
func splitForwarded(h string) []net.IP {
	if strings.TrimSpace(h) == "" {
		return nil
	}
	parts := strings.Split(h, ",")
	out := make([]net.IP, 0, len(parts))
	for _, p := range parts {
		if ip := parseIP(p); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

func parseIP(s string) net.IP {
	return net.ParseIP(strings.TrimSpace(s))
}
