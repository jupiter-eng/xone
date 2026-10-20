package xecho

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/xecho/middleware"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xlog"
	"github.com/jupiter-eng/xone/xmetric"
)

func load(t *testing.T, yml string) Config {
	t.Helper()
	c := DefaultConfig()
	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.Reset)
	if err := config.LoadInto(path, ConfigKey, &c); err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	return c
}

func loadErr(t *testing.T, yml string) error {
	t.Helper()
	c := DefaultConfig()
	path := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(path, []byte(yml), 0o644)
	t.Cleanup(config.Reset)
	return config.LoadInto(path, ConfigKey, &c)
}

// configWith 默认配置上改几项。
//
// 测试一律经 WithConfig 把配置交进去，不读配置文件：跑测试的机器上设了
// XONE_CONFIG 也不受影响。专门测「读配置文件」的那几条用 testkit.UseConfigEnv
func configWith(mutate ...func(*Config)) Config {
	c := DefaultConfig()
	for _, m := range mutate {
		m(&c)
	}
	return c
}

// quiet 关掉访问日志和指标：测的不是它们时，省得日志刷屏、指标端点占着路由
func quiet(c *Config) { c.Log, c.Metric = false, false }

// on 监听本机的这个端口
func on(port int) func(*Config) {
	return func(c *Config) { c.Host, c.Port = "127.0.0.1", port }
}

// serving 起服务并等它真的开始监听，测试结束时关掉。返回 http://127.0.0.1:port
func serving(t *testing.T, x *XEcho, port int) string {
	t.Helper()
	go func() { _ = x.Start(context.Background()) }()
	t.Cleanup(func() { _ = x.Stop(context.Background()) })
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/nothing")
	return base
}

// startErr 调 Start 并取它返回的错误。配置非法时它该当场返回、不开始监听——
// 2s 还没返回就当它在监听，关掉并判失败
func startErr(t *testing.T, x *XEcho) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- x.Start(context.Background()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		_ = x.Stop(context.Background())
		t.Fatal("配置非法时 Start 该直接返回错误，它却开始监听了")
		return nil
	}
}

// ok 只回一个 200
func ok(c echo.Context) error { return c.NoContent(200) }

// echoClientIP 注册 GET /client-ip，响应体就是 handler 看到的 c.RealIP()
func echoClientIP(e *echo.Echo) {
	e.GET("/client-ip", func(c echo.Context) error { return c.String(200, c.RealIP()) })
}

// clientIPOf 从 remote 带着伪造的 X-Forwarded-For: 1.2.3.4 请求一次 /client-ip（见 echoClientIP），
// 返回 handler 看到的 c.RealIP()
func clientIPOf(t *testing.T, e *echo.Echo, remote string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/client-ip", nil)
	req.RemoteAddr = remote
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w.Body.String()
}

// captureLog 把 slog 的默认输出换成 JSON 写进 buf，测试结束还原
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// ---- 配置 ----

func TestConfig_Defaults(t *testing.T) {
	c := DefaultConfig()
	if c.Host != "0.0.0.0" || c.Port != 8080 {
		t.Errorf("监听默认值不对，got=%+v", c)
	}
	// 不限制的话，慢客户端可以一直占着连接不放
	if c.ReadHeaderTimeout != 10*time.Second || c.IdleTimeout != 60*time.Second {
		t.Errorf("读请求头和空闲连接必须有超时，got=%v %v", c.ReadHeaderTimeout, c.IdleTimeout)
	}
	if c.ReadTimeout != 0 || c.WriteTimeout != 0 {
		t.Errorf("读写超时默认不限，got=%v %v", c.ReadTimeout, c.WriteTimeout)
	}
	if !c.Log || !c.Trace || !c.Metric {
		t.Errorf("三个内置中间件默认都该开着，got=%+v", c)
	}
	if c.LogRequestBody || c.LogResponseBody || c.LogQuery || c.LogRequestHeaders || c.LogResponseHeaders || c.UseH2C {
		t.Errorf("记 body、查询串、请求头、响应头和 h2c 默认都该关着，got=%+v", c)
	}
	// 开着的话 echo 默认的错误处理会把错误原文写进响应
	if c.Debug {
		t.Errorf("Debug 默认该关着，got=%+v", c)
	}
	if c.MetricPath != "/metrics" || c.TLS.MinVersion != "1.2" {
		t.Errorf("指标路径默认应为 /metrics、MinVersion 1.2，got=%+v", c)
	}
	if len(c.LogSkipPaths) != 0 {
		t.Errorf("默认不跳过任何路径，got=%v", c.LogSkipPaths)
	}
	if !reflect.DeepEqual(c.TrustedProxies, []string{"private"}) {
		t.Errorf("默认只信私有网段，got=%v", c.TrustedProxies)
	}
}

func TestConfig_LoadFromFile(t *testing.T) {
	c := load(t, "XEcho:\n  Port: 9090\n  ReadTimeout: 30s\n  UseH2C: true\n  Log: false\n  LogSkipPaths: [/healthz, /static/]\n")
	if c.Port != 9090 || c.ReadTimeout != 30*time.Second || !c.UseH2C {
		t.Errorf("配置没生效，got=%+v", c)
	}
	if c.Log || strings.Join(c.LogSkipPaths, " ") != "/healthz /static/" {
		t.Errorf("中间件的开关也在配置里，got=%+v", c)
	}
	if c.Host != "0.0.0.0" || !c.Trace || c.MetricPath != "/metrics" {
		t.Errorf("没写的字段应保持默认，got=%+v", c)
	}
}

func TestConfig_FailsOnTypo(t *testing.T) {
	if err := loadErr(t, "XEcho:\n  Prot: 9090\n"); err == nil {
		t.Fatal("字段拼错应当启动失败")
	}
	// gin 特有的几项这里没有：照抄 XGin 块的配置要当场报出来，不是静默不生效
	for _, f := range []string{"Mode: release", "MaxMultipartMemory: 1024", "ZHTranslations: true"} {
		if err := loadErr(t, "XEcho:\n  "+f+"\n"); err == nil {
			t.Errorf("%s 不是 XEcho 的字段，应当启动失败", f)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("默认配置应当合法：%v", err)
	}

	for name, mutate := range map[string]func(*Config){
		"端口为 0":              func(c *Config) { c.Port = 0 },
		"端口越界":               func(c *Config) { c.Port = 70000 },
		"只配了证书":              func(c *Config) { c.TLS.CertFile = "a.pem" },
		"只配了私钥":              func(c *Config) { c.TLS.KeyFile = "a.key" },
		"MetricPath 不以 / 开头": func(c *Config) { c.MetricPath = "metrics" },
		"MetricPath 为空":      func(c *Config) { c.MetricPath = "" },
	} {
		c := DefaultConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s 应当报错", name)
		}
	}

	// 关掉指标时 MetricPath 用不上，写成什么都不该拦
	c := DefaultConfig()
	c.Metric, c.MetricPath = false, ""
	if err := c.Validate(); err != nil {
		t.Errorf("关掉指标时不该检查 MetricPath：%v", err)
	}
}

func TestValidate_RejectsZeroOrNegativeTimeouts(t *testing.T) {
	// net/http 对这几项的零值和负值都是「不设防」而不是「用个默认值」，
	// 前提由 xgin 的 TestNetHTTP_ReadHeaderTimeoutZeroLetsSlowClientHoldConn 钉着
	for name, mutate := range map[string]func(*Config){
		"ReadHeaderTimeout 为 0": func(c *Config) { c.ReadHeaderTimeout = 0 },
		"ReadHeaderTimeout 为负":  func(c *Config) { c.ReadHeaderTimeout = -time.Second },
		"IdleTimeout 为 0":       func(c *Config) { c.IdleTimeout = 0 },
		"IdleTimeout 为负":        func(c *Config) { c.IdleTimeout = -time.Second },
		"ReadTimeout 为负":        func(c *Config) { c.ReadTimeout = -time.Second },
		"WriteTimeout 为负":       func(c *Config) { c.WriteTimeout = -time.Second },
	} {
		c := DefaultConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s 应当报错", name)
		}
	}

	// ReadTimeout / WriteTimeout 的 0 是有意的「不限制」，默认就是它
	c := DefaultConfig()
	c.ReadTimeout, c.WriteTimeout = 0, 0
	if err := c.Validate(); err != nil {
		t.Errorf("ReadTimeout / WriteTimeout 为 0 是默认值，不该报错：%v", err)
	}
}

func TestValidate_HalfConfiguredTLS(t *testing.T) {
	// 这是最危险的一种配错：服务会以明文起来，而配置文件看上去是配了证书的
	c := DefaultConfig()
	c.TLS.CertFile = "cert.pem"
	err := c.Validate()
	if err == nil {
		t.Fatal("只配一半的 TLS 应当启动失败，而不是静默降级成明文")
	}
	if !strings.Contains(err.Error(), "TLS.CertFile") || !strings.Contains(err.Error(), "TLS.KeyFile") {
		t.Errorf("错误该说清楚缺了什么，got=%v", err)
	}
}

func TestValidate_InvalidProxyCIDRFailsStartup(t *testing.T) {
	// 写错一段就起不来：半对半错的代理表让 client_ip 一半真一半假，比起不来难查得多
	c := DefaultConfig()
	c.TrustedProxies = []string{"10.0.0.0/8", "10.0.0.0/33"}
	if err := c.Validate(); err == nil {
		t.Fatal("网段写错了应该报错")
	}

	c.TrustedProxies = []string{"10.0.0.0/8", "192.168.1.1", "::1"}
	if err := c.Validate(); err != nil {
		t.Errorf("这几个都是合法写法，不该报错: %v", err)
	}
}

// ---- 装配 ----

func TestBuild_PanicRecoveredAndStillLogged(t *testing.T) {
	// Recover 必须是内置里最内层的：panic 在哪一层被兜住，
	// 比它更内层的中间件里 next(c) 之后的代码就都不执行了。
	// echo 自己不兜 panic：实测连接直接断掉，栈进 stderr
	buf := captureLog(t)
	x := New().WithConfig(DefaultConfig()).WithRoutes(func(e *echo.Echo) {
		e.GET("/boom", func(echo.Context) error { panic("炸了") })
	})

	w := doRequest(t, x.Engine(), "GET", "/boom")
	if w.Code != 500 || strings.TrimSpace(w.Body.String()) != `{"message":"Internal Server Error"}` {
		t.Errorf("panic 应被兜住并返回 echo 默认的 500，got=%d %q", w.Code, w.Body.String())
	}
	out := buf.String()
	if !strings.Contains(out, `"msg":"panic while handling request"`) ||
		!strings.Contains(out, `"msg":"request completed"`) || !strings.Contains(out, `"status":500`) {
		t.Errorf("panic 日志和状态码 500 的访问日志都该有：\n%s", out)
	}
}

func TestEcho_PanicNotRecoveredByDefault(t *testing.T) {
	// 钉住上面那条的前提：echo 自己不兜 panic，一路抛到 net/http（它断开连接、把栈写进 stderr）。
	// 哪天升级后 echo 开始自己兜了，这条先红，Recover 可以跟着重新评估
	e := echo.New()
	e.GET("/boom", func(echo.Context) error { panic("炸了") })
	defer func() {
		if recover() == nil {
			t.Error("echo 默认不兜 panic，该抛到 ServeHTTP 外面")
		}
	}()
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/boom", nil))
}

