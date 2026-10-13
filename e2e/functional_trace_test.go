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

// Span 的名字、种类、属性和父子关系：
//
//	服务端  xgin      名字是「方法 路由模板」，属性 http.route / url.path / 状态码；5xx 标错误、4xx 不标
//	SQL     xgorm     gorm.<操作>，db.query.text 是带占位符的语句，不含参数值
//	Redis   xredis    名字是命令名；xredis/README.md：「Span 里只有命令名，不含参数」
//	出站    xhttp     名字只用方法；url.full 去掉查询串（xhttp/README.md「行为与实测」那张表）
//
// 客户端 Span 的父都是这次请求的服务端 Span，同一条链路
func TestFunctional_SpanNamesAttributesAndParentage(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	stub := harness.NewStub(t)
	p := harness.Start(t, harness.Options{Spans: true, Downstream: stub.URL})
	name, email := "span-name-"+harness.NewID(), "span-"+harness.NewID()+"@example.com"

	// childOf 客户端 Span 的父是服务端 Span、在同一条链路上
	childOf := func(t *testing.T, c, srv harness.Span) {
		t.Helper()
		if c.TraceID != srv.TraceID || c.ParentSpanID != srv.SpanID || c.ParentRemote {
			t.Errorf("%s 的父应是服务端 Span %s（同一条链路 %s），实际 trace=%s parent=%s remote=%v",
				c.Name, srv.SpanID, srv.TraceID, c.TraceID, c.ParentSpanID, c.ParentRemote)
		}
		if c.Kind != "client" {
			t.Errorf("%s 的 kind 应是 client，实际 %s", c.Name, c.Kind)
		}
	}
	// noValue 这条链路上任何 Span 的任何属性值里都不许出现这些明文
	noValue := func(t *testing.T, spans []harness.Span, secrets ...string) {
		t.Helper()
		for _, s := range spans {
			mustNotContain(t, "Span "+s.Name+" 的属性", fmt.Sprint(s.Attributes), secrets...)
		}
	}

	var created user
	t.Run("POST /users：服务端 Span、gorm.create、redis del", func(t *testing.T) {
		r := p.PostJSON(t, "/users", map[string]string{"name": name, "email": email})
		r.JSON(t, &created)
		tid := traceIDOf(t, r)
		srv := serverSpan(t, p, tid)
		spans := traceSpans(t, p, tid)

		if srv.Name != "POST /users" || srv.ParentSpanID != "" || srv.StatusCode != "Unset" {
			t.Errorf("服务端 Span 应叫 POST /users、是根 Span、状态 Unset，实际 %q parent=%q status=%s", srv.Name, srv.ParentSpanID, srv.StatusCode)
		}
		for k, want := range map[string]string{
			"http.request.method": "POST", "http.route": "/users", "url.path": "/users", "http.response.status_code": "201",
		} {
			if got := srv.Str(k); got != want {
				t.Errorf("服务端 Span 的 %s 应是 %q，实际 %q", k, want, got)
			}
		}
		// xapp/README.md App：链路的 service.name / service.version 取自这里
		if srv.Resource["service.name"] != "xone.e2e.service" || srv.Resource["service.version"] != "e2e" {
			t.Errorf("resource 的 service.name / service.version 应取自 App（xone.e2e.service / e2e），实际 %v / %v",
				srv.Resource["service.name"], srv.Resource["service.version"])
		}

		ins := spansNamed(spans, "gorm.create")
		if len(ins) != 1 {
			t.Fatalf("链路上应有一个 gorm.create，实际 %s", spanNames(spans))
		}
		childOf(t, ins[0], srv)
		stmt := ins[0].Str("db.query.text")
		if !strings.HasPrefix(stmt, `INSERT INTO "`+p.Table+`"`) || !strings.Contains(stmt, "$1") {
			t.Errorf("db.query.text 应是带占位符的 INSERT，实际 %q", stmt)
		}
		// xgorm/README.md「链路」：OTel 数据库语义约定 v1.43.0 的名字
		host, port, _ := net.SplitHostPort(harness.PGAddr())
		for k, want := range map[string]string{
			"db.system.name": "postgresql", "db.namespace": "xone_e2e", "db.operation.name": "INSERT",
			"server.address": host, "server.port": port, "db.rows_affected": "1",
		} {
			if got := ins[0].Str(k); got != want {
				t.Errorf("gorm.create 的 %s 应是 %q，实际 %q", k, want, got)
			}
		}

		del := spansNamed(spans, "del")
		if len(del) != 1 {
			t.Fatalf("链路上应有一个 Redis del（删缓存），实际 %s", spanNames(spans))
		}
		childOf(t, del[0], srv)
		if del[0].Str("db.system") != "redis" {
			t.Errorf("redis Span 的 db.system 应是 redis，实际 %q", del[0].Str("db.system"))
		}
		// 参数值不进 Span：SQL 的参数（名字、邮箱）、Redis 命令的参数（key）
		noValue(t, spans, name, email, p.KeyPrefix)
	})

	t.Run("GET /users/:id 落到 PG：get、gorm.query、set 都挂在服务端 Span 下", func(t *testing.T) {
		r := p.Get(t, fmt.Sprintf("/users/%d", created.ID))
		tid := traceIDOf(t, r)
		srv := serverSpan(t, p, tid)
		spans := traceSpans(t, p, tid)
		if srv.Name != "GET /users/:id" || srv.Str("http.route") != "/users/:id" || srv.Str("url.path") != fmt.Sprintf("/users/%d", created.ID) {
			t.Errorf("服务端 Span 名和 http.route 用路由模板、url.path 是真实路径，实际 %q route=%q path=%q", srv.Name, srv.Str("http.route"), srv.Str("url.path"))
		}
		if got := clientSpanNames(spans); got != "get,gorm.query,set" {
			t.Fatalf("链路上应依次是 get、gorm.query、set，实际 %s", spanNames(spans))
		}
		for _, s := range spans {
			if s.Kind == "client" {
				childOf(t, s, srv)
			}
		}
		q := spansNamed(spans, "gorm.query")[0].Str("db.query.text")
		if !strings.Contains(q, "WHERE id = $1") || strings.Contains(q, fmt.Sprintf("= %d", created.ID)) {
			t.Errorf("gorm.query 的 db.query.text 应是 WHERE id = $1，不带参数值 %d，实际 %q", created.ID, q)
		}
		// set 的值是整个用户的 JSON：名字和邮箱都在里面
		for _, s := range spans {
			if s.Name == "get" || s.Name == "set" {
				if st, ok := s.Attr("db.statement"); ok && st != s.Name {
					t.Errorf("xredis/README.md XRedis.Trace：Span 里只有命令名，实际 %s 的 db.statement=%q", s.Name, st)
				}
			}
		}
		noValue(t, spans, name, email, p.KeyPrefix)
	})

	t.Run("GET /proxy：出站 Span 的 url.full 不带查询串，下游拿到 traceparent", func(t *testing.T) {
		tok := "span-token-" + harness.NewID()
		r := p.Get(t, "/proxy?token="+tok)
		tid := traceIDOf(t, r)
		srv := serverSpan(t, p, tid)
		spans := traceSpans(t, p, tid)
		out := spansNamed(spans, "GET")
		if len(out) != 1 {
			t.Fatalf("xhttp/README.md：出站 Span 名只用方法 GET，链路上应有一个，实际 %s", spanNames(spans))
		}
		childOf(t, out[0], srv)
		if got := out[0].Str("url.full"); got != stub.URL+"/echo" {
			t.Errorf("xhttp/README.md：url.full 去掉查询串和片段，应是 %s/echo，实际 %q", stub.URL, got)
		}
		if out[0].Str("http.request.method") != "GET" || out[0].Str("http.response.status_code") != "200" {
			t.Errorf("出站 Span 应记方法 GET 和状态码 200，实际 %v", out[0].Attributes)
		}
		noValue(t, spans, tok)

		last := stub.Last(t)
		if last.Query.Get("token") != tok {
			t.Errorf("查询串只从 Span 里去掉，下游仍该收到 token=%s，实际 %q", tok, last.RawQuery)
		}
		// W3C traceparent：00-<trace>-<父 span>-<flags>，父就是那个出站 Span
		want := "00-" + tid + "-" + out[0].SpanID + "-01"
		if got := last.Header.Get("Traceparent"); got != want {
			t.Errorf("下游收到的 traceparent 应是 %s（父是出站 Span），实际 %q", want, got)
		}
	})

	t.Run("5xx 的服务端 Span 标成错误，4xx 不标", func(t *testing.T) {
		boom := serverSpan(t, p, traceIDOf(t, p.Get(t, "/boom")))
		if boom.StatusCode != "Error" || boom.Str("http.response.status_code") != "500" {
			t.Errorf("/boom 的服务端 Span 应是 Error、状态码 500，实际 %s %s", boom.StatusCode, boom.Str("http.response.status_code"))
		}
		nf := serverSpan(t, p, traceIDOf(t, p.Get(t, "/users/999999999")))
		if nf.StatusCode != "Unset" || nf.Str("http.response.status_code") != "404" {
			t.Errorf("404 是客户端的错，服务端 Span 不该标错误（xgin/middleware/trace.go），实际 %s %s", nf.StatusCode, nf.Str("http.response.status_code"))
		}
	})

	t.Run("上游带 traceparent：服务端 Span 接上上游，下游拿到同一条链路", func(t *testing.T) {
		up, parent := randomTraceID(), randomSpanID()
		r := p.Get(t, "/proxy", "traceparent", "00-"+up+"-"+parent+"-01")
		if traceIDOf(t, r) != up {
			t.Fatalf("X-Trace-Id 应是上游的 %s，实际 %s", up, traceIDOf(t, r))
		}
		srv := serverSpan(t, p, up)
		if srv.ParentSpanID != parent || !srv.ParentRemote {
			t.Errorf("服务端 Span 的父应是上游的 %s（remote），实际 %q remote=%v", parent, srv.ParentSpanID, srv.ParentRemote)
		}
		if got := stub.Last(t).Header.Get("Traceparent"); !strings.HasPrefix(got, "00-"+up+"-") {
			t.Errorf("下游收到的 traceparent 应在上游那条链路 %s 上，实际 %q", up, got)
		}
	})

	t.Run("上游说不采样：不产生 Span，traceparent 照样往下传", func(t *testing.T) {
		// xtrace/README.md XTrace.SampleRatio：「有上游时一律听上游的 sampled 位，1 也不例外」
		up, parent := randomTraceID(), randomSpanID()
		r := p.Get(t, "/proxy", "traceparent", "00-"+up+"-"+parent+"-00")
		if r.Status != http.StatusOK {
			t.Fatalf("GET /proxy：%v", r)
		}
		got := stub.Last(t).Header.Get("Traceparent")
		if !strings.HasPrefix(got, "00-"+up+"-") || !strings.HasSuffix(got, "-00") {
			t.Errorf("下游应收到同一条链路、sampled=00 的 traceparent，实际 %q", got)
		}
		// 界碑：再发一个采样的请求，它的服务端 Span 落盘了，前一个请求的 Span（如果有）早就落盘了
		serverSpan(t, p, traceIDOf(t, p.Get(t, "/ping")))
		if spans := traceSpans(t, p, up); len(spans) != 0 {
			t.Errorf("上游 sampled=00 时不该导出任何 Span，实际导出了 %s", spanNames(spans))
		}
	})
}

