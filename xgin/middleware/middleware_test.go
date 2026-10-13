package middleware

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone/xlog"
)

func init() { gin.SetMode(gin.TestMode) }

// capture 把默认 logger 换成一个真的 xlog 实例，返回取解析结果的函数。
//
// 必须用 xlog 而不是随便一个 slog handler：LogScope 往 context 里塞的字段
// 是 xlog 的 handler 负责取出来的，换成别的 handler 就什么都看不到——
// 这也正是「LogScope 只在用 xlog 时生效」这件事的实际含义。
func capture(t *testing.T) func() []map[string]any {
	t.Helper()
	dir := t.TempDir()
	c := xlog.DefaultConfig()
	c.Console = false
	c.File = xlog.FileConfig{Enable: true, Path: dir, Name: "app.log", RotateTime: time.Hour, Perm: "0644"}

	l, closer, err := xlog.New(c)
	if err != nil {
		t.Fatal(err)
	}
	old := slog.Default()
	slog.SetDefault(l)
	t.Cleanup(func() { slog.SetDefault(old); closer.Close() })

	return func() []map[string]any {
		closer.Close() // 冲刷出来再读
		files, _ := filepath.Glob(filepath.Join(dir, "app.log.*"))
		if len(files) == 0 {
			return nil
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
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
}

// serve 装上中间件跑一次请求
func serve(t *testing.T, req *http.Request, mws []gin.HandlerFunc, h gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	e := gin.New()
	e.Use(mws...)
	e.Any("/hello/:id", h)
	e.Any("/hello", h)

	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func get(path string) *http.Request { return httptest.NewRequest("GET", path, nil) }

// ---- LogScope ----

func TestLogScope_HandlersCanAddFieldsAtAnyDepth(t *testing.T) {
	// 调用栈深处拿不到 *gin.Context，本来就没机会把新 context 回传上来
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{LogScope(), Log()}, func(c *gin.Context) {
		deepInBusinessCode(c.Request.Context())
		c.String(200, "ok")
	})

	got := lines()
	if len(got) == 0 {
		t.Fatal("没有访问日志")
	}
	last := got[len(got)-1]
	if last["用户"] != "u9" {
		t.Errorf("业务补的字段应出现在访问日志里，got=%v", last)
	}
}

func deepInBusinessCode(ctx context.Context) { xlog.AddKV(ctx, "用户", "u9") }

// ---- Recover ----

func TestRecover_CatchesPanicAndReturns500(t *testing.T) {
	lines := capture(t)
	w := serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) {
		panic("炸了")
	})

	if w.Code != 500 {
		t.Errorf("应返回 500，got=%d", w.Code)
	}
	got := lines()
	if len(got) == 0 || got[0]["level"] != "ERROR" {
		t.Fatalf("应记一条 error 日志，got=%v", got)
	}
	if got[0]["stack"] == nil || !strings.Contains(got[0]["stack"].(string), "middleware_test.go") {
		t.Errorf("应带上栈信息，got=%v", got[0])
	}
	if got[0]["path"] != "/hello" {
		t.Errorf("应带上请求路径，got=%v", got[0])
	}
}

func TestRecover_ErrAbortHandlerRepanicsToNetHTTP(t *testing.T) {
	// http.ErrAbortHandler 是 handler 主动说「断掉这个连接」的约定写法，
	// net/http 收到它会静默断连、不打栈。兜住它变成 500 的话，
	// 本该被中止的响应照常发出去了，还多一份毫无意义的 panic 栈
	lines := capture(t)
	var got any
	func() {
		defer func() { got = recover() }()
		serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) {
			panic(http.ErrAbortHandler)
		})
	}()
	if got != http.ErrAbortHandler {
		t.Fatalf("ErrAbortHandler 该原样抛出去，got=%v", got)
	}
	for _, l := range lines() {
		if l["msg"] == "panic while handling request" {
			t.Errorf("主动中止不是故障，不该打 panic 日志，got=%v", l)
		}
	}
}