func TestBuild_MetricsEndpointAutoRegistered(t *testing.T) {
	x := New().WithConfig(DefaultConfig())
	if w := doRequest(t, x.Engine(), "GET", "/metrics"); w.Code != 200 {
		t.Errorf("启用指标时应自动注册 /metrics，got=%d", w.Code)
	}
	if !strings.Contains(doRequest(t, x.Engine(), "GET", "/metrics").Body.String(), "http_requests_total") {
		t.Error("指标端点应导出请求数指标")
	}
}

func TestBuild_MetricsPathConfigurable(t *testing.T) {
	x := New().WithConfig(configWith(func(c *Config) { c.MetricPath = "/internal/metrics" }))
	if w := doRequest(t, x.Engine(), "GET", "/internal/metrics"); w.Code != 200 {
		t.Errorf("应注册在配置的路径上，got=%d", w.Code)
	}
	if w := doRequest(t, x.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("默认路径上不该再有")
	}
}

func TestBuild_NoMetricsEndpointWhenDisabled(t *testing.T) {
	x := New().WithConfig(configWith(func(c *Config) { c.Metric = false }))
	if w := doRequest(t, x.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("关掉指标后不该有 /metrics")
	}
}

func TestEcho_MetricPathWithoutSlashIsRewritten(t *testing.T) {
	// 钉住 MetricPath 那条校验的前提（echo v4.16.0）：不以 / 开头的路径被悄悄改写，
	// 空串注册在根路径上、被后注册的首页悄悄盖掉——不报错，也不 panic
	e := echo.New()
	e.GET("metrics", func(c echo.Context) error { return c.String(200, "M") })
	if w := doRequest(t, e, "GET", "/metrics"); w.Body.String() != "M" {
		t.Errorf("echo 该把 metrics 改写成 /metrics，got=%d %q", w.Code, w.Body.String())
	}
	e = echo.New()
	e.GET("", func(c echo.Context) error { return c.String(200, "M") })
	e.GET("/", func(c echo.Context) error { return c.String(200, "home") })
	if w := doRequest(t, e, "GET", "/"); w.Body.String() != "home" {
		t.Errorf("空路径该落在 / 上、被后注册的首页盖掉，got=%q", w.Body.String())
	}
}

func TestBuild_Idempotent(t *testing.T) {
	// 装配两遍会把中间件注册两遍，表现是每个请求打两条日志、指标翻倍
	x := New().WithConfig(DefaultConfig())
	if x.Engine() != x.Engine() {
		t.Error("重复装配应返回同一个 echo")
	}
}

func TestBuild_UserMiddlewareAfterBuiltin(t *testing.T) {
	var order []string
	mark := func(name string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error { order = append(order, name); return next(c) }
		}
	}
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace, c.Metric = false, false, false })).
		WithMiddleware(mark("用户1"), mark("用户2")).
		WithRoutes(func(e *echo.Echo) {
			e.GET("/x", func(c echo.Context) error { order = append(order, "handler"); return c.NoContent(200) })
		})

	doRequest(t, x.Engine(), "GET", "/x")
	if strings.Join(order, ",") != "用户1,用户2,handler" {
		t.Errorf("用户中间件应按顺序排在 handler 之前，got=%v", order)
	}
}

func TestBuild_UserMiddlewareInsideRecover(t *testing.T) {
	// 用户中间件排在所有内置中间件之后：它自己 panic 也由 Recover 兜住，访问日志照样记
	buf := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false })).
		WithMiddleware(func(echo.HandlerFunc) echo.HandlerFunc {
			return func(echo.Context) error { panic("中间件炸了") }
		})
	if w := doRequest(t, x.Engine(), "GET", "/x"); w.Code != 500 {
		t.Errorf("用户中间件的 panic 该被兜住，got=%d", w.Code)
	}
	if !strings.Contains(buf.String(), `"status":500`) {
		t.Errorf("访问日志该记下 500：\n%s", buf.String())
	}
}

// ---- 开关 ----

func TestLog_NoAccessLogWhenDisabled(t *testing.T) {
	buf := captureLog(t)
	route := func(e *echo.Echo) { e.GET("/work", func(c echo.Context) error { return c.String(200, "ok") }) }

	e := New().WithConfig(configWith(quiet)).WithRoutes(route).Engine()
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/work", nil))
	if strings.Contains(buf.String(), "request completed") {
		t.Errorf("Log: false 时不该有访问日志：%s", buf.String())
	}

	e = New().WithConfig(configWith(func(c *Config) { c.Metric = false })).WithRoutes(route).Engine()
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/work", nil))
	if !strings.Contains(buf.String(), "request completed") {
		t.Errorf("默认该记访问日志：%s", buf.String())
	}
}

func TestLog_ErrorStatusesLoggedAsSent(t *testing.T) {
	// 装配出来的整条链：handler 返回的错误、router 的 404 / 405，访问日志记的是客户端收到的那个
	for _, c := range []struct {
		method, path string
		status       int
		route        string
	}{
		{"GET", "/nope", 404, "unmatched"},
		{"POST", "/only-get", 405, "unmatched"},
		{"GET", "/fail", 500, "/fail"},
		{"GET", "/teapot", 418, "/teapot"},
	} {
		buf := captureLog(t)
		e := New().WithConfig(DefaultConfig()).WithRoutes(func(e *echo.Echo) {
			e.GET("/only-get", ok)
			e.GET("/fail", func(echo.Context) error { return errors.New("boom") })
			e.GET("/teapot", func(echo.Context) error { return echo.NewHTTPError(418) })
		}).Engine()
		w := doRequest(t, e, c.method, c.path)
		want := fmt.Sprintf(`"route":%q,"path":%q,"status":%d`, c.route, c.path, c.status)
		if w.Code != c.status || !strings.Contains(buf.String(), want) ||
			!strings.Contains(buf.String(), fmt.Sprintf(`"bytes_out":%d`, w.Body.Len())) {
			t.Errorf("%s %s：客户端收到 %d（%d 字节），访问日志该对得上：\n%s", c.method, c.path, w.Code, w.Body.Len(), buf.String())
		}
	}
}

func TestLogSkipPaths_ReallySkipsThesePaths(t *testing.T) {
	// 光在配置里写上不够：它得一路传到 middleware.Log 里
	buf := captureLog(t)
	e := New().WithConfig(configWith(func(c *Config) {
		c.Metric, c.Trace = false, false
		c.LogSkipPaths = []string{"/healthz", "/static/"}
	})).WithRoutes(func(e *echo.Echo) {
		e.GET("/healthz", ok)
		e.GET("/static/*", ok)
		e.GET("/work", ok)
	}).Engine()

	for _, path := range []string{"/healthz", "/static/app.js"} {
		e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
		if strings.Contains(buf.String(), path) {
			t.Errorf("跳过的路径 %s 不该留下访问日志：%s", path, buf.String())
		}
	}
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/work", nil))
	if !strings.Contains(buf.String(), "/work") {
		t.Errorf("没跳过的路径应当照常记：%s", buf.String())
	}
}

func TestLogSkipPaths_MetricsPathAddedAutomatically(t *testing.T) {
	// 指标端点会被抓取系统按秒轮询，记日志纯属刷屏
	buf := captureLog(t)
	e := New().WithConfig(configWith(func(c *Config) {
		c.Trace = false
		c.MetricPath = "/internal/metrics"
	})).Engine()
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/internal/metrics", nil))

	if strings.Contains(buf.String(), "/internal/metrics") {
		t.Errorf("指标端点不该留下访问日志：%s", buf.String())
	}
}

func TestLogBody_TogglesAreIndependent(t *testing.T) {
	// 请求体和响应体各有一个开关。两个都要一路传到 middleware.Log 里，
	// 也不能接反：只开了请求体的人，响应体（可能带着令牌）不该进日志
	for _, c := range []struct {
		name      string
		req, resp bool
	}{
		{"只记请求体", true, false},
		{"只记响应体", false, true},
		{"默认都不记", false, false},
	} {
		buf := captureLog(t)
		e := New().WithConfig(configWith(func(cfg *Config) {
			cfg.Metric, cfg.Trace = false, false
			cfg.LogRequestBody, cfg.LogResponseBody = c.req, c.resp
		})).WithRoutes(func(e *echo.Echo) {
			e.POST("/echo", func(c echo.Context) error { return c.JSON(200, map[string]string{"greeting": "hi-bob"}) })
		}).Engine()

		req := httptest.NewRequest("POST", "/echo", strings.NewReader(`{"name":"alice"}`))
		req.Header.Set("Content-Type", "application/json")
		e.ServeHTTP(httptest.NewRecorder(), req)

		out := buf.String()
		if got := strings.Contains(out, "request_body") && strings.Contains(out, "alice"); got != c.req {
			t.Errorf("%s：请求体进了日志=%v，want %v\n%s", c.name, got, c.req, out)
		}
		if got := strings.Contains(out, "response_body") && strings.Contains(out, "hi-bob"); got != c.resp {
			t.Errorf("%s：响应体进了日志=%v，want %v\n%s", c.name, got, c.resp, out)
		}
	}
}

func TestLogQueryAndHeaders_TogglesReachMiddleware(t *testing.T) {
	// 三个开关各自一路传到 middleware.Log：接反了或接成常量，关着的那项照样进日志
	for _, c := range []struct {
		name                string
		query, reqH, header bool
	}{
		{"只记查询串", true, false, false},
		{"只记请求头", false, true, false},
		{"只记响应头", false, false, true},
		{"默认都不记", false, false, false},
	} {
		buf := captureLog(t)
		e := New().WithConfig(configWith(func(cfg *Config) {
			cfg.Metric, cfg.Trace = false, false
			cfg.LogQuery, cfg.LogRequestHeaders, cfg.LogResponseHeaders = c.query, c.reqH, c.header
		})).WithRoutes(func(e *echo.Echo) {
			e.GET("/search", func(c echo.Context) error {
				c.Response().Header().Set("X-Page-Total", "7")
				c.Response().Header().Set("Set-Cookie", "sid=abc123")
				return c.NoContent(200)
			})
		}).Engine()
		req := httptest.NewRequest("GET", "/search?page=2&token=t0p", nil)
		req.Header.Set("X-Visible", "keep-me")
		req.Header.Set("Authorization", "Bearer h0rse")
		e.ServeHTTP(httptest.NewRecorder(), req)

		out := buf.String()
		if got := strings.Contains(out, `"query":"page=2`); got != c.query {
			t.Errorf("%s：查询串进了日志=%v，want %v\n%s", c.name, got, c.query, out)
		}
		if got := strings.Contains(out, "request_headers") && strings.Contains(out, "keep-me"); got != c.reqH {
			t.Errorf("%s：请求头进了日志=%v，want %v\n%s", c.name, got, c.reqH, out)
		}
		if got := strings.Contains(out, "response_headers") && strings.Contains(out, "X-Page-Total"); got != c.header {
			t.Errorf("%s：响应头进了日志=%v，want %v\n%s", c.name, got, c.header, out)
		}
		if strings.Contains(out, "t0p") || strings.Contains(out, "h0rse") || strings.Contains(out, "abc123") {
			t.Errorf("%s：查询串里的 token、Authorization、Set-Cookie 该被遮掉\n%s", c.name, out)
		}
	}
}