// xtrace/README.md XTrace.SampleRatio：「0 是『不采样但照常生成透传 TraceID』」。
// 于是 X-Trace-Id、日志里的 trace_id、给下游的 traceparent 都还在，只是一个 Span 都不导出
func TestFunctional_ZeroSampleRatioExportsNoSpans_StillPropagatesTraceID(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	stub := harness.NewStub(t)
	p := harness.Start(t, harness.Options{Spans: true, Downstream: stub.URL, Overlay: sampleRatio("0")})

	r := p.PostJSON(t, "/users", map[string]string{"name": "unsampled"})
	tid := traceIDOf(t, r)
	if al := accessLog(t, p, tid); al.Str("span_id") == "" {
		t.Errorf("不采样时访问日志照样带 trace_id / span_id，实际 %s", al.Line)
	}
	p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "user created" && l.Str("trace_id") == tid })

	r = p.Get(t, "/proxy")
	tp := stub.Last(t).Header.Get("Traceparent")
	if want := "00-" + traceIDOf(t, r) + "-"; !strings.HasPrefix(tp, want) || !strings.HasSuffix(tp, "-00") {
		t.Errorf("不采样时下游照样收到 traceparent（%s…-00），实际 %q", want, tp)
	}

	// 退出时 xtrace 关掉导出器：到这时还没写进文件的 Span 就不会再有了
	if exit := p.Terminate(t, 20*time.Second); exit.Code != 0 {
		t.Fatalf("SIGTERM 之后应以 0 退出，实际 %v", exit)
	}
	if spans := p.Spans(t); len(spans) != 0 {
		t.Errorf("SampleRatio: 0 时不该导出任何 Span，实际导出了 %d 个：%s", len(spans), spanNames(spans))
	}
}

