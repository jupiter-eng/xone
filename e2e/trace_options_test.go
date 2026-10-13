package e2e

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// forwardProxy 一个 HTTP 正向代理桩：服务的 HTTP_PROXY 指向它，于是服务调任何 http://域名/ 都落到这里。
// 按域名透传的规则要一个真的域名才测得到，而测试环境改不了 DNS / hosts
type forwardProxy struct {
	URL string
	mu  sync.Mutex
	got map[string]http.Header // 目标 host → 最后一次收到的请求头
}

func newForwardProxy(t *testing.T) *forwardProxy {
	t.Helper()
	fp := &forwardProxy{got: map[string]http.Header{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.mu.Lock()
		fp.got[r.URL.Host] = r.Header.Clone() // 经代理的请求是绝对 URI，URL.Host 就是目标
		fp.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	fp.URL = srv.URL
	return fp
}

func (fp *forwardProxy) header(t *testing.T, host string) http.Header {
	t.Helper()
	fp.mu.Lock()
	defer fp.mu.Unlock()
	h, ok := fp.got[host]
	if !ok {
		t.Fatalf("代理桩没收到发给 %s 的请求", host)
	}
	return h
}

// proxyEnv 让服务的出站 HTTP 走 fp。NO_PROXY 设成一个不会命中的值：测试进程的环境里可能带着别的 NO_PROXY
func proxyEnv(fp *forwardProxy) map[string]string {
	return map[string]string{"HTTP_PROXY": fp.URL, "http_proxy": fp.URL, "NO_PROXY": "no-proxy.invalid", "no_proxy": "no-proxy.invalid"}
}

// xtrace/README.md XTrace.ForwardHeaderRules：「只发给匹配域名的 Header」。
//
//	Domains 只认精确的 api.internal.com 和通配的 *.trusted.com；*.trusted.com 匹配任意层级的子域，
//	不匹配裸域 trusted.com；其余带 * 的写法启动失败（*trusted.com 原先会匹配 eviltrusted.com）
//	同一个 header 同时出现在 ForwardHeaders 和 ForwardHeaderRules 里会启动失败
//	透传只收可信对端（XGin.TrustedProxies）发来的值
func TestCoverage_ForwardHeaderRulesMatchingDomainsOnly_AcceptOnlyTrusted(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	const rules = `XTrace:
  ForwardHeaderRules:
    - Domains: ["api.internal.test", "*.trusted.test"]
      Headers: ["X-Internal-Token"]
`
	send := func(t *testing.T, p *harness.Process, host string) {
		t.Helper()
		r := p.Get(t, "/probe/fwd?url=http://"+host+"/echo", "X-Request-Id", "rid-"+host, "X-Internal-Token", "tok-"+host)
		if r.Status != http.StatusOK {
			t.Fatalf("GET /probe/fwd 到 %s：%v", host, r)
		}
	}

	t.Run("可信对端：按域名发", func(t *testing.T) {
		t.Parallel()
		fp := newForwardProxy(t)
		p := harness.Start(t, harness.Options{Env: proxyEnv(fp), Overlay: "XGin:\n  TrustedProxies: [\"127.0.0.1\"]\n" + rules})
		for _, c := range []struct {
			host      string
			wantToken bool
		}{
			{"api.internal.test", true},
			{"api.internal.test:8080", true}, // 带端口的照样按域名比
			{"other.internal.test", false},   // 精确写法不带子域
			{"a.trusted.test", true},
			{"a.b.trusted.test", true}, // 任意层级
			{"trusted.test", false},    // 不含裸域
			{"eviltrusted.test", false},
			{"example.test", false},
		} {
			send(t, p, c.host)
			h := fp.header(t, c.host)
			if got := h.Get("X-Internal-Token"); (got != "") != c.wantToken || (c.wantToken && got != "tok-"+c.host) {
				t.Errorf("发给 %s：X-Internal-Token 应%s，实际 %q", c.host, map[bool]string{true: "带上", false: "不带"}[c.wantToken], got)
			}
			// ForwardHeaders（service/application.yml 里的 X-Request-Id）发给所有下游
			if got := h.Get("X-Request-Id"); got != "rid-"+c.host {
				t.Errorf("ForwardHeaders 里的 X-Request-Id 应发给所有下游，发给 %s 的是 %q", c.host, got)
			}
		}
	})

	t.Run("不可信对端：规则里的头也不收", func(t *testing.T) {
		t.Parallel()
		fp := newForwardProxy(t)
		p := harness.Start(t, harness.Options{Env: proxyEnv(fp), Overlay: "XGin:\n  TrustedProxies: []\n" + rules})
		send(t, p, "api.internal.test")
		h := fp.header(t, "api.internal.test")
		if h.Get("X-Internal-Token") != "" || h.Get("X-Request-Id") != "" {
			t.Errorf("TrustedProxies: [] 时什么都不透传，发给 api.internal.test 的却带着 X-Internal-Token=%q X-Request-Id=%q",
				h.Get("X-Internal-Token"), h.Get("X-Request-Id"))
		}
	})

	t.Run("写错的规则启动失败", func(t *testing.T) {
		t.Parallel()
		for _, c := range []struct {
			name, overlay string
			want          []string
		}{
			{"*trusted.test", "XTrace:\n  ForwardHeaderRules:\n    - Domains: [\"*trusted.test\"]\n      Headers: [X-Internal-Token]\n",
				[]string{"xtrace", `domain pattern "*trusted.test" is not supported`}},
			{"a.*.test", "XTrace:\n  ForwardHeaderRules:\n    - Domains: [\"a.*.test\"]\n      Headers: [X-Internal-Token]\n",
				[]string{`domain pattern "a.*.test" is not supported`}},
			{"同一个头两边都写了", "XTrace:\n  ForwardHeaderRules:\n    - Domains: [api.internal.test]\n      Headers: [x-request-id]\n",
				[]string{"X-Request-Id appears in both ForwardHeaders and ForwardHeaderRules"}},
		} {
			stderr := covStartupError(t, harness.Options{Overlay: c.overlay})
			faultMustContain(t, c.name+" 的启动错误", stderr, c.want...)
		}
	})
}

// xtrace/README.md XTrace.SampleRatio：「根 Span 的采样率，[0, 1]」，「有上游时一律听上游的 sampled 位（ParentBased）」。
// 0.5 时根请求大约一半被导出；带着上游 traceparent 的，sampled=01 的全导出、并以 -01 往下游传，
// sampled=00 的一个都不导出、以 -00 往下游传，和采样率无关
func TestCoverage_SampleRatioSamplesRoots_FollowsUpstreamWhenPresent(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	stub := harness.NewStub(t)
	p := harness.Start(t, harness.Options{Spans: true, Downstream: stub.URL, Overlay: sampleRatio("0.5")})

	const roots = 400
	rootIDs := map[string]bool{}
	for range roots {
		rootIDs[traceIDOf(t, p.Get(t, "/ping"))] = true
	}
	const parented = 30
	sampledUp, unsampledUp := map[string]bool{}, map[string]bool{}
	for i := range 2 * parented {
		up, flags := randomTraceID(), "-01"
		if i%2 == 1 {
			flags = "-00"
		}
		p.Get(t, "/proxy", "traceparent", "00-"+up+"-"+randomSpanID()+flags)
		if got := stub.Last(t).Header.Get("Traceparent"); !strings.HasPrefix(got, "00-"+up+"-") || !strings.HasSuffix(got, flags) {
			t.Errorf("上游 %s 时下游应收到同一条链路、同样的 sampled 位，实际 %q", flags, got)
		}
		if flags == "-01" {
			sampledUp[up] = true
		} else {
			unsampledUp[up] = true
		}
	}

	// 退出时 xtrace 关掉导出器，之后文件里的就是全部
	if exit := p.Terminate(t, 20*time.Second); exit.Code != 0 {
		t.Fatalf("SIGTERM 之后应以 0 退出，实际 %v", exit)
	}
	exported, fromSampled, fromUnsampled := 0, map[string]bool{}, 0
	for _, s := range p.Spans(t) {
		if s.Kind != "server" {
			continue
		}
		switch {
		case rootIDs[s.TraceID]:
			exported++
		case sampledUp[s.TraceID]:
			fromSampled[s.TraceID] = true
		case unsampledUp[s.TraceID]:
			fromUnsampled++
		}
	}
	// 二项分布 n=400 p=0.5：标准差 10，给 ±50（5σ），误判的概率约百万分之一
	if math.Abs(float64(exported)-roots*0.5) > 50 {
		t.Errorf("SampleRatio: 0.5 时 %d 个根请求应导出约 %d 个服务端 Span（±50），实际 %d", roots, roots/2, exported)
	}
	if len(fromSampled) != parented {
		t.Errorf("上游 sampled=01 的 %d 个请求都该导出（ParentBased，不按 0.5 抽），实际导出了 %d 个", parented, len(fromSampled))
	}
	if fromUnsampled != 0 {
		t.Errorf("上游 sampled=00 的请求一个都不该导出，实际导出了 %d 个", fromUnsampled)
	}
	t.Logf("数字：SampleRatio 0.5，%d 个根请求导出了 %d 个（%.1f%%）；上游 01 导出 %d/%d，上游 00 导出 %d/%d",
		roots, exported, float64(exported)*100/roots, len(fromSampled), parented, fromUnsampled, parented)
}

// xtrace/README.md XTrace.Console：「把 Span 打到标准输出，本地调试用，默认关」
func TestCoverage_ConsolePrintsSpansToStdout(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("Console=%v", on), func(t *testing.T) {
			t.Parallel()
			o := harness.Options{Overlay: fmt.Sprintf("XTrace:\n  Console: %v\n", on)}
			if on {
				o.NonJSON = "XTrace.Console: true pretty-prints spans to stdout"
			}
			p := harness.Start(t, o)
			tid := traceIDOf(t, p.Get(t, "/probe/log?msg=console"))
			accessLog(t, p, tid)
			want := `"TraceID": "` + tid + `"`
			if on {
				deadline := time.Now().Add(waitFor)
				for !strings.Contains(p.Stdout(), want) && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
				out := p.Stdout()
				if !strings.Contains(out, want) || !strings.Contains(out, `"Name": "GET /probe/log"`) {
					t.Errorf("Console: true 时服务端 Span（GET /probe/log，TraceID %s）应打到标准输出，实际没有", tid)
				}
			} else if strings.Contains(p.Stdout(), `"SpanContext"`) {
				t.Errorf("Console 默认关，标准输出里不该有 Span")
			}
		})
	}
}