func TestTrace_NoSpanWhenDisabled(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	for _, on := range []bool{true, false} {
		exp.Reset()
		e := New().WithConfig(configWith(quiet, func(c *Config) { c.Trace = on })).Engine()
		w := doRequest(t, e, "GET", "/nope")

		if got := len(exp.GetSpans()) > 0; got != on {
			t.Errorf("Trace=%v 时开了 Span=%v", on, got)
		}
		if got := w.Header().Get(middleware.TraceIDHeader) != ""; got != on {
			t.Errorf("Trace=%v 时响应里带了 %s=%v", on, middleware.TraceIDHeader, got)
		}
	}
}

func TestMetric_NoRequestMetricsWhenDisabled(t *testing.T) {
	// 光不注册 /metrics 不够：指标中间件本身也得跟着开关走，
	// 否则关掉指标的服务照样在每个请求上付记指标的代价
	m, closer, err := xmetric.New(xmetric.Config{Namespace: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	for _, metric := range []bool{true, false} {
		route := fmt.Sprintf("/probe-%v", metric)
		e := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace, c.Metric = false, false, metric })).
			WithRoutes(func(e *echo.Echo) { e.GET(route, ok) }).
			Engine()
		e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", route, nil))

		out := testkit.Scrape(m.Handler)
		if got := strings.Contains(out, `route="`+route+`"`); got != metric {
			t.Errorf("Metric=%v 时记了请求指标=%v\n%s", metric, got, out)
		}
	}
}

func TestWithRecoverFunc_ReplacesPanicResponse(t *testing.T) {
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace, c.Metric = false, false, false })).
		WithRecoverFunc(func(c echo.Context, _ any) error { return c.String(503, "custom") }).
		WithRoutes(func(e *echo.Echo) {
			e.GET("/boom", func(echo.Context) error { panic("boom") })
		})

	w := doRequest(t, x.Engine(), "GET", "/boom")
	if w.Code != 503 || w.Body.String() != "custom" {
		t.Errorf("自定义 recover 没生效，got=%d %q", w.Code, w.Body.String())
	}
}

// validatorFunc 一个最小的 echo.Validator：真实的实现用 go-playground/validator 之类，
// 选哪个是业务的事，框架只负责把它装到 e.Validator 上
type validatorFunc func(any) error

func (f validatorFunc) Validate(i any) error { return f(i) }

func TestEcho_NoValidatorByDefault(t *testing.T) {
	// 钉住 echo 的默认（v4.16.0）：没装校验器时 c.Validate 返回的是 ErrValidatorNotRegistered，
	// 不是「校验通过」——一个忘了装校验器的服务不会有任何迹象，直到有人发一份不合法的请求体进来。
	// 它是普通 error，handler 直接 return 它的话客户端收到的是 500 {"message":"Internal Server Error"}
	var got error
	e := New().WithConfig(configWith(quiet)).WithRoutes(func(e *echo.Echo) {
		e.GET("/users", func(c echo.Context) error {
			got = c.Validate(&struct{ Name string }{})
			return c.NoContent(204)
		})
	}).Engine()
	doRequest(t, e, "GET", "/users")

	if !errors.Is(got, echo.ErrValidatorNotRegistered) {
		t.Errorf("没装校验器时 c.Validate 应返回 ErrValidatorNotRegistered，got=%v", got)
	}
}

func TestWithValidator_HandlerCanValidate(t *testing.T) {
	var seen any
	x := New().WithConfig(configWith(quiet)).
		WithValidator(validatorFunc(func(i any) error { seen = i; return errors.New("name is required") })).
		WithRoutes(func(e *echo.Echo) {
			e.GET("/users", func(c echo.Context) error {
				if err := c.Validate(&struct{ Name string }{}); err != nil {
					return echo.NewHTTPError(http.StatusBadRequest, err.Error())
				}
				return c.NoContent(204)
			})
		})

	w := doRequest(t, x.Engine(), "GET", "/users")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "name is required") {
		t.Errorf("校验器没装上，got=%d %q", w.Code, w.Body.String())
	}
	if _, ok := seen.(*struct{ Name string }); !ok {
		t.Errorf("c.Validate 应把 handler 传进去的那个值原样交给校验器，got=%T", seen)
	}
}

func TestWithValidator_CallbackOverrides(t *testing.T) {
	// 和 e.IPExtractor、e.HTTPErrorHandler 一样：回调里设的盖得过前面装上的
	x := New().WithConfig(configWith(quiet)).
		WithValidator(validatorFunc(func(any) error { return errors.New("from WithValidator") })).
		WithRoutes(func(e *echo.Echo) {
			e.Validator = validatorFunc(func(any) error { return errors.New("from callback") })
			e.GET("/users", func(c echo.Context) error { return c.String(200, c.Validate(&struct{}{}).Error()) })
		})

	if got := doRequest(t, x.Engine(), "GET", "/users").Body.String(); got != "from callback" {
		t.Errorf("回调里设的 e.Validator 该盖过 WithValidator，got=%q", got)
	}
}

// ---- 配置落到 echo 上 ----

func TestEcho_DefaultIPExtractorTrustsEveryone(t *testing.T) {
	// 钉住下面那几条的前提（echo v4.16.0）：IPExtractor 为 nil 时 c.RealIP() 谁发来的转发头都信。
	// 哪天 echo 的默认变安全了，这条先红
	e := echo.New()
	echoClientIP(e)
	if got := clientIPOf(t, e, "203.0.113.9:1234"); got != "1.2.3.4" {
		t.Errorf("echo 的默认是全都信，公网对端伪造的 X-Forwarded-For 照收，got=%q", got)
	}
	req := httptest.NewRequest("GET", "/client-ip", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Real-IP", "5.6.7.8")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Body.String() != "5.6.7.8" {
		t.Errorf("只带 X-Real-IP 也照收，got=%q", w.Body.String())
	}
}

func TestBuild_TrustsOnlyPrivateProxiesByDefault(t *testing.T) {
	// echo 自己的默认是谁发来的都信，任何人发一个 X-Forwarded-For 就能决定访问日志里的
	// client_ip——日志可以伪造，建在这个字段上的限流和审计一起失效。
	// 默认只信私有网段：负载均衡、Ingress 转发来的认，公网直连的不认
	buf := captureLog(t)
	e := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false })).WithRoutes(echoClientIP).Engine()
	if got := clientIPOf(t, e, "203.0.113.9:1234"); got != "203.0.113.9" {
		t.Errorf("公网对端的 X-Forwarded-For 不该认，client_ip 应该是对端地址本身，got=%q（请求头里伪造的是 1.2.3.4）", got)
	}
	if !strings.Contains(buf.String(), `"client_ip":"203.0.113.9"`) {
		t.Errorf("访问日志的 client_ip 也该是对端本身：\n%s", buf.String())
	}
	if got := clientIPOf(t, e, "10.0.0.5:1234"); got != "1.2.3.4" {
		t.Errorf("私有网段的对端（负载均衡）发来的 X-Forwarded-For 该认，got=%q", got)
	}
	req := httptest.NewRequest("GET", "/client-ip", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Real-IP", "5.6.7.8")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Body.String() != "203.0.113.9" {
		t.Errorf("公网对端的 X-Real-IP 也不该认，got=%q", w.Body.String())
	}
}

func TestBuild_ClientIPWalksForwardedChainLikeXgin(t *testing.T) {
	// 多跳代理：从右往左找第一个不可信的地址，和 xgin（gin 的 ClientIP）一样，
	// 两边一致由 xgin 的 TestClientIP_SharedRuleMatchesGin 逐条比对着
	e := New().WithConfig(configWith(quiet)).WithRoutes(echoClientIP).Engine()
	for xff, want := range map[string]string{
		"1.2.3.4, 5.6.7.8, 10.0.0.9": "5.6.7.8",  // 10.0.0.9 是自己的代理，5.6.7.8 是离我们最近的外人
		"10.0.0.7, 10.0.0.8":         "10.0.0.7", // 全都可信取最左边
		"1.2.3.4, junk":              "10.0.0.5", // 解不出的一项之后不再往左看，退回对端
	} {
		req := httptest.NewRequest("GET", "/client-ip", nil)
		req.RemoteAddr = "10.0.0.5:1"
		req.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		if w.Body.String() != want {
			t.Errorf("X-Forwarded-For: %s → client_ip 该是 %s，got=%q", xff, want, w.Body.String())
		}
	}
}

func TestBuild_TrustedProxiesEmptyListTrustsNone(t *testing.T) {
	// 默认值是 [private]，要一个都不信得能写出来
	e := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = []string{} })).
		WithRoutes(echoClientIP).Engine()
	if got := clientIPOf(t, e, "10.0.0.5:1234"); got != "10.0.0.5" {
		t.Errorf("写了 [] 就该谁都不信，got=%q", got)
	}
}

func TestBuild_ForwardedHeadersOnlyWithProxiesConfigured(t *testing.T) {
	e := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = []string{"203.0.113.0/24"} })).
		WithRoutes(echoClientIP).Engine()
	if got := clientIPOf(t, e, "203.0.113.9:1234"); got != "1.2.3.4" {
		t.Errorf("对端在信任网段内，应该认转发头里的地址，got=%q", got)
	}
	if got := clientIPOf(t, e, "10.0.0.5:1234"); got != "10.0.0.5" {
		t.Errorf("列表整体替换默认值，没写 private 就不信私有网段，got=%q", got)
	}
}

func TestBuild_CallbackEngineSettingsOverrideConfig(t *testing.T) {
	// 回调在配置落到 echo 上之后才跑：使用者在代码里明确设了的，就以代码为准。
	// 起了服务再看一次：Start 不能再落一遍配置
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port))).
		WithRoutes(echoClientIP, func(e *echo.Echo) { e.IPExtractor = echo.ExtractIPDirect() })
	x.Engine()
	serving(t, x, port)

	if got := clientIPOf(t, x.Engine(), "10.0.0.5:1234"); got != "10.0.0.5" {
		t.Errorf("回调里换成了 ExtractIPDirect，client_ip 该是直连地址，got=%q", got)
	}
}

func TestBuild_CallbackHTTPErrorHandlerStillLoggedAsSent(t *testing.T) {
	// 回调里换了错误处理：中间件经 c.Error 调的就是它，访问日志记它写出去的状态码
	buf := captureLog(t)
	e := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false })).
		WithRoutes(func(e *echo.Echo) {
			e.HTTPErrorHandler = func(err error, c echo.Context) { _ = c.String(503, "custom error") }
			e.GET("/fail", func(echo.Context) error { return errors.New("boom") })
		}).Engine()
	w := doRequest(t, e, "GET", "/fail")
	if w.Code != 503 || w.Body.String() != "custom error" || !strings.Contains(buf.String(), `"status":503`) {
		t.Errorf("该用回调里的错误处理、访问日志记 503，got=%d %q\n%s", w.Code, w.Body.String(), buf.String())
	}
}

