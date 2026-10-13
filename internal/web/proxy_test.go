package web

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestExpandProxies_PrivateExpandsOthersKept(t *testing.T) {
	got := ExpandProxies([]string{TrustPrivate, "203.0.113.0/24"})
	want := append(slices.Clone(privateNetworks), "203.0.113.0/24")
	if !slices.Equal(got, want) {
		t.Errorf("got=%v want=%v", got, want)
	}
	if got := ExpandProxies(nil); len(got) != 0 {
		t.Errorf("empty list should stay empty, got=%v", got)
	}
}

func TestProxies_Trusts(t *testing.T) {
	ps := ParseProxies([]string{TrustPrivate, "203.0.113.7", "not-an-ip"})
	for ip, want := range map[string]bool{
		"10.1.2.3":        true,
		"::ffff:10.1.2.3": true, // IPv4 映射成 IPv6 的写法也认
		"100.64.0.1":      true, // 运营商级 NAT
		"fd00::1":         true, // ULA
		"203.0.113.7":     true, // 单个 IP 当作只含它自己的网段
		"203.0.113.8":     false,
		"8.8.8.8":         false,
		"":                false, // 解不出来的一律不信
		"10.1.2.3:8080":   false, // 带端口的不是地址
	} {
		if got := ps.Trusts(ip); got != want {
			t.Errorf("Trusts(%q)=%v want %v", ip, got, want)
		}
	}
	if ParseProxies(nil).Trusts("127.0.0.1") {
		t.Error("an empty list trusts nobody")
	}
}

func TestValidateProxies(t *testing.T) {
	if err := ValidateProxies([]string{TrustPrivate, "10.0.0.0/8", "::1", "203.0.113.7"}); err != nil {
		t.Errorf("valid list rejected: %v", err)
	}
	err := ValidateProxies([]string{"10.0.0.0/8", "10.0.0.0/33"})
	if err == nil || !strings.Contains(err.Error(), `got="10.0.0.0/33"`) {
		t.Errorf("an invalid CIDR must be reported by name, got=%v", err)
	}
}

func TestProxies_ClientIP(t *testing.T) {
	ps := ParseProxies([]string{TrustPrivate})
	for _, c := range []struct {
		name, remote, xff, xrip, want string
	}{
		{"public peer spoofing XFF", "203.0.113.9:1234", "1.2.3.4", "", "203.0.113.9"},
		{"public peer spoofing X-Real-IP", "203.0.113.9:1234", "", "1.2.3.4", "203.0.113.9"},
		{"trusted peer", "10.0.0.5:1234", "1.2.3.4", "", "1.2.3.4"},
		{"rightmost untrusted wins", "10.0.0.5:1234", "1.2.3.4, 5.6.7.8, 10.0.0.9", "", "5.6.7.8"},
		{"all trusted takes leftmost", "10.0.0.5:1234", "10.0.0.7, 10.0.0.8", "", "10.0.0.7"},
		{"garbage stops the header, falls back to X-Real-IP", "10.0.0.5:1234", "junk", "9.9.9.9", "9.9.9.9"},
		{"nothing usable falls back to peer", "10.0.0.5:1234", "junk", "", "10.0.0.5"},
		{"garbage stops the walk before further-left entries", "10.0.0.5:1234", "1.2.3.4, junk", "", "10.0.0.5"},
		{"entries are trimmed", "10.0.0.5:1234", " 1.2.3.4 , 10.0.0.9 ", "", "1.2.3.4"},
		{"v4-mapped peer is normalized", "[::ffff:10.0.0.5]:1", "", "", "10.0.0.5"},
		{"unparsable peer", "nonsense", "1.2.3.4", "", ""},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.xrip != "" {
			r.Header.Set("X-Real-IP", c.xrip)
		}
		if got := ps.ClientIP(r); got != c.want {
			t.Errorf("%s: ClientIP=%q want %q", c.name, got, c.want)
		}
	}

	// 同名的头有好几行时拼成一个列表
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:1"
	r.Header.Add("X-Forwarded-For", "1.2.3.4")
	r.Header.Add("X-Forwarded-For", "5.6.7.8")
	if got := ps.ClientIP(r); got != "5.6.7.8" {
		t.Errorf("multiple header lines should be joined, got=%q", got)
	}
	if got := ParseProxies(nil).ClientIP(r); got != "10.0.0.5" {
		t.Errorf("an empty list trusts nobody, got=%q", got)
	}
}

func TestValidateProxies_RejectsIPv4MappedEntries(t *testing.T) {
	// gin v1.12.0 把单个的 ::ffff:10.0.0.1 解成了信任 ::1、::5 这类地址、偏偏不信 10.0.0.1；
	// 网段写法 gin 认对了，本包的 ParseProxies 却不 Unmap 网段。同一行配置两处判断不一样，所以直接拒掉
	for entry, suggest := range map[string]string{
		"::ffff:10.0.0.1":     "10.0.0.1",
		"::ffff:a00:1":        "10.0.0.1",
		"::ffff:10.0.0.0/104": "10.0.0.0/8",
		"::ffff:10.1.2.3/128": "10.1.2.3/32",
		"::ffff:0:0/96":       "0.0.0.0/0",
	} {
		err := ValidateProxies([]string{TrustPrivate, entry})
		if err == nil {
			t.Errorf("%q should be rejected", entry)
			continue
		}
		if !strings.Contains(err.Error(), "IPv4-mapped") || !strings.Contains(err.Error(), "write it as "+suggest) {
			t.Errorf("%q: error should suggest %q, got %v", entry, suggest, err)
		}
	}
	for _, ok := range []string{"10.0.0.1", "10.0.0.0/8", "::1", "fd00::/8", "2001:db8::/32", "::/0", TrustPrivate} {
		if err := ValidateProxies([]string{ok}); err != nil {
			t.Errorf("%q should be accepted, got %v", ok, err)
		}
	}
}
