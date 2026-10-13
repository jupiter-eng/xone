package xhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
)

// secret 一个长随机串：「日志里不该出现」的检查用它，短的数字会和时间戳、耗时撞上
func secret(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}

// captureLogs 把 slog 默认 logger 换成写 JSON 进 buffer 的，返回取「全部原文」和「解析后的各行」的函数
func captureLogs(t *testing.T) (raw func() string, parsed func() []map[string]any) {
	t.Helper()
	var buf strings.Builder
	w := &lockedWriter{w: &buf}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	raw = func() string {
		w.mu.Lock()
		defer w.mu.Unlock()
		return buf.String()
	}
	parsed = func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(raw()), "\n") {
			if line == "" {
				continue
			}
			m := map[string]any{}
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("日志不是 JSON：%v，内容=%q", err, line)
			}
			out = append(out, m)
		}
		return out
	}
	return raw, parsed
}

// requestLines 取这几种消息的日志
func requestLines(all []map[string]any, msgs ...string) []map[string]any {
	var out []map[string]any
	for _, l := range all {
		for _, m := range msgs {
			if l["msg"] == m {
				out = append(out, l)
			}
		}
	}
	return out
}

var allRequestMsgs = []string{"http request", "http request failed", "slow http request"}

// logClient Log 开着、链路和指标关着的客户端；不装 newQuiet 的 discardLogger，resty 的日志照样进 slog
func logClient(t *testing.T, mutate ...func(*Config)) *resty.Client {
	t.Helper()
	c := DefaultConfig()
	c.Log, c.Trace, c.Metric = true, false, false
	c.RetryWaitTime, c.RetryMaxWaitTime = time.Millisecond, time.Millisecond
	for _, m := range mutate {
		m(&c)
	}
	client, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	return client
}

// cutServer 每个请求都把连接掐断：传输层错误，resty 会重试
func cutServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLog_SuccessOneInfoLineWithoutQuery(t *testing.T) {
	token := secret(t, "token")
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	raw, lines := captureLogs(t)
	client := logClient(t)
	if _, err := client.R().SetHeader("Authorization", "Bearer "+token).SetBody(token).
		Post(srv.URL + "/api/items?token=" + token + "#frag"); err != nil {
		t.Fatal(err)
	}

	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 {
		t.Fatalf("一个请求一行，got=%v", lines())
	}
	l := got[0]
	host := strings.TrimPrefix(srv.URL, "http://")
	if l["msg"] != "http request" || l["level"] != "INFO" || l["method"] != "POST" || l["host"] != host ||
		l["path"] != "/api/items" || l["status"] != float64(200) || l["attempts"] != float64(1) {
		t.Errorf("字段不对，got=%v", l)
	}
	if _, ok := l["elapsed_ms"].(float64); !ok {
		t.Errorf("该带 elapsed_ms，got=%v", l)
	}
	if _, ok := l["error"]; ok {
		t.Errorf("成功的请求不带 error，got=%v", l)
	}
	if strings.Contains(raw(), token) {
		t.Errorf("查询串、Header、body 里的令牌进了日志：%s", raw())
	}
}

func TestLog_5xxWithRetriesConfiguredIsOneWarnLine(t *testing.T) {
	// 拿到了响应就不重试，5xx 也不重试：一次尝试，一行 WARN
	token := secret(t, "token")
	var hits atomic.Int32
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(500) })
	raw, lines := captureLogs(t)
	client := logClient(t, func(c *Config) { c.RetryCount = 2 })
	resp, err := client.R().Get(srv.URL + "/x?token=" + token)
	if err != nil || resp.StatusCode() != 500 {
		t.Fatalf("前提：该拿到 500，got=%v %v", resp, err)
	}
	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 {
		t.Fatalf("一个请求一行，got=%v", lines())
	}
	if l := got[0]; l["msg"] != "http request failed" || l["level"] != "WARN" || l["status"] != float64(500) || l["attempts"] != float64(hits.Load()) {
		t.Errorf("5xx 该记 WARN 的 http request failed、带状态码和尝试次数（下游收到 %d 次），got=%v", hits.Load(), l)
	}
	if strings.Contains(raw(), token) {
		t.Errorf("令牌进了日志：%s", raw())
	}
}