func TestEcho_TrailingSlashAndHEADNotRouted(t *testing.T) {
	// 钉住 README「行为与实测」里写的两条 echo 默认（v4.16.0），框架没改：
	// 末尾斜杠严格匹配（/a/ 是 404，gin 默认 301 到 /a）；HEAD 不自动路由到 GET（405）
	e := New().WithConfig(configWith(quiet)).WithRoutes(func(e *echo.Echo) { e.GET("/a", ok) }).Engine()
	if w := doRequest(t, e, "GET", "/a/"); w.Code != 404 {
		t.Errorf("/a/ 该是 404，got=%d", w.Code)
	}
	if w := doRequest(t, e, "HEAD", "/a"); w.Code != 405 || w.Header().Get("Allow") != "OPTIONS, GET" {
		t.Errorf("HEAD 该是 405、Allow 是 OPTIONS, GET，got=%d %q", w.Code, w.Header().Get("Allow"))
	}
	if w := doRequest(t, e, "OPTIONS", "/a"); w.Code != 204 || w.Header().Get("Allow") != "OPTIONS, GET" {
		t.Errorf("OPTIONS 由 echo 自己回 204，got=%d %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestEcho_TrailingParamMatchesSlashes(t *testing.T) {
	// 钉住 README「行为与实测」里的一条 echo 默认：路径参数在路由末尾时吃得下后面的 /，
	// /users/:id 匹配 /users/1/z，id 是 1/z
	e := echo.New()
	e.GET("/users/:id", func(c echo.Context) error { return c.String(200, c.Param("id")) })
	if w := doRequest(t, e, "GET", "/users/1/z"); w.Code != 200 || w.Body.String() != "1/z" {
		t.Errorf("got=%d %q", w.Code, w.Body.String())
	}
}

func TestDebug_OffByDefaultKeepsErrorDetailOutOfResponse(t *testing.T) {
	// 错误原文里常有 DSN、SQL、内网地址：它们进访问日志时是遮过的，进响应不遮。
	// 默认（e.Debug 为 false，同 echo.New）只回 echo 的那句通用文案
	e := New().WithConfig(configWith(quiet)).WithRoutes(func(e *echo.Echo) {
		e.GET("/fail", func(echo.Context) error { return errors.New("dial 10.0.0.9:5432: password=hunter2") })
	}).Engine()

	w := doRequest(t, e, "GET", "/fail")
	if w.Code != 500 || strings.TrimSpace(w.Body.String()) != `{"message":"Internal Server Error"}` {
		t.Errorf("默认不该把错误原文写进响应，got=%d %q", w.Code, w.Body.String())
	}
}

func TestDebug_PutsErrorDetailInResponse(t *testing.T) {
	// Debug: true 落到 e.Debug 上，echo 默认的错误处理就把错误原文放进响应（v4.16.0）
	e := New().WithConfig(configWith(quiet, func(c *Config) { c.Debug = true })).WithRoutes(func(e *echo.Echo) {
		e.GET("/fail", func(echo.Context) error { return errors.New("boom") })
	}).Engine()

	w := doRequest(t, e, "GET", "/fail")
	if w.Code != 500 || w.Body.String() != "{\n  \"error\": \"boom\",\n  \"message\": \"Internal Server Error\"\n}\n" {
		t.Errorf("Debug 开着时错误原文该进响应（而且是缩进过的），got=%d %q", w.Code, w.Body.String())
	}
}

func TestDebug_PrettyPrintsJSON(t *testing.T) {
	// e.Debug 的另一件事：c.JSON / c.JSONP / c.XML 一律缩进两个空格（context.go 的
	// c.echo.Debug || pretty），等于每个请求都带了 ?pretty。响应体变大，bytes_out 跟着变
	route := func(e *echo.Echo) {
		e.GET("/j", func(c echo.Context) error { return c.JSON(200, map[string]string{"a": "b"}) })
	}
	plain := New().WithConfig(configWith(quiet)).WithRoutes(route).Engine()
	debug := New().WithConfig(configWith(quiet, func(c *Config) { c.Debug = true })).WithRoutes(route).Engine()

	if got := doRequest(t, plain, "GET", "/j").Body.String(); got != "{\"a\":\"b\"}\n" {
		t.Errorf("Debug 关着时 c.JSON 不缩进（10 字节），got=%q", got)
	}
	if got := doRequest(t, debug, "GET", "/j").Body.String(); got != "{\n  \"a\": \"b\"\n}\n" {
		t.Errorf("Debug 开着时 c.JSON 缩进两个空格（15 字节），got=%q", got)
	}

	// 访问日志记的是变大之后的字节数：bytes_out 数的是经 c.Response() 写出去的正文
	buf := captureLog(t)
	logged := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace, c.Debug = false, false, true })).
		WithRoutes(route).Engine()
	doRequest(t, logged, "GET", "/j")
	if !strings.Contains(buf.String(), `"bytes_out":15`) {
		t.Errorf("Debug 开着时访问日志该记 15 字节，got=%s", buf.String())
	}
}

func TestDebug_RaisesEchoLoggerLevel(t *testing.T) {
	// echo 只在 e.Start 里按 e.Debug 调 gommon 的级别，xecho 不用 e.Start，自己设（见 applyConfig）。
	// echo 唯一一条低于 ERROR 的自有日志：往已经提交的响应上再写一次（response.go，WARN）
	twice := func(e *echo.Echo) {
		e.GET("/twice", func(c echo.Context) error {
			_ = c.String(200, "a")
			return c.String(200, "b")
		})
	}
	for _, debug := range []bool{false, true} {
		buf := captureLog(t)
		e := New().WithConfig(configWith(quiet, func(c *Config) { c.Debug = debug })).WithRoutes(twice).Engine()
		doRequest(t, e, "GET", "/twice")

		if got := strings.Contains(buf.String(), "response already committed"); got != debug {
			t.Errorf("Debug=%v 时 echo 的 WARN 该 present=%v，got=%v\n%s", debug, debug, got, buf.String())
		}
	}
}

// ---- echo 自己的日志 ----

// failWriter 写响应体一律失败，像是客户端已经断开
type failWriter struct{ h http.Header }

func (w *failWriter) Header() http.Header       { return w.h }
func (w *failWriter) WriteHeader(int)           {}
func (w *failWriter) Write([]byte) (int, error) { return 0, errors.New("write: broken pipe") }

func TestEchoLogger_DefaultWritesToStdout(t *testing.T) {
	// 钉住 applyConfig 那一段的前提（echo v4.16.0 / gommon v0.5.0）：echo 自己的日志写 os.Stdout，
	// 格式是它自己的 JSON，不经过 slog
	if out := echo.New().Logger.Output(); out != os.Stdout {
		t.Errorf("echo 的日志默认写 os.Stdout，got=%T", out)
	}
}

func TestEchoLogger_RoutedToSlog(t *testing.T) {
	// 错误响应写失败时 echo 的默认错误处理调 e.Logger.Error：接到 slog 上，不再写 stdout
	buf := captureLog(t)
	e := New().WithConfig(configWith(quiet)).WithRoutes(func(e *echo.Echo) {
		e.GET("/fail", func(echo.Context) error { return errors.New("boom") })
	}).Engine()
	if out := e.Logger.Output(); out == os.Stdout || out == os.Stderr {
		t.Fatalf("echo 的日志不该再直写标准输出，got=%T", out)
	}
	e.ServeHTTP(&failWriter{h: http.Header{}}, httptest.NewRequest("GET", "/fail", nil))

	out := buf.String()
	if !strings.Contains(out, `"level":"ERROR","msg":"echo internal log","message":"write: broken pipe"`) {
		t.Errorf("echo 的日志该转成一条 slog 的 ERROR：\n%s", out)
	}

	// 级别沿用 echo 的默认（ERROR 以上才写）；各级别对得上
	buf.Reset()
	e.Logger.Warn("dropped")
	e.Logger.Print("printed")
	if strings.Contains(buf.String(), "dropped") || !strings.Contains(buf.String(), `"level":"INFO","msg":"echo internal log","message":"printed"`) {
		t.Errorf("WARN 被 echo 的级别挡掉，Print 记成 INFO：\n%s", buf.String())
	}
}

func TestEchoLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"DEBUG": slog.LevelDebug, "INFO": slog.LevelInfo, "WARN": slog.LevelWarn,
		"ERROR": slog.LevelError, "PANIC": slog.LevelError, "FATAL": slog.LevelError, "-": slog.LevelInfo,
	} {
		if got := echoLevel(in); got != want {
			t.Errorf("echoLevel(%q)=%v want %v", in, got, want)
		}
	}
}

// ---- 启停 ----

func TestStartStop_GracefulShutdown(t *testing.T) {
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *echo.Echo) {
		e.GET("/ping", func(c echo.Context) error { return c.String(200, "pong") })
	})

	done := make(chan error, 1)
	go func() { done <- x.Start(context.Background()) }()

	url := fmt.Sprintf("http://127.0.0.1:%d/ping", port)
	waitServing(t, url)

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "pong" {
		t.Errorf("响应不对，got=%s", body)
	}

	if err := x.Stop(context.Background()); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("优雅关闭不该返回错误，got=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start 没有在关闭后返回")
	}
}

func TestStart_ServerTimeoutsFromConfig(t *testing.T) {
	// echo 自己的 e.Server 四个超时全是 0；本模块不用它，用 web.Server 按配置建的那个。
	// 发半个请求头的连接到 ReadHeaderTimeout 就该被断开
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port), func(c *Config) { c.ReadHeaderTimeout = 200 * time.Millisecond }))
	serving(t, x, port)
	if e := echo.New(); e.Server.ReadHeaderTimeout != 0 || e.Server.IdleTimeout != 0 {
		t.Fatalf("前提：echo 的 e.Server 超时是 0，got=%+v", e.Server)
	}

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")) // 头只发一半
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err = io.ReadAll(conn)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("服务端该在 ReadHeaderTimeout 到点时断开，3s 了还没断")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("断开得太晚：%v", d)
	}
}

func TestStart_DoesNotListenOnInvalidConfig(t *testing.T) {
	err := startErr(t, New().WithConfig(configWith(func(c *Config) { c.TLS.CertFile = "只配了一半" })))

	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Module != "xecho" || xe.Op != "config" {
		t.Fatalf("该是 xecho 的 config 错误，got=%v", err)
	}
	// 进程里可能有两个服务：错误得说清楚是 WithConfig 那份，以及哪一项
	if !strings.Contains(err.Error(), "WithConfig") || !strings.Contains(err.Error(), "TLS.CertFile") {
		t.Errorf("错误该说清楚是哪份配置的哪一项，got=%v", err)
	}
}

func TestStart_UsesConfigFromBuildTime(t *testing.T) {
	// 监听地址、超时和 echo 上的中间件、信任的代理必须出自同一份配置。
	// 「装配之后才 WithConfig」不生效，监听的端口也就不该跟着变
	portA, portB := testkit.FreePort(t), testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(portA)))
	x.Engine()
	x.WithConfig(configWith(quiet, on(portB)))

	serving(t, x, portA)
}

