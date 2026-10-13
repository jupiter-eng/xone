package middleware

import (
	"bufio"
	"bytes"
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

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"

	"github.com/jupiter-eng/xone/xlog"
)

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

// accessLogs 只留访问日志那几行
func accessLogs(lines []map[string]any) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["msg"] == "request completed" {
			out = append(out, l)
		}
	}
	return out
}

// newEcho 装上中间件的 echo，/hello 和 /hello/:id 两条路由对所有标准方法都注册 h，
// 请求用了别的方法（CUSTOM1）时另外为它注册一遍——否则那是一次 405
func newEcho(mws []echo.MiddlewareFunc, h echo.HandlerFunc, extra ...string) *echo.Echo {
	e := echo.New()
	e.Use(mws...)
	e.Any("/hello/:id", h)
	e.Any("/hello", h)
	for _, m := range extra {
		e.Add(m, "/hello/:id", h)
		e.Add(m, "/hello", h)
	}
	return e
}

// serve 装上中间件跑一次请求
func serve(t *testing.T, req *http.Request, mws []echo.MiddlewareFunc, h echo.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	newEcho(mws, h, req.Method).ServeHTTP(w, req)
	return w
}

func get(path string) *http.Request { return httptest.NewRequest("GET", path, nil) }

// status 让 handler 只回一个状态码
func statusOnly(code int) echo.HandlerFunc {
	return func(c echo.Context) error { return c.NoContent(code) }
}

// ---- LogScope ----

func TestLogScope_HandlersCanAddFieldsAtAnyDepth(t *testing.T) {
	// 调用栈深处拿不到 echo.Context，本来就没机会把新 context 回传上来
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{LogScope(), Log()}, func(c echo.Context) error {
		deepInBusinessCode(c.Request().Context())
		return c.String(200, "ok")
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
	// echo 自己不兜 panic：连接直接断掉，栈进 stderr 而不是日志平台
	lines := capture(t)
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(nil)}, func(c echo.Context) error {
		panic("炸了")
	})

	if w.Code != 500 || strings.TrimSpace(w.Body.String()) != `{"message":"Internal Server Error"}` {
		t.Errorf("应返回 echo 默认的 500 响应，got=%d %q", w.Code, w.Body.String())
	}
	got := lines()
	if len(got) == 0 || got[0]["level"] != "ERROR" || got[0]["msg"] != "panic while handling request" {
		t.Fatalf("应记一条 error 日志，got=%v", got)
	}
	if got[0]["stack"] == nil || !strings.Contains(got[0]["stack"].(string), "middleware_test.go") {
		t.Errorf("应带上栈信息，got=%v", got[0])
	}
	if got[0]["path"] != "/hello" || got[0]["method"] != "GET" || got[0]["error"] != "炸了" {
		t.Errorf("应带上请求路径、方法和 panic 的值，got=%v", got[0])
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
		serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(nil)}, func(c echo.Context) error {
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
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(func(c echo.Context, _ any) error {
		return c.JSON(503, map[string]string{"msg": "稍后再试"})
	})}, func(c echo.Context) error { panic("炸了") })

	if w.Code != 503 || !strings.Contains(w.Body.String(), "稍后再试") {
		t.Errorf("应走自定义响应，got=%d %s", w.Code, w.Body.String())
	}
}

func TestRecover_CustomFuncErrorGoesThroughHTTPErrorHandler(t *testing.T) {
	// 返回的错误和 handler 返回的一样由 e.HTTPErrorHandler 渲染：自定义的错误格式对 panic 也生效
	capture(t)
	e := newEcho([]echo.MiddlewareFunc{Recover(func(echo.Context, any) error {
		return echo.NewHTTPError(503, "busy")
	})}, func(c echo.Context) error { panic("炸了") })
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		var he *echo.HTTPError
		errors.As(err, &he)
		_ = c.String(he.Code, "custom:"+fmt.Sprint(he.Message))
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, get("/hello"))
	if w.Code != 503 || w.Body.String() != "custom:busy" {
		t.Errorf("该经 HTTPErrorHandler 渲染，got=%d %q", w.Code, w.Body.String())
	}
}

func TestRecover_KeepsStatusOnceResponseStarted(t *testing.T) {
	// 响应一旦开始往外发，再改状态码只会得到一个半截的响应
	capture(t)
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(nil)}, func(c echo.Context) error {
		_ = c.String(200, "已经写出去一部分")
		panic("写到一半炸了")
	})

	if w.Code != 200 {
		t.Errorf("已经写出的响应不该被改成 500，got=%d", w.Code)
	}
	if w.Body.String() != "已经写出去一部分" {
		t.Errorf("已写出的内容应保留、不再追加错误响应，got=%q", w.Body.String())
	}
}