func TestLog_4xxIsInfo(t *testing.T) {
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	_, lines := captureLogs(t)
	client := logClient(t)
	client.R().Get(srv.URL + "/missing")
	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 || got[0]["msg"] != "http request" || got[0]["status"] != float64(404) {
		t.Errorf("4xx 是下游的业务回答，记 INFO，got=%v", lines())
	}
}

func TestLog_TransportErrorAfterRetriesOneLineNoQuery(t *testing.T) {
	// resty v2.17.2 在所有重试结束之后只调一次 OnError，Attempt 是用掉的尝试次数
	token := secret(t, "token")
	var hits atomic.Int32
	srv := cutServer(t, &hits)
	raw, lines := captureLogs(t)
	client := logClient(t, func(c *Config) { c.RetryCount = 2 })
	u := strings.Replace(srv.URL, "http://", "http://someone:"+token+"@", 1) + "/cut?token=" + token
	if _, err := client.R().Get(u); err == nil {
		t.Fatal("前提：连接被掐断，该失败")
	}

	all := lines()
	got := requestLines(all, allRequestMsgs...)
	if len(got) != 1 {
		t.Fatalf("重试 2 次也只记一行，got=%v", all)
	}
	l := got[0]
	if l["msg"] != "http request failed" || l["level"] != "WARN" || l["status"] != float64(0) || l["attempts"] != float64(3) || l["path"] != "/cut" {
		t.Errorf("传输层错误：WARN、status 0、attempts 3，got=%v", l)
	}
	e, _ := l["error"].(string)
	if !strings.Contains(e, "/cut") || !strings.Contains(e, "EOF") {
		t.Errorf("error 该留着去掉查询串的 URL 和原因，got=%q", e)
	}
	if strings.Contains(raw(), token) || strings.Contains(raw(), "someone") {
		t.Errorf("查询串或 userinfo 从错误原文进了日志：%s", raw())
	}
	// 同一件事不说两遍：resty 自己重试路径上的 WARN（Attempt N）和 ERROR 不再打
	if n := len(requestLines(all, "xhttp resty log")); n != 0 {
		t.Errorf("Log 开着时 resty 的重试日志不该再打，got %d 行：%v", n, all)
	}
}

func TestLog_TransportErrorRefusedNoQuery(t *testing.T) {
	token := secret(t, "token")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	raw, lines := captureLogs(t)
	client := logClient(t)
	client.R().Get("http://" + addr + "/x?token=" + token)
	got := requestLines(lines(), "http request failed")
	if len(got) != 1 || !strings.Contains(fmt.Sprint(got[0]["error"]), "connection refused") {
		t.Fatalf("连不上该记一行 http request failed、带原因，got=%v", lines())
	}
	if strings.Contains(raw(), token) {
		t.Errorf("令牌进了日志：%s", raw())
	}
}

func TestLog_OffMeansNoRequestLinesAndRestyLogsUnchanged(t *testing.T) {
	var hits atomic.Int32
	srv := cutServer(t, &hits)
	_, lines := captureLogs(t)
	client := logClient(t, func(c *Config) { c.Log, c.RetryCount = false, 1 })
	client.R().Get(srv.URL)
	all := lines()
	if got := requestLines(all, allRequestMsgs...); len(got) != 0 {
		t.Errorf("Log 默认关着，不该有请求日志，got=%v", got)
	}
	// 关着时 resty 自己的重试日志照旧（每次尝试一行 WARN，用完一行 ERROR）
	if n := len(requestLines(all, "xhttp resty log")); n != 3 {
		t.Errorf("Log 关着时 resty 的重试日志照旧 3 行，got=%v", all)
	}
}