func TestStart_SignalArrivesBeforeStart(t *testing.T) {
	// 照常监听的话，服务会在「已经收到停止信号」之后才起来，
	// 然后一直跑到框架等超时为止
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port)))
	if err := x.Stop(context.Background()); err != nil {
		t.Fatalf("还没启动就 Stop 不该报错：%v", err)
	}

	done := make(chan error, 1)
	go func() { done <- x.Start(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("应当直接返回，got=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 之后 Start 不该真的开始监听")
	}

	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
		conn.Close()
		t.Error("服务不该起来")
	}
}

func TestStart_RepeatedStartFails(t *testing.T) {
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port)))
	serving(t, x, port)

	err := x.Start(context.Background())
	if err == nil || xerror.Module(err) != "xecho" {
		t.Errorf("重复启动应当报 xecho 的错，got=%v", err)
	}
}

func TestStop_SafeWithoutStart(t *testing.T) {
	if err := New().Stop(context.Background()); err != nil {
		t.Errorf("没启动过的 Stop 不该报错：%v", err)
	}
}

// servingWithHungRequestDone 起一个服务，并让一个请求挂在 handler 里不返回；
// 返回一个在「那个挂住的请求结束时」关闭的 channel——强制断连有没有生效，只有它看得出来
func servingWithHungRequestDone(t *testing.T) (*XEcho, <-chan struct{}) {
	t.Helper()
	port := testkit.FreePort(t)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	x := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *echo.Echo) {
		e.GET("/ping", ok)
		e.GET("/hang", func(c echo.Context) error { <-release; return c.NoContent(200) })
	})
	go x.Start(context.Background())
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/ping")

	hung := make(chan struct{})
	go func() {
		defer close(hung)
		if resp, err := http.Get(base + "/hang"); err == nil {
			resp.Body.Close()
		}
	}()
	// 等这个请求真的到了 handler 里，否则 Shutdown 可能在它之前就走完了
	time.Sleep(100 * time.Millisecond)
	return x, hung
}

// stopWithin 带 200ms 的截止时间调 Stop，3s 还没返回就判失败
func stopWithin(t *testing.T, x *XEcho) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- x.Stop(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 该按调用方给的截止时间收手，3s 了还没返回")
		return nil
	}
}

func TestStop_HonorsCallerDeadline(t *testing.T) {
	// 等多久只看调用方的 ctx，xone.Run 给的是服务那一段停止预算
	x, _ := servingWithHungRequestDone(t)
	if err := stopWithin(t, x); err == nil {
		t.Error("在途请求没做完就到点了，该如实报错")
	}
}

func TestStop_ForceClosesInFlightConnsAfterTimeout(t *testing.T) {
	// Shutdown 超时只返回错误，它不动那些连接；要看的是那个在途请求有没有被断掉
	x, hung := servingWithHungRequestDone(t)
	if err := stopWithin(t, x); err == nil {
		t.Fatal("在途请求没做完就到点了，该报错")
	}

	select {
	case <-hung:
	case <-time.After(2 * time.Second):
		t.Error("超时之后在途请求仍在继续——框架接着就去关数据库了，它会摸到已关闭的连接池")
	}
}

func TestStop_WaitsForHandlersAfterForceClose(t *testing.T) {
	// Close 只关连接、取消请求的 ctx，handler 所在的协程照跑：
	// Stop 得等正在收尾的 handler 真正返回
	port := testkit.FreePort(t)
	var returned atomic.Bool
	x := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *echo.Echo) {
		e.GET("/ping", ok)
		e.GET("/hang", func(c echo.Context) error {
			<-c.Request().Context().Done()
			time.Sleep(50 * time.Millisecond) // 收尾：回滚事务、写审计……
			returned.Store(true)
			return nil
		})
	})
	go x.Start(context.Background())
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/ping")
	go func() {
		if resp, err := http.Get(base + "/hang"); err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond) // 等请求真的进了 handler

	// 预算的 20% 留给断连之后的收尾（shutdownCtx）：2s 的预算留 400ms，是这里 50ms 收尾的 8 倍
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := x.Stop(ctx)
	if !returned.Load() {
		t.Fatal("Stop 返回时 handler 还没返回——框架接着就会去关数据库")
	}
	if err == nil || strings.Contains(err.Error(), "still running") {
		t.Errorf("handler 都返回了，该只报超时断连，got=%v", err)
	}
}

func TestStop_HandlerIgnoringCtxReportsRemainingCount(t *testing.T) {
	// Go 没有从外面终止协程的办法，不看 ctx 的 handler 断连之后照跑。
	// 框架停不下它，但至少要如实说出来，不能让人以为已经停干净了
	x, _ := servingWithHungRequestDone(t)
	err := stopWithin(t, x)
	if err == nil || !strings.Contains(err.Error(), "1 handler(s) still running") || xerror.Module(err) != "xecho" {
		t.Errorf("该是 xecho 报的、还有 1 个 handler 没返回，got=%v", err)
	}
}

// ---- 读配置文件 ----

func TestEngine_UsesFinalConfigEvenBeforeRun(t *testing.T) {
	// 使用者在 main 顶上建好 XEcho、调 Engine()，之后才 xone.Run：
	// 配置在第一次读的时候才加载，New 在前、配置文件就位在后，装配时读到的照样是最终值
	x := New().WithRoutes(echoClientIP)
	testkit.UseConfigEnv(t, "XEcho:\n  TrustedProxies: [203.0.113.0/24]\n  Metric: false\n")

	if got := clientIPOf(t, x.Engine(), "203.0.113.9:1234"); got != "1.2.3.4" {
		t.Errorf("配置里的 TrustedProxies 没生效，client_ip=%q", got)
	}
	if w := doRequest(t, x.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("配置里关掉了指标，不该有 /metrics")
	}
}

func TestEngine_InvalidConfigFallsBackToSafeDefaults(t *testing.T) {
	// 照样给一个 echo（使用者可能在 Run 之前就拿了它），但按默认值装：
	// 解到一半的非法配置里，TrustedProxies 可能正是 0.0.0.0/0。错误由 Start 报，不监听
	testkit.UseConfigEnv(t, "XEcho:\n  Port: 0\n  TrustedProxies: [0.0.0.0/0]\n")
	x := New().WithRoutes(echoClientIP)
	if got := clientIPOf(t, x.Engine(), "203.0.113.9:1234"); got != "203.0.113.9" {
		t.Errorf("配置不合法时该退回默认值（只信私有网段），公网对端的转发头不该认，client_ip=%q", got)
	}

	err := startErr(t, x)
	if xerror.Module(err) != "xecho" || !xerror.Is(err, "xconfig") || !strings.Contains(err.Error(), "Port") {
		t.Errorf("该是 xecho 报的、裹着配置文件那一块的错误，got=%v", err)
	}
}

func TestCurrentConfig_AlwaysReadsConfigSection(t *testing.T) {
	testkit.UseConfigEnv(t, "XEcho:\n  Port: 18080\n  Log: false\n")
	c := CurrentConfig()
	if c.Port != 18080 || c.Log {
		t.Errorf("该读到配置文件里的值，got=%+v", c)
	}
	if c.Host != "0.0.0.0" || !c.Metric {
		t.Errorf("没写的字段该保持默认值，got=%+v", c)
	}
}

func TestCurrentConfig_ReturnsDefaultsWhenSectionInvalid(t *testing.T) {
	// 这里返回的东西会被拿去 WithConfig，必须是安全的：解到一半的非法配置不算
	testkit.UseConfigEnv(t, "XEcho:\n  Port: 0\n  TrustedProxies: [0.0.0.0/0]\n")
	if got := CurrentConfig(); !reflect.DeepEqual(got, DefaultConfig()) {
		t.Errorf("该退回默认值，got=%+v", got)
	}
}

func TestLoadConfig_StartupFailsOnFieldTypo(t *testing.T) {
	testkit.UseConfigEnv(t, "XEcho:\n  Prot: 8080\n")
	if err := loadConfig(context.Background()); err == nil {
		t.Fatal("字段拼错应当让启动失败")
	}
}

func TestLoadConfig_StartupFailsOnInvalidValue(t *testing.T) {
	// Config 实现了 Validate，解码时就一起查了，于是 StageServer 的这个钩子让启动当场失败
	for name, yml := range map[string]string{
		"端口越界":        "XEcho:\n  Port: 70000\n",
		"只配一半的 TLS":   "XEcho:\n  TLS:\n    CertFile: cert.pem\n",
		"指标路径不以 / 开头": "XEcho:\n  MetricPath: metrics\n",
	} {
		testkit.UseConfigEnv(t, yml)
		if err := loadConfig(context.Background()); err == nil {
			t.Errorf("%s 应当让启动失败", name)
		}
	}
}

func TestLoadConfig_EmptyTrustedProxiesInFileTrustsNone(t *testing.T) {
	// 配置文件写 [] 得真的换成空列表，而不是解码时被当成「没写」留下默认的 private
	testkit.UseConfigEnv(t, "XEcho:\n  TrustedProxies: []\n")
	if got := clientIPOf(t, New().WithRoutes(echoClientIP).Engine(), "10.0.0.5:1234"); got != "10.0.0.5" {
		t.Errorf("配置文件写了 [] 就该谁都不信，got=%q", got)
	}
}

func TestWithConfig_TwoInstancesListenOnOwnPorts(t *testing.T) {
	// 配置文件里那一块留在默认端口上，证明两个实例谁都没按它监听
	testkit.UseConfigEnv(t, "XEcho:\n  Port: 8080\n")

	portA, portB := testkit.FreePort(t), testkit.FreePort(t)
	for _, s := range []struct {
		port int
		body string
	}{{portA, "A"}, {portB, "B"}} {
		c := CurrentConfig()
		c.Host, c.Port = "127.0.0.1", s.port
		quiet(&c)
		body := s.body
		x := New().WithConfig(c).WithRoutes(func(e *echo.Echo) {
			e.GET("/who", func(c echo.Context) error { return c.String(200, body) })
		})
		serving(t, x, s.port)
	}

	for _, c := range []struct {
		port int
		want string
	}{{portA, "A"}, {portB, "B"}} {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/who", c.port))
		if err != nil {
			t.Fatalf("端口 %d 打不通：%v", c.port, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(got) != c.want {
			t.Errorf("端口 %d 该是实例 %s，got=%q", c.port, c.want, got)
		}
	}
}

func TestWithConfig_InstancesKeepTheirOwnTrustedProxies(t *testing.T) {
	// IPExtractor 是每个 echo 实例自己的（不像 gin.SetMode 是进程级的）：两份配置各管各的
	a := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = []string{} })).WithRoutes(echoClientIP).Engine()
	b := New().WithConfig(configWith(quiet)).WithRoutes(echoClientIP).Engine()
	if got := clientIPOf(t, a, "10.0.0.5:1"); got != "10.0.0.5" {
		t.Errorf("实例 A 谁都不信，got=%q", got)
	}
	if got := clientIPOf(t, b, "10.0.0.5:1"); got != "1.2.3.4" {
		t.Errorf("实例 B 信私有网段，got=%q", got)
	}
}