func TestRecover_CustomResponse(t *testing.T) {
	capture(t)
	w := serve(t, get("/hello"), []gin.HandlerFunc{Recover(func(c *gin.Context, _ any) {
		c.JSON(503, gin.H{"msg": "稍后再试"})
	})}, func(c *gin.Context) { panic("炸了") })

	if w.Code != 503 || !strings.Contains(w.Body.String(), "稍后再试") {
		t.Errorf("应走自定义响应，got=%d %s", w.Code, w.Body.String())
	}
}

func TestRecover_KeepsStatusOnceResponseStarted(t *testing.T) {
	// 响应一旦开始往外发，再改状态码只会得到一个半截的响应
	capture(t)
	w := serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) {
		c.String(200, "已经写出去一部分")
		panic("写到一半炸了")
	})

	if w.Code != 200 {
		t.Errorf("已经写出的响应不该被改成 500，got=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "已经写出去一部分") {
		t.Errorf("已写出的内容应保留，got=%s", w.Body.String())
	}
}

func TestRecover_NoOpWithoutPanic(t *testing.T) {
	w := serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) {
		c.String(201, "ok")
	})
	if w.Code != 201 {
		t.Errorf("正常请求不该被干预，got=%d", w.Code)
	}
}

// ---- Log ----

func TestLog_RecordsKeyFields(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello/42?q=1"), []gin.HandlerFunc{Log()}, func(c *gin.Context) {
		c.String(201, "ok")
	})

	got := lines()
	if len(got) != 1 {
		t.Fatalf("应记一条，got=%v", got)
	}
	l := got[0]
	// 路由用模板而不是真实路径：/hello/42 和 /hello/43 是同一个接口
	if l["route"] != "/hello/:id" {
		t.Errorf("路由应是模板，got=%v", l["route"])
	}
	if l["path"] != "/hello/42" {
		t.Errorf("路径应是真实路径，got=%v", l["path"])
	}
	if l["status"] != float64(201) || l["method"] != "GET" {
		t.Errorf("状态和方法不对，got=%v", l)
	}
	if l["elapsed_ms"] == nil || l["client_ip"] == nil {
		t.Errorf("应记耗时和客户端，got=%v", l)
	}
}

func TestLog_RecordsRequestAndResponseSizes(t *testing.T) {
	// 开了 body 日志时请求体被缓存、响应被截一份，两个字节数照样得对
	for name, opts := range map[string][]LogOption{"默认": nil, "记 body": {WithBody(true, true)}} {
		lines := capture(t)
		req := httptest.NewRequest("POST", "http://api.example.com/hello/1", strings.NewReader("12345"))
		req.Header.Set("User-Agent", "probe/1.0")
		serve(t, req, []gin.HandlerFunc{Log(opts...)}, func(c *gin.Context) { c.String(200, "hello!") })
		l := lines()[0]
		for k, want := range map[string]any{
			"host": "api.example.com", "proto": "HTTP/1.1", "user_agent": "probe/1.0",
			"bytes_in": float64(5), "bytes_out": float64(6),
		} {
			if l[k] != want {
				t.Errorf("%s：%s 应是 %v，got=%v", name, k, want, l[k])
			}
		}
	}
}

func TestLog_BytesOutIsZeroWhenNothingWritten(t *testing.T) {
	// 没写响应体时 gin 的 Size() 是 -1，原样记下来像是出了错
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) { c.Status(204) })
	if l := lines()[0]; l["bytes_out"] != float64(0) {
		t.Errorf("没写响应体时 bytes_out 应是 0，got=%v", l["bytes_out"])
	}
}

func TestLog_ElapsedIsMilliseconds(t *testing.T) {
	// slog 的 JSON 把 Duration 写成纳秒整数：一个 20ms 的请求记成 20000000，
	// 照着「毫秒」配的告警阈值差出一百万倍
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) {
		time.Sleep(20 * time.Millisecond)
		c.Status(200)
	})
	ms, ok := lines()[0]["elapsed_ms"].(float64)
	if !ok || ms < 20 || ms > 1000 {
		t.Errorf("elapsed_ms 该是毫秒（约 20），got=%v", lines()[0]["elapsed_ms"])
	}
}

