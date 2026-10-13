package xtrace

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/propagation"
)

func mustProp(t *testing.T, global []string, rules []ForwardHeaderRule) *HeaderPropagator {
	t.Helper()
	p, err := NewHeaderPropagator(global, rules)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	return p
}

// fromTrusted 声明了「对端可信」的 carrier，扮演 xgin 按 TrustedProxies 判过之后交来的那个
type fromTrusted struct{ propagation.HeaderCarrier }

func (fromTrusted) TrustedPeer() bool { return true }

// trusted 把请求头包成可信对端发来的 carrier
func trusted(h http.Header) propagation.TextMapCarrier {
	return fromTrusted{propagation.HeaderCarrier(h)}
}

// roundTrip 模拟一次「从可信的上游收，向下游发」：Extract 再 Inject，返回下游收到的 header
func roundTrip(p *HeaderPropagator, in http.Header, targetHost string) http.Header {
	ctx := p.Extract(context.Background(), trusted(in))
	if targetHost != "" {
		ctx = WithTargetHost(ctx, targetHost)
	}
	out := http.Header{}
	p.Inject(ctx, propagation.HeaderCarrier(out))
	return out
}

func TestHeaderPropagator_GlobalForwarding(t *testing.T) {
	p := mustProp(t, []string{"X-Request-Id", "X-Tenant-Id"}, nil)
	in := http.Header{"X-Request-Id": {"r1"}, "X-Tenant-Id": {"t1"}, "X-Other": {"o"}}

	out := roundTrip(p, in, "any.example.com")
	if out.Get("X-Request-Id") != "r1" || out.Get("X-Tenant-Id") != "t1" {
		t.Errorf("配置了的应当透传，got=%v", out)
	}
	if out.Get("X-Other") != "" {
		t.Errorf("没配置的不该透传，got=%v", out)
	}
}

// declared 自己说明可不可信的 carrier
type declared struct {
	propagation.HeaderCarrier
	ok bool
}

func (d declared) TrustedPeer() bool { return d.ok }

func TestHeaderPropagator_AcceptsOnlyFromTrustedPeers(t *testing.T) {
	// 回归用例。透传的值原先从任何入站请求里都照单全收：公网客户端发一个
	// X-Tenant-Id / X-Internal-Token，就被当成「上游给的」带进了内网的每一次调用。
	// 没有 TrustedPeer() 的 carrier（otelhttp 的 handler、业务自己拼的）一律不收
	p := mustProp(t, []string{"X-Tenant-Id"}, []ForwardHeaderRule{
		{Domains: []string{"*.internal.com"}, Headers: []string{"X-Internal-Token"}},
	})
	in := http.Header{"X-Tenant-Id": {"forged"}, "X-Internal-Token": {"forged"}}

	for name, c := range map[string]struct {
		carrier propagation.TextMapCarrier
		want    bool
	}{
		"普通的 HeaderCarrier": {propagation.HeaderCarrier(in), false},
		"声明了不可信":            {declared{propagation.HeaderCarrier(in), false}, false},
		"声明了可信":             {declared{propagation.HeaderCarrier(in), true}, true},
	} {
		ctx := WithTargetHost(p.Extract(context.Background(), c.carrier), "api.internal.com")
		out := http.Header{}
		p.Inject(ctx, propagation.HeaderCarrier(out))
		for _, h := range []string{"X-Tenant-Id", "X-Internal-Token"} {
			if got := out.Get(h) != ""; got != c.want {
				t.Errorf("%s：%s 透传=%v，want %v", name, h, got, c.want)
			}
		}
	}
}

func TestHeaderPropagator_WarnsOnceForUntrustedPeerHeaders(t *testing.T) {
	// 「配了透传却什么都没传」时这是唯一的线索；但公网上谁都能发这些头，
	// 每次都打就成了一个刷日志的入口
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	p := mustProp(t, []string{"X-Tenant-Id"}, nil)
	p.Extract(context.Background(), propagation.HeaderCarrier(http.Header{"X-Other": {"x"}}))
	if buf.Len() != 0 {
		t.Fatalf("没带透传头就不该告警，got=%s", buf.String())
	}
	for i := 0; i < 3; i++ {
		p.Extract(context.Background(), propagation.HeaderCarrier(http.Header{"X-Tenant-Id": {"forged"}}))
	}
	if n := strings.Count(buf.String(), "untrusted peer"); n != 1 {
		t.Errorf("该只告警一次，got=%d 次：%s", n, buf.String())
	}
	// 提示不能只点 XGin：只用 xecho 的进程照着加一个 XGin 块，启动就报「没人读的配置」
	if want := "xtrace ignored forward headers from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted"; !strings.Contains(buf.String(), want) {
		t.Errorf("告警该写成 %q，got=%s", want, buf.String())
	}
}