func TestWithConfig_FollowsConfigFileWhenOmitted(t *testing.T) {
	port := testkit.FreePort(t)
	testkit.UseConfigEnv(t, fmt.Sprintf("XEcho:\n  Host: 127.0.0.1\n  Port: %d\n  Log: false\n  Metric: false\n", port))

	x := New().WithRoutes(func(e *echo.Echo) { e.GET("/ping", ok) })
	serving(t, x, port)
}

// ---- 登记 ----

func TestRegister_OnlyReadsConfigNoStopHook(t *testing.T) {
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xecho" {
			got = &e
		}
	}
	if got == nil {
		t.Fatal("没有登记启动钩子，XEcho 那一块就没人认领、配错了也要等到 Start 才报")
	}
	if got.Stage != hook.StageServer {
		t.Errorf("服务应在最后一档，got=%v", got.Stage)
	}
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xecho" {
			t.Error("服务由使用者交给 xone.Run 启停，不该在这里登记停止钩子")
		}
	}
}

func TestXEcho_ImplementsRunnable(t *testing.T) {
	// 结构化满足即可，不 import 根包——「集成不依赖框架」这条要在编译层面成立
	var _ interface {
		Start(context.Context) error
		Stop(context.Context) error
	} = New()
}

// doRequest 对 echo 发一次请求
func doRequest(t *testing.T, e *echo.Echo, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

// waitServing 等服务真的开始监听
func waitServing(t *testing.T, url string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("服务没有起来：%s", url)
}

func TestBuild_MetricsKeptWhenEngineFetchedFirst(t *testing.T) {
	// 装配时如果就把 xmetric 的 registry 抓走，而那时 xmetric 还没初始化，
	// 指标会被注册到一个永远不会被导出的兜底 registry 上
	x := New().WithConfig(DefaultConfig()).WithRoutes(func(e *echo.Echo) { e.GET("/x", ok) })
	e := x.Engine() // 使用者在 Run 之前拿一下 echo，很自然的写法

	m, closer, err := xmetric.New(xmetric.Config{Namespace: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))

	if out := testkit.Scrape(m.Handler); !strings.Contains(out, "demo_http_requests_total") {
		t.Errorf("指标应记在初始化之后的 registry 上\n实际=\n%s", out)
	}
}

func TestBuild_MetricsEndpointOKWhenEngineFetchedFirst(t *testing.T) {
	// /metrics 的 handler 同理：装配时定死就会一直导出那个空的兜底 registry
	e := New().WithConfig(DefaultConfig()).Engine()

	m, closer, err := xmetric.New(xmetric.Config{Namespace: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/nope", nil))
	w := doRequest(t, e, "GET", "/metrics")

	if !strings.Contains(w.Body.String(), "demo_http_requests_total") {
		t.Errorf("/metrics 应导出当前生效的 registry\n实际=\n%s", w.Body.String())
	}
}

func TestBuild_MetricsEndpointRunsUserMiddleware(t *testing.T) {
	// WithMiddleware 挂的统一鉴权对业务路由、指标端点、没匹配上的路径都生效：
	// 一个以为被保护起来的端点不能是敞开的
	auth := func(echo.HandlerFunc) echo.HandlerFunc {
		return func(echo.Context) error { return echo.ErrUnauthorized }
	}

	e := New().WithConfig(configWith(func(c *Config) { c.Log = false })).
		WithMiddleware(auth).
		WithRoutes(func(e *echo.Echo) { e.GET("/biz", ok) }).Engine()

	for _, path := range []string{"/biz", "/metrics", "/nope"} {
		if w := doRequest(t, e, "GET", path); w.Code != http.StatusUnauthorized {
			t.Errorf("%s 该被鉴权中间件拦住，got=%d", path, w.Code)
		}
	}
}

// ---- 透传 Header 的信任边界 ----

// trustProbe 只记录「交给 Extract 的 carrier 有没有声明对端可信」的 Propagator。
// 声明的方式是 xtrace 那边约定的 TrustedPeer() bool，见 xtrace.HeaderPropagator.Extract
type trustProbe struct{ got *atomic.Bool }

func (p trustProbe) Extract(ctx context.Context, c propagation.TextMapCarrier) context.Context {
	t, ok := c.(interface{ TrustedPeer() bool })
	p.got.Store(ok && t.TrustedPeer())
	return ctx
}
func (trustProbe) Inject(context.Context, propagation.TextMapCarrier) {}
func (trustProbe) Fields() []string                                   { return nil }

// probeTrust 装上 trustProbe，返回「这次请求的对端被当成可信了吗」
func probeTrust(t *testing.T) func(e *echo.Echo, remote string) bool {
	t.Helper()
	old := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(old) })
	var got atomic.Bool
	otel.SetTextMapPropagator(trustProbe{got: &got})

	return func(e *echo.Echo, remote string) bool {
		got.Store(false)
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		// 可信与否只看直连对端，不看 c.RealIP()：可信对端转发来的公网地址不该让它变得不可信，
		// 公网对端也不能靠转发头冒充成自己人
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		e.ServeHTTP(httptest.NewRecorder(), req)
		return got.Load()
	}
}

func TestBuild_PassthroughHeadersOnlyFromTrustedProxies(t *testing.T) {
	// 透传 Header 照单全收的话：公网客户端发一个 X-Tenant-Id / X-Internal-Token，
	// 就被当成自己人给的，带进内网的每一次调用。「谁是自己人」用的是信任代理的那张表
	trusted := probeTrust(t)
	e := New().WithConfig(configWith(quiet, func(c *Config) {
		c.TrustedProxies = []string{"10.0.0.0/8", "192.168.1.7"}
	})).Engine()

	for remote, want := range map[string]bool{
		"10.0.0.5:1234":         true,  // 网段内
		"192.168.1.7:80":        true,  // 单个 IP
		"[::ffff:10.0.0.5]:443": true,  // IPv4 映射成 IPv6 的写法
		"192.168.1.8:80":        false, // 单个 IP 的邻居
		"1.2.3.4:1234":          false, // 公网
		"[2001:db8::1]:443":     false,
	} {
		if got := trusted(e, remote); got != want {
			t.Errorf("对端 %s 可信=%v，want %v", remote, got, want)
		}
	}
}

func TestBuild_PassthroughHeadersOnlyFromPrivateByDefault(t *testing.T) {
	// K8s 里 Pod IP 随机，但都在私有网段里：默认就认，不用一个个写
	trusted := probeTrust(t)
	e := New().WithConfig(configWith(quiet)).Engine()

	for remote, want := range map[string]bool{
		"127.0.0.1:1234":        true, // sidecar
		"10.0.0.5:1234":         true, // Pod / 负载均衡
		"172.20.1.2:1234":       true,
		"192.168.1.7:80":        true,
		"100.64.3.4:80":         true, // 有的 CNI 拿它当 Pod 网段
		"[::1]:443":             true,
		"[fd00::5]:443":         true,  // IPv6 ULA
		"[::ffff:10.0.0.5]:443": true,  // IPv4 映射成 IPv6 的写法
		"203.0.113.9:1234":      false, // 公网
		"172.32.0.1:1234":       false, // 172.16.0.0/12 的邻居
		"[2001:db8::1]:443":     false,
	} {
		if got := trusted(e, remote); got != want {
			t.Errorf("默认配置下对端 %s 可信=%v，want %v", remote, got, want)
		}
	}
}

func TestBuild_TrustedProxiesMixesPrivateWithOtherCIDRs(t *testing.T) {
	// 列表整体替换默认值：要在私有网段之外再加一段，把 private 一起写上
	trusted := probeTrust(t)
	e := New().WithConfig(configWith(quiet, func(c *Config) {
		c.TrustedProxies = []string{"private", "203.0.113.0/24"}
	})).WithRoutes(echoClientIP).Engine()
	for remote, want := range map[string]bool{"10.0.0.5:1": true, "203.0.113.9:1": true, "198.51.100.1:1": false} {
		if got := trusted(e, remote); got != want {
			t.Errorf("对端 %s 可信=%v，want %v", remote, got, want)
		}
	}
	// client_ip 那一边同一张表
	if got := clientIPOf(t, e, "203.0.113.9:1"); got != "1.2.3.4" {
		t.Errorf("203.0.113.0/24 里的对端转发来的该认，got=%q", got)
	}
	// 写 [] 就谁都不信，透传 Header 也一样
	none := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = []string{} })).Engine()
	if trusted(none, "10.0.0.5:1") {
		t.Error("写了 [] 就该谁都不信")
	}
}

func TestBuild_TraceDisabledOnlySkipsSpan_StillPropagates(t *testing.T) {
	// XEcho.Trace: false 只管 Span：可信对端发来的 X-Request-Id、上游的 traceparent 照样接
	trusted := probeTrust(t)
	off := func(c *Config) { c.Trace = false }
	e := New().WithConfig(configWith(quiet, off, func(c *Config) {
		c.TrustedProxies = []string{"10.0.0.0/8"}
	})).Engine()
	for remote, want := range map[string]bool{"10.0.0.5:1234": true, "1.2.3.4:1234": false} {
		if got := trusted(e, remote); got != want {
			t.Errorf("Trace 关着时对端 %s 可信=%v，want %v：可信规则要和开着时一样", remote, got, want)
		}
	}

	// 上游的链路标识接进了请求的 ctx，但这一跳不开 Span、不回带 X-Trace-Id
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	oldTP := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(oldTP) })
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	const upstream = "4bf92f3577b34da6a3ce929d0e0e4736"
	var seen string
	e = New().WithConfig(configWith(quiet, off)).WithRoutes(func(e *echo.Echo) {
		e.GET("/x", func(c echo.Context) error {
			seen = trace.SpanContextFromContext(c.Request().Context()).TraceID().String()
			return nil
		})
	}).Engine()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("traceparent", "00-"+upstream+"-00f067aa0ba902b7-01")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if seen != upstream {
		t.Errorf("Trace 关着也该接上上游的链路标识，handler 里看到的 TraceID=%q", seen)
	}
	if spans := exp.GetSpans(); len(spans) != 0 {
		t.Errorf("Trace 关着不该开 Span，got=%d 个", len(spans))
	}
	if w.Header().Get(middleware.TraceIDHeader) != "" {
		t.Error("Trace 关着不回带 X-Trace-Id")
	}
}

// ---- h2c ----

// h2cClient 只说明文 HTTP/2 的客户端：连上去直接发 HTTP/2 前言，不走 HTTP/1.1 升级
func h2cClient() *http.Client {
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: tr}
}

// servingH2C 起一个开了 h2c 的服务，/slow 会在 handler 里睡 d
func servingH2C(t *testing.T, d time.Duration) (base string, x *XEcho, entered, finished chan struct{}) {
	t.Helper()
	port := testkit.FreePort(t)

	entered, finished = make(chan struct{}), make(chan struct{})
	x = New().WithConfig(configWith(quiet, on(port), func(c *Config) { c.UseH2C = true })).
		WithRoutes(func(e *echo.Echo) {
			e.GET("/ping", ok)
			e.GET("/slow", func(c echo.Context) error {
				close(entered)
				time.Sleep(d)
				close(finished)
				return c.String(200, "done")
			})
		})
	go x.Start(context.Background())
	base = fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/ping")
	return base, x, entered, finished
}