func TestLog_UnmatchedRouteIsUnmatched(t *testing.T) {
	// 和指标、Span 一致：填真实路径的话，日志里分不出 /nope 是路由还是 404
	lines := capture(t)
	serve(t, get("/nope/1"), []gin.HandlerFunc{Log()}, func(c *gin.Context) {})
	l := lines()[0]
	if l["route"] != "unmatched" || l["path"] != "/nope/1" || l["status"] != float64(404) {
		t.Errorf("没匹配上的请求 route 该是 unmatched、path 是真实路径，got=%v", l)
	}
}

func TestLog_QueryIsRedacted(t *testing.T) {
	// 查询串里常有凭证（access_token、签名）：打开之后也得逐字段遮
	lines := capture(t)
	serve(t, get("/hello?page=2&access_token=t0p"), []gin.HandlerFunc{Log(WithQuery(true))}, func(c *gin.Context) {
		c.Status(200)
	})
	q, _ := lines()[0]["query"].(string)
	if !strings.Contains(q, "page=2") || strings.Contains(q, "t0p") {
		t.Errorf("query 该留 page、遮掉 access_token，got=%q", q)
	}
}

func TestLog_QueryOffByDefault(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello?page=2"), []gin.HandlerFunc{Log()}, func(c *gin.Context) { c.Status(200) })
	if _, ok := lines()[0]["query"]; ok {
		t.Errorf("默认不该记 query，got=%v", lines()[0])
	}
}

func TestLog_ResponseHeadersAreRedacted(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log(WithHeaders(false, true))}, func(c *gin.Context) {
		c.Header("X-Page-Total", "7")
		c.Header("Set-Cookie", "sid=abc123")
		c.Status(200)
	})
	h, ok := lines()[0]["response_headers"].(map[string]any)
	if !ok || fmt.Sprint(h["X-Page-Total"]) != "7" || strings.Contains(fmt.Sprint(h), "abc123") {
		t.Errorf("response_headers 该留普通头、遮掉 Set-Cookie，got=%v", lines()[0]["response_headers"])
	}
}

func TestLog_ResponseHeadersOffByDefault(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) { c.Status(200) })
	if _, ok := lines()[0]["response_headers"]; ok {
		t.Errorf("默认不该记 response_headers，got=%v", lines()[0])
	}
}

func TestLog_SkipsConfiguredPaths(t *testing.T) {
	lines := capture(t)
	mws := []gin.HandlerFunc{Log(WithSkipPaths("/hello", "/internal/"))}
	e := gin.New()
	e.Use(mws...)
	e.GET("/hello", func(c *gin.Context) { c.Status(200) })
	e.GET("/internal/x", func(c *gin.Context) { c.Status(200) })
	e.GET("/other", func(c *gin.Context) { c.Status(200) })

	for _, p := range []string{"/hello", "/internal/x", "/other"} {
		e.ServeHTTP(httptest.NewRecorder(), get(p))
	}

	got := lines()
	if len(got) != 1 || got[0]["path"] != "/other" {
		t.Errorf("只有 /other 该被记下来，got=%v", got)
	}
}

func TestLog_NoBodyByDefault(t *testing.T) {
	// 记 body 要缓存整个请求体并对每个字段脱敏，代价和风险都不小
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader(`{"password":"`+secret+`"}`))
	req.Header.Set("Content-Type", "application/json")
	serve(t, req, []gin.HandlerFunc{Log()}, func(c *gin.Context) { c.String(200, "ok") })

	got := lines()[0]
	if _, has := got["request_body"]; has {
		t.Errorf("默认不该记请求体，got=%v", got)
	}
}