// XTrace.ForwardHeaders 只收可信对端（XGin.TrustedProxies）发来的值；traceparent 不受这条影响。
// xtrace/README.md XTrace：默认只信私有网段；不可信的对端带着这些头来时打一条告警，整个进程只打一次。
// e2e 的请求都从本机发出，「不可信的对端」用 TrustedProxies: [] 造——和公网对端走的是同一条路
func TestFunctional_DownstreamGetsTraceparent_ForwardHeadersOnlyFromTrusted(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	t.Run("TrustedProxies 写 [] 就谁都不信", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		p := harness.Start(t, harness.Options{Downstream: stub.URL, Overlay: "XGin:\n  TrustedProxies: []\n"})
		for i := range 3 {
			r := p.Get(t, "/proxy", "X-Request-Id", fmt.Sprintf("rid-untrusted-%d", i), "X-Forwarded-For", "198.51.100.7",
				"Baggage", "tenant=forged")
			last := stub.Last(t)
			// xtrace/README.md XTrace：「baggage 同样只收可信对端的」
			if got := last.Header.Get("Baggage"); got != "" {
				t.Errorf("TrustedProxies: [] 时 baggage 不该透传，下游却收到了 %q", got)
			}
			if tp := last.Header.Get("Traceparent"); !strings.HasPrefix(tp, "00-"+traceIDOf(t, r)+"-") {
				t.Errorf("下游应收到这次请求那条链路的 traceparent（%s），实际 %q", traceIDOf(t, r), tp)
			}
			if got := last.Header.Get("X-Request-Id"); got != "" {
				t.Errorf("TrustedProxies: [] 时 X-Request-Id 不该透传，下游却收到了 %q", got)
			}
		}
		warn := "xtrace ignored forward headers from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted"
		p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == warn })
		if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == warn })); n != 1 {
			t.Errorf("文档说不可信对端带透传头时告警、整个进程只打一次，三次请求之后实际打了 %d 次", n)
		}
		// 访问日志的 client_ip 同样不信 X-Forwarded-For
		for _, l := range p.FindLogs(func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("route") == "/proxy" }) {
			if l.Str("client_ip") != "127.0.0.1" {
				t.Errorf("TrustedProxies: [] 时 client_ip 应是直连对端 127.0.0.1，实际 %q", l.Str("client_ip"))
			}
		}
	})

	t.Run("默认就信私有网段的对端（本机 127.0.0.1）", func(t *testing.T) {
		// K8s 里 Ingress、Pod、sidecar 都在私有网段里：不配 TrustedProxies 透传就生效
		t.Parallel()
		stub := harness.NewStub(t)
		p := harness.Start(t, harness.Options{Downstream: stub.URL})
		r := p.Get(t, "/proxy", "X-Request-Id", "rid-trusted-1", "X-Forwarded-For", "198.51.100.7", "Baggage", "tenant=acme")
		last := stub.Last(t)
		if got := last.Header.Get("X-Request-Id"); got != "rid-trusted-1" {
			t.Errorf("直连对端在 TrustedProxies 里时 X-Request-Id 应透传给下游，下游收到 %q", got)
		}
		if got := last.Header.Get("Baggage"); got != "tenant=acme" {
			t.Errorf("直连对端在 TrustedProxies 里时 baggage 应透传给下游，下游收到 %q", got)
		}
		if tp := last.Header.Get("Traceparent"); !strings.HasPrefix(tp, "00-"+traceIDOf(t, r)+"-") {
			t.Errorf("下游应收到 traceparent（%s），实际 %q", traceIDOf(t, r), tp)
		}
		if l := accessLog(t, p, traceIDOf(t, r)); l.Str("client_ip") != "198.51.100.7" {
			t.Errorf("直连对端可信时 client_ip 取 X-Forwarded-For 里的 198.51.100.7，实际 %q", l.Str("client_ip"))
		}
	})

	// xtrace/README.md XTrace：「透传和链路标识不跟着 XGin.Trace / XHttp.Trace 走」，
	// 那两个开关只管开不开 Span
	t.Run("XGin 和 XHttp 的 Trace 都关掉", func(t *testing.T) {
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
			t.Errorf("XGin.Trace 关着不回带 X-Trace-Id，实际 %q", id)
		}
		last := stub.Last(t)
		if got := last.Header.Get("X-Request-Id"); got != "rid-notrace-1" {
			t.Errorf("Trace 关着 X-Request-Id 照样透传给下游，下游收到 %q", got)
		}
		if got := last.Header.Get("Baggage"); got != "tenant=acme" {
			t.Errorf("Trace 关着 baggage 照样透传给下游，下游收到 %q", got)
		}
		if tp := last.Header.Get("Traceparent"); !strings.HasPrefix(tp, "00-"+upstream+"-") {
			t.Errorf("Trace 关着上游的链路标识照样带给下游，下游收到 %q", tp)
		}
		if exit := p.Terminate(t, 20*time.Second); exit.Code != 0 {
			t.Fatalf("SIGTERM 之后应以 0 退出，实际 %v", exit)
		}
		for _, s := range p.Spans(t) {
			if s.TraceID == upstream {
				t.Errorf("Trace 都关着，这条链路上不该导出服务端或出站 Span，实际导出了 %s（%s）", s.Name, s.Kind)
			}
		}
	})
}
