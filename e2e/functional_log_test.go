package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// redacted 脱敏之后的值，xgin/middleware.Redacted
const redacted = "***REDACTED***"

// 访问日志一条请求一行 JSON，字段以 internal/web/accesslog.go（AccessLog.Log）为准：
// method、route、path、status、elapsed_ms、client_ip，外加 xtrace 注入的 trace_id / span_id。
// 业务日志（slog.InfoContext）和访问日志在同一条链路上，trace_id 相同、且就是 Span 的 trace_id
func TestFunctional_AccessLogFieldsComplete_TraceIDSharedWithAppLogAndSpan(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	// 下面要核对「查询串、头、body 默认不记」这个框架默认值：service/application.yml 没写这几项，读到的就是 xgin.DefaultConfig
	p := harness.Start(t, harness.Options{Spans: true})

	start := time.Now()
	r := p.PostJSON(t, "/users", map[string]string{"name": "logan", "email": "logan@example.com"},
		"X-Forwarded-For", "203.0.113.9")
	rtt := time.Since(start)
	var u user
	r.JSON(t, &u)
	tid := traceIDOf(t, r)
	al := accessLog(t, p, tid)

	t.Run("字段齐全", func(t *testing.T) {
		for k, want := range map[string]string{
			"level": "INFO", "method": "POST", "route": "/users", "path": "/users", "status": "201",
			// xgin/README.md XGin.TrustedProxies：默认信私有网段，本机发来的 X-Forwarded-For 被认，client_ip 是它里面的地址
			"client_ip": "203.0.113.9",
			"proto":     "HTTP/1.1",
		} {
			if got := al.Str(k); got != want {
				t.Errorf("访问日志 %s 应是 %q，实际 %q\n%s", k, want, got, al.Line)
			}
		}
		// xgin/README.md：host、user_agent 原样；bytes_in 是请求的 Content-Length，bytes_out 是响应体字节数
		if al.Str("host") == "" || al.Str("user_agent") == "" {
			t.Errorf("访问日志应带 host 和 user_agent，实际 %s", al.Line)
		}
		if in, _ := num(al, "bytes_in"); in <= 0 {
			t.Errorf("POST /users 带 JSON 请求体，bytes_in 应 > 0，实际 %s", al.Line)
		}
		if out, _ := num(al, "bytes_out"); int(out) != len(r.Body) {
			t.Errorf("bytes_out 应等于响应体长度 %d，实际 %v", len(r.Body), al.Str("bytes_out"))
		}
		// xgin/README.md：elapsed_ms 是毫秒，保留到微秒
		el, ok := num(al, "elapsed_ms")
		elapsed := time.Duration(el * float64(time.Millisecond))
		if !ok || el <= 0 || elapsed > rtt {
			t.Errorf("elapsed_ms 应是 (0, 客户端往返 %v] 之间的毫秒数，实际 %v", rtt, al.Str("elapsed_ms"))
		}
		if _, err := time.Parse(time.RFC3339Nano, al.Str("time")); err != nil {
			t.Errorf("time 应是 RFC3339，实际 %q", al.Str("time"))
		}
		if !hexSpanID.MatchString(al.Str("span_id")) {
			t.Errorf("span_id 应是 16 位十六进制，实际 %q", al.Str("span_id"))
		}
		// xgin/README.md：LogQuery / LogRequestHeaders / LogRequestBody / LogResponseHeaders / LogResponseBody 默认关
		for _, k := range []string{"query", "request_headers", "request_body", "response_headers", "response_body"} {
			if _, ok := al.Get(k); ok {
				t.Errorf("文档说 %s 默认不记，实际访问日志里有：%s", k, al.Line)
			}
		}
		t.Logf("数字：POST /users 客户端往返 %v，访问日志里的 elapsed_ms %v", rtt, elapsed)
	})

	t.Run("业务日志与访问日志同一个 trace_id，就是 Span 的 trace_id", func(t *testing.T) {
		biz := p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "user created" && l.Str("trace_id") == tid })
		if biz.Str("user_id") != fmt.Sprint(u.ID) {
			t.Errorf("业务日志 user created 的 user_id 应是 %d，实际 %s", u.ID, biz.Line)
		}
		srv := serverSpan(t, p, tid)
		if srv.TraceID != tid || al.Str("trace_id") != tid {
			t.Errorf("X-Trace-Id、访问日志、Span 的 trace_id 应是同一个：%s / %s / %s", tid, al.Str("trace_id"), srv.TraceID)
		}
		// 两条日志都在服务端 Span 之内打的，span_id 就是服务端 Span 的
		if al.Str("span_id") != srv.SpanID || biz.Str("span_id") != srv.SpanID {
			t.Errorf("访问日志和业务日志的 span_id 应是服务端 Span 的 %s，实际 %s / %s", srv.SpanID, al.Str("span_id"), biz.Str("span_id"))
		}
	})

	t.Run("route 是路由模板，path 不带查询串", func(t *testing.T) {
		r := p.Get(t, fmt.Sprintf("/users/%d?cache=off&token=query-token-in-url", u.ID))
		l := accessLog(t, p, traceIDOf(t, r))
		if l.Str("route") != "/users/:id" || l.Str("path") != fmt.Sprintf("/users/%d", u.ID) {
			t.Errorf("route 应是 /users/:id、path 应是 /users/%d（不带查询串），实际 route=%q path=%q", u.ID, l.Str("route"), l.Str("path"))
		}
		// xgin/README.md：「path 不带查询串」——链接上的 ?token= 不会从这里进日志
		mustNotContain(t, "访问日志", l.Line, "query-token-in-url")
	})

	t.Run("上游带 traceparent 时日志里是上游的 trace_id", func(t *testing.T) {
		up, parent := randomTraceID(), randomSpanID()
		r := p.Get(t, "/ping", "traceparent", "00-"+up+"-"+parent+"-01")
		if got := traceIDOf(t, r); got != up {
			t.Fatalf("接上上游链路时 X-Trace-Id 应是上游的 %s，实际 %s", up, got)
		}
		l := accessLog(t, p, up)
		srv := serverSpan(t, p, up)
		if srv.ParentSpanID != parent || !srv.ParentRemote {
			t.Errorf("服务端 Span 的父应是上游的 %s（remote），实际 parent=%q remote=%v", parent, srv.ParentSpanID, srv.ParentRemote)
		}
		if l.Str("span_id") != srv.SpanID {
			t.Errorf("访问日志的 span_id 应是服务端 Span 的 %s，实际 %s", srv.SpanID, l.Str("span_id"))
		}
	})

	t.Run("指标端点不记访问日志", func(t *testing.T) {
		p.Metrics(t)
		p.Metrics(t)
		// 再发一个请求当界碑：它的访问日志出来了，前面 /metrics 的就该已经出来了
		accessLog(t, p, traceIDOf(t, p.Get(t, "/ping")))
		if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("path") == "/metrics" })); n != 0 {
			t.Errorf("xgin/README.md LogSkipPaths：Metric 开着时指标端点自动不记访问日志，实际记了 %d 条", n)
		}
	})

	t.Run("未匹配的路由也有访问日志", func(t *testing.T) {
		l := accessLog(t, p, traceIDOf(t, p.Get(t, "/no/such/route")))
		// xgin/README.md：没匹配上路由时 route 是 unmatched（和指标、Span 一致），真实路径看 path
		if l.Str("status") != "404" || l.Str("route") != "unmatched" || l.Str("path") != "/no/such/route" {
			t.Errorf("未匹配的路由应记 404、route=unmatched 和原始 path，实际 %s", l.Line)
		}
	})
}