func TestLog_RecordsRedactedBodyWhenEnabled(t *testing.T) {
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader(`{"user":"alice","password":"`+secret+`"}`))
	req.Header.Set("Content-Type", "application/json")

	serve(t, req, []gin.HandlerFunc{Log(WithBody(true, true))}, func(c *gin.Context) {
		body, _ := c.GetRawData()
		if !strings.Contains(string(body), "alice") {
			t.Errorf("下游应当仍然读得到完整请求体，got=%s", body)
		}
		c.JSON(200, gin.H{"token": secret, "ok": true})
	})

	got := lines()[0]
	reqBody, _ := got["request_body"].(string)
	if strings.Contains(reqBody, secret) {
		t.Errorf("请求体里的密码应被遮掉，got=%v", reqBody)
	}
	if !strings.Contains(reqBody, "alice") {
		t.Errorf("非敏感字段应保留，got=%v", reqBody)
	}
	respBody, _ := got["response_body"].(string)
	if strings.Contains(respBody, secret) {
		t.Errorf("响应体里的令牌也该被遮掉，got=%v", respBody)
	}
}

func TestLog_RedactsRequestHeaders(t *testing.T) {
	lines := capture(t)
	req := get("/hello")
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Visible", "keep-me")
	serve(t, req, []gin.HandlerFunc{Log(WithHeaders(true, false))}, func(c *gin.Context) { c.Status(200) })

	h, ok := lines()[0]["request_headers"].(map[string]any)
	if !ok || fmt.Sprint(h["X-Visible"]) != "keep-me" || strings.Contains(fmt.Sprint(h), secret) {
		t.Errorf("请求头该留普通头、遮掉凭证，got=%v", lines()[0]["request_headers"])
	}
}

func TestLog_RequestHeadersOffByDefault(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) { c.Status(200) })
	if _, ok := lines()[0]["request_headers"]; ok {
		t.Errorf("默认不该记 request_headers，got=%v", lines()[0])
	}
}