func TestRecover_CustomFuncNotCalledOnceResponseStarted(t *testing.T) {
	// 响应已经开始往外写了就不调自定义的 recover 函数：它写的东西只会接在半截响应后面
	capture(t)
	called := false
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(func(c echo.Context, _ any) error {
		called = true
		return c.String(503, "稍后再试")
	})}, func(c echo.Context) error {
		_ = c.String(200, "partial")
		panic("写到一半炸了")
	})
	if called || w.Body.String() != "partial" {
		t.Errorf("响应开始之后不该再调 recover 函数，called=%v body=%q", called, w.Body.String())
	}
}

func TestRecover_NoOpWithoutPanic(t *testing.T) {
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(nil)}, func(c echo.Context) error {
		return c.String(201, "ok")
	})
	if w.Code != 201 {
		t.Errorf("正常请求不该被干预，got=%d", w.Code)
	}
}

func TestRecover_BrokenPipeLoggedWithoutStack(t *testing.T) {
	// 客户端提前断开不算故障：记一条 connection broken，不打栈，也不再写响应
	lines := capture(t)
	broken := &net.OpError{Op: "write", Err: &os.SyscallError{Syscall: "write", Err: errors.New("broken pipe")}}
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log(), Recover(nil)}, func(c echo.Context) error { panic(broken) })

	got := lines()
	var sawBroken bool
	for _, l := range got {
		if l["msg"] == "panic while handling request" {
			t.Errorf("断连不该当成 panic 打栈，got=%v", l)
		}
		if l["msg"] == "connection broken" {
			// 客户端走了不是服务的故障，记 WARN：ERROR 会让告警跟着客户端的网络抖动响
			sawBroken = l["level"] == "WARN"
		}
	}
	if !sawBroken {
		t.Errorf("应记一条 WARN 级别的 connection broken，got=%v", got)
	}
	if a := accessLogs(got); len(a) != 1 || !strings.Contains(fmt.Sprint(a[0]["errors"]), "broken pipe") {
		t.Errorf("访问日志的 errors 里该带着断连的原因，got=%v", a)
	}
}

// ---- Log ----

func TestLog_RecordsKeyFields(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello/42?q=1"), []echo.MiddlewareFunc{Log()}, func(c echo.Context) error {
		return c.String(201, "ok")
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
	if l["elapsed_ms"] == nil || l["client_ip"] != "192.0.2.1" {
		t.Errorf("应记耗时和客户端，got=%v", l)
	}
	if _, ok := l["errors"]; ok {
		t.Errorf("没出错就不写 errors，got=%v", l)
	}
}

func TestLog_RecordsRequestAndResponseSizes(t *testing.T) {
	// 开了 body 日志时请求体被缓存、响应被截一份，两个字节数照样得对
	for name, opts := range map[string][]LogOption{"默认": nil, "记 body": {WithBody(true, true)}} {
		lines := capture(t)
		req := httptest.NewRequest("POST", "http://api.example.com/hello/1", strings.NewReader("12345"))
		req.Header.Set("User-Agent", "probe/1.0")
		serve(t, req, []echo.MiddlewareFunc{Log(opts...)}, func(c echo.Context) error { return c.String(200, "hello!") })
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
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, statusOnly(204))
	if l := lines()[0]; l["bytes_out"] != float64(0) || l["status"] != float64(204) {
		t.Errorf("c.NoContent(204)：bytes_out 应是 0、status 是 204，got=%v", l)
	}
}

func TestLog_BytesOutCountsJSONAndStreamedResponses(t *testing.T) {
	// c.JSON 的正文带一个换行（echo 的 JSON 序列化用 json.Encoder）；分几次写、每次 Flush 的也要记全
	for name, c := range map[string]struct {
		h    echo.HandlerFunc
		want int
	}{
		"c.JSON": {func(c echo.Context) error { return c.JSON(200, map[string]string{"a": "b"}) }, len(`{"a":"b"}` + "\n")},
		"分块写": {func(c echo.Context) error {
			c.Response().WriteHeader(200)
			for i := 0; i < 3; i++ {
				_, _ = c.Response().Write([]byte("chunk"))
				c.Response().Flush()
			}
			return nil
		}, 15},
	} {
		lines := capture(t)
		w := serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, c.h)
		if l := lines()[0]; l["bytes_out"] != float64(c.want) || w.Body.Len() != c.want {
			t.Errorf("%s：bytes_out 该是 %d（客户端收到 %d），got=%v", name, c.want, w.Body.Len(), l["bytes_out"])
		}
	}
}

