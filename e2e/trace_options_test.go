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

// forwardProxy is a stub HTTP forward proxy: the service's HTTP_PROXY points at it, so every
// http://domain/ the service calls lands here.
// The per-domain forwarding rules take a real domain name to test, and the test environment cannot
// change DNS / hosts
type forwardProxy struct {
	URL string
	mu  sync.Mutex
	got map[string]http.Header // the target host -> the headers of the last request
}

func newForwardProxy(t *testing.T) *forwardProxy {
	t.Helper()
	fp := &forwardProxy{got: map[string]http.Header{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.mu.Lock()
		fp.got[r.URL.Host] = r.Header.Clone() // a request through a proxy carries an absolute URI, so URL.Host is the target
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
		t.Fatalf("the proxy stub received no request for %s", host)
	}
	return h
}

// proxyEnv sends the service's outbound HTTP through fp. NO_PROXY is set to something that never
// matches: the test process's own environment may carry a NO_PROXY of its own
func proxyEnv(fp *forwardProxy) map[string]string {
	return map[string]string{"HTTP_PROXY": fp.URL, "http_proxy": fp.URL, "NO_PROXY": "no-proxy.invalid", "no_proxy": "no-proxy.invalid"}
}

// xtrace/README.md, XTrace.ForwardHeaderRules: "the headers go to the matching domains alone".
//
//	Domains takes the exact api.internal.com and the wildcard *.trusted.com alone; *.trusted.com matches a subdomain at any depth
//	and does not match the bare trusted.com; every other spelling with a * fails the start (*trusted.com used to match eviltrusted.com)
//	one header appearing in both ForwardHeaders and ForwardHeaderRules fails the start
//	the forwarding takes the values from a trusted peer alone (XGin.TrustedProxies)
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
			t.Fatalf("GET /probe/fwd to %s: %v", host, r)
		}
	}

	t.Run("a trusted peer: sent by domain", func(t *testing.T) {
		t.Parallel()
		fp := newForwardProxy(t)
		p := harness.Start(t, harness.Options{Env: proxyEnv(fp), Overlay: "XGin:\n  TrustedProxies: [\"127.0.0.1\"]\n" + rules})
		for _, c := range []struct {
			host      string
			wantToken bool
		}{
			{"api.internal.test", true},
			{"api.internal.test:8080", true}, // a port and all, it still compares by domain
			{"other.internal.test", false},   // the exact spelling takes no subdomain
			{"a.trusted.test", true},
			{"a.b.trusted.test", true}, // any depth
			{"trusted.test", false},    // the bare domain is not included
			{"example.test", false},
		} {
			send(t, p, c.host)
			h := fp.header(t, c.host)
			if got := h.Get("X-Internal-Token"); (got != "") != c.wantToken || (c.wantToken && got != "tok-"+c.host) {
				t.Errorf("sent to %s: X-Internal-Token should %s, got %q", c.host, map[bool]string{true: "be there", false: "not be there"}[c.wantToken], got)
			}
			// ForwardHeaders (X-Request-Id in service/application.yml) goes to every downstream
			if got := h.Get("X-Request-Id"); got != "rid-"+c.host {
				t.Errorf("the X-Request-Id of ForwardHeaders should go to every downstream, and the one sent to %s was %q", c.host, got)
			}
		}
	})

	t.Run("an untrusted peer: not even the headers in the rules are taken", func(t *testing.T) {
		t.Parallel()
		fp := newForwardProxy(t)
		p := harness.Start(t, harness.Options{Env: proxyEnv(fp), Overlay: "XGin:\n  TrustedProxies: []\n" + rules})
		send(t, p, "api.internal.test")
		h := fp.header(t, "api.internal.test")
		if h.Get("X-Internal-Token") != "" || h.Get("X-Request-Id") != "" {
			t.Errorf("with TrustedProxies: [] nothing is forwarded, and what went to api.internal.test carried X-Internal-Token=%q X-Request-Id=%q",
				h.Get("X-Internal-Token"), h.Get("X-Request-Id"))
		}
	})

	t.Run("a malformed rule fails the start", func(t *testing.T) {
		t.Parallel()
		for _, c := range []struct {
			name, overlay string
			want          []string
		}{
			{"*trusted.test", "XTrace:\n  ForwardHeaderRules:\n    - Domains: [\"*trusted.test\"]\n      Headers: [X-Internal-Token]\n",
				[]string{"xtrace", `domain pattern "*trusted.test" is not supported`}},
			{"a.*.test", "XTrace:\n  ForwardHeaderRules:\n    - Domains: [\"a.*.test\"]\n      Headers: [X-Internal-Token]\n",
				[]string{`domain pattern "a.*.test" is not supported`}},
			{"one header written in both places", "XTrace:\n  ForwardHeaderRules:\n    - Domains: [api.internal.test]\n      Headers: [x-request-id]\n",
				[]string{"X-Request-Id appears in both ForwardHeaders and ForwardHeaderRules"}},
		} {
			stderr := covStartupError(t, harness.Options{Overlay: c.overlay})
			faultMustContain(t, "the start error for "+c.name, stderr, c.want...)
		}
	})
}