// 访问日志能记的全打开之后，xgin/README.md「访问日志」那一节承诺的每一条脱敏：
// 按敏感词「含」匹配、任意嵌套、Unicode 折叠、表单、纯文本整段遮、请求头名单与词表、
// 值是 URL 的头去掉查询串、multipart / octet-stream 不读
func TestFunctional_RequestBodyLogMasksAllSecrets(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Overlay: payloadLogs})
	var secrets []string // 每个子测试用到的明文，最后在整个输出里再查一遍
	secret := func(label string) string {
		s := label + "-" + harness.NewID()
		secrets = append(secrets, s)
		return s
	}
	// bodyOf 请求体日志解回 map（它本身是一个 JSON 字符串）
	bodyOf := func(t *testing.T, l harness.Log, field string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(l.Str(field)), &m); err != nil {
			t.Fatalf("%s 应是脱敏后的 JSON，实际 %q（%v）", field, l.Str(field), err)
		}
		return m
	}

	t.Run("login 请求体的 password token 与响应体的 session_token", func(t *testing.T) {
		pw, tok := secret("pw"), secret("tok")
		r := p.PostJSON(t, "/login", map[string]string{"username": "carol", "password": pw, "token": tok})
		sess, _ := r.Map(t)["session_token"].(string)
		secrets = append(secrets, sess)
		l := accessLog(t, p, traceIDOf(t, r))
		req := bodyOf(t, l, "request_body")
		if req["password"] != redacted || req["token"] != redacted || req["username"] != "carol" {
			t.Errorf("文档说 body 里键含敏感词的值被遮、其余原样，实际 request_body=%s", l.Str("request_body"))
		}
		resp := bodyOf(t, l, "response_body")
		if resp["session_token"] != redacted || resp["username"] != "carol" {
			t.Errorf("文档说响应体同样脱敏（session_token 含 session/token），实际 response_body=%s", l.Str("response_body"))
		}
		mustNotContain(t, "访问日志", l.Line, pw, tok, sess)
	})

	t.Run("请求头：名单里的、名字含敏感词的都遮，其余原样", func(t *testing.T) {
		h := map[string]string{
			"Authorization":       "Bearer " + secret("auth"),
			"Cookie":              "sid=" + secret("cookie"),
			"Proxy-Authorization": "Basic " + secret("proxyauth"),
			"X-Api-Key":           secret("apikey"),
			"X-Auth-Token":        secret("authtoken"),
			"X-Csrf-Token":        secret("csrf"),    // 不在名单里，名字含 token
			"X-Session-Id":        secret("session"), // 不在名单里，名字含 session
			// 名字不含敏感词，只在名单里（service/main.go 用 AddSensitiveHeaders 加的）：
			// 上面那几个名单里的头名字都带敏感词，名单那一条不管用了它们照样被词表遮掉，只有这一个看得出来
			"X-Tenant-Id": secret("tenant"),
			"X-Visible":   "keep-me",
		}
		var kv []string
		for k, v := range h {
			kv = append(kv, k, v)
		}
		r := p.PostJSON(t, "/login", map[string]string{"username": "hdr"}, kv...)
		l := accessLog(t, p, traceIDOf(t, r))
		for k := range h {
			want := redacted
			if k == "X-Visible" {
				want = "keep-me"
			}
			if got := l.Str("request_headers." + k); got != want {
				t.Errorf("请求头 %s 应记成 %q，实际 %q", k, want, got)
			}
		}
		mustNotContain(t, "访问日志", l.Line, secrets...)
	})

	t.Run("值是 URL 的请求头去掉查询串和片段", func(t *testing.T) {
		code, tok, x := secret("oauthcode"), secret("urltok"), secret("fwd")
		r := p.PostJSON(t, "/login", map[string]string{"username": "ref"},
			"Referer", "https://example.com/cb?code="+code+"#frag",
			"X-Original-URL", "/internal/path?token="+tok,
			"X-Forwarded-URI", "/a/b#"+x)
		l := accessLog(t, p, traceIDOf(t, r))
		// 日志里的头名是 Go 规范化之后的写法：X-Original-URL 记成 X-Original-Url
		for k, want := range map[string]string{
			"Referer": "https://example.com/cb", "X-Original-URL": "/internal/path", "X-Forwarded-URI": "/a/b",
		} {
			if got := l.Str("request_headers." + textproto.CanonicalMIMEHeaderKey(k)); got != want {
				t.Errorf("文档说 %s 去掉 ? 和 # 之后的部分，应记 %q，实际 %q", k, want, got)
			}
		}
		mustNotContain(t, "访问日志", l.Line, code, tok, x)
	})

	t.Run("Content-Type 大写写法的 multipart 上传内容不读", func(t *testing.T) {
		content := []byte("file content with password=" + secret("upload") + "\n")
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {`form-data; name="file"; filename="notes.txt"`},
			"Content-Type":        {"text/plain"},
		})
		part.Write(content)
		mw.Close()
		// xgin/README.md：「Content-Type 大小写不敏感，Multipart/Form-Data 一样不读」
		ct := "Multipart/Form-Data; boundary=" + mw.Boundary()

		r := p.Do(t, http.MethodPost, "/upload", buf.Bytes(), "Content-Type", ct)
		sum := sha256.Sum256(content)
		m := r.Map(t)
		if r.Status != http.StatusOK || m["size"] != float64(len(content)) || m["sha256"] != hex.EncodeToString(sum[:]) {
			t.Errorf("handler 应收到完整的文件（记日志不动请求体），实际 %v", r)
		}
		l := accessLog(t, p, traceIDOf(t, r))
		if got := l.Str("request_body"); got != "[multipart/form-data omitted]" {
			t.Errorf("文档说 multipart 的请求体不读、只记一句 omitted，实际 request_body=%q", got)
		}
		mustNotContain(t, "访问日志", l.Line, secrets[len(secrets)-1])
	})

	t.Run("Content-Type 大写写法的 octet-stream 不读", func(t *testing.T) {
		b := secret("binary")
		r := p.Do(t, http.MethodPost, "/login", b, "Content-Type", "Application/Octet-Stream")
		l := accessLog(t, p, traceIDOf(t, r))
		if got := l.Str("request_body"); got != "[binary content omitted]" {
			t.Errorf("文档说 application/octet-stream 的请求体不读，实际 request_body=%q", got)
		}
		mustNotContain(t, "访问日志", l.Line, b)
	})

	t.Run("Unicode 折叠的字段名 ſecret 与 toKen 也被遮", func(t *testing.T) {
		s1, s2 := secret("longs"), secret("kelvin")
		// ſ 是 U+017F（长 s），K 是 U+212A（开尔文符号）：encoding/json 把它们绑到 Secret / Token 上
		r := p.PostJSON(t, "/login", map[string]string{"username": "fold", "ſecret": s1, "toKen": s2})
		l := accessLog(t, p, traceIDOf(t, r))
		req := bodyOf(t, l, "request_body")
		if req["ſecret"] != redacted || req["toKen"] != redacted {
			t.Errorf("文档说 Unicode 折叠后等于敏感词的键同样被遮，实际 request_body=%s", l.Str("request_body"))
		}
		mustNotContain(t, "访问日志", l.Line, s1, s2)
	})

	t.Run("嵌套对象与数组里的敏感字段", func(t *testing.T) {
		s1, s2, s3 := secret("newpw"), secret("oldpw"), secret("userpw")
		body := fmt.Sprintf(`{"username":"nest","profile":{"new_password":%q},"history":[{"old-password":%q}],"user.password":%q,"note":"keep"}`, s1, s2, s3)
		r := p.Do(t, http.MethodPost, "/login", body, "Content-Type", "application/json")
		l := accessLog(t, p, traceIDOf(t, r))
		req := bodyOf(t, l, "request_body")
		prof, _ := req["profile"].(map[string]any)
		hist, _ := req["history"].([]any)
		var old any
		if len(hist) == 1 {
			old = hist[0].(map[string]any)["old-password"]
		}
		if prof["new_password"] != redacted || old != redacted || req["user.password"] != redacted || req["note"] != "keep" {
			t.Errorf("文档说 new_password、user.password、数组里的 old-password 都认得出，实际 request_body=%s", l.Str("request_body"))
		}
		mustNotContain(t, "访问日志", l.Line, s1, s2, s3)
	})

	t.Run("反斜杠转义写出来的字段名", func(t *testing.T) {
		s := secret("escaped")
		// 键是 password，但 a 写成 JSON 转义（反斜杠、u、0061）：原始字节里没有 password 这个子串，
		// 字面预检认不出来，只有「见到反斜杠就交给解析器」那一条能拦住。
		// 转义用解释型字符串的 \\ 写出来，不写进反引号：原先这里的转义在落盘时被还原成了 a，
		// body 里就是明文的 password，测的其实是字面预检，那一条改坏了也没人发现
		body := "{\"username\":\"esc\",\"p\\u0061ssword\":\"" + s + "\"}"
		if strings.Contains(body, "password") || !strings.Contains(body, "\\u0061") {
			t.Fatalf("测试自己写错了：body 里应只有转义写法、没有 password 这个子串，实际 %s", body)
		}
		r := p.Do(t, http.MethodPost, "/login", body, "Content-Type", "application/json")
		l := accessLog(t, p, traceIDOf(t, r))
		if req := bodyOf(t, l, "request_body"); req["password"] != redacted {
			t.Errorf("转义之后的键就是 password，应被遮，实际 request_body=%s", l.Str("request_body"))
		}
		mustNotContain(t, "访问日志", l.Line, s)
	})

	t.Run("表单里百分号编码的字段名", func(t *testing.T) {
		s := secret("formpw")
		r := p.Do(t, http.MethodPost, "/login", "username=form&p%61ssword="+s, "Content-Type", "application/x-www-form-urlencoded")
		l := accessLog(t, p, traceIDOf(t, r))
		if got := l.Str("request_body"); !strings.Contains(got, "password="+redacted) || !strings.Contains(got, "username=form") {
			t.Errorf("文档说表单按键脱敏（p%%61ssword 就是 password），实际 request_body=%q", got)
		}
		mustNotContain(t, "访问日志", l.Line, s)
	})

	t.Run("纯文本 body 出现敏感词就整段遮掉", func(t *testing.T) {
		s := secret("plain")
		r := p.Do(t, http.MethodPost, "/login", "my api token is "+s, "Content-Type", "text/plain")
		l := accessLog(t, p, traceIDOf(t, r))
		if got := l.Str("request_body"); got != redacted {
			t.Errorf("文档说纯文本定位不了字段、出现敏感词就整个遮掉，实际 request_body=%q", got)
		}
		mustNotContain(t, "访问日志", l.Line, s)
	})

	t.Run("非敏感的值原样写回：大整数不丢精度，< > & 不转义", func(t *testing.T) {
		s := secret("bigint")
		body := `{"username":"a<b>&c","n":12345678901234567890,"password":"` + s + `"}`
		r := p.Do(t, http.MethodPost, "/login", body, "Content-Type", "application/json")
		l := accessLog(t, p, traceIDOf(t, r))
		got := l.Str("request_body")
		if !strings.Contains(got, `"n":12345678901234567890`) || !strings.Contains(got, `"username":"a<b>&c"`) {
			t.Errorf("文档说其余的值原样写回（大整数不丢精度，< > & 不被转义），实际 request_body=%s", got)
		}
		mustNotContain(t, "访问日志", l.Line, s)
	})

	t.Run("查询串逐字段脱敏", func(t *testing.T) {
		tok := secret("qtok")
		l := accessLog(t, p, traceIDOf(t, p.Get(t, "/users/1?page=2&access_token="+tok)))
		q, err := url.ParseQuery(l.Str("query"))
		if err != nil || q.Get("page") != "2" || q.Get("access_token") != redacted {
			t.Errorf("query 应留 page、遮掉 access_token，实际 %q", l.Str("query"))
		}
		mustNotContain(t, "访问日志", l.Line, tok)
	})

	t.Run("整个进程输出里一个明文都没有", func(t *testing.T) {
		mustNotContain(t, "进程的 stdout / stderr", p.Output(), secrets...)
		t.Logf("数字：核对了 %d 个明文", len(secrets))
	})
}