func TestLog_OtherRestyWarningsStillLogged(t *testing.T) {
	// 只挑掉重试路径那两种，resty 别的提醒照旧：比如明文 HTTP 上用 Basic Auth
	srv, _ := echo(t, nil)
	_, lines := captureLogs(t)
	client := logClient(t)
	client.R().SetBasicAuth("u", "p").Get(srv.URL)
	for _, l := range requestLines(lines(), "xhttp resty log") {
		if strings.Contains(fmt.Sprint(l["detail"]), "Basic Auth") {
			return
		}
	}
	t.Errorf("resty 的其余提醒不该被吞掉，got=%v", lines())
}

func TestLog_SlowThreshold(t *testing.T) {
	for _, c := range []struct {
		name      string
		threshold time.Duration
		wantMsg   string
	}{
		{"over threshold", 20 * time.Millisecond, "slow http request"},
		{"zero disables", 0, "http request"},
		{"under threshold", time.Second, "http request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(60 * time.Millisecond) })
			_, lines := captureLogs(t)
			client := logClient(t, func(cc *Config) { cc.SlowThreshold = c.threshold })
			client.R().Get(srv.URL)
			got := requestLines(lines(), allRequestMsgs...)
			if len(got) != 1 || got[0]["msg"] != c.wantMsg {
				t.Fatalf("该是一行 %s，got=%v", c.wantMsg, lines())
			}
			if c.wantMsg == "slow http request" && (got[0]["level"] != "WARN" || got[0]["threshold_ms"] != float64(20)) {
				t.Errorf("慢请求该是 WARN、带 threshold_ms，got=%v", got[0])
			}
		})
	}
}

func TestLog_ElapsedCoversRetriesWithMetricOff(t *testing.T) {
	// 起点要在 Metric 关着时照样记下，否则耗时只是最后一次尝试
	var hits atomic.Int32
	srv := cutServer(t, &hits)
	_, lines := captureLogs(t)
	const backoff = 60 * time.Millisecond
	client := logClient(t, func(c *Config) {
		c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, backoff, backoff
	})
	client.R().Get(srv.URL)
	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 {
		t.Fatalf("got=%v", lines())
	}
	// 两次退避，每次 resty 在 [wait, maxWait] 里取值，这里上下界相同
	if e, _ := got[0]["elapsed_ms"].(float64); e < float64(2*backoff/time.Millisecond)*0.9 {
		t.Errorf("耗时该算整次逻辑请求（含两次 %v 的退避），got %vms", backoff, e)
	}
}

func TestLog_UsesCallerCtx(t *testing.T) {
	// ctx 原样交给 slog：xlog 从这里取 trace_id / span_id
	type key struct{}
	srv, _ := echo(t, nil)
	var mu sync.Mutex
	var seen []any
	old := slog.Default()
	slog.SetDefault(slog.New(ctxHandler{fn: func(ctx context.Context, r slog.Record) {
		if r.Message == "http request" {
			mu.Lock()
			seen = append(seen, ctx.Value(key{}))
			mu.Unlock()
		}
	}}))
	t.Cleanup(func() { slog.SetDefault(old) })
	client := logClient(t)
	client.R().SetContext(context.WithValue(context.Background(), key{}, "caller")).Get(srv.URL)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "caller" {
		t.Errorf("日志该用调用方的 ctx，got=%v", seen)
	}
}

type ctxHandler struct {
	fn func(context.Context, slog.Record)
}

func (h ctxHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h ctxHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h ctxHandler) WithGroup(string) slog.Handler            { return h }
func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	h.fn(ctx, r)
	return nil
}

func TestScrubText(t *testing.T) {
	for in, want := range map[string]string{
		`Get "http://user:***@h:1/x?token=a": EOF`:    `Get "http://h:1/x": EOF`,
		`Get "https://u@h/x": EOF`:                    `Get "https://h/x": EOF`,
		`Get "http://h/x": context deadline exceeded`: `Get "http://h/x": context deadline exceeded`,
	} {
		if got := scrubText(in); got != want {
			t.Errorf("scrubText(%q)\n got=%q\nwant=%q", in, got, want)
		}
	}
}

func TestConfig_LogDefaults(t *testing.T) {
	c := DefaultConfig()
	if c.Log || c.SlowThreshold != time.Second {
		t.Errorf("默认 Log=false、SlowThreshold=1s，got %v %v", c.Log, c.SlowThreshold)
	}
	c.SlowThreshold = -time.Millisecond
	if err := c.Validate(); err == nil {
		t.Error("SlowThreshold 为负应当报错")
	}
}

