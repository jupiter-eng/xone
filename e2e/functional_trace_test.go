package e2e

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// The span names, kinds, attributes and parent / child relations:
//
//	server    xgin      named "method route template", with the attributes http.route / url.path / the status code; a 5xx is marked an error and a 4xx is not
//	SQL       xgorm     gorm.<operation>, with db.query.text the statement with its placeholders and no parameter values
//	Redis     xredis     named after the command; xredis/README.md: "the span holds the command name alone and no arguments"
//	outbound  xhttp     named after the method alone; url.full drops the query string (the table in xhttp/README.md, "Behavior (measured)")
//
// Every client span's parent is this request's server span, on the same trace
func TestFunctional_SpanNamesAttributesAndParentage(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	stub := harness.NewStub(t)
	p := harness.Start(t, harness.Options{Spans: true, Downstream: stub.URL})
	name, email := "span-name-"+harness.NewID(), "span-"+harness.NewID()+"@example.com"

	// childOf: a client span's parent is the server span, on the same trace
	childOf := func(t *testing.T, c, srv harness.Span) {
		t.Helper()
		if c.TraceID != srv.TraceID || c.ParentSpanID != srv.SpanID || c.ParentRemote {
			t.Errorf("%s's parent should be the server span %s (on the same trace %s), got trace=%s parent=%s remote=%v",
				c.Name, srv.SpanID, srv.TraceID, c.TraceID, c.ParentSpanID, c.ParentRemote)
		}
		if c.Kind != "client" {
			t.Errorf("%s's kind should be client, got %s", c.Name, c.Kind)
		}
	}
	// noValue: none of these secrets may appear in any attribute value of any span on this trace
	noValue := func(t *testing.T, spans []harness.Span, secrets ...string) {
		t.Helper()
		for _, s := range spans {
			mustNotContain(t, "the attributes of the span "+s.Name, fmt.Sprint(s.Attributes), secrets...)
		}
	}

	var created user
	t.Run("POST /users: the server span, gorm.create and a redis del", func(t *testing.T) {
		r := p.PostJSON(t, "/users", map[string]string{"name": name, "email": email})
		r.JSON(t, &created)
		tid := traceIDOf(t, r)
		srv := serverSpan(t, p, tid)
		spans := traceSpans(t, p, tid)

		if srv.Name != "POST /users" || srv.ParentSpanID != "" || srv.StatusCode != "Unset" {
			t.Errorf("the server span should be named POST /users, be the root span and have status Unset, got %q parent=%q status=%s", srv.Name, srv.ParentSpanID, srv.StatusCode)
		}
		for k, want := range map[string]string{
			"http.request.method": "POST", "http.route": "/users", "url.path": "/users", "http.response.status_code": "201",
		} {
			if got := srv.Str(k); got != want {
				t.Errorf("the server span's %s should be %q, got %q", k, want, got)
			}
		}
		// xapp/README.md, App: a trace's service.name / service.version come from here
		if srv.Resource["service.name"] != "xone.e2e.service" || srv.Resource["service.version"] != "e2e" {
			t.Errorf("the resource's service.name / service.version should come from App (xone.e2e.service / e2e), got %v / %v",
				srv.Resource["service.name"], srv.Resource["service.version"])
		}

		ins := spansNamed(spans, "gorm.create")
		if len(ins) != 1 {
			t.Fatalf("the trace should hold one gorm.create, got %s", spanNames(spans))
		}
		childOf(t, ins[0], srv)
		stmt := ins[0].Str("db.query.text")
		if !strings.HasPrefix(stmt, `INSERT INTO "`+p.Table+`"`) || !strings.Contains(stmt, "$1") {
			t.Errorf("db.query.text should be an INSERT with its placeholders, got %q", stmt)
		}
		// xgorm/README.md, "Tracing": the names of the OTel database semantic conventions v1.43.0
		host, port, _ := net.SplitHostPort(harness.PGAddr())
		for k, want := range map[string]string{
			"db.system.name": "postgresql", "db.namespace": "xone_e2e", "db.operation.name": "INSERT",
			"server.address": host, "server.port": port, "db.rows_affected": "1",
		} {
			if got := ins[0].Str(k); got != want {
				t.Errorf("gorm.create's %s should be %q, got %q", k, want, got)
			}
		}

		del := spansNamed(spans, "del")
		if len(del) != 1 {
			t.Fatalf("the trace should hold one Redis del (dropping the cache), got %s", spanNames(spans))
		}
		childOf(t, del[0], srv)
		if del[0].Str("db.system") != "redis" {
			t.Errorf("the redis span's db.system should be redis, got %q", del[0].Str("db.system"))
		}
		// No parameter value reaches a span: the SQL parameters (the name, the email) and the Redis command's argument (the key)
		noValue(t, spans, name, email, p.KeyPrefix)
	})

	t.Run("GET /users/:id reaching PG: get, gorm.query and set all hang under the server span", func(t *testing.T) {
		r := p.Get(t, fmt.Sprintf("/users/%d", created.ID))
		tid := traceIDOf(t, r)
		srv := serverSpan(t, p, tid)
		spans := traceSpans(t, p, tid)
		if srv.Name != "GET /users/:id" || srv.Str("http.route") != "/users/:id" || srv.Str("url.path") != fmt.Sprintf("/users/%d", created.ID) {
			t.Errorf("the server span's name and http.route use the route template and url.path the real path, got %q route=%q path=%q", srv.Name, srv.Str("http.route"), srv.Str("url.path"))
		}
		if got := clientSpanNames(spans); got != "get,gorm.query,set" {
			t.Fatalf("the trace should hold get, gorm.query and set in order, got %s", spanNames(spans))
		}
		for _, s := range spans {
			if s.Kind == "client" {
				childOf(t, s, srv)
			}
		}
		q := spansNamed(spans, "gorm.query")[0].Str("db.query.text")
		if !strings.Contains(q, "WHERE id = $1") || strings.Contains(q, fmt.Sprintf("= %d", created.ID)) {
			t.Errorf("gorm.query's db.query.text should be WHERE id = $1 and carry no parameter value %d, got %q", created.ID, q)
		}
		// set's value is the whole user as JSON: the name and the email are both inside it
		for _, s := range spans {
			if s.Name == "get" || s.Name == "set" {
				if st, ok := s.Attr("db.statement"); ok && st != s.Name {
					t.Errorf("xredis/README.md, XRedis.Trace: the span holds the command name alone, and %s has db.statement=%q", s.Name, st)
				}
			}
		}
		noValue(t, spans, name, email, p.KeyPrefix)
	})

	t.Run("GET /proxy: the outbound span's url.full carries no query string and the downstream gets a traceparent", func(t *testing.T) {
		tok := "span-token-" + harness.NewID()
		r := p.Get(t, "/proxy?token="+tok)
		tid := traceIDOf(t, r)
		srv := serverSpan(t, p, tid)
		spans := traceSpans(t, p, tid)
		out := spansNamed(spans, "GET")
		if len(out) != 1 {
			t.Fatalf("xhttp/README.md: an outbound span is named after the method, GET, and the trace should hold one, got %s", spanNames(spans))
		}
		childOf(t, out[0], srv)
		if got := out[0].Str("url.full"); got != stub.URL+"/echo" {
			t.Errorf("xhttp/README.md: url.full drops the query string and the fragment and should be %s/echo, got %q", stub.URL, got)
		}
		if out[0].Str("http.request.method") != "GET" || out[0].Str("http.response.status_code") != "200" {
			t.Errorf("the outbound span should record the method GET and the status code 200, got %v", out[0].Attributes)
		}
		noValue(t, spans, tok)

		last := stub.Last(t)
		if last.Query.Get("token") != tok {
			t.Errorf("the query string is dropped from the span alone and the downstream should still receive token=%s, got %q", tok, last.RawQuery)
		}
		// The W3C traceparent: 00-<trace>-<parent span>-<flags>, where the parent is that outbound span
		want := "00-" + tid + "-" + out[0].SpanID + "-01"
		if got := last.Header.Get("Traceparent"); got != want {
			t.Errorf("the traceparent the downstream received should be %s (the parent being the outbound span), got %q", want, got)
		}
	})

	t.Run("a 5xx marks the server span an error and a 4xx does not", func(t *testing.T) {
		boom := serverSpan(t, p, traceIDOf(t, p.Get(t, "/boom")))
		if boom.StatusCode != "Error" || boom.Str("http.response.status_code") != "500" {
			t.Errorf("/boom's server span should be Error with the status code 500, got %s %s", boom.StatusCode, boom.Str("http.response.status_code"))
		}
		nf := serverSpan(t, p, traceIDOf(t, p.Get(t, "/users/999999999")))
		if nf.StatusCode != "Unset" || nf.Str("http.response.status_code") != "404" {
			t.Errorf("a 404 is the client's fault and the server span should not be marked an error (xgin/middleware/trace.go), got %s %s", nf.StatusCode, nf.Str("http.response.status_code"))
		}
	})

	t.Run("an upstream traceparent: the server span joins the upstream and the downstream lands on the same trace", func(t *testing.T) {
		up, parent := randomTraceID(), randomSpanID()
		r := p.Get(t, "/proxy", "traceparent", "00-"+up+"-"+parent+"-01")
		if traceIDOf(t, r) != up {
			t.Fatalf("X-Trace-Id should be the upstream's %s, got %s", up, traceIDOf(t, r))
		}
		srv := serverSpan(t, p, up)
		if srv.ParentSpanID != parent || !srv.ParentRemote {
			t.Errorf("the server span's parent should be the upstream's %s (remote), got %q remote=%v", parent, srv.ParentSpanID, srv.ParentRemote)
		}
		if got := stub.Last(t).Header.Get("Traceparent"); !strings.HasPrefix(got, "00-"+up+"-") {
			t.Errorf("the traceparent the downstream received should be on the upstream's trace %s, got %q", up, got)
		}
	})

	t.Run("the upstream says not sampled: no span is made and the traceparent still goes downstream", func(t *testing.T) {
		// xtrace/README.md, XTrace.SampleRatio: "with an upstream, its sampled bit always wins, 1 included"
		up, parent := randomTraceID(), randomSpanID()
		r := p.Get(t, "/proxy", "traceparent", "00-"+up+"-"+parent+"-00")
		if r.Status != http.StatusOK {
			t.Fatalf("GET /proxy: %v", r)
		}
		got := stub.Last(t).Header.Get("Traceparent")
		if !strings.HasPrefix(got, "00-"+up+"-") || !strings.HasSuffix(got, "-00") {
			t.Errorf("the downstream should receive a traceparent on the same trace with sampled=00, got %q", got)
		}
		// A landmark: send one more sampled request, and once its server span is on disk the previous request's span (if any) has long been written
		serverSpan(t, p, traceIDOf(t, p.Get(t, "/ping")))
		if spans := traceSpans(t, p, up); len(spans) != 0 {
			t.Errorf("with the upstream at sampled=00 no span should be exported, and %s was", spanNames(spans))
		}
	})
}