// PG 的密码在 DSN 里（harness 默认 e2e-secret-pw）。正常跑、出错、启动失败、DSN 写错，
// 日志、响应体、Span、/metrics 里都不该有它。
//
// xgorm/README.md XGorm：DSN 预检失败「不回传」pgx 的原始错误（那里面是整串 DSN）；
// Log: true 记的是带占位符的 SQL
func TestFunctional_PGPasswordNeverInLogsOrErrors(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := pgPassword(t)
	dsn := harness.PGDSN(harness.PGAddr())

	t.Run("正常跑一圈：SQL 日志、debug、请求体日志、Span 全开，中途断一次 PG", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		pg := harness.NewProxy(t, harness.PGAddr())
		p := harness.Start(t, harness.Options{
			PGAddr: pg.Addr(), Spans: true, Downstream: stub.URL,
			Overlay: sqlLog("default") + debugLogs + payloadLogs,
		})
		var bodies []string
		keep := func(r harness.Response) { bodies = append(bodies, string(r.Body)) }

		u := createUser(t, p, "pw-check", "pw@example.com")
		for _, path := range []string{
			fmt.Sprintf("/users/%d", u.ID), fmt.Sprintf("/users/%d", u.ID), fmt.Sprintf("/users/%d?cache=off", u.ID),
			"/users/999999999", "/users/abc", "/boom", "/proxy?token=t", "/stuck?ms=1&db=1",
		} {
			keep(p.Get(t, path))
		}
		keep(p.PostJSON(t, "/login", map[string]string{"username": "u", "password": "p"}))
		keep(p.PostJSON(t, "/orders", map[string]any{"user_id": u.ID, "amount": 1}))
		keep(p.PostJSON(t, "/orders", map[string]any{"user_id": u.ID, "amount": 1, "fail_at": 3, "rollback_fail_at": 1}))

		// 断开 PG：现有连接断掉、新连接被拒。这时的错误最可能把连接串带出来
		pg.Cut()
		start := time.Now()
		r := p.Get(t, fmt.Sprintf("/users/%d?cache=off", u.ID))
		cutElapsed := time.Since(start)
		keep(r)
		if r.Status != http.StatusInternalServerError {
			t.Errorf("PG 断开时 ?cache=off 读应 500，实际 %v", r)
		}
		// 写也要真的走到出错那一支：没断成的话它是 201，这一圈就少了一类会带出连接串的错误
		r = p.PostJSON(t, "/users", map[string]string{"name": "while-cut"})
		keep(r)
		if r.Status != http.StatusInternalServerError {
			t.Errorf("PG 断开时 POST /users 应 500，实际 %v", r)
		}
		pg.Restore()
		// 连接池里的坏连接在出错时被丢掉，恢复后很快就能读到；这里不断言恢复得多快，
		// 只是让进程回到健康状态再走优雅退出。最多试 50 次、间隔 50ms
		start = time.Now()
		attempts := 0
		for recovered := false; !recovered; {
			if attempts++; attempts > 50 {
				t.Fatalf("PG 恢复之后 %v 内仍读不到：%v", time.Since(start), r)
			}
			r = p.Get(t, fmt.Sprintf("/users/%d?cache=off", u.ID))
			keep(r)
			if recovered = r.Status == http.StatusOK; !recovered {
				time.Sleep(50 * time.Millisecond)
			}
		}
		t.Logf("数字：PG 断开时一次读用了 %v 返回 500；恢复后第 %d 次读回 200，用了 %v", cutElapsed, attempts, time.Since(start))

		metrics, err := http.Get(p.URL("/metrics"))
		if err == nil {
			var b bytes.Buffer
			b.ReadFrom(metrics.Body)
			metrics.Body.Close()
			mustNotContain(t, "/metrics", b.String(), pw)
		}
		exit := p.Terminate(t, 20*time.Second)
		if exit.Code != 0 {
			t.Errorf("SIGTERM 之后应以 0 退出，实际 %v", exit)
		}

		out := p.Output()
		spans, _ := os.ReadFile(p.SpanFile)
		mustNotContain(t, "进程的 stdout / stderr", out, pw, dsn)
		mustNotContain(t, "Span 文件", string(spans), pw)
		mustNotContain(t, "响应体", strings.Join(bodies, "\n"), pw)
		// 反过来确认这一圈真的走到了会出错、会记 SQL 的路径，不是空跑
		for _, msg := range []string{"SQL", "read user failed", "panic while handling request", "xflow rollback did not complete, resources may be left dangling"} {
			if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == msg })) == 0 {
				t.Errorf("这一圈应该产生过 %q 日志，一条都没有：核对范围不够", msg)
			}
		}
		t.Logf("数字：核对了 %d 字节输出、%d 字节 Span、%d 个响应体", len(out), len(spans), len(bodies))
	})

	// 启动失败时的错误由 MustRun 打到 stderr，xlog 装好之后的日志在 stdout
	startFails := func(t *testing.T, o harness.Options, secrets ...string) (harness.Exit, *harness.Process) {
		t.Helper()
		o.NoWait = true
		p := harness.Start(t, o)
		exit, ok := p.Wait(30 * time.Second)
		if !ok {
			t.Fatalf("启动应该失败退出，30s 了还在跑\n%s", p.Output())
		}
		if exit.Code == 0 {
			t.Fatalf("启动应该失败（非 0 退出），实际 %v\n%s", exit, p.Output())
		}
		if !strings.Contains(p.Stderr(), "xgorm") {
			t.Errorf("stderr 里应有 xgorm 报的错，实际：\n%s", p.Stderr())
		}
		mustNotContain(t, "进程的 stdout / stderr", p.Output(), secrets...)
		return exit, p
	}

	t.Run("启动失败：PG 拒绝连接", func(t *testing.T) {
		t.Parallel()
		addr := fmt.Sprintf("127.0.0.1:%d", harness.FreePort(t)) // 没人监听
		exit, p := startFails(t, harness.Options{PGAddr: addr}, pw)
		if !strings.Contains(p.Stderr(), "connection refused") {
			t.Errorf("错误里应说清是连不上（connection refused），实际：\n%s", p.Stderr())
		}
		// docs/behavior.md「启动期建连探测」：连不上时按 3 次重试
		t.Logf("数字：PG 拒绝连接时启动 %v 后失败退出（%v）", exit.Uptime, exit)
	})

	t.Run("启动失败：密码错误", func(t *testing.T) {
		t.Parallel()
		wrong := "wrong-pw-" + harness.NewID()
		bad := strings.Replace(dsn, ":"+pw+"@", ":"+wrong+"@", 1)
		exit, p := startFails(t, harness.Options{Env: map[string]string{"E2E_PG_DSN": bad}}, pw, wrong)
		if !strings.Contains(p.Stderr(), "password authentication failed") || !strings.Contains(p.Stderr(), "authentication to ") {
			t.Errorf("错误里应说清是认证失败，实际：\n%s", p.Stderr())
		}
		if strings.Contains(p.Stderr(), "cannot reach") {
			t.Errorf("认证失败不是连不上，不该报 cannot reach：\n%s", p.Stderr())
		}
		t.Logf("数字：密码错误时启动 %v 后失败退出", exit.Uptime)
	})

	for _, c := range []struct{ name, dsn string }{
		{"URL 里 sslmode 写错", strings.Replace(dsn, "sslmode=disable", "sslmode=bogus", 1)},
		{"URL 里 pgx 专有参数写错", dsn + "&default_query_exec_mode=bogus"},
		{"URL 里端口不是数字", strings.Replace(dsn, harness.PGAddr(), "127.0.0.1:notaport", 1)},
		// xgorm/README.md：pgx 的原始错误「只遮得住 password=x 这种规整写法，password = hunter2 原样带出」
		{"key=value 写法、等号两边有空格", "host=127.0.0.1 user=xone password = " + pw + " dbname=xone_e2e sslmode=bogus"},
		// xgorm/dsn.go parsePostgres：预检必须用 pgx.ParseConfig，只用 pgconn 的话 pgx 专有参数写错照样放行，
		// 错误留到 gorm.Open 才由 pgx 报出来、原文带着整串 DSN。URL 写法的密码 pgx 自己会遮成 xxxxx，
		// 所以上面那条 URL 的用例看不出密码漏没漏；这一条是 xgorm/README.md 点名的组合，漏了就是明文
		{"key=value 写法、等号两边有空格、pgx 专有参数写错", "host=127.0.0.1 user=xone password = " + pw + " dbname=xone_e2e sslmode=disable default_query_exec_mode=bogus"},
	} {
		t.Run("启动失败：DSN 写错（"+c.name+"）", func(t *testing.T) {
			t.Parallel()
			exit, p := startFails(t, harness.Options{Env: map[string]string{"E2E_PG_DSN": c.dsn}}, pw)
			if !strings.Contains(p.Stderr(), "failed to parse DSN") {
				t.Errorf("错误里应说清是 DSN 写错了，实际：\n%s", p.Stderr())
			}
			t.Logf("数字：%s 时启动 %v 后失败退出", c.name, exit.Uptime)
		})
	}
}