func TestLog_SkipsFileUploadBody(t *testing.T) {
	// 内容对排查没用，读一遍却要付全部的内存和时间
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader("大量二进制内容"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	serve(t, req, []gin.HandlerFunc{Log(WithBody(true, false))}, func(c *gin.Context) { c.Status(200) })

	if b, _ := lines()[0]["request_body"].(string); !strings.Contains(b, "omitted") {
		t.Errorf("文件上传的 body 不该被读进日志，got=%v", b)
	}
}

func TestLog_DoesNoWorkWhenLevelDisabled(t *testing.T) {
	// 缓存 body、包装 writer、脱敏序列化，全部只为拼出这一行日志。
	// 级别关掉还照做，就是白付了全部代价再把结果丢掉
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var buf strings.Builder
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))

	read := false
	req := httptest.NewRequest("POST", "/hello", strings.NewReader(`{"a":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Body = &trackingBody{ReadCloser: req.Body, read: &read}

	serve(t, req, []gin.HandlerFunc{Log(WithBody(true, true))}, func(c *gin.Context) { c.Status(200) })

	if buf.Len() != 0 {
		t.Errorf("级别关掉时不该有日志，got=%s", buf.String())
	}
	if read {
		t.Error("级别关掉时不该去读请求体")
	}
}

type trackingBody struct {
	io.ReadCloser
	read *bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	*b.read = true
	return b.ReadCloser.Read(p)
}

func TestLog_PanicStillWritesAccessLog(t *testing.T) {
	// 用户自定义的 RecoveryFunc 自己炸了的话，panic 会穿过 Log 这一层
	lines := capture(t)
	defer func() { recover() }()

	func() {
		defer func() { recover() }()
		serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) { panic("炸了") })
	}()

	if got := lines(); len(got) == 0 {
		t.Error("panic 穿过时访问日志仍应写出")
	}
}

func TestLog_BodyReadErrorDoesNotSwallowRequest(t *testing.T) {
	// 回归用例。退路里读一半出错就 return 的话，那半截请求体已经消失了，
	// 下游 handler 拿到的是个空 body——记日志不该有能力改变请求本身。
	capture(t)

	const head = "前半截还读得到"
	req := httptest.NewRequest("POST", "/hello", &failingReader{data: head})
	req.Header.Set("Content-Type", "text/plain")
	req.GetBody = nil // 逼它走「读出来再塞回去」的退路

	var seen string
	serve(t, req, []gin.HandlerFunc{Log(WithBody(true, false))}, func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		seen = string(b)
		c.Status(200)
	})

	if seen != head {
		t.Errorf("读到多少就该还给下游多少，got=%q want=%q", seen, head)
	}
}

// failingReader 先吐一段数据，然后报错
type failingReader struct {
	data string
	done bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errors.New("连接断了")
	}
	r.done = true
	return copy(p, r.data), nil
}

func TestLog_NeverLogsQueryString(t *testing.T) {
	// 访问日志记的是 URL.Path，不含查询串——GET /login?token=hunter2
	// 这种请求里，凭证就在 URL 上。改成 RequestURI 或者 URL.String()
	// 看着都像是「把日志记全一点」，实际是把凭证明文写进日志，
	// 而且不会有任何迹象。这条没有测试盯着的话，迟早会被顺手改掉
	lines := capture(t)
	serve(t, get("/hello/42?token=hunter2&password=s3cret"), []gin.HandlerFunc{Log()},
		func(c *gin.Context) { c.String(200, "ok") })

	got := lines()
	if len(got) != 1 {
		t.Fatalf("应记一条，got=%v", got)
	}
	raw, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"hunter2", "s3cret", "token=", "password="} {
		if bytes.Contains(raw, []byte(leaked)) {
			t.Errorf("查询串里的 %q 出现在了日志里: %s", leaked, raw)
		}
	}
}

func TestLog_c_String_ResponseIsRecorded(t *testing.T) {
	lines := capture(t)
	req := httptest.NewRequest("GET", "/hello", nil)
	serve(t, req, []gin.HandlerFunc{Log(WithBody(false, true))}, func(c *gin.Context) {
		c.String(200, "hello alice")
	})

	if got, _ := lines()[0]["response_body"].(string); got != "hello alice" {
		t.Errorf("c.String 写的响应体没被截下来，got=%q", got)
	}
}

func TestLog_handler_DirectWriteStringIsRecorded(t *testing.T) {
	// gin 的 ResponseWriter 接口带 WriteString，handler 直接调它是常见写法。
	// 包装层只包 Write 的话，这条路写出去的响应在日志里永远是空的
	lines := capture(t)
	serve(t, httptest.NewRequest("GET", "/hello", nil),
		[]gin.HandlerFunc{Log(WithBody(false, true))},
		func(c *gin.Context) {
			c.Header("Content-Type", "text/plain")
			c.Status(200)
			if _, err := c.Writer.WriteString("hello alice"); err != nil {
				t.Error(err)
			}
		})

	if got, _ := lines()[0]["response_body"].(string); got != "hello alice" {
		t.Errorf("WriteString 写的响应体没被截下来，got=%q", got)
	}
}

func TestLog_WriteString_TruncatesToPrefixOverLimit(t *testing.T) {
	lines := capture(t)
	serve(t, httptest.NewRequest("GET", "/hello", nil),
		[]gin.HandlerFunc{Log(WithBody(false, true))},
		func(c *gin.Context) {
			c.Header("Content-Type", "text/plain")
			c.Status(200)
			_, _ = c.Writer.WriteString(strings.Repeat("a", maxResponseBody*2))
		})

	if got, _ := lines()[0]["response_body"].(string); len(got) > maxResponseBody {
		t.Errorf("响应体没截断，记了 %d 字节", len(got))
	}
}

func TestLog_c_String_ResponseIsRedacted(t *testing.T) {
	lines := capture(t)
	serve(t, httptest.NewRequest("GET", "/hello", nil),
		[]gin.HandlerFunc{Log(WithBody(false, true))},
		func(c *gin.Context) { c.String(200, "token="+secret) })

	if got, _ := lines()[0]["response_body"].(string); strings.Contains(got, secret) {
		t.Errorf("凭证漏进日志了，got=%q", got)
	}
}

func TestLog_ResponseBodyTruncatedToPrefixOverLimit(t *testing.T) {
	lines := capture(t)
	big := strings.Repeat("a", maxResponseBody*2)
	serve(t, httptest.NewRequest("GET", "/hello", nil),
		[]gin.HandlerFunc{Log(WithBody(false, true))},
		func(c *gin.Context) { c.String(200, big) })

	got, _ := lines()[0]["response_body"].(string)
	if len(got) > maxResponseBody {
		t.Errorf("响应体没截断，记了 %d 字节", len(got))
	}
}

func TestSnapshotBody_SkipsBinaryStream(t *testing.T) {
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader("\x00\x01\x02"))
	req.Header.Set("Content-Type", "application/octet-stream")
	serve(t, req, []gin.HandlerFunc{Log(WithBody(true, false))}, func(c *gin.Context) { c.Status(200) })

	if b, _ := lines()[0]["request_body"].(string); !strings.Contains(b, "omitted") {
		t.Errorf("二进制流不该被读进日志，got=%v", b)
	}
}

// serveAborted 跑一个写出 200 和半截 body 之后以 http.ErrAbortHandler 中止的请求。
// Recover 排在最内层，与 xgin 装配的顺序一致；它抛回来的 panic 在这里接住
func serveAborted(t *testing.T, mws ...gin.HandlerFunc) {
	t.Helper()
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("ErrAbortHandler 该一路抛到 net/http，got=%v", got)
		}
	}()
	serve(t, get("/hello"), append(mws, Recover(nil)), func(c *gin.Context) {
		c.String(200, "partial")
		panic(http.ErrAbortHandler)
	})
}

func TestLog_ErrAbortHandlerAbortLoggedAs499(t *testing.T) {
	// 中止的请求往往已经写出了 200 的响应头。照读 c.Writer.Status() 的话，
	// 一个被截断的响应在访问日志里是一条普普通通的成功
	lines := capture(t)
	serveAborted(t, Log())

	got := lines()
	if len(got) != 1 {
		t.Fatalf("应有且只有一条访问日志，got=%v", got)
	}
	if got[0]["status"] != float64(statusAborted) {
		t.Errorf("中止的请求该记成 %d，got=%v", statusAborted, got[0]["status"])
	}
	if e, _ := got[0]["errors"].(string); !strings.Contains(e, "net/http: abort Handler") {
		t.Errorf("errors 里该带着中止的原因，got=%v", got[0]["errors"])
	}
}

func TestLog_ErrorTextIsRedacted(t *testing.T) {
	// c.Error 登记的错误原样进 errors 字段；驱动、下游报的错里常夹着凭证
	for msg, want := range map[string]string{"db down": "db down", "login failed password=hunter2": Redacted} {
		lines := capture(t)
		serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) {
			_ = c.Error(errors.New(msg))
			c.Status(500)
		})
		got, _ := lines()[0]["errors"].(string)
		if want == Redacted && got != Redacted || want != Redacted && !strings.Contains(got, msg) {
			t.Errorf("errors=%q，%q 该记成 %q", got, msg, want)
		}
	}
}

func TestLog_DSNPasswordInErrorIsMasked(t *testing.T) {
	// 驱动报的错里常带整串 DSN，里面一个敏感词都没有：只看词表的话密码原样进 errors 字段
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) {
		_ = c.Error(errors.New("dial postgres://app:hunter2@db:5432/prod failed"))
		c.Status(500)
	})
	got, _ := lines()[0]["errors"].(string)
	if strings.Contains(got, "hunter2") || !strings.Contains(got, "postgres://app:"+Redacted+"@db:5432/prod") {
		t.Errorf("errors 里 DSN 的密码该被遮掉、其余留着，got=%q", got)
	}
}

func TestLog_MultipleErrorsStaySeparated(t *testing.T) {
	// c.Errors.String() 一条错误一行；换行直接删掉的话两条错误粘成一句
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) {
		_ = c.Error(errors.New("a"))
		_ = c.Error(errors.New("b"))
		c.Status(500)
	})
	if got := lines()[0]["errors"]; got != "Error #01: a; Error #02: b" {
		t.Errorf("两条错误该用 ; 隔开，got=%q", got)
	}
}

func TestRecover_PanicValueIsRedacted(t *testing.T) {
	// panic 的值和 handler 返回的错误一样会夹带凭证，原样进 error 字段就是密码落盘
	for msg, want := range map[string]string{
		"dial postgres://app:hunter2@db:5432/prod failed": "dial postgres://app:" + Redacted + "@db:5432/prod failed",
		"login failed password=hunter2":                   Redacted,
	} {
		lines := capture(t)
		serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) { panic(errors.New(msg)) })
		var got any = "（没打）"
		for _, l := range lines() {
			if l["msg"] == "panic while handling request" {
				got = l["error"]
			}
		}
		if got != want {
			t.Errorf("panic(%q) 的 error 字段=%v，want %q", msg, got, want)
		}
	}
}

// gzipWriter 测试用的压缩中间件：和 gin-contrib/gzip 一样换掉 c.Writer，写进来的经 gzip 压缩再往下写
type gzipWriter struct {
	gin.ResponseWriter
	gz *gzip.Writer
}

func (w *gzipWriter) Write(b []byte) (int, error)       { return w.gz.Write(b) }
func (w *gzipWriter) WriteString(s string) (int, error) { return w.gz.Write([]byte(s)) }

func gzipped(c *gin.Context) {
	c.Header("Content-Encoding", "gzip")
	gz := gzip.NewWriter(c.Writer)
	c.Writer = &gzipWriter{ResponseWriter: c.Writer, gz: gz}
	c.Next()
	_ = gz.Close()
}

func TestLog_EncodedResponseBodyOmitted(t *testing.T) {
	// 回归用例：Log 在压缩中间件外面，截下来的是压缩过的字节，原先整段二进制乱码进了 response_body。
	// 压缩过的不记内容，记一句 omitted
	lines := capture(t)
	w := serve(t, get("/hello"), []gin.HandlerFunc{Log(WithBody(false, true)), gzipped}, func(c *gin.Context) {
		c.String(200, strings.Repeat("hello ", 200))
	})
	l := lines()[0]
	if got := l["response_body"]; got != "[gzip-encoded content omitted]" {
		t.Errorf("压缩过的响应体该记 omitted，got=%q", got)
	}
	// gin 这边 bytes_out 是线上的（压缩后的）字节数：Log 读的是压缩中间件下面那个 writer
	if l["bytes_out"] != float64(w.Body.Len()) {
		t.Errorf("bytes_out 该是压缩后的 %d 字节，got=%v", w.Body.Len(), l["bytes_out"])
	}
}

func TestRecover_BrokenPipeLoggedAsWarnWithoutStack(t *testing.T) {
	// 客户端提前断开不算故障：记一条 WARN 的 connection broken，不打栈。
	// 原先记 ERROR，告警跟着客户端的网络抖动响
	lines := capture(t)
	broken := &net.OpError{Op: "write", Err: &os.SyscallError{Syscall: "write", Err: errors.New("broken pipe")}}
	serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) { panic(broken) })

	var sawBroken bool
	for _, l := range lines() {
		if l["msg"] == "panic while handling request" {
			t.Errorf("断连不该当成 panic 打栈，got=%v", l)
		}
		if l["msg"] == "connection broken" {
			sawBroken = l["level"] == "WARN"
		}
	}
	if !sawBroken {
		t.Error("应记一条 WARN 级别的 connection broken")
	}
}