// xapp/README.md App：「链路的 service.name / service.version 按这个优先级取，后面的压过前面的：
// OTel 自己的兜底名 unknown_service:<可执行文件名> → App.Name / App.Version → OTEL_RESOURCE_ATTRIBUTES → OTEL_SERVICE_NAME」；
// XTrace：「OTEL_RESOURCE_ATTRIBUTES 写错一项（其余写对的照常生效）……都不再让服务起不来」
func TestCoverage_service_NameFromAppOverriddenByOTELEnv(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	for _, c := range []struct {
		name          string
		env           map[string]string
		noApp         bool
		wantName      string
		wantVersion   any // nil 表示不该有这个属性
		wantExtraKey  string
		wantExtraVal  string
		wantStartWarn bool
	}{
		{name: "只有 App", wantName: "xone.e2e.service", wantVersion: "e2e"},
		{name: "没有 App：OTel 的兜底名，没有版本", noApp: true, wantName: "unknown_service:service", wantVersion: nil},
		{name: "OTEL_RESOURCE_ATTRIBUTES 压过 App",
			env:      map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "service.name=from-attrs,service.version=v-attrs"},
			wantName: "from-attrs", wantVersion: "v-attrs"},
		{name: "OTEL_SERVICE_NAME 再压过 OTEL_RESOURCE_ATTRIBUTES",
			env:      map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "service.name=from-attrs,service.version=v-attrs", "OTEL_SERVICE_NAME": "from-env"},
			wantName: "from-env", wantVersion: "v-attrs"},
		{name: "OTEL_RESOURCE_ATTRIBUTES 写错一项：照常起来，写对的那项生效",
			env:      map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "deployment.environment=e2e,broken-item"},
			wantName: "xone.e2e.service", wantVersion: "e2e", wantExtraKey: "deployment.environment", wantExtraVal: "e2e", wantStartWarn: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o := harness.Options{Spans: true, Env: c.env}
			if c.noApp {
				o.Config = covConfig(t, map[string]string{"XApp": ""})
			}
			p := harness.Start(t, o)
			srv := serverSpan(t, p, traceIDOf(t, p.Get(t, "/ping")))
			if got := srv.Resource["service.name"]; got != c.wantName {
				t.Errorf("service.name 应是 %q，实际 %v", c.wantName, got)
			}
			if got, ok := srv.Resource["service.version"]; (c.wantVersion == nil && ok) || (c.wantVersion != nil && got != c.wantVersion) {
				t.Errorf("service.version 应是 %v，实际 %v（有=%v）", c.wantVersion, got, ok)
			}
			if c.wantExtraKey != "" && srv.Resource[c.wantExtraKey] != c.wantExtraVal {
				t.Errorf("写对的 %s=%s 应照常生效，实际 %v", c.wantExtraKey, c.wantExtraVal, srv.Resource[c.wantExtraKey])
			}
			if c.wantStartWarn && !strings.Contains(p.Output(), "xtrace some resource attributes could not be detected") {
				t.Errorf("OTEL_RESOURCE_ATTRIBUTES 写错时应打一条告警")
			}
			t.Logf("数字：%s → service.name=%v service.version=%v", c.name, srv.Resource["service.name"], srv.Resource["service.version"])
		})
	}
}
