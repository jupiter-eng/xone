package web

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// TrustPrivate TrustedProxies 里代表 privateNetworks 的关键字
const TrustPrivate = "private"

// privateNetworks TrustedProxies 里写 private 时展开成的网段：
// 回环、RFC 1918 私有网段、运营商级 NAT（有的 CNI 和云厂商拿它当 Pod 网段）、IPv6 的回环和 ULA
var privateNetworks = []string{
	"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10",
	"::1/128", "fc00::/7",
}

// ExpandProxies 把 TrustedProxies 里的 private 展开成网段，其余原样保留。
//
// 交给框架自己的代理设置（gin 的 SetTrustedProxies）用的是它：框架不认识 private 这个词
func ExpandProxies(list []string) []string {
	out := make([]string, 0, len(list))
	for _, p := range list {
		if p == TrustPrivate {
			out = append(out, privateNetworks...)
			continue
		}
		out = append(out, p)
	}
	return out
}

// ValidateProxies 检查 TrustedProxies 的每一项都是 IP、网段或 private。
//
// 网段写错了就直接起不来。gin 那边的行为是解析到出错为止、把已经解出来的
// 留下，于是前半段代理被信任、后半段被悄悄丢掉——日志里的 client_ip
// 一半真一半假，是比起不来难查得多的状态。
//
// IPv4 映射成 IPv6 的写法（::ffff:10.0.0.1、::ffff:10.0.0.0/104）也拦住，报错里给出该怎么写：
// 同一行配置各处的理解不一样。实测 gin v1.12.0 把单个的 ::ffff:10.0.0.1 解歪了——
// 信任的是 ::1、::5 这类地址，10.0.0.1 本身反而不信；网段写法 gin 解对了，
// ParseProxies 却只 Unmap 单个 IP、不 Unmap 网段，于是 client_ip 和透传 Header 的可信判断对不上
func ValidateProxies(list []string) error {
	for _, p := range list {
		if p != TrustPrivate && !isIPOrCIDR(p) {
			return fmt.Errorf("TrustedProxies contains an invalid address, want an IP, a CIDR or %q, got=%q", TrustPrivate, p)
		}
		if plain, ok := unmapped(p); ok {
			return fmt.Errorf("TrustedProxies entry %q is an IPv4-mapped IPv6 address; write it as %s", p, plain)
		}
	}
	return nil
}

// unmapped p 是 IPv4 映射成 IPv6 的写法时，返回它的 IPv4 写法（网段的前缀长度减掉 96）。
//
// 前缀短于 96 位的网段已经不只是映射地址那一段了，给的是它规范化之后的 IPv6 写法
func unmapped(p string) (string, bool) {
	if pfx, err := netip.ParsePrefix(p); err == nil {
		if !pfx.Addr().Is4In6() {
			return "", false
		}
		if pfx.Bits() < 96 {
			return pfx.Masked().String(), true
		}
		return netip.PrefixFrom(pfx.Addr().Unmap(), pfx.Bits()-96).Masked().String(), true
	}
	if a, err := netip.ParseAddr(p); err == nil && a.Is4In6() {
		return a.Unmap().String(), true
	}
	return "", false
}

// isIPOrCIDR 判断一段是不是合法的 IP 或者网段，与 gin 接受的写法一致
func isIPOrCIDR(s string) bool {
	if strings.Contains(s, "/") {
		_, _, err := net.ParseCIDR(s)
		return err == nil
	}
	return net.ParseIP(s) != nil
}

// Proxies 解好的可信网段，见 ParseProxies
type Proxies []netip.Prefix

// ParseProxies 把 TrustedProxies（可以含 private）解成网段，单个 IP 当作只含它自己的网段。
// 写错的在 ValidateProxies 里就拦下了，这里解不出的直接跳过——拿不准就不信
func ParseProxies(list []string) Proxies {
	expanded := ExpandProxies(list)
	out := make(Proxies, 0, len(expanded))
	for _, s := range expanded {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
		} else if a, err := netip.ParseAddr(s); err == nil {
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out
}

// Trusts ip 是否落在任一网段里。ip 是不带端口的地址，解不出来的一律不信。
//
// 该问的是直连的对端（RemoteAddr），不是框架算出来的 client IP：后者正是从
// X-Forwarded-For 这些可伪造的头里推出来的。
//
// Unmap 是为了 ::ffff:10.0.0.1 这种写法的 IPv4 也能对上 10.0.0.0/8
func (ps Proxies) Trusts(ip string) bool {
	a, err := netip.ParseAddr(ip)
	return err == nil && ps.has(a)
}

// has a 是否落在任一网段里
func (ps Proxies) has(a netip.Addr) bool {
	a = a.Unmap()
	return slices.ContainsFunc(ps, func(p netip.Prefix) bool { return p.Contains(a) })
}

// RemoteIP 直连对端（RemoteAddr）的地址，不带端口；解不出来是空串。
//
// 该问「可不可信」的就是它，见 Trusts
func RemoteIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return ""
	}
	return ip
}

// forwardedHeaders 算 client IP 时按顺序看的请求头
var forwardedHeaders = []string{"X-Forwarded-For", "X-Real-IP"}

// ClientIP 访问日志里的 client_ip：直连对端可信时从 X-Forwarded-For / X-Real-IP 里往回找，否则就是对端本身。
//
// 规则和 gin v1.12.0 的 Context.ClientIP 一字不差（xgin 用的是 gin 自己的那个）：
//
//   - 直连对端不可信：就是对端地址，转发头一概不看；
//   - 可信：先看 X-Forwarded-For，再看 X-Real-IP。同名的头有好几行时拼成一个列表，
//     从右往左找第一个不可信的地址——每一跳代理都往右边追加，最右边那个不可信的
//     就是离我们最近、没法再往回追的那一跳；全都可信时取最左边的；
//     碰到解不出的一项就放弃这个头；
//   - 两个头都没有可用的值：退回对端地址。
//
// 两边一致是 xgin 的测试钉着的（逐条比对 gin 的结果）：同一个服务换一个框架，client_ip 不该变。
// 对端解不出来时返回空串，与 gin 相同
func (ps Proxies) ClientIP(r *http.Request) string {
	remote := net.ParseIP(RemoteIP(r))
	if remote == nil {
		return ""
	}
	if ps.contains(remote) {
		for _, name := range forwardedHeaders {
			if ip, ok := ps.fromHeader(strings.Join(r.Header.Values(name), ",")); ok {
				return ip
			}
		}
	}
	return remote.String()
}

// fromHeader 从一个转发头里找 client IP，规则见 ClientIP
func (ps Proxies) fromHeader(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	items := strings.Split(header, ",")
	for i := len(items) - 1; i >= 0; i-- {
		s := strings.TrimSpace(items[i])
		ip := net.ParseIP(s)
		if ip == nil {
			break
		}
		if i == 0 || !ps.contains(ip) {
			return s, true
		}
	}
	return "", false
}

// contains 同 Trusts，收的是解好的地址
func (ps Proxies) contains(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	return ok && ps.has(a)
}