type h2cResult struct {
	proto int
	body  string
	err   error
}

// h2cGet 在后台发一个 h2c 请求
func h2cGet(url string) <-chan h2cResult {
	got := make(chan h2cResult, 1)
	go func() {
		resp, err := h2cClient().Get(url)
		if err != nil {
			got <- h2cResult{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		got <- h2cResult{proto: resp.ProtoMajor, body: string(b)}
	}()
	return got
}

func TestStop_H2CWaitsForInFlightRequests(t *testing.T) {
	// h2c 的连接也归 Shutdown 管：x/net 的 h2c.NewHandler 会把连接劫持走，
	// Shutdown 约 60µs 就返回 nil 而在途请求照跑
	base, x, entered, finished := servingH2C(t, time.Second)
	got := h2cGet(base + "/slow")
	<-entered

	if err := x.Stop(context.Background()); err != nil {
		t.Fatalf("在途请求能在预算内做完，Stop 不该报错：%v", err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("Stop 返回时 h2c 的在途请求还没做完——框架接着就会去关数据库")
	}
	r := <-got
	if r.err != nil || r.body != "done" {
		t.Fatalf("在途请求该完整地拿到响应，got=%+v", r)
	}
	if r.proto != 2 {
		t.Errorf("该走 HTTP/2，got=HTTP/%d", r.proto)
	}
}

func TestStop_H2CForceClosesAfterTimeout(t *testing.T) {
	base, x, entered, _ := servingH2C(t, 5*time.Second)
	got := h2cGet(base + "/slow")
	<-entered

	if err := stopWithin(t, x); err == nil {
		t.Fatal("在途请求没做完就到点了，该报错")
	}
	select {
	case r := <-got:
		if r.err == nil {
			t.Errorf("连接该被断掉，got=%+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Error("超时之后 h2c 的在途请求仍在继续")
	}
}

func TestStart_CleartextHTTP2RejectedWithoutH2C(t *testing.T) {
	// 开关要真的是开关：没开 UseH2C 时，明文 HTTP/2 的前言不该被接受
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *echo.Echo) { e.GET("/ping", ok) })
	base := serving(t, x, port)

	if r := <-h2cGet(base + "/ping"); r.err == nil {
		t.Errorf("没开 UseH2C，明文 HTTP/2 不该成功，got=HTTP/%d", r.proto)
	}
}

// ---- 零值 ----

func TestZeroValue_ReportsUnderModuleName(t *testing.T) {
	// &XEcho{} 不经过 New 也要能用：错误和日志照样记在 xecho 名下，
	// 调用方的 xerror.Is(err, "xecho") 才成立，日志里也不会冒出 " listening" 这种没头的消息
	buf := captureLog(t)
	cert := filepath.Join(t.TempDir(), "missing.pem")
	err := startErr(t, (&XEcho{}).WithConfig(configWith(quiet, on(testkit.FreePort(t)), func(c *Config) { c.TLS.CertFile, c.TLS.KeyFile = cert, cert })))
	if !xerror.Is(err, "xecho") || xerror.Module(err) != "xecho" {
		t.Fatalf("零值的 Start 报的错该算 xecho 的，got=%v", err)
	}

	z := &XEcho{}
	if err := z.Stop(context.Background()); err != nil {
		t.Fatalf("Start 之前 Stop 该什么都不做，got=%v", err)
	}
	if err := z.Start(context.Background()); err != nil {
		t.Fatalf("Stop 之后的 Start 该直接返回 nil，got=%v", err)
	}
	if !strings.Contains(buf.String(), `"msg":"xecho received the shutdown signal before starting`) {
		t.Errorf("日志消息该以 xecho 开头，got=%s", buf.String())
	}
}

func TestStart_NoListeningLogWhenListenFails(t *testing.T) {
	// 「listening」要在真的监听上之后才打：证书读不出来、端口被占时先说 listening
	// 再报 listen failed，排查的人会以为服务起来过
	cert := filepath.Join(t.TempDir(), "missing.pem")
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	for name, mutate := range map[string]func(*Config){
		"missing cert": func(c *Config) { c.Port = testkit.FreePort(t); c.TLS.CertFile, c.TLS.KeyFile = cert, cert },
		"port in use":  func(c *Config) { c.Port = busy.Addr().(*net.TCPAddr).Port },
	} {
		buf := captureLog(t)
		err := startErr(t, New().WithConfig(configWith(quiet, on(0), mutate)))
		if err == nil || !strings.Contains(err.Error(), "listen on") {
			t.Fatalf("%s: 该报 listen failed，got=%v", name, err)
		}
		if strings.Contains(buf.String(), "xecho listening") {
			t.Errorf("%s: 没监听上就不该打 listening，got=%s", name, buf.String())
		}
	}
}

func TestLoadConfig_IPv4MappedProxyFailsStartup(t *testing.T) {
	// ::ffff:10.0.0.1 在 gin 和 xtrace 的可信判断里各是一个意思，只能拦住，并告诉使用者该怎么写
	err := loadErr(t, "XEcho:\n  TrustedProxies: [private, \"::ffff:10.0.0.1\"]\n")
	if err == nil || !strings.Contains(err.Error(), "IPv4-mapped") || !strings.Contains(err.Error(), "write it as 10.0.0.1") {
		t.Fatalf("IPv4 映射写法该启动失败并给出 IPv4 写法，got=%v", err)
	}
}

// ---- e.Pre ----

// preEcho 装一个带 e.Pre 的服务，链路记进返回的 exporter，指标记进返回的 Metrics
func preEcho(t *testing.T, pre ...echo.MiddlewareFunc) (*echo.Echo, *tracetest.InMemoryExporter, *xmetric.Metrics) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	m.Install()

	e := New().WithConfig(DefaultConfig()).WithRoutes(func(e *echo.Echo) {
		e.Pre(pre...)
		e.GET("/a", func(c echo.Context) error { return c.String(200, "a") })
	}).Engine()
	return e, exp, m
}

func TestBuild_PreRejectionLoggedMeasuredAndTraced(t *testing.T) {
	// 回归用例：内置中间件原先挂在 e.Use 上，e.Pre 里拒掉的请求（鉴权、限流常写在这里）
	// 不进访问日志、指标、链路，响应也不带 X-Trace-Id。router 还没跑，路由记 unmatched
	buf := captureLog(t)
	e, exp, m := preEcho(t, func(echo.HandlerFunc) echo.HandlerFunc {
		return func(echo.Context) error { return echo.ErrUnauthorized }
	})
	w := doRequest(t, e, "GET", "/a")
	if w.Code != 401 || w.Header().Get(middleware.TraceIDHeader) == "" {
		t.Errorf("该回 401 并带 X-Trace-Id，got=%d %v", w.Code, w.Header())
	}
	if out := buf.String(); !strings.Contains(out, `"msg":"request completed"`) || !strings.Contains(out, `"route":"unmatched"`) || !strings.Contains(out, `"status":401`) {
		t.Errorf("访问日志该记下 401 和 unmatched：\n%s", out)
	}
	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `http_requests_total{method="GET",route="unmatched",status="401"} 1`) {
		t.Errorf("指标该记下 401\n%s", out)
	}
	if spans := exp.GetSpans(); len(spans) != 1 || spans[0].Name != "GET unmatched" {
		t.Errorf("该有一个 GET unmatched 的 Span，got=%v", spans)
	}
}

func TestBuild_PreRedirectLogged(t *testing.T) {
	// echo 文档里的写法：末尾斜杠在 Pre 里 301 回去。这个 301 也是一次请求
	buf := captureLog(t)
	e, _, _ := preEcho(t, echomw.RemoveTrailingSlashWithConfig(echomw.TrailingSlashConfig{RedirectCode: http.StatusMovedPermanently}))
	w := doRequest(t, e, "GET", "/a/")
	if w.Code != 301 || !strings.Contains(buf.String(), `"status":301`) {
		t.Errorf("Pre 里的 301 该进访问日志，got=%d\n%s", w.Code, buf.String())
	}
}

func TestBuild_PrePanicRecovered(t *testing.T) {
	// echo 不兜 panic：Pre 里 panic 原先没人接，客户端读到 EOF（Empty reply），栈由 net/http 写进 stderr
	buf := captureLog(t)
	e, _, _ := preEcho(t, func(echo.HandlerFunc) echo.HandlerFunc {
		return func(echo.Context) error { panic("pre boom") }
	})
	w := doRequest(t, e, "GET", "/a")
	out := buf.String()
	if w.Code != 500 || !strings.Contains(out, `"msg":"panic while handling request"`) || !strings.Contains(out, `"status":500`) {
		t.Errorf("Pre 里的 panic 该被兜住、回 500、记 ERROR 日志和访问日志，got=%d\n%s", w.Code, out)
	}
}

func TestBuild_PreMethodOverrideStillRoutes(t *testing.T) {
	// echo 的 router 按请求刚进来时的那个 *http.Request 找路由（Echo.ServeHTTP 的闭包里捕获的），
	// 不是 c.Request()。内置中间件在 Pre 里换成 r.WithContext 的副本的话，
	// 排在后面的 MethodOverride 改的是副本上的 Method，router 照旧按 POST 找
	// Trace 关着时换 ctx 的是 Propagate，同样要原地换
	for _, trace := range []bool{true, false} {
		buf := captureLog(t)
		e := New().WithConfig(configWith(func(c *Config) { c.Trace = trace })).WithRoutes(func(e *echo.Echo) {
			e.Pre(echomw.MethodOverride())
			e.POST("/a", func(c echo.Context) error { return c.String(200, "post") })
			e.PUT("/a", func(c echo.Context) error { return c.String(200, "put") })
		}).Engine()
		req := httptest.NewRequest("POST", "/a", nil)
		req.Header.Set(echo.HeaderXHTTPMethodOverride, "PUT")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		if w.Code != 200 || w.Body.String() != "put" {
			t.Errorf("Trace=%v：MethodOverride 该把请求路由到 PUT，got=%d %q", trace, w.Code, w.Body.String())
		}
		if out := buf.String(); !strings.Contains(out, `"method":"PUT","route":"/a"`) {
			t.Errorf("Trace=%v：访问日志该记改过之后的方法和路由：\n%s", trace, out)
		}
	}
}

func TestBuild_RouteTemplateKnownWithBuiltinsInPre(t *testing.T) {
	// 内置中间件挂在 Pre 上，router 在它们里面跑：next(c) 返回之后 c.Path() 已经有了
	exp := tracetest.NewInMemoryExporter()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })
	buf := captureLog(t)
	e := New().WithConfig(DefaultConfig()).WithRoutes(func(e *echo.Echo) {
		e.GET("/users/:id", ok)
	}).Engine()
	doRequest(t, e, "GET", "/users/1")
	if !strings.Contains(buf.String(), `"route":"/users/:id"`) {
		t.Errorf("访问日志该记路由模板：\n%s", buf.String())
	}
	if spans := exp.GetSpans(); len(spans) != 1 || spans[0].Name != "GET /users/:id" {
		t.Errorf("Span 名该带路由模板，got=%v", spans)
	}
}