// xtrace/README.md, XTrace.SampleRatio: "the sampling ratio of a root span, in [0, 1]" and "with
// an upstream, its sampled bit always wins (ParentBased)".
// At 0.5 about half of the root requests are exported; of those carrying an upstream traceparent,
// every sampled=01 one is exported and passed on with -01, and not one sampled=00 one is exported
// and they are passed on with -00, whatever the sampling ratio
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
			t.Errorf("with the upstream at %s the downstream should receive the same trace and the same sampled bit, got %q", flags, got)
		}
		if flags == "-01" {
			sampledUp[up] = true
		} else {
			unsampledUp[up] = true
		}
	}

	// On exit xtrace closes the exporter, and what is in the file afterwards is all of it
	if exit := p.Terminate(t, 20*time.Second); exit.Code != 0 {
		t.Fatalf("it should exit with 0 after the SIGTERM, got %v", exit)
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
	// A binomial distribution with n=400 and p=0.5: the standard deviation is 10, and +-50 (5 sigma) makes a false alarm about one in a million
	if math.Abs(float64(exported)-roots*0.5) > 50 {
		t.Errorf("with SampleRatio: 0.5, %d root requests should export about %d server spans (+-50), got %d", roots, roots/2, exported)
	}
	if len(fromSampled) != parented {
		t.Errorf("all %d requests with the upstream at sampled=01 should be exported (ParentBased, not drawn at 0.5), and %d were", parented, len(fromSampled))
	}
	if fromUnsampled != 0 {
		t.Errorf("not one request with the upstream at sampled=00 should be exported, and %d were", fromUnsampled)
	}
	t.Logf("numbers: with SampleRatio 0.5, %d root requests exported %d (%.1f%%); the upstream at 01 exported %d/%d and at 00 exported %d/%d",
		roots, exported, float64(exported)*100/roots, len(fromSampled), parented, fromUnsampled, parented)
}

// xtrace/README.md, XTrace.Console: "it prints the spans to standard output, for local debugging, off by default"
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
					t.Errorf("with Console: true the server span (GET /probe/log, TraceID %s) should be printed to standard output, and it is not", tid)
				}
			} else if strings.Contains(p.Stdout(), `"SpanContext"`) {
				t.Errorf("Console is off by default and standard output should hold no span")
			}
		})
	}
}

// xtrace/README.md: "service.name's precedence, each overriding the one before: XApp.Name /
// XApp.Version -> OTEL_RESOURCE_ATTRIBUTES -> OTEL_SERVICE_NAME. Configured nowhere, the attribute
// is not written at all -- not even OTel's own unknown_service:<executable name>";
// XTrace: "one malformed item in OTEL_RESOURCE_ATTRIBUTES (where the well-formed ones still take)
// ... no longer keeps the service from coming up"
func TestCoverage_service_NameFromAppOverriddenByOTELEnv(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	for _, c := range []struct {
		name          string
		env           map[string]string
		noApp         bool
		wantName      any // nil means the attribute should not be there
		wantVersion   any // nil means the attribute should not be there
		wantExtraKey  string
		wantExtraVal  string
		wantStartWarn bool
	}{
		{name: "App alone", wantName: "xone.e2e.service", wantVersion: "e2e"},
		{name: "no App: no service.name and no service.version at all", noApp: true, wantName: nil, wantVersion: nil},
		{name: "OTEL_RESOURCE_ATTRIBUTES wins over App",
			env:      map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "service.name=from-attrs,service.version=v-attrs"},
			wantName: "from-attrs", wantVersion: "v-attrs"},
		{name: "OTEL_SERVICE_NAME wins over OTEL_RESOURCE_ATTRIBUTES in turn",
			env:      map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "service.name=from-attrs,service.version=v-attrs", "OTEL_SERVICE_NAME": "from-env"},
			wantName: "from-env", wantVersion: "v-attrs"},
		{name: "one malformed item in OTEL_RESOURCE_ATTRIBUTES: it comes up as usual and the well-formed one takes",
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
			if got, ok := srv.Resource["service.name"]; (c.wantName == nil && ok) || (c.wantName != nil && got != c.wantName) {
				t.Errorf("service.name should be %v, got %v (present=%v)", c.wantName, got, ok)
			}
			// The half of OTel's WithService that is kept: it tells replicas apart and does not depend on
			// an identity having been configured
			if srv.Resource["service.instance.id"] == nil {
				t.Error("service.instance.id must be written whether or not an identity is configured")
			}
			if got, ok := srv.Resource["service.version"]; (c.wantVersion == nil && ok) || (c.wantVersion != nil && got != c.wantVersion) {
				t.Errorf("service.version should be %v, got %v (present=%v)", c.wantVersion, got, ok)
			}
			if c.wantExtraKey != "" && srv.Resource[c.wantExtraKey] != c.wantExtraVal {
				t.Errorf("the well-formed %s=%s should take as usual, got %v", c.wantExtraKey, c.wantExtraVal, srv.Resource[c.wantExtraKey])
			}
			if c.wantStartWarn && !strings.Contains(p.Output(), "xtrace some resource attributes could not be detected") {
				t.Errorf("a malformed OTEL_RESOURCE_ATTRIBUTES should log a warning")
			}
			t.Logf("numbers: %s -> service.name=%v service.version=%v", c.name, srv.Resource["service.name"], srv.Resource["service.version"])
		})
	}
}