func TestLog_RedirectPolicyErrorsScrubbed(t *testing.T) {
	// 重定向策略拒绝时，标准库把 Location 原样放进 *url.Error 的 URL：相对的 Location
	// 没有 http://，按文本找 URL 的办法认不出来
	token := secret(t, "token")
	mux := http.NewServeMux()
	mux.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/b?token="+token+"#frag", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop?token="+token, http.StatusFound)
	})
	mux.HandleFunc("/space", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/a b?token="+token)
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/badloc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/b%zz?token="+token)
		w.WriteHeader(http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	for _, c := range []struct {
		name, path, want string
		policy           resty.RedirectPolicy
		status           float64
	}{
		{"NoRedirectPolicy", "/redir", `Get "/b": auto redirect is disabled`, resty.NoRedirectPolicy(), 302},
		{"Location 里问号前有空格", "/space", `Get "/a%20b": auto redirect is disabled`, resty.NoRedirectPolicy(), 302},
		{"FlexibleRedirectPolicy 用完", "/loop", `Get "/loop": stopped after 2 redirects`, resty.FlexibleRedirectPolicy(2), 302},
		{"Location 转义不合法", "/badloc", `failed to parse Location header "/b%zz"`, nil, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw, lines := captureLogs(t)
			client := logClient(t)
			if c.policy != nil {
				client.SetRedirectPolicy(c.policy)
			}
			if _, err := client.R().Get(srv.URL + c.path); err == nil {
				t.Fatal("前提：该失败")
			}
			got := requestLines(lines(), allRequestMsgs...)
			if len(got) != 1 || got[0]["msg"] != "http request failed" || got[0]["status"] != c.status {
				t.Fatalf("该是一行 http request failed、status %v，got=%v", c.status, lines())
			}
			if e, _ := got[0]["error"].(string); !strings.Contains(e, c.want) {
				t.Errorf("error 该留着去掉查询串的 Location 和原因\n got=%q\nwant 含 %q", e, c.want)
			}
			if strings.Contains(raw(), token) {
				t.Errorf("Location 里的令牌进了日志：%s", raw())
			}
		})
	}
}

func TestLog_QueryWithRawSpaceScrubbed(t *testing.T) {
	// 查询串里有未转义的空格：按文本找的话到空格就停了，空格后面的令牌留在日志里
	token := secret(t, "token")
	raw, lines := captureLogs(t)
	client := logClient(t)
	client.R().Get("http://" + deadAddr(t) + "/x?q=hello world&token=" + token)
	got := requestLines(lines(), "http request failed")
	if len(got) != 1 || !strings.Contains(fmt.Sprint(got[0]["error"]), `/x": dial tcp`) {
		t.Fatalf("该是一行 http request failed、error 里的 URL 只到路径，got=%v", lines())
	}
	if strings.Contains(raw(), token) || strings.Contains(raw(), "world") {
		t.Errorf("空格后面的查询串进了日志：%s", raw())
	}
}

func TestScrubError(t *testing.T) {
	ue := func(u string) error { return &url.Error{Op: "Get", URL: u, Err: io.EOF} }
	for _, c := range []struct {
		err  error
		want string
	}{
		{ue("http://user:***@h:1/x?token=a#f"), `Get "http://h:1/x": EOF`},
		{ue("/b?token=a"), `Get "/b": EOF`},
		{ue("b?token=a"), `Get "b": EOF`},
		{ue("http://h/x?q=hello world&token=a"), `Get "http://h/x": EOF`},
		{ue("/a b?token=a"), `Get "/a%20b": EOF`},                   // 问号前有空格：按文本认不出来，只能按结构
		{ue("http://h/%zz x?token=a"), `Get "http://h/%zz x": EOF`}, // 解析不了：切在第一个 ? 上
		{ue("ws://u:p@h/%zz#token=a"), `Get "ws://h/%zz": EOF`},     // 解析不了也去掉 userinfo
		{fmt.Errorf("wrapped: %w", ue("/w x?token=a")), `wrapped: Get "/w%20x": EOF`},
		{errors.Join(ue("/a b?token=a"), ue("/c d?token=b")), "Get \"/a%20b\": EOF\nGet \"/c%20d\": EOF"},
		{errors.New(`free text "/c?token=a" and http://h/d?token=a`), `free text "/c" and http://h/d`},
	} {
		got := scrubError(c.err)
		if got != c.want {
			t.Errorf("scrubError(%q)\n got=%q\nwant=%q", c.err, got, c.want)
		}
		if strings.Contains(got, "token") {
			t.Errorf("scrubError(%q) 留下了查询串：%q", c.err, got)
		}
	}
}