func TestLog_ElapsedIsMilliseconds(t *testing.T) {
	// slog 的 JSON 把 Duration 写成纳秒整数：一个 20ms 的请求记成 20000000，
	// 照着「毫秒」配的告警阈值差出一百万倍
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, func(c echo.Context) error {
		time.Sleep(20 * time.Millisecond)
		return c.NoContent(200)
	})
	ms, ok := lines()[0]["elapsed_ms"].(float64)
	if !ok || ms < 20 || ms > 1000 {
		t.Errorf("elapsed_ms 该是毫秒（约 20），got=%v", lines()[0]["elapsed_ms"])
	}
}

func TestLog_UnmatchedRouteIsUnmatched(t *testing.T) {
	// 和指标、Span 一致：填真实路径的话，日志里分不出 /nope 是路由还是 404
	lines := capture(t)
	serve(t, get("/nope/1"), []echo.MiddlewareFunc{Log()}, statusOnly(200))
	l := lines()[0]
	if l["route"] != "unmatched" || l["path"] != "/nope/1" || l["status"] != float64(404) {
		t.Errorf("没匹配上的请求 route 该是 unmatched、path 是真实路径，got=%v", l)
	}
}

// errorCases handler 返回错误、或者 router 自己报错的几种请求，及客户端实际收到的状态码和路由标签
var errorCases = []struct {
	name, method, path string
	status             int
	route, errs        string
}{
	{"没匹配上的路由（404）", "GET", "/nope", 404, "unmatched", "code=404, message=Not Found"},
	{"方法不对（405）", "POST", "/only-get", 405, "unmatched", "code=405, message=Method Not Allowed"},
	{"普通 error（500）", "GET", "/plain", 500, "/plain", Redacted}, // 错误原文带着 password=，整段遮掉
	{"echo.NewHTTPError(418)", "GET", "/teapot", 418, "/teapot", "code=418, message=I'm a teapot"},
	// 带中间件的分组：echo 悄悄给它注册了 RouteNotFound 兜底（/api/v1、/api/v1/*），c.Path() 是兜底的模板
	{"分组下没匹配上的路径", "GET", "/api/v1/nope", 404, "unmatched", "code=404, message=Not Found"},
	{"分组下方法不对（echo 回的是 404，不是 405）", "POST", "/api/v1/users", 404, "unmatched", "code=404, message=Not Found"},
	{"使用者自己注册的 RouteNotFound", "GET", "/custom/x", 404, "unmatched", "code=404, message=custom"},
}

// passThrough 什么都不做的分组中间件：有了它，echo 就给分组注册 RouteNotFound 兜底
func passThrough(next echo.HandlerFunc) echo.HandlerFunc { return next }

// errorEcho 注册 errorCases 用到的路由
func errorEcho(mws ...echo.MiddlewareFunc) *echo.Echo {
	e := echo.New()
	e.Use(mws...)
	e.GET("/only-get", statusOnly(200))
	e.GET("/plain", func(echo.Context) error { return errors.New("db down: password=s3cret") })
	e.GET("/teapot", func(echo.Context) error { return echo.NewHTTPError(http.StatusTeapot) })
	e.Group("/api/v1", passThrough).GET("/users", statusOnly(200))
	e.RouteNotFound("/custom/*", func(echo.Context) error { return echo.NewHTTPError(http.StatusNotFound, "custom") })
	return e
}

func TestLog_FallbackTemplateKeptForMethodsRegisteredOnIt(t *testing.T) {
	// 兜底和真正的路由可以是同一个模板：分组里 GET("/*") 做转发，echo 又给分组注册了 /proxy/* 的兜底。
	// GET 命中的是真正的路由，记模板；别的方法落到兜底上（404），记 unmatched
	lines := capture(t)
	e := echo.New()
	e.Use(Log())
	e.Group("/proxy", passThrough).GET("/*", statusOnly(200))
	e.ServeHTTP(httptest.NewRecorder(), get("/proxy/a"))
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/proxy/a", nil))
	got := accessLogs(lines())
	if len(got) != 2 || got[0]["route"] != "/proxy/*" || got[0]["status"] != float64(200) ||
		got[1]["route"] != "unmatched" || got[1]["status"] != float64(404) {
		t.Errorf("GET 该记模板 /proxy/*、POST 落到兜底该记 unmatched，got=%v", got)
	}
}