// xgorm/README.md XGorm.Log：「记的是带占位符的 SQL，不含参数值」
func TestFunctional_SQLLogHasPlaceholdersNotArgs(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Spans: true, Overlay: sqlLog("default")})
	name, email := "sql-name-"+harness.NewID(), "sql-email-"+harness.NewID()+"@example.com"
	r := p.PostJSON(t, "/users", map[string]string{"name": name, "email": email})
	tid := traceIDOf(t, r)
	l := p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "SQL" && l.Str("trace_id") == tid })
	logged := l.Str("sql")

	if !strings.HasPrefix(logged, "INSERT INTO") {
		t.Fatalf("POST /users 的 SQL 日志应是那条 INSERT，实际 %s", l.Line)
	}
	mustNotContain(t, "SQL 日志", l.Line, name, email)
	if _, ok := num(l, "elapsed_ms"); !ok || l.Str("rows_affected") != "1" {
		t.Errorf("SQL 日志应带 elapsed_ms 和 rows_affected=1，实际 %s", l.Line)
	}

	t.Run("日志里的 SQL 就是发给 PG 的那条", func(t *testing.T) {
		serverSpan(t, p, tid)
		ins := spansNamed(traceSpans(t, p, tid), "gorm.create")
		if len(ins) != 1 {
			t.Fatalf("这条链路上应有一个 gorm.create Span")
		}
		sent := ins[0].Str("db.query.text")
		// gorm v1.31.2 logger/sql.go ExplainSQL：PG 的 Dialector 先把 $1 改写成 $1$ 等着代入参数，
		// 而 ParamsFilter 返回的是空参数列表；xgorm/logger.go statement 负责把记号改回去
		if logged != sent {
			t.Errorf("SQL 日志和 Span 的 db.query.text 应是同一条语句：日志 %q，Span %q", logged, sent)
		}
		if !strings.Contains(logged, "$1") || strings.Contains(logged, "$1$") {
			t.Errorf("PG 的 SQL 日志应带 $1 这样的占位符（发给 PG 的原样），实际 %q", logged)
		}
	})
}