func TestEchoStdLogger_DefaultWritesToStdout(t *testing.T) {
	// 钉住前提（echo v4.16.0）：e.StdLogger 在 echo.New 里就绑定了 e.Logger 当时的输出 os.Stdout，
	// 之后再 e.Logger.SetOutput 也改不到它
	if out := echo.New().StdLogger.Writer(); out != os.Stdout {
		t.Errorf("e.StdLogger 默认写 os.Stdout，got=%T", out)
	}
}

func TestEchoStdLogger_RoutedToSlog(t *testing.T) {
	// e.StdLogger 是 echo 给 http.Server.ErrorLog 准备的（e.Start 里就这么用）：和服务的 ErrorLog 接到同一处
	buf := captureLog(t)
	e := New().WithConfig(configWith(quiet)).Engine()
	e.StdLogger.Print("http: TLS handshake error from 203.0.113.9:1234: EOF")
	if want := `"level":"WARN","msg":"xecho http server error","error":"http: TLS handshake error from 203.0.113.9:1234: EOF"`; !strings.Contains(buf.String(), want) {
		t.Errorf("e.StdLogger 该转成 %s，got=%s", want, buf.String())
	}
}

func TestBuild_ContextTimeoutRecordedAsSent(t *testing.T) {
	// README 推荐的超时写法：echomw.ContextTimeout 给请求套截止时间，handler 看 ctx 返回，
	// 由它换成 503。它返回时 defer cancel() 了那个 ctx——出了它那一层请求的 ctx 就是 Canceled，
	// 不能因此当成客户端走了（499）。echo 已弃用的 middleware.Timeout 记不对，见 README
	// 指标开着时最里面渲染错误的是 Metric，关着时是 Log：两层各自都得对
	for _, metric := range []bool{true, false} {
		buf := captureLog(t)
		e := New().WithConfig(configWith(func(c *Config) { c.Metric = metric })).
			WithMiddleware(echomw.ContextTimeout(20 * time.Millisecond)).
			WithRoutes(func(e *echo.Echo) {
				e.GET("/slow", func(c echo.Context) error {
					<-c.Request().Context().Done()
					return c.Request().Context().Err()
				})
			}).Engine()
		w := doRequest(t, e, "GET", "/slow")
		if w.Code != 503 || !strings.Contains(buf.String(), `"status":503`) || !strings.Contains(buf.String(), fmt.Sprintf(`"bytes_out":%d`, w.Body.Len())) {
			t.Errorf("Metric=%v：客户端收到 %d，访问日志该记同一个状态码和字节数：\n%s", metric, w.Code, buf.String())
		}
	}
}

// xlogCapture 把默认 logger 换成真的 xlog（trace_id、xlog.AddKV 的字段是它的 handler 加上的），返回取日志行的函数
func xlogCapture(t *testing.T) func() string {
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
	// trace_id 靠提取器写进日志，平时由 xtrace 的 Install 装上；测试自己装，不依赖别的用例先跑过
	xlog.SetTraceExtractor(func(ctx context.Context) (string, string) {
		sc := trace.SpanContextFromContext(ctx)
		if !sc.IsValid() {
			return "", ""
		}
		return sc.TraceID().String(), sc.SpanID().String()
	})
	t.Cleanup(func() { xlog.SetTraceExtractor(nil); slog.SetDefault(old); closer.Close() })
	return func() string {
		closer.Close()
		files, _ := filepath.Glob(filepath.Join(dir, "app.log.*"))
		if len(files) == 0 {
			return ""
		}
		b, _ := os.ReadFile(files[0])
		return string(b)
	}
}

func TestBuild_PreRejectionAccessLogCarriesTraceID(t *testing.T) {
	// Pre 里就结束了的请求，日志作用域和 Span 还没换到请求上（路由之后才换）：
	// 访问日志用的是存在 echo.Context 上的那个 ctx，照样带 trace_id，和 X-Trace-Id 对得上
	logs := xlogCapture(t)
	e, _, _ := preEcho(t, func(echo.HandlerFunc) echo.HandlerFunc {
		return func(echo.Context) error { return echo.ErrUnauthorized }
	})
	w := doRequest(t, e, "GET", "/a")
	id := w.Header().Get(middleware.TraceIDHeader)
	if out := logs(); id == "" || !strings.Contains(out, `"trace_id":"`+id+`"`) || !strings.Contains(out, `"status":401`) {
		t.Errorf("401 的访问日志该带 trace_id=%q：\n%s", id, out)
	}
}

func TestBuild_HandlerSeesSpanAndLogScope(t *testing.T) {
	// 路由之后 xecho 把 Pre 里建好的 ctx 换到请求上：handler 和 WithMiddleware 看到的 ctx 带着服务端 Span，
	// xlog.AddKV 写进去的字段出现在访问日志里
	logs := xlogCapture(t)
	exp := tracetest.NewInMemoryExporter()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })
	var inMiddleware, inHandler string
	e := New().WithConfig(configWith(func(c *Config) { c.Metric = false })).
		WithMiddleware(func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				inMiddleware = trace.SpanContextFromContext(c.Request().Context()).TraceID().String()
				return next(c)
			}
		}).
		WithRoutes(func(e *echo.Echo) {
			e.GET("/a", func(c echo.Context) error {
				inHandler = trace.SpanContextFromContext(c.Request().Context()).TraceID().String()
				xlog.AddKV(c.Request().Context(), "user_id", "u9")
				return c.NoContent(200)
			})
		}).Engine()
	w := doRequest(t, e, "GET", "/a")
	id := w.Header().Get(middleware.TraceIDHeader)
	if id == "" || inMiddleware != id || inHandler != id {
		t.Errorf("WithMiddleware 和 handler 都该看到服务端 Span，X-Trace-Id=%q middleware=%q handler=%q", id, inMiddleware, inHandler)
	}
	if out := logs(); !strings.Contains(out, `"user_id":"u9"`) {
		t.Errorf("xlog.AddKV 的字段该进访问日志：\n%s", out)
	}
}

func TestBuild_PreRewritingMiddlewaresStillRoute(t *testing.T) {
	// 使用者的 e.Pre 改的是 echo 按它找路由的那个请求（Method、URL.Path）：内置中间件不能换掉它
	for name, c := range map[string]struct {
		pre    echo.MiddlewareFunc
		method string
		path   string
		header string
	}{
		"MethodOverride":      {echomw.MethodOverride(), "POST", "/a", "PUT"},
		"RemoveTrailingSlash": {echomw.RemoveTrailingSlash(), "PUT", "/a/", ""},
		"Rewrite":             {echomw.Rewrite(map[string]string{"/old": "/a"}), "PUT", "/old", ""},
	} {
		e := New().WithConfig(configWith(quiet)).WithRoutes(func(e *echo.Echo) {
			e.Pre(c.pre)
			e.PUT("/a", func(c echo.Context) error { return c.String(200, "put") })
		}).Engine()
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.header != "" {
			req.Header.Set(echo.HeaderXHTTPMethodOverride, c.header)
		}
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		if w.Code != 200 || w.Body.String() != "put" {
			t.Errorf("%s：该路由到 PUT /a，got=%d %q", name, w.Code, w.Body.String())
		}
	}
}

func TestHandler_RequestUsedAfterReturnIsRaceFree(t *testing.T) {
	// 回归用例（-race）：handler 把请求交给活得比它久的协程（异步记日志、打点）是常见写法。
	// 内置中间件原地改交进来的 *http.Request 的话，这里就是一次数据竞争
	var wg sync.WaitGroup
	e := New().WithConfig(DefaultConfig()).WithRoutes(func(e *echo.Echo) {
		e.GET("/async", func(c echo.Context) error {
			r := c.Request()
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(5 * time.Millisecond)
				_ = r.Context().Value("k")
				_ = r.Method
			}()
			return c.String(200, "ok")
		})
	}).Engine()
	for range 20 {
		e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/async", nil))
	}
	wg.Wait()
}

// servingHijacked 起一个服务，/ws 劫持连接后一直等请求的 ctx（WebSocket 的读循环就是这样）
func servingHijacked(t *testing.T) *XEcho {
	t.Helper()
	port := testkit.FreePort(t)
	entered, release := make(chan struct{}), make(chan struct{})
	x := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *echo.Echo) {
		e.GET("/ping", ok)
		e.GET("/ws", func(c echo.Context) error {
			conn, _, err := c.Response().Hijack()
			if err != nil {
				return err
			}
			defer conn.Close()
			close(entered)
			select {
			case <-c.Request().Context().Done():
			case <-release: // 测试失败时也让 Cleanup 里的 Stop 返回
			}
			return nil
		})
	})
	base := serving(t, x, port)
	t.Cleanup(func() { close(release) }) // 在 serving 的 Stop 之前跑
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprint(conn, "GET /ws HTTP/1.1\r\nHost: x\r\n\r\n")
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("请求没进到 handler")
	}
	return x
}

func TestStop_HijackedConnGetsCancelled(t *testing.T) {
	// 回归用例。被劫持的连接（WebSocket）Shutdown 不等、Close 也断不掉，请求的 ctx 原先永远不取消：
	// 等着它的 handler 不返回，Stop 把整份预算耗完再报 1 handler(s) still running
	x := servingHijacked(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := x.Stop(ctx); err != nil {
		t.Fatalf("劫持走的 handler 看到 ctx 取消就返回了，Stop 不该报错：%v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Stop 该在 Shutdown 返回之后马上取消劫持连接的 ctx，却用了 %v", d)
	}
}

func TestStart_RetryAfterListenFailure(t *testing.T) {
	// 回归用例。监听失败（端口被占）之后原先还记着「在跑」，之后每次 Start 都说 already running
	port := testkit.FreePort(t)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	x := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *echo.Echo) { e.GET("/ping", ok) })
	if err := startErr(t, x); err == nil || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("端口被占时该报 listen 失败，got=%v", err)
	}
	ln.Close()

	done := make(chan error, 1)
	go func() { done <- x.Start(context.Background()) }()
	t.Cleanup(func() { _ = x.Stop(context.Background()) })
	select {
	case err := <-done:
		t.Fatalf("端口空出来之后该能再起来，got=%v", err)
	case <-time.After(300 * time.Millisecond):
	}
	waitServing(t, fmt.Sprintf("http://127.0.0.1:%d/ping", port))
}

func TestTrace_MetricsScrapeNotTraced(t *testing.T) {
	// 回归用例。抓取系统按秒轮询 /metrics，访问日志早就跳过了它，Span 却每次都开一个
	exp := tracetest.NewInMemoryExporter()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	e := New().WithConfig(configWith(func(c *Config) { c.Log = false; c.MetricPath = "/internal/metrics" })).Engine()
	if w := doRequest(t, e, "GET", "/internal/metrics"); w.Code != 200 {
		t.Fatalf("指标端点该是 200，got=%d", w.Code)
	}
	if n := len(exp.GetSpans()); n != 0 {
		t.Errorf("抓一次指标不该开 Span，got %d 个", n)
	}
	doRequest(t, e, "GET", "/work")
	if n := len(exp.GetSpans()); n != 1 {
		t.Errorf("别的路径照常开 Span，got %d 个", n)
	}
}