func TestLog_StaticMissingFileKeepsRouteTemplate(t *testing.T) {
	// e.Static 注册的是一条真正的 GET 路由（/static*），文件不存在时由它自己回 404：
	// 记它的模板，基数照样是有界的
	lines := capture(t)
	e := echo.New()
	e.Use(Log())
	e.Static("/static", t.TempDir())
	w := httptest.NewRecorder()
	e.ServeHTTP(w, get("/static/missing.txt"))
	if l := lines()[0]; w.Code != 404 || l["route"] != "/static*" || l["status"] != float64(404) {
		t.Errorf("静态文件不存在该记 404 和 /static*，got code=%d log=%v", w.Code, l)
	}
}

func TestLog_ErrorStatusIsTheRenderedResponse(t *testing.T) {
	// 回归用例（设计时量出来的）：echo 在整条链返回之后才渲染错误，中间件拿到错误的那一刻
	// 响应还没写，状态码 200、Size 0。不在这一层渲染的话，每个 404、500 都记成 bytes_out 为 0 的 200
	for _, c := range errorCases {
		lines := capture(t)
		w := httptest.NewRecorder()
		errorEcho(Log()).ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))

		if w.Code != c.status {
			t.Errorf("%s：客户端该收到 %d，got=%d", c.name, c.status, w.Code)
		}
		l := lines()[0]
		if l["status"] != float64(c.status) || l["bytes_out"] != float64(w.Body.Len()) || w.Body.Len() == 0 {
			t.Errorf("%s：访问日志该记真正发出去的状态码 %d 和字节数 %d，got status=%v bytes_out=%v",
				c.name, c.status, w.Body.Len(), l["status"], l["bytes_out"])
		}
		if l["route"] != c.route || l["errors"] != c.errs {
			t.Errorf("%s：route 该是 %q、errors 该是 %q，got=%v", c.name, c.route, c.errs, l)
		}
	}
}

func TestLog_ErrorBodyIsEchoDefaultAndHidesErrorText(t *testing.T) {
	// 响应体保持 echo 的默认：普通 error 只回一句 Internal Server Error，错误原文只进日志
	capture(t)
	w := httptest.NewRecorder()
	errorEcho(Log()).ServeHTTP(w, httptest.NewRequest("GET", "/plain", nil))
	if got := strings.TrimSpace(w.Body.String()); got != `{"message":"Internal Server Error"}` || strings.Contains(got, "s3cret") {
		t.Errorf("错误原文不该进响应，got=%q", got)
	}
}

func TestLog_ErrorAfterResponseCommittedKeepsSentStatus(t *testing.T) {
	// handler 已经写了 200 再返回错误：echo 的默认错误处理见 Committed 就什么都不做，
	// 客户端收到的是那个 200。日志如实记 200，错误进 errors
	lines := capture(t)
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, func(c echo.Context) error {
		_ = c.String(200, "ok")
		return errors.New("after commit")
	})
	l := lines()[0]
	if w.Code != 200 || w.Body.String() != "ok" || l["status"] != float64(200) || l["bytes_out"] != float64(2) || l["errors"] != "after commit" {
		t.Errorf("该如实记已经发出去的 200 和那个错误，got code=%d body=%q log=%v", w.Code, w.Body.String(), l)
	}
}

func TestFinish_HTTPErrorHandlerRunsOnceAcrossTheChain(t *testing.T) {
	// 三层都在自己这里渲染错误，只有最内层那一次真的渲染：往外返回的是 nil，
	// 否则 echo 会再调一遍 HTTPErrorHandler，不查 Committed 的自定义错误处理把错误响应写两遍
	capture(t)
	_ = recording(t)
	withMetrics(t)
	calls := 0
	e := errorEcho(Trace(), Log(), Metric())
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		calls++
		_ = c.String(500, "E")
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/plain", nil))
	if calls != 1 || w.Body.String() != "E" {
		t.Errorf("HTTPErrorHandler 该只跑一次，got calls=%d body=%q", calls, w.Body.String())
	}
}

func TestLog_ClientIPFollowsIPExtractor(t *testing.T) {
	// client_ip 取的是 c.RealIP()：按 e.IPExtractor 算，xecho 按 TrustedProxies 装好了它
	lines := capture(t)
	e := newEcho([]echo.MiddlewareFunc{Log()}, statusOnly(200))
	e.IPExtractor = func(*http.Request) string { return "198.51.100.7" }
	e.ServeHTTP(httptest.NewRecorder(), get("/hello"))
	if l := lines()[0]; l["client_ip"] != "198.51.100.7" {
		t.Errorf("client_ip 该按 IPExtractor 算，got=%v", l["client_ip"])
	}
}