func TestLog_UserLoggerStillGetsRestyWarnings(t *testing.T) {
	// Log 开着也不能替使用者换掉他自己设的 logger：明文 HTTP 上用 Basic Auth 这类提醒要到他那里
	srv, _ := echo(t, nil)
	for _, c := range []struct {
		name string
		req  func(*resty.Client, resty.Logger) *resty.Request
	}{
		{"client.SetLogger", func(c *resty.Client, l resty.Logger) *resty.Request { return c.SetLogger(l).R() }},
		{"R().SetLogger", func(c *resty.Client, l resty.Logger) *resty.Request { return c.R().SetLogger(l) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := logClient(t)
			my := &recLogger{}
			if _, err := c.req(client, my).SetBasicAuth("u", "p").Get(srv.URL); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(my.String(), "Basic Auth") {
				t.Errorf("使用者自己的 logger 该收到 resty 的 Basic Auth 提醒，got=%q", my.String())
			}
		})
	}
}

func TestLog_RestyConfigErrorsStillLogged(t *testing.T) {
	// 配置调用被忽略时 resty 只打一行 ERROR（格式也是 "%v"），Log 开着不能把它当成重试用完的那行吞掉。
	// transport 外面包着链路那几层，不是 *http.Transport，SetProxy 就只打这一行、什么也不做
	_, lines := captureLogs(t)
	client := logClient(t)
	client.SetProxy("http://proxy.internal:8080")
	for _, l := range requestLines(lines(), "xhttp resty log") {
		if l["level"] == "ERROR" && strings.Contains(fmt.Sprint(l["detail"]), "not an *http.Transport") {
			return
		}
	}
	t.Errorf("SetProxy 被忽略的那行 ERROR 不该被吞掉，got=%v", lines())
}

// recLogger 记下收到的每一行
type recLogger struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *recLogger) add(level, f string, v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, level+" "+f+"\n", v...)
}
func (l *recLogger) Errorf(f string, v ...any) { l.add("ERROR", f, v...) }
func (l *recLogger) Warnf(f string, v ...any)  { l.add("WARN", f, v...) }
func (l *recLogger) Debugf(f string, v ...any) { l.add("DEBUG", f, v...) }
func (l *recLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestLog_FailureBeforeAnyAttemptElapsedZero(t *testing.T) {
	// SRV 查不到时 resty 还没开始第一次尝试就调 OnError：起点没记下，Request.Time 也是零值
	_, lines := captureLogs(t)
	client := logClient(t)
	client.R().SetSRV(&resty.SRVRecord{Service: "nope", Domain: "invalid.invalid"}).Get("/x")
	got := requestLines(lines(), "http request failed")
	if len(got) != 1 || got[0]["elapsed_ms"] != float64(0) || got[0]["attempts"] != float64(0) {
		t.Errorf("一次都没发出去：elapsed_ms 0、attempts 0，got=%v", lines())
	}
}

func TestLog_DecodeErrorOn200IsFailed(t *testing.T) {
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{not json"))
	})
	_, lines := captureLogs(t)
	client := logClient(t)
	var out struct{ A int }
	if _, err := client.R().SetResult(&out).Get(srv.URL); err == nil {
		t.Fatal("前提：SetResult 解析失败该返回错误")
	}
	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 || got[0]["msg"] != "http request failed" || got[0]["status"] != float64(200) ||
		!strings.Contains(fmt.Sprint(got[0]["error"]), "invalid character") {
		t.Errorf("200 但解不开 SetResult：记 http request failed、status 200、带解析错误，got=%v", lines())
	}
}