// xtrace/README.md, XTrace.SampleRatio: "0 means 'do not sample, but still generate a TraceID to
// hand on'".
// So X-Trace-Id, trace_id in the logs and the traceparent given to the downstream are all still
// there, and not one span is exported
func TestFunctional_ZeroSampleRatioExportsNoSpans_StillPropagatesTraceID(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	stub := harness.NewStub(t)
	p := harness.Start(t, harness.Options{Spans: true, Downstream: stub.URL, Overlay: sampleRatio("0")})

	r := p.PostJSON(t, "/users", map[string]string{"name": "unsampled"})
	tid := traceIDOf(t, r)
	if al := accessLog(t, p, tid); al.Str("span_id") == "" {
		t.Errorf("without sampling the access log still carries trace_id / span_id, got %s", al.Line)
	}
	p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "user created" && l.Str("trace_id") == tid })

	r = p.Get(t, "/proxy")
	tp := stub.Last(t).Header.Get("Traceparent")
	if want := "00-" + traceIDOf(t, r) + "-"; !strings.HasPrefix(tp, want) || !strings.HasSuffix(tp, "-00") {
		t.Errorf("without sampling the downstream still receives a traceparent (%s...-00), got %q", want, tp)
	}

	// On exit xtrace closes the exporter: no span that has not reached the file by then ever will
	if exit := p.Terminate(t, 20*time.Second); exit.Code != 0 {
		t.Fatalf("it should exit with 0 after the SIGTERM, got %v", exit)
	}
	if spans := p.Spans(t); len(spans) != 0 {
		t.Errorf("with SampleRatio: 0 no span should be exported, and %d were: %s", len(spans), spanNames(spans))
	}
}