func TestLog_QueryIsRedacted(t *testing.T) {
	// 查询串里常有凭证（access_token、签名）：打开之后也得逐字段遮
	lines := capture(t)
	serve(t, get("/hello?page=2&access_token=t0p"), []echo.MiddlewareFunc{Log(WithQuery(true))}, statusOnly(200))
	q, _ := lines()[0]["query"].(string)
	if !strings.Contains(q, "page=2") || strings.Contains(q, "t0p") {
		t.Errorf("query 该留 page、遮掉 access_token，got=%q", q)
	}
}

func TestLog_QueryOffByDefault(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello?page=2"), []echo.MiddlewareFunc{Log()}, statusOnly(200))
	if _, ok := lines()[0]["query"]; ok {
		t.Errorf("默认不该记 query，got=%v", lines()[0])
	}
}

func TestLog_ResponseHeadersAreRedacted(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithHeaders(false, true))}, func(c echo.Context) error {
		c.Response().Header().Set("X-Page-Total", "7")
		c.Response().Header().Set("Set-Cookie", "sid=abc123")
		return c.NoContent(200)
	})
	h, ok := lines()[0]["response_headers"].(map[string]any)
	if !ok || fmt.Sprint(h["X-Page-Total"]) != "7" || strings.Contains(fmt.Sprint(h), "abc123") {
		t.Errorf("response_headers 该留普通头、遮掉 Set-Cookie，got=%v", lines()[0]["response_headers"])
	}
}

func TestLog_ResponseHeadersOffByDefault(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, statusOnly(200))
	if _, ok := lines()[0]["response_headers"]; ok {
		t.Errorf("默认不该记 response_headers，got=%v", lines()[0])
	}
}

func TestLog_SkipsConfiguredPaths(t *testing.T) {
	lines := capture(t)
	e := echo.New()
	e.Use(Log(WithSkipPaths("/hello", "/internal/")))
	e.GET("/hello", statusOnly(200))
	e.GET("/internal/x", statusOnly(200))
	e.GET("/other", statusOnly(200))

	for _, p := range []string{"/hello", "/internal/x", "/other"} {
		e.ServeHTTP(httptest.NewRecorder(), get(p))
	}

	got := lines()
	if len(got) != 1 || got[0]["path"] != "/other" {
		t.Errorf("只有 /other 该被记下来，got=%v", got)
	}
}

func TestLog_SkippedPathStillRendersErrors(t *testing.T) {
	// 跳过的路径不记日志，但错误照样由 echo 渲染成响应
	capture(t)
	e := errorEcho(Log(WithSkipPaths("/plain")))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/plain", nil))
	if w.Code != 500 {
		t.Errorf("跳过日志的路径，错误响应照旧，got=%d", w.Code)
	}
}

func TestLog_NoBodyByDefault(t *testing.T) {
	// 记 body 要缓存整个请求体并对每个字段脱敏，代价和风险都不小
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader(`{"password":"`+secret+`"}`))
	req.Header.Set("Content-Type", "application/json")
	serve(t, req, []echo.MiddlewareFunc{Log()}, func(c echo.Context) error { return c.String(200, "ok") })

	got := lines()[0]
	if _, has := got["request_body"]; has {
		t.Errorf("默认不该记请求体，got=%v", got)
	}
}

func TestLog_RecordsRedactedBodyWhenEnabled(t *testing.T) {
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader(`{"user":"alice","password":"`+secret+`"}`))
	req.Header.Set("Content-Type", "application/json")

	serve(t, req, []echo.MiddlewareFunc{Log(WithBody(true, true))}, func(c echo.Context) error {
		body, _ := io.ReadAll(c.Request().Body)
		if !strings.Contains(string(body), "alice") {
			t.Errorf("下游应当仍然读得到完整请求体，got=%s", body)
		}
		return c.JSON(200, map[string]any{"token": secret, "ok": true})
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
	if respBody == "" || strings.Contains(respBody, secret) {
		t.Errorf("响应体该记下来、里面的令牌被遮掉，got=%v", respBody)
	}
}

func TestLog_RedactsRequestHeaders(t *testing.T) {
	lines := capture(t)
	req := get("/hello")
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Visible", "keep-me")
	serve(t, req, []echo.MiddlewareFunc{Log(WithHeaders(true, false))}, statusOnly(200))

	h, ok := lines()[0]["request_headers"].(map[string]any)
	if !ok || fmt.Sprint(h["X-Visible"]) != "keep-me" || strings.Contains(fmt.Sprint(h), secret) {
		t.Errorf("请求头该留普通头、遮掉凭证，got=%v", lines()[0]["request_headers"])
	}
}

func TestLog_RequestHeadersOffByDefault(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, statusOnly(200))
	if _, ok := lines()[0]["request_headers"]; ok {
		t.Errorf("默认不该记 request_headers，got=%v", lines()[0])
	}
}