func TestHeaderPropagator_GlobalForwardingIgnoresDomain(t *testing.T) {
	// 全局就是全局：连目标域名都不知道时也要发出去
	p := mustProp(t, []string{"X-Request-Id"}, nil)
	out := roundTrip(p, http.Header{"X-Request-Id": {"r1"}}, "")
	if out.Get("X-Request-Id") != "r1" {
		t.Errorf("没有目标域名也该透传全局 header，got=%v", out)
	}
}

func TestHeaderPropagator_DomainRules(t *testing.T) {
	// 内部标识只发给自己人，不发给第三方——这是这条规则存在的全部理由
	p := mustProp(t, nil, []ForwardHeaderRule{
		{Domains: []string{"api.internal.com", "*.trusted.com"}, Headers: []string{"X-Internal-Token"}},
	})
	in := http.Header{"X-Internal-Token": {"secret"}}

	for _, c := range []struct {
		host string
		want bool
	}{
		{"api.internal.com", true},
		{"api.internal.com:8080", true}, // 带端口
		{"API.Internal.com", true},      // 大小写不敏感
		{"sub.trusted.com", true},       // 通配命中
		{"a.b.trusted.com", true},       // 多级子域
		{"trusted.com", false},          // *.trusted.com 不匹配裸域
		{"nottrusted.com", false},       // 不是后缀就是不是
		{"eviltrusted.com", false},      // 后缀相似但不是子域
		{"third-party.com", false},      // 第三方
		{"", false},                     // 不知道发给谁，宁可不发
	} {
		out := roundTrip(p, in, c.host)
		if got := out.Get("X-Internal-Token") != ""; got != c.want {
			t.Errorf("host=%q 应当透传=%v，实际=%v", c.host, c.want, got)
		}
	}
}

func TestNewHeaderPropagator_OnlyLeadingStarDotWildcard(t *testing.T) {
	// 回归用例。原先只是把开头的 * 切掉再做后缀匹配，于是 *trusted.com
	// 也能匹配 eviltrusted.com——内部令牌发给了一个谁都能注册的域名。
	// 能写对的只有 *.example.com 这一种，其余带 * 的写法一律在启动时拦下
	for _, d := range []string{"*trusted.com", "a.*.com", "*", "*.", "**.a.com", "*.*.a.com", "api.*"} {
		_, err := NewHeaderPropagator(nil, []ForwardHeaderRule{{Domains: []string{d}, Headers: []string{"X-Internal-Token"}}})
		if err == nil {
			t.Errorf("Domains=%q 应当报错", d)
		}
	}
	if _, err := NewHeaderPropagator(nil, []ForwardHeaderRule{{Domains: []string{"*.trusted.com", "api.a.com"}, Headers: []string{"X-T"}}}); err != nil {
		t.Errorf("合法的写法不该报错：%v", err)
	}
}

func TestNewHeaderPropagator_ContradictoryConfigFails(t *testing.T) {
	// 一边说发给所有人，一边说只发给这些人。猜哪边为准都可能把内部标识发给第三方
	_, err := NewHeaderPropagator(
		[]string{"X-Internal-Token", "X-Request-Id"},
		[]ForwardHeaderRule{{Domains: []string{"*.internal.com"}, Headers: []string{"x-internal-token"}}},
	)
	if err == nil {
		t.Fatal("同一个 header 同时出现在两处应当报错")
	}
	if !strings.Contains(err.Error(), "X-Internal-Token") {
		t.Errorf("错误里要点名是哪个 header，got=%v", err)
	}
	if strings.Contains(err.Error(), "X-Request-Id") {
		t.Errorf("不冲突的 header 不该出现在错误里，got=%v", err)
	}
}