// xredis/README.md XRedis.Log：「只记命令名和第一个 key，不记值、不记其余参数」「key 不存在（redis.Nil）不算失败」
//
// GET /users/:id 读穿：Redis 里没有 → GET 拿到 nil → 读 PG → SET 把整个用户（带 name、email）写回 Redis。
// 值就是那段 JSON，进程的整段输出里都不该有它
func TestFunctional_RedisLogHasKeysNotValues(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Overlay: "XRedis:\n  Log: true\n"})
	name, email := "redis-name-"+harness.NewID()+harness.NewID(), "redis-email-"+harness.NewID()+harness.NewID()+"@example.com"
	var u user
	p.PostJSON(t, "/users", map[string]string{"name": name, "email": email}).JSON(t, &u)
	r := p.Get(t, fmt.Sprintf("/users/%d", u.ID))
	if r.Status != http.StatusOK {
		t.Fatalf("GET /users/:id 该 200，实际 %v", r)
	}
	tid := traceIDOf(t, r)
	key := p.KeyPrefix + "user:" + fmt.Sprint(u.ID)

	cmd := func(c string) harness.Log {
		return p.WaitLog(t, waitFor, func(l harness.Log) bool {
			return l.Msg() == "redis command" && l.Str("cmd") == c && l.Str("trace_id") == tid
		})
	}
	get, set := cmd("get"), cmd("set")
	for _, l := range []harness.Log{get, set} {
		if l.Str("key") != key || l.Str("name") != "default" || l.Level() != "INFO" {
			t.Errorf("命令日志该带实例名 default、key %s，级别 INFO，实际 %s", key, l.Line)
		}
		if _, ok := num(l, "elapsed_ms"); !ok {
			t.Errorf("命令日志该带 elapsed_ms，实际 %s", l.Line)
		}
	}
	if v, _ := get.Get("nil"); v != true {
		t.Errorf("缓存没命中的 GET 该带 nil=true（不是失败），实际 %s", get.Line)
	}
	if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "redis command failed" })); n != 0 {
		t.Errorf("redis.Nil 不该记成 redis command failed，实际 %d 行", n)
	}
	mustNotContain(t, "进程的 stdout / stderr", p.Output(), name, email)
}