func TestLog_SkipsFileUploadBody(t *testing.T) {
	// 内容对排查没用，读一遍却要付全部的内存和时间
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader("大量二进制内容"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	serve(t, req, []echo.MiddlewareFunc{Log(WithBody(true, false))}, statusOnly(200))

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

	serve(t, req, []echo.MiddlewareFunc{Log(WithBody(true, true))}, statusOnly(200))

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
	// 自定义的 recover 函数自己炸了的话，panic 会穿过 Log 这一层
	lines := capture(t)
	func() {
		defer func() { recover() }()
		serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, func(c echo.Context) error { panic("炸了") })
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
	serve(t, req, []echo.MiddlewareFunc{Log(WithBody(true, false))}, func(c echo.Context) error {
		b, _ := io.ReadAll(c.Request().Body)
		seen = string(b)
		return c.NoContent(200)
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
	// 这种请求里，凭证就在 URL 上
	lines := capture(t)
	serve(t, get("/hello/42?token=hunter2&password=s3cret"), []echo.MiddlewareFunc{Log()},
		func(c echo.Context) error { return c.String(200, "ok") })

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

func TestLog_StringResponseIsRecorded(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		return c.String(200, "hello alice")
	})

	if got, _ := lines()[0]["response_body"].(string); got != "hello alice" {
		t.Errorf("c.String 写的响应体没被截下来，got=%q", got)
	}
}

func TestLog_ResponseCaptureKeepsFlushWorking(t *testing.T) {
	// 截响应换掉了 echo.Response 里的 Writer。echo 的 Flush 走 http.ResponseController：
	// 包装层不交出原来的 writer（Unwrap）的话，打开响应体日志之后 SSE 一 Flush 就 panic
	lines := capture(t)
	var flushed bool
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		c.Response().Header().Set("Content-Type", "text/event-stream")
		c.Response().WriteHeader(200)
		_, _ = c.Response().Write([]byte("data: 1\n\n"))
		c.Response().Flush()
		flushed = true
		return nil
	})
	if !flushed || !w.Flushed {
		t.Fatalf("Flush 该落到真正的 writer 上，flushed=%v recorder.Flushed=%v", flushed, w.Flushed)
	}
	// 记进日志时换行被去掉（web.RedactBody 防日志注入）
	if got, _ := lines()[0]["response_body"].(string); got != "data: 1" {
		t.Errorf("流式写的响应体也该截下来，got=%q", got)
	}
}

func TestLog_ResponseWriterRestoredAfterRequest(t *testing.T) {
	// 截响应的 writer 是池子里的，请求结束前必须换回原来的：外层中间件还要用 c.Response()
	capture(t)
	var inner, outer http.ResponseWriter
	outerMW := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			before := c.Response().Writer
			err := next(c)
			outer = c.Response().Writer
			if outer != before {
				t.Error("请求结束时 Response().Writer 该换回原来的")
			}
			return err
		}
	}
	serve(t, get("/hello"), []echo.MiddlewareFunc{outerMW, Log(WithBody(false, true))}, func(c echo.Context) error {
		inner = c.Response().Writer
		return c.String(200, "x")
	})
	if _, ok := inner.(*captureWriter); !ok {
		t.Errorf("handler 里该是截响应的 writer，got=%T", inner)
	}
}

func TestLog_StringResponseIsRedacted(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithBody(false, true))},
		func(c echo.Context) error { return c.String(200, "token="+secret) })

	if got, _ := lines()[0]["response_body"].(string); strings.Contains(got, secret) {
		t.Errorf("凭证漏进日志了，got=%q", got)
	}
}

func TestLog_ResponseBodyTruncatedToPrefixOverLimit(t *testing.T) {
	lines := capture(t)
	big := strings.Repeat("a", maxResponseBody*2)
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithBody(false, true))},
		func(c echo.Context) error { return c.String(200, big) })

	got, _ := lines()[0]["response_body"].(string)
	if len(got) != maxResponseBody {
		t.Errorf("响应体该截成前 %d 字节，记了 %d 字节", maxResponseBody, len(got))
	}
	if w.Body.Len() != len(big) {
		t.Errorf("截的只是日志里的副本，客户端该收到全部 %d 字节，got=%d", len(big), w.Body.Len())
	}
}