// XTrace.ForwardHeaders takes these values from a trusted peer alone (XGin.TrustedProxies);
// traceparent is not subject to this.
// xtrace/README.md, XTrace: the private ranges are trusted by default; when an untrusted peer
// arrives carrying these headers a warning is logged, once for the whole process.
// The e2e requests all come from this host, and "an untrusted peer" is made with
// TrustedProxies: [] -- which is the same path a peer on the public internet takes
func TestFunctional_DownstreamGetsTraceparent_ForwardHeadersOnlyFromTrusted(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	t.Run("TrustedProxies: [] trusts nobody", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		p := harness.Start(t, harness.Options{Downstream: stub.URL, Overlay: "XGin:\n  TrustedProxies: []\n"})
		for i := range 3 {
			r := p.Get(t, "/proxy", "X-Request-Id", fmt.Sprintf("rid-untrusted-%d", i), "X-Forwarded-For", "198.51.100.7",
				"Baggage", "tenant=forged")
			last := stub.Last(t)
			// xtrace/README.md, XTrace: "baggage likewise comes from a trusted peer alone"
			if got := last.Header.Get("Baggage"); got != "" {
				t.Errorf("with TrustedProxies: [] baggage should not be forwarded, and the downstream received %q", got)
			}
			if tp := last.Header.Get("Traceparent"); !strings.HasPrefix(tp, "00-"+traceIDOf(t, r)+"-") {
				t.Errorf("the downstream should receive the traceparent of this request's trace (%s), got %q", traceIDOf(t, r), tp)
			}
			if got := last.Header.Get("X-Request-Id"); got != "" {
				t.Errorf("with TrustedProxies: [] X-Request-Id should not be forwarded, and the downstream received %q", got)
			}
		}
		warn := "xtrace ignored forward headers from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted"
		p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == warn })
		if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == warn })); n != 1 {
			t.Errorf("the documentation says an untrusted peer carrying forwarded headers logs a warning once for the whole process, and after three requests it logged %d", n)
		}
		// client_ip in the access log likewise does not trust X-Forwarded-For
		for _, l := range p.FindLogs(func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("route") == "/proxy" }) {
			if l.Str("client_ip") != "127.0.0.1" {
				t.Errorf("with TrustedProxies: [] client_ip should be the direct peer 127.0.0.1, got %q", l.Str("client_ip"))
			}
		}
	})

	t.Run("the private ranges are trusted by default (this host being 127.0.0.1)", func(t *testing.T) {
		// In K8s the Ingress, the Pods and the sidecars all sit in private ranges: without configuring TrustedProxies the forwarding takes
		t.Parallel()
		stub := harness.NewStub(t)
		p := harness.Start(t, harness.Options{Downstream: stub.URL})
		r := p.Get(t, "/proxy", "X-Request-Id", "rid-trusted-1", "X-Forwarded-For", "198.51.100.7", "Baggage", "tenant=acme")
		last := stub.Last(t)
		if got := last.Header.Get("X-Request-Id"); got != "rid-trusted-1" {
			t.Errorf("with the direct peer in TrustedProxies, X-Request-Id should be forwarded downstream, and the downstream received %q", got)
		}
		if got := last.Header.Get("Baggage"); got != "tenant=acme" {
			t.Errorf("with the direct peer in TrustedProxies, baggage should be forwarded downstream, and the downstream received %q", got)
		}
		if tp := last.Header.Get("Traceparent"); !strings.HasPrefix(tp, "00-"+traceIDOf(t, r)+"-") {
			t.Errorf("the downstream should receive the traceparent (%s), got %q", traceIDOf(t, r), tp)
		}
		if l := accessLog(t, p, traceIDOf(t, r)); l.Str("client_ip") != "198.51.100.7" {
			t.Errorf("with the direct peer trusted, client_ip comes from X-Forwarded-For, 198.51.100.7, got %q", l.Str("client_ip"))
		}
	})

	// xtrace/README.md, XTrace: "the forwarding and the trace identifiers do not follow XGin.Trace /
	// XHttp.Trace", which only say whether the spans are made
	t.Run("the Trace of XGin and of XHttp both turned off", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		p := harness.Start(t, harness.Options{
			Spans:      true,
			Downstream: stub.URL,
			Overlay:    "XGin:\n  TrustedProxies: [\"127.0.0.1/32\"]\n  Trace: false\nXHttp:\n  Trace: false\n",
		})
		const upstream = "4bf92f3577b34da6a3ce929d0e0e4736"
		r := p.Get(t, "/proxy", "X-Request-Id", "rid-notrace-1", "Baggage", "tenant=acme",
			"Traceparent", "00-"+upstream+"-00f067aa0ba902b7-01")
		if id := r.Header.Get("X-Trace-Id"); id != "" {
			t.Errorf("with XGin.Trace off no X-Trace-Id comes back, got %q", id)
		}
		last := stub.Last(t)
		if got := last.Header.Get("X-Request-Id"); got != "rid-notrace-1" {
			t.Errorf("with Trace off X-Request-Id is still forwarded downstream, and the downstream received %q", got)
		}
		if got := last.Header.Get("Baggage"); got != "tenant=acme" {
			t.Errorf("with Trace off baggage is still forwarded downstream, and the downstream received %q", got)
		}
		if tp := last.Header.Get("Traceparent"); !strings.HasPrefix(tp, "00-"+upstream+"-") {
			t.Errorf("with Trace off the upstream's trace identifiers still reach the downstream, and the downstream received %q", tp)
		}
		if exit := p.Terminate(t, 20*time.Second); exit.Code != 0 {
			t.Fatalf("it should exit with 0 after the SIGTERM, got %v", exit)
		}
		for _, s := range p.Spans(t) {
			if s.TraceID == upstream {
				t.Errorf("with both Traces off no server or outbound span should be exported on this trace, and %s (%s) was", s.Name, s.Kind)
			}
		}
	})
}