// xhttp/README.md XHttp.Log：「每个逻辑请求记一条日志」「查询串……一律不记，错误原文里 URL 的查询串也去掉」
//
// /proxy?token=… 把 token 放进出站 URL 的查询串。成功、下游 5xx（配了重试也只一次尝试）、
// 传输层错误（重试用完）三种各看一遍：xhttp 的那一行里有路径和尝试次数，没有 token
func TestFunctional_HTTPClientLogHasNoQuery(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	const overlay = "XHttp:\n  Timeout: 2s\n  Log: true\n  RetryCount: 2\n  RetryWaitTime: 10ms\n  RetryMaxWaitTime: 10ms\n"
	for _, c := range []struct {
		name     string
		status   int // 下游回的状态码；0 表示下游连不上
		msg      string
		attempts float64
	}{
		{"下游 200", http.StatusOK, "http request", 1},
		{"下游 500_拿到响应不重试", http.StatusInternalServerError, "http request failed", 1},
		{"下游连不上_重试用完", 0, "http request failed", 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			downstream := fmt.Sprintf("http://127.0.0.1:%d", harness.FreePort(t)) // 没人监听
			if c.status != 0 {
				stub := harness.NewStub(t)
				stub.SetStatus(c.status)
				downstream = stub.URL
			}
			p := harness.Start(t, harness.Options{Downstream: downstream, Overlay: overlay})
			token := "tok-" + harness.NewID() + harness.NewID()
			r := p.Get(t, "/proxy?token="+token)
			tid := traceIDOf(t, r)

			l := p.WaitLog(t, waitFor, func(l harness.Log) bool {
				return strings.HasPrefix(l.Msg(), "http request") && l.Str("trace_id") == tid
			})
			if l.Msg() != c.msg || l.Str("method") != http.MethodGet || l.Str("path") != "/echo" {
				t.Errorf("该是一行 %s、GET /echo，实际 %s", c.msg, l.Line)
			}
			if got, _ := num(l, "attempts"); got != c.attempts {
				t.Errorf("attempts 该是 %v，实际 %s", c.attempts, l.Line)
			}
			if got, _ := num(l, "status"); int(got) != c.status {
				t.Errorf("status 该是 %d，实际 %s", c.status, l.Line)
			}
			if c.status == 0 && !strings.Contains(l.Str("error"), "connection refused") {
				t.Errorf("连不上时 error 该说清原因，实际 %s", l.Line)
			}
			if n := len(p.FindLogs(func(l harness.Log) bool { return strings.HasPrefix(l.Msg(), "http request") })); n != 1 {
				t.Errorf("一个逻辑请求一行，实际 %d 行", n)
			}
			if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xhttp resty log" })); n != 0 {
				t.Errorf("Log 开着时 resty 的重试日志不再打，实际 %d 行", n)
			}
			// 框架写的每一行都不该有它。业务代码自己那句 downstream call failed 把 err 原样记下（带整条 URL），
			// 那是 service/routes.go 的写法，不归 xhttp 管，这里跳过
			for _, fl := range p.Logs() {
				if fl.Msg() != "downstream call failed" {
					mustNotContain(t, "日志 "+fl.Msg(), fl.Line, token)
				}
			}
			mustNotContain(t, "进程的 stderr", p.Stderr(), token)
			t.Logf("数字：%s：%s", c.name, l.Line)
		})
	}
}