func TestSnapshotBody_SkipsBinaryStream(t *testing.T) {
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader("\x00\x01\x02"))
	req.Header.Set("Content-Type", "application/octet-stream")
	serve(t, req, []echo.MiddlewareFunc{Log(WithBody(true, false))}, statusOnly(200))

	if b, _ := lines()[0]["request_body"].(string); !strings.Contains(b, "omitted") {
		t.Errorf("二进制流不该被读进日志，got=%v", b)
	}
}

// serveAborted 跑一个写出 200 和半截 body 之后以 http.ErrAbortHandler 中止的请求。
// Recover 排在最内层，与 xecho 装配的顺序一致；它抛回来的 panic 在这里接住
func serveAborted(t *testing.T, mws ...echo.MiddlewareFunc) {
	t.Helper()
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("ErrAbortHandler 该一路抛到 net/http，got=%v", got)
		}
	}()
	serve(t, get("/hello"), append(mws, Recover(nil)), func(c echo.Context) error {
		_ = c.String(200, "partial")
		panic(http.ErrAbortHandler)
	})
}

func TestLog_ErrAbortHandlerAbortLoggedAs499(t *testing.T) {
	// 中止的请求往往已经写出了 200 的响应头。照读 Response().Status 的话，
	// 一个被截断的响应在访问日志里是一条普普通通的成功
	lines := capture(t)
	serveAborted(t, Log())

	got := lines()
	if len(got) != 1 {
		t.Fatalf("应有且只有一条访问日志，got=%v", got)
	}
	if got[0]["status"] != float64(499) {
		t.Errorf("中止的请求该记成 499，got=%v", got[0]["status"])
	}
	if e, _ := got[0]["errors"].(string); !strings.Contains(e, "net/http: abort Handler") {
		t.Errorf("errors 里该带着中止的原因，got=%v", got[0]["errors"])
	}
}

func TestLog_ErrorTextIsRedacted(t *testing.T) {
	// echo 里返回 error 是常规写法，错误原文进 errors 字段：驱动、下游报的错里常夹着凭证
	for msg, want := range map[string]string{"db down": "db down", "login failed password=hunter2": Redacted} {
		lines := capture(t)
		serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, func(c echo.Context) error { return errors.New(msg) })
		if got := accessLogs(lines())[0]["errors"]; got != want {
			t.Errorf("errors=%v，%q 该记成 %q", got, msg, want)
		}
	}
}

func TestLog_DSNPasswordInHTTPErrorIsMasked(t *testing.T) {
	// echo 的 HTTPError 把 Internal 拼进 Error()：code=502, message=..., internal=<驱动报的错>
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, func(c echo.Context) error {
		return echo.NewHTTPError(502, "upstream").SetInternal(errors.New("dial postgres://app:hunter2@db:5432/prod failed"))
	})
	want := "code=502, message=upstream, internal=dial postgres://app:" + Redacted + "@db:5432/prod failed"
	if got := accessLogs(lines())[0]["errors"]; got != want {
		t.Errorf("errors=%v, want %q", got, want)
	}
}

func TestRecover_PanicValueIsRedacted(t *testing.T) {
	// panic 的值和 handler 返回的错误一样会夹带凭证，原样进 error 字段就是密码落盘
	for msg, want := range map[string]string{
		"dial app:hunter2@tcp(db:3306)/prod failed": "dial app:" + Redacted + "@tcp(db:3306)/prod failed",
		"login failed password=hunter2":             Redacted,
	} {
		lines := capture(t)
		serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(nil)}, func(c echo.Context) error { panic(errors.New(msg)) })
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

func TestLog_EncodedResponseBodyOmitted(t *testing.T) {
	// 回归用例：Log 在 echo 的 Gzip 外面，截下来的是压缩过的字节：JSON 解不出来，
	// 原先整个记成 ***REDACTED***，什么都看不到。压缩过的不记内容，记一句 omitted
	lines := capture(t)
	req := get("/hello")
	req.Header.Set("Accept-Encoding", "gzip")
	body := strings.Repeat("hello ", 200)
	w := serve(t, req, []echo.MiddlewareFunc{Log(WithBody(false, true)), echomw.Gzip()}, func(c echo.Context) error {
		return c.JSON(200, map[string]string{"greeting": body})
	})
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("前提：响应该是 gzip 压缩过的，got=%v", w.Header())
	}
	l := lines()[0]
	if got := l["response_body"]; got != "[gzip-encoded content omitted]" {
		t.Errorf("压缩过的响应体该记 omitted，got=%q", got)
	}
	// 钉住 README 里写的数：echo 的 Response.Size 数的是交给 Gzip 之前的字节，不是线上的
	if want := len(`{"greeting":""}`+"\n") + len(body); l["bytes_out"] != float64(want) || w.Body.Len() >= want {
		t.Errorf("bytes_out 该是压缩前的 %d 字节（线上 %d），got=%v", want, w.Body.Len(), l["bytes_out"])
	}
}

