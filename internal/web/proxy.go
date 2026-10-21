package web

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// TrustPrivate is the keyword standing for privateNetworks inside TrustedProxies
const TrustPrivate = "private"

// privateNetworks are the ranges a private in TrustedProxies expands to: loopback, the RFC 1918
// private ranges, carrier-grade NAT (which some CNIs and cloud providers use as the Pod range), and
// IPv6's loopback and ULA
var privateNetworks = []string{
	"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10",
	"::1/128", "fc00::/7",
}

// ExpandProxies expands a private in TrustedProxies into ranges and keeps everything else as it
// is.
//
// It is what goes into the framework's own proxy settings (gin's SetTrustedProxies): the framework
// does not know the word private
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

// ValidateProxies checks that every entry of TrustedProxies is an IP, a range or private.
//
// A malformed range keeps the service from starting. gin parses until it hits an error and keeps
// whatever it parsed so far, so the first half of the proxies is trusted and the second half
// quietly dropped -- the client_ip in the logs is half real and half not, a state far harder to
// track down than a service that does not start.
//
// The IPv4-mapped IPv6 spellings (::ffff:10.0.0.1, ::ffff:10.0.0.0/104) are caught as well, with
// the error saying how to write it: one line of configuration is understood differently in
// different places. gin v1.12.0 was measured reading a single ::ffff:10.0.0.1 wrong -- it trusts
// addresses such as ::1 and ::5 while not trusting 10.0.0.1 itself; gin reads the range spelling
// right, but ParseProxies only Unmaps a single IP and not a range, leaving client_ip and the trust
// decision for forwarded headers out of step
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

// unmapped returns p's IPv4 spelling when p is an IPv4-mapped IPv6 one (with a range's prefix
// length reduced by 96).
//
// A range whose prefix is shorter than 96 bits covers more than the mapped segment, and what is
// given for it is its normalized IPv6 spelling
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

// isIPOrCIDR reports whether a string is a valid IP or range, accepting what gin accepts
func isIPOrCIDR(s string) bool {
	if strings.Contains(s, "/") {
		_, _, err := net.ParseCIDR(s)
		return err == nil
	}
	return net.ParseIP(s) != nil
}

// Proxies are the trusted ranges once parsed, see ParseProxies
type Proxies []netip.Prefix

// ParseProxies parses TrustedProxies (private included) into ranges, taking a single IP as the
// range holding it alone.
// A malformed one was caught by ValidateProxies already, and anything that cannot be parsed here is
// skipped -- what cannot be told is not trusted
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

// Trusts reports whether ip falls into any of the ranges. ip is an address without a port, and one
// that cannot be parsed is never trusted.
//
// What has to be asked about is the directly connected peer (RemoteAddr), not the client IP the
// framework computed: the latter is derived from exactly the forgeable headers such as
// X-Forwarded-For.
//
// Unmap is there so that an IPv4 written as ::ffff:10.0.0.1 matches 10.0.0.0/8 too
func (ps Proxies) Trusts(ip string) bool {
	a, err := netip.ParseAddr(ip)
	return err == nil && ps.has(a)
}

// has reports whether a falls into any of the ranges
func (ps Proxies) has(a netip.Addr) bool {
	a = a.Unmap()
	return slices.ContainsFunc(ps, func(p netip.Prefix) bool { return p.Contains(a) })
}

// RemoteIP is the directly connected peer's address (RemoteAddr) without the port, or an empty
// string when it cannot be parsed.
//
// It is what "is this trusted" has to be asked about, see Trusts
func RemoteIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return ""
	}
	return ip
}

// forwardedHeaders are the request headers looked at, in order, when computing the client IP
var forwardedHeaders = []string{"X-Forwarded-For", "X-Real-IP"}

// ClientIP is the client_ip in the access log: with the directly connected peer trusted it walks
// back through X-Forwarded-For / X-Real-IP, and otherwise it is the peer itself.
//
// The rules match gin v1.12.0's Context.ClientIP word for word (xgin uses gin's own):
//
//   - the directly connected peer is not trusted: it is the peer's address and no forwarded header
//     is looked at;
//   - it is trusted: X-Forwarded-For first, X-Real-IP second. Several lines of the same header are
//     joined into one list, and the first untrusted address is looked for right to left -- every
//     proxy hop appends on the right, so the rightmost untrusted one is the hop closest to us that
//     cannot be walked back any further; with all of them trusted the leftmost is taken; an entry
//     that cannot be parsed abandons this header;
//   - neither header holds anything usable: it falls back to the peer's address.
//
// That the two agree is pinned by xgin's tests (comparing against gin's result case by case): the
// same service on another framework must not change client_ip.
// A peer that cannot be parsed returns an empty string, as gin does
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

// fromHeader looks for the client IP inside one forwarded header; the rules are in ClientIP
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

// contains is Trusts, taking an address that is already parsed
func (ps Proxies) contains(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	return ok && ps.has(a)
}