func TestNewHeaderPropagator_Normalization(t *testing.T) {
	p := mustProp(t, []string{"x-request-id", "", "X-REQUEST-ID"}, []ForwardHeaderRule{
		{Domains: []string{" Example.COM ", ""}, Headers: []string{"x-trace-tag"}},
		{Domains: nil, Headers: []string{"X-Dropped"}}, // 没域名，整条丢弃
		{Domains: []string{"a.com"}, Headers: nil},     // 没 header，整条丢弃
	})

	if want := []string{"X-Request-Id", "X-Trace-Tag"}; !reflect.DeepEqual(p.Fields(), want) {
		t.Errorf("header 名应规范化并去重，got=%v want=%v", p.Fields(), want)
	}
	out := roundTrip(p, http.Header{"X-Trace-Tag": {"v"}}, "example.com")
	if out.Get("X-Trace-Tag") != "v" {
		t.Errorf("域名应 trim 并转小写后匹配，got=%v", out)
	}
}

func TestHeaderPropagator_FieldsReturnsCopy(t *testing.T) {
	p := mustProp(t, []string{"X-Request-Id"}, nil)
	f := p.Fields()
	f[0] = "改掉了"
	if p.Fields()[0] != "X-Request-Id" {
		t.Error("Fields 返回的应是拷贝，调用方改不动内部状态")
	}
}

func TestHeaderPropagator_NoOpWithEmptyConfig(t *testing.T) {
	p := mustProp(t, nil, nil)
	ctx := p.Extract(context.Background(), trusted(http.Header{"X-Request-Id": {"r1"}}))
	if ForwardHeadersFromContext(ctx) != nil {
		t.Error("没配置任何 header 时不该往 ctx 里塞东西")
	}
	out := http.Header{}
	p.Inject(ctx, propagation.HeaderCarrier(out))
	if len(out) != 0 {
		t.Errorf("没配置时不该注入，got=%v", out)
	}
}

func TestHeaderPropagator_ExtractMergesExistingValues(t *testing.T) {
	// 组合 Propagator 里可能有多个 HeaderPropagator，后面的不能把前面的覆盖掉
	p1 := mustProp(t, []string{"X-A"}, nil)
	p2 := mustProp(t, []string{"X-B"}, nil)

	ctx := p1.Extract(context.Background(), trusted(http.Header{"X-A": {"a"}}))
	ctx = p2.Extract(ctx, trusted(http.Header{"X-B": {"b"}}))

	got := ForwardHeadersFromContext(ctx)
	if got["X-A"] != "a" || got["X-B"] != "b" {
		t.Errorf("两次 Extract 的结果应合并，got=%v", got)
	}
}

func TestForwardHeaderFromContext(t *testing.T) {
	p := mustProp(t, []string{"X-Request-Id"}, nil)
	ctx := p.Extract(context.Background(), trusted(http.Header{"X-Request-Id": {"r1"}}))

	if got := ForwardHeaderFromContext(ctx, "x-request-id"); got != "r1" {
		t.Errorf("取值应大小写不敏感，got=%q", got)
	}
	if got := ForwardHeaderFromContext(ctx, "X-Nope"); got != "" {
		t.Errorf("没有的 header 应返回空，got=%q", got)
	}
	if got := ForwardHeaderFromContext(context.Background(), "X-Request-Id"); got != "" {
		t.Errorf("空 ctx 应返回空，got=%q", got)
	}
}

func TestForwardHeadersFromContext_ReturnsCopy(t *testing.T) {
	p := mustProp(t, []string{"X-Request-Id"}, nil)
	ctx := p.Extract(context.Background(), trusted(http.Header{"X-Request-Id": {"r1"}}))

	m := ForwardHeadersFromContext(ctx)
	m["X-Request-Id"] = "改掉了"
	if ForwardHeaderFromContext(ctx, "X-Request-Id") != "r1" {
		t.Error("返回的应是拷贝，调用方改不动 ctx 里的值")
	}
	if ForwardHeadersFromContext(context.Background()) != nil {
		t.Error("空 ctx 应返回 nil")
	}
}

func TestHeaderPropagator_EmptyValuesNotInjected(t *testing.T) {
	p := mustProp(t, []string{"X-Request-Id"}, nil)
	out := roundTrip(p, http.Header{"X-Request-Id": {""}}, "example.com")
	if _, ok := out["X-Request-Id"]; ok {
		t.Errorf("上游没给值就不该往下游写空 header，got=%v", out)
	}
}

func TestWithTargetHost(t *testing.T) {
	if got := TargetHostFromContext(context.Background()); got != "" {
		t.Errorf("没设过应为空，got=%q", got)
	}
	ctx := WithTargetHost(context.Background(), "a.example.com:80")
	if got := TargetHostFromContext(ctx); got != "a.example.com:80" {
		t.Errorf("取出来的应和存进去的一致，got=%q", got)
	}
}