func TestLog_ResponseCaptureSatisfiesFlusherAndHijacker(t *testing.T) {
	// 回归用例：打开响应体日志之后 c.Response().Writer 是截响应的 writer。原先它只有 Unwrap：
	// http.ResponseController 和 c.Response().Flush() 找得到下面的 writer，直接做类型断言的代码
	// （老的 SSE、WebSocket 库）却断言失败
	capture(t)
	var flusher, hijacker bool
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		_, flusher = c.Response().Writer.(http.Flusher)
		_, hijacker = c.Response().Writer.(http.Hijacker)
		return nil
	})
	if !flusher || !hijacker {
		t.Errorf("截响应的 writer 该满足 http.Flusher=%v、http.Hijacker=%v", flusher, hijacker)
	}
}

func TestLog_ResponseCaptureFlushReachesClient(t *testing.T) {
	// 真实连接上：经类型断言 Flush 的那一段，客户端要在 handler 返回之前就读得到
	capture(t)
	release := make(chan struct{})
	e := newEcho([]echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		c.Response().Header().Set("Content-Type", "text/event-stream")
		c.Response().WriteHeader(200)
		_, _ = c.Response().Write([]byte("data: 1\n\n"))
		c.Response().Writer.(http.Flusher).Flush()
		<-release
		return nil
	})
	srv := httptest.NewServer(e)
	defer srv.Close()
	defer close(release)
	resp, err := http.Get(srv.URL + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: 1\n" {
		t.Errorf("Flush 之后客户端该读到第一段，got=%q err=%v", line, err)
	}
}

func TestLog_ResponseCaptureHijack(t *testing.T) {
	// 真实连接上经类型断言 Hijack，拿到的是原始连接；下面的 writer 不支持时返回错误，和 net/http 一样
	capture(t)
	e := newEcho([]echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		conn, buf, err := c.Response().Writer.(http.Hijacker).Hijack()
		if err != nil {
			return err
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 6\r\nConnection: close\r\n\r\nhijack")
		return buf.Flush()
	})
	srv := httptest.NewServer(e)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hijack" {
		t.Errorf("Hijack 该拿到原始连接，got=%q", b)
	}

	var herr error
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		_, _, herr = c.Response().Writer.(http.Hijacker).Hijack() // httptest.ResponseRecorder 不支持
		return nil
	})
	if !errors.Is(herr, http.ErrNotSupported) {
		t.Errorf("下面的 writer 不支持 Hijack 时该返回 http.ErrNotSupported，got=%v", herr)
	}
}

func TestLog_ResponseCaptureKeepsResponseControllerFeatures(t *testing.T) {
	// Flush、Hijack 之外，http.ResponseController 的读写超时、全双工也得落到真正的连接上：
	// 它靠 Unwrap 一层层找下去，截响应的 writer 不交出原来的 writer 的话一律 ErrNotSupported
	capture(t)
	var werr, rerr error
	e := newEcho([]echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		rc := http.NewResponseController(c.Response())
		werr = rc.SetWriteDeadline(time.Now().Add(time.Minute))
		rerr = rc.SetReadDeadline(time.Now().Add(time.Minute))
		return c.String(200, "ok")
	})
	srv := httptest.NewServer(e)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if werr != nil || rerr != nil {
		t.Errorf("读写超时该设到真正的连接上，got write=%v read=%v", werr, rerr)
	}
}

func TestRequestUnchangedAfterServeHTTP(t *testing.T) {
	// 交进来的 *http.Request 一律不改（net/http 的约定）：换 ctx 用的是 r.WithContext 的副本，
	// 调 e.ServeHTTP 的一方拿回的是原样的请求，同一个请求交进来两次 ctx 也不会越套越深
	recording(t) // 装上 TraceContext：Propagate 接上游的 traceparent，ctx 才真的变了
	for name, mw := range map[string]echo.MiddlewareFunc{"LogScope": LogScope(), "Trace": Trace(), "Propagate": Propagate()} {
		req := get("/hello")
		req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		before := req.Context()
		var inside context.Context
		serve(t, req, []echo.MiddlewareFunc{mw}, func(c echo.Context) error {
			inside = c.Request().Context()
			return nil
		})
		if req.Context() != before {
			t.Errorf("%s：交进来的请求不该被改", name)
		}
		if inside == before {
			t.Errorf("%s：handler 看到的该是换过的 ctx", name)
		}
	}
}