func TestLog_SlowAndFailedLogsFailed(t *testing.T) {
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(40 * time.Millisecond); w.WriteHeader(503) })
	_, lines := captureLogs(t)
	client := logClient(t, func(c *Config) { c.SlowThreshold = 10 * time.Millisecond })
	client.R().Get(srv.URL)
	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 || got[0]["msg"] != "http request failed" || got[0]["status"] != float64(503) {
		t.Errorf("又慢又失败记 http request failed，不记成慢请求，got=%v", lines())
	}
}

func TestLog_WarnLevelHandlerStillGetsProblems(t *testing.T) {
	// slog 只收 WARN 时跳过拼字段的捷径，不能把失败和慢请求也跳过
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(40 * time.Millisecond)
		}
		if r.URL.Path == "/fail" {
			w.WriteHeader(500)
		}
	})
	var buf strings.Builder
	w := &lockedWriter{w: &buf}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })
	client := logClient(t, func(c *Config) { c.SlowThreshold = 10 * time.Millisecond })
	for _, p := range []string{"/ok", "/fail", "/slow"} {
		client.R().Get(srv.URL + p)
	}
	w.mu.Lock()
	out := buf.String()
	w.mu.Unlock()
	if strings.Count(out, `"msg":"http request failed"`) != 1 || strings.Count(out, `"msg":"slow http request"`) != 1 ||
		strings.Contains(out, `"msg":"http request"`) {
		t.Errorf("只收 WARN 的 handler 该拿到失败和慢请求各一行、不拿成功的，got=%s", out)
	}
}

func TestLog_DoNotParseResponseElapsedIsTimeToHeaders(t *testing.T) {
	// 响应体交给调用方自己读：resty 拿到响应头就算完，耗时里不含读 body
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond)
	})
	_, lines := captureLogs(t)
	client := logClient(t)
	resp, err := client.R().SetDoNotParseResponse(true).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.RawBody().Close()
	got := requestLines(lines(), allRequestMsgs...)
	if e, _ := got[0]["elapsed_ms"].(float64); len(got) != 1 || e >= 100 {
		t.Errorf("SetDoNotParseResponse 时 elapsed_ms 是到响应头的时间，got=%v", lines())
	}
}

func TestLog_RejectedBeforeSendingNoLine(t *testing.T) {
	// resty 在发出去之前就拒掉的（GET 带 multipart）走 OnInvalid，不走 OnError
	srv, _ := echo(t, nil)
	_, lines := captureLogs(t)
	client := logClient(t)
	if _, err := client.R().SetMultipartField("f", "a.txt", "text/plain", strings.NewReader("x")).Get(srv.URL); err == nil {
		t.Fatal("前提：GET 带 multipart 该被 resty 拒掉")
	}
	if got := requestLines(lines(), allRequestMsgs...); len(got) != 0 {
		t.Errorf("没发出去的请求没有请求日志，got=%v", got)
	}
}

func TestMs_KeepsSubMillisecond(t *testing.T) {
	if got := ms(1500 * time.Microsecond); got != 1.5 {
		t.Errorf("ms(1.5ms) = %v，want 1.5", got)
	}
	if got := ms(250 * time.Microsecond); got != 0.25 {
		t.Errorf("ms(250µs) = %v，want 0.25", got)
	}
}

// deadAddr 一个没人监听的地址
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestLog_PathIsEscaped(t *testing.T) {
	// 路径记转义过的形式：/a%2Fb 不记成 /a/b
	srv, _ := echo(t, nil)
	_, lines := captureLogs(t)
	client := logClient(t)
	client.R().Get(srv.URL + "/a%2Fb/c%20d")
	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 || got[0]["path"] != "/a%2Fb/c%20d" {
		t.Errorf("path 该是转义过的 /a%%2Fb/c%%20d，got=%v", lines())
	}
}
