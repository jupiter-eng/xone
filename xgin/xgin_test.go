package xgin

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xgin/middleware"
	"github.com/jupiter-eng/xone/xgin/trans"
	"github.com/jupiter-eng/xone/xlog"
	"github.com/jupiter-eng/xone/xmetric"
)

func init() { gin.SetMode(gin.TestMode) }

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
func serving(t *testing.T, g *XGin, port int) string {
	t.Helper()
	go func() { _ = g.Start(context.Background()) }()
	t.Cleanup(func() { _ = g.Stop(context.Background()) })
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/nothing")
	return base
}

// startErr 调 Start 并取它返回的错误。配置非法时它该当场返回、不开始监听——
// 2s 还没返回就当它在监听，关掉并判失败
func startErr(t *testing.T, g *XGin) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- g.Start(context.Background()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		_ = g.Stop(context.Background())
		t.Fatal("配置非法时 Start 该直接返回错误，它却开始监听了")
		return nil
	}
}

// echoClientIP 注册 GET /client-ip，响应体就是 handler 看到的 ClientIP
func echoClientIP(e *gin.Engine) {
	e.GET("/client-ip", func(c *gin.Context) { c.String(200, c.ClientIP()) })
}

// clientIPOf 从 remote 带着伪造的 X-Forwarded-For: 1.2.3.4 请求一次 /client-ip（见 echoClientIP），
// 返回 handler 看到的 ClientIP
func clientIPOf(t *testing.T, e *gin.Engine, remote string) string {
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
	if c.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("读请求头必须有超时，got=%v", c.ReadHeaderTimeout)
	}
	// 线上忘了设环境变量的代价，比本地少一行提示大得多
	if c.Mode != "release" {
		t.Errorf("默认应为 release，got=%q", c.Mode)
	}
	if !c.Log || !c.Trace || !c.Metric {
		t.Errorf("三个内置中间件默认都该开着，got=%+v", c)
	}
	if c.LogRequestBody || c.LogResponseBody || c.LogQuery || c.LogRequestHeaders || c.LogResponseHeaders || c.ZHTranslations {
		t.Errorf("记 body、查询串、请求头、响应头和中文翻译默认都该关着（代价和风险都不小），got=%+v", c)
	}
	if c.MetricPath != "/metrics" {
		t.Errorf("指标路径默认应为 /metrics，got=%q", c.MetricPath)
	}
	if len(c.LogSkipPaths) != 0 {
		t.Errorf("默认不跳过任何路径，got=%v", c.LogSkipPaths)
	}
}

func TestConfig_LoadFromFile(t *testing.T) {
	c := load(t, "XGin:\n  Port: 9090\n  ReadTimeout: 30s\n  UseH2C: true\n  Log: false\n  LogSkipPaths: [/healthz, /static/]\n")
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
	if err := loadErr(t, "XGin:\n  Prot: 9090\n"); err == nil {
		t.Fatal("字段拼错应当启动失败")
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
		"Mode 不认识":           func(c *Config) { c.Mode = "prod" },
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
	// net/http 对这几项的零值和负值都是「不设防」而不是「用个默认值」：
	// ReadHeaderTimeout 为 0 时退到 ReadTimeout，而后者默认也是 0，
	// 于是慢连接攻击的主要防线整个消失，配置文件看上去只是写了个 0
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

func TestNetHTTP_ReadHeaderTimeoutZeroLetsSlowClientHoldConn(t *testing.T) {
	// 钉住上面那条校验的前提：标准库对 0 的处理是「不限时」。
	// 哪天升级后它变成了「用个默认值」，这条会先红，校验可以跟着放宽
	for _, rht := range []time.Duration{0, -time.Second} {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Config.ReadHeaderTimeout = rht
		srv.Start()

		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")) // 头只发一半
		conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		_, err = conn.Read(make([]byte, 1))
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Errorf("ReadHeaderTimeout=%v 时服务端该一直等着（我们这边读超时），got=%v", rht, err)
		}
		conn.Close()
		srv.Close()
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
	// gin 的 SetTrustedProxies 解析到出错为止、把已经解出来的留下，
	// 于是前半段代理被信任、后半段被悄悄丢掉——日志里的 client_ip
	// 一半真一半假，比起不来难查得多
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

func TestBuild_BuiltinMiddlewareOrder(t *testing.T) {
	// Recover 必须是内置里最内层的：panic 在哪一层被兜住，
	// 比它更内层的中间件里 c.Next() 之后的代码就都不执行了
	g := New().WithConfig(DefaultConfig()).WithRoutes(func(e *gin.Engine) {
		e.GET("/boom", func(c *gin.Context) { panic("炸了") })
	})

	w := doRequest(t, g.Engine(), "GET", "/boom")
	if w.Code != 500 {
		t.Errorf("panic 应被兜住并返回 500，got=%d", w.Code)
	}
}

func TestBuild_MetricsEndpointAutoRegistered(t *testing.T) {
	g := New().WithConfig(DefaultConfig())
	if w := doRequest(t, g.Engine(), "GET", "/metrics"); w.Code != 200 {
		t.Errorf("启用指标时应自动注册 /metrics，got=%d", w.Code)
	}
	if !strings.Contains(doRequest(t, g.Engine(), "GET", "/metrics").Body.String(), "http_requests_total") {
		t.Error("指标端点应导出请求数指标")
	}
}

func TestBuild_MetricsPathConfigurable(t *testing.T) {
	g := New().WithConfig(configWith(func(c *Config) { c.MetricPath = "/internal/metrics" }))
	if w := doRequest(t, g.Engine(), "GET", "/internal/metrics"); w.Code != 200 {
		t.Errorf("应注册在配置的路径上，got=%d", w.Code)
	}
	if w := doRequest(t, g.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("默认路径上不该再有")
	}
}

func TestBuild_NoMetricsEndpointWhenDisabled(t *testing.T) {
	g := New().WithConfig(configWith(func(c *Config) { c.Metric = false }))
	if w := doRequest(t, g.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("关掉指标后不该有 /metrics")
	}
}

func TestBuild_405InsteadOf404(t *testing.T) {
	// 不开 HandleMethodNotAllowed 的话，方法用错会得到 404，
	// 调用方会以为是路径写错了
	g := New().WithConfig(DefaultConfig()).WithRoutes(func(e *gin.Engine) {
		e.GET("/only-get", func(c *gin.Context) { c.Status(200) })
	})
	if w := doRequest(t, g.Engine(), "POST", "/only-get"); w.Code != 405 {
		t.Errorf("方法不对应返回 405，got=%d", w.Code)
	}
}

func TestBuild_Idempotent(t *testing.T) {
	// 装配两遍会把中间件注册两遍，表现是每个请求打两条日志、指标翻倍
	g := New().WithConfig(DefaultConfig())
	if g.Engine() != g.Engine() {
		t.Error("重复装配应返回同一个 engine")
	}
}

func TestBuild_UserMiddlewareAfterBuiltin(t *testing.T) {
	var order []string
	g := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace, c.Metric = false, false, false })).
		WithMiddleware(func(c *gin.Context) { order = append(order, "用户"); c.Next() }).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/x", func(c *gin.Context) { order = append(order, "handler"); c.Status(200) })
		})

	doRequest(t, g.Engine(), "GET", "/x")
	if len(order) != 2 || order[0] != "用户" || order[1] != "handler" {
		t.Errorf("用户中间件应在 handler 之前，got=%v", order)
	}
}

func TestBuild_ModeSetFromConfigBeforeEngine(t *testing.T) {
	var out bytes.Buffer
	oldW := gin.DefaultWriter
	gin.DefaultWriter = &out
	t.Cleanup(func() { gin.DefaultWriter = oldW; gin.SetMode(gin.TestMode) })

	New().WithConfig(configWith(quiet, func(c *Config) { c.Mode = "debug" })).Engine()
	if gin.Mode() != gin.DebugMode {
		t.Errorf("装配时应按配置设 Mode，got=%q", gin.Mode())
	}

	// 使用者的二进制里没设 GIN_MODE 时，gin 默认就是 debug。Mode 要在 gin.New 之前设，
	// 否则配成 release 的服务照样打出 gin 那段「正在以 debug 模式运行」的警告
	gin.SetMode(gin.DebugMode)
	out.Reset()
	New().WithConfig(DefaultConfig()).Engine()
	if out.Len() > 0 {
		t.Errorf("配成 release 的服务不该有 gin 的 debug 输出：\n%s", out.String())
	}
}

// ---- 开关 ----

func TestLog_NoAccessLogWhenDisabled(t *testing.T) {
	buf := captureLog(t)
	route := func(e *gin.Engine) { e.GET("/work", func(c *gin.Context) { c.String(200, "ok") }) }

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

func TestLogSkipPaths_ReallySkipsThesePaths(t *testing.T) {
	// 光在配置里写上不够：它得一路传到 middleware.Log 里
	buf := captureLog(t)
	e := New().WithConfig(configWith(func(c *Config) {
		c.Metric, c.Trace = false, false
		c.LogSkipPaths = []string{"/healthz", "/static/"}
	})).WithRoutes(func(e *gin.Engine) {
		e.GET("/healthz", func(c *gin.Context) { c.String(200, "ok") })
		e.GET("/static/*file", func(c *gin.Context) { c.String(200, "ok") })
		e.GET("/work", func(c *gin.Context) { c.String(200, "ok") })
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
		})).WithRoutes(func(e *gin.Engine) {
			e.POST("/echo", func(c *gin.Context) { c.JSON(200, gin.H{"greeting": "hi-bob"}) })
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
		})).WithRoutes(func(e *gin.Engine) {
			e.GET("/search", func(c *gin.Context) {
				c.Header("X-Page-Total", "7")
				c.Header("Set-Cookie", "sid=abc123")
				c.Status(200)
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

	for _, trace := range []bool{true, false} {
		exp.Reset()
		e := New().WithConfig(configWith(quiet, func(c *Config) { c.Trace = trace })).Engine()
		w := doRequest(t, e, "GET", "/nope")

		if got := len(exp.GetSpans()) > 0; got != trace {
			t.Errorf("Trace=%v 时开了 Span=%v", trace, got)
		}
		if got := w.Header().Get(middleware.TraceIDHeader) != ""; got != trace {
			t.Errorf("Trace=%v 时响应里带了 %s=%v", trace, middleware.TraceIDHeader, got)
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
			WithRoutes(func(e *gin.Engine) { e.GET(route, func(c *gin.Context) { c.Status(200) }) }).
			Engine()
		e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", route, nil))

		w := httptest.NewRecorder()
		m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
		if got := strings.Contains(w.Body.String(), `route="`+route+`"`); got != metric {
			t.Errorf("Metric=%v 时记了请求指标=%v\n%s", metric, got, w.Body.String())
		}
	}
}

func TestZHTranslations_InstallsChineseTranslations(t *testing.T) {
	New().WithConfig(configWith(quiet, func(c *Config) { c.Trace, c.ZHTranslations = false, true })).Engine()

	var req struct {
		Name string `binding:"required"`
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	c.Request.Header.Set("Content-Type", "application/json")
	err := c.ShouldBindJSON(&req)
	if err == nil {
		t.Fatal("缺必填字段应当报错")
	}
	if got := trans.Msg(err); !strings.Contains(got, "必填") {
		t.Errorf("没翻成中文，got=%q", got)
	}
}

func TestWithRecoverFunc_ReplacesPanicResponse(t *testing.T) {
	g := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace, c.Metric = false, false, false })).
		WithRecoverFunc(func(c *gin.Context, _ any) { c.String(503, "custom") }).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/boom", func(*gin.Context) { panic("boom") })
		})

	w := httptest.NewRecorder()
	g.Engine().ServeHTTP(w, httptest.NewRequest("GET", "/boom", nil))
	if w.Code != 503 || w.Body.String() != "custom" {
		t.Errorf("自定义 recover 没生效，got=%d %q", w.Code, w.Body.String())
	}
}

// ---- 配置落到 engine 上 ----

func TestBuild_TrustsOnlyPrivateProxiesByDefault(t *testing.T) {
	// gin 自己的默认是 trustedProxies = 0.0.0.0/0 + ::/0，也就是全都信。
	// 那意味着任何人发一个 X-Forwarded-For 就能决定访问日志里的
	// client_ip 是什么——日志可以伪造，建在这个字段上的限流和审计一起失效。
	// 默认只信私有网段：负载均衡、Ingress 转发来的认，公网直连的不认
	e := New().WithConfig(configWith(quiet)).WithRoutes(echoClientIP).Engine()
	if got := clientIPOf(t, e, "203.0.113.9:1234"); got != "203.0.113.9" {
		t.Errorf("公网对端的 X-Forwarded-For 不该认，client_ip 应该是对端地址本身，got=%q（请求头里伪造的是 1.2.3.4）", got)
	}
	if got := clientIPOf(t, e, "10.0.0.5:1234"); got != "1.2.3.4" {
		t.Errorf("私有网段的对端（负载均衡）发来的 X-Forwarded-For 该认，got=%q", got)
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
	e := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/8"} })).
		WithRoutes(echoClientIP).Engine()
	if got := clientIPOf(t, e, "10.0.0.5:1234"); got != "1.2.3.4" {
		t.Errorf("对端在信任网段内，应该认转发头里的地址，got=%q", got)
	}
}

func TestBuild_MultipartMemoryThresholdFromConfig(t *testing.T) {
	// gin 自己默认 32MB，而这个数不是「请求体上限」是「超过多少才落盘」，
	// 实际代价约是它的三倍：一次 60MB 的上传，配 32MB 时解析这一步
	// 让堆多占 96MB，二十个并发就是两个 G
	g := New().WithConfig(configWith(quiet, func(c *Config) { c.MaxMultipartMemory = 2 << 20 }))
	if got := g.Engine().MaxMultipartMemory; got != 2<<20 {
		t.Errorf("该用配置里的阈值，got=%d want=%d", got, 2<<20)
	}
}

func TestBuild_CallbackEngineSettingsOverrideConfig(t *testing.T) {
	// 回调在配置落到 engine 上之后才跑：使用者在代码里明确设了的，就以代码为准。
	// 原先反过来——Start 时再把配置落一遍，回调里的设置被悄悄盖掉。
	// 所以起了服务再看一次：Start 不能再落一遍配置
	port := testkit.FreePort(t)
	g := New().WithConfig(configWith(quiet, on(port), func(c *Config) { c.MaxMultipartMemory = 2 << 20 })).
		WithRoutes(echoClientIP, func(e *gin.Engine) {
			e.MaxMultipartMemory = 64 << 20
			if err := e.SetTrustedProxies([]string{"10.0.0.0/8"}); err != nil {
				t.Error(err)
			}
		})
	g.Engine() // 在测试协程里装配：回调里的 t.Error 不能跑在别的协程上
	serving(t, g, port)

	if got := g.Engine().MaxMultipartMemory; got != 64<<20 {
		t.Errorf("该以回调里设的为准，got=%d", got)
	}
	if got := clientIPOf(t, g.Engine(), "10.0.0.5:1234"); got != "1.2.3.4" {
		t.Errorf("回调里信任了 10.0.0.0/8，client_ip 该认转发头，got=%q", got)
	}
}

// ---- 启停 ----

func TestStartStop_GracefulShutdown(t *testing.T) {
	port := testkit.FreePort(t)
	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.String(200, "pong") })
	})

	done := make(chan error, 1)
	go func() { done <- g.Start(context.Background()) }()

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

	if err := g.Stop(context.Background()); err != nil {
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

func TestStart_DoesNotListenOnInvalidConfig(t *testing.T) {
	err := startErr(t, New().WithConfig(configWith(func(c *Config) { c.TLS.CertFile = "只配了一半" })))

	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Module != "xgin" || xe.Op != "config" {
		t.Fatalf("该是 xgin 的 config 错误，got=%v", err)
	}
	// 进程里可能有两个服务：错误得说清楚是 WithConfig 那份，以及哪一项
	if !strings.Contains(err.Error(), "WithConfig") || !strings.Contains(err.Error(), "TLS.CertFile") {
		t.Errorf("错误该说清楚是哪份配置的哪一项，got=%v", err)
	}
}

func TestStart_UsesConfigFromBuildTime(t *testing.T) {
	// 监听地址、超时和 engine 上的中间件、信任的代理必须出自同一份配置。
	// Start 自己再读一遍的话两边可能对不上——这里用「装配之后才 WithConfig」
	// 造出这种局面：它在装配之后不生效，监听的端口也就不该跟着变
	portA, portB := testkit.FreePort(t), testkit.FreePort(t)
	g := New().WithConfig(configWith(quiet, on(portA)))
	g.Engine()
	g.WithConfig(configWith(quiet, on(portB)))

	serving(t, g, portA)
}

func TestStart_SignalArrivesBeforeStart(t *testing.T) {
	// 照常监听的话，服务会在「已经收到停止信号」之后才起来，
	// 然后一直跑到框架等超时为止
	port := testkit.FreePort(t)
	g := New().WithConfig(configWith(quiet, on(port)))
	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("还没启动就 Stop 不该报错：%v", err)
	}

	done := make(chan error, 1)
	go func() { done <- g.Start(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("应当直接返回，got=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 之后 Start 不该真的开始监听")
	}

	// 确认端口上真的没人监听
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
		conn.Close()
		t.Error("服务不该起来")
	}
}

func TestStart_RepeatedStartFails(t *testing.T) {
	port := testkit.FreePort(t)
	g := New().WithConfig(configWith(quiet, on(port)))
	serving(t, g, port)

	if err := g.Start(context.Background()); err == nil {
		t.Error("重复启动应当报错")
	}
}

func TestStop_SafeWithoutStart(t *testing.T) {
	if err := New().Stop(context.Background()); err != nil {
		t.Errorf("没启动过的 Stop 不该报错：%v", err)
	}
}

// servingWithHungRequest 起一个服务，并让一个请求挂在 handler 里不返回，
// 这样 Shutdown 必须等它 —— 才测得出等多久
func servingWithHungRequest(t *testing.T) *XGin {
	g, _ := servingWithHungRequestDone(t)
	return g
}

// servingWithHungRequestDone 同上，另外返回一个在「那个挂住的请求结束时」
// 关闭的 channel —— 强制断连有没有生效，只有它看得出来
func servingWithHungRequestDone(t *testing.T) (*XGin, <-chan struct{}) {
	t.Helper()
	port := testkit.FreePort(t)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
		e.GET("/hang", func(c *gin.Context) { <-release; c.Status(200) })
	})
	go g.Start(context.Background())
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
	return g, hung
}

// stopWithin 带 200ms 的截止时间调 Stop，3s 还没返回就判失败。
// 在另一个协程里调：Stop 不听截止时间的话会一直挂在那个请求上，测试不能跟着挂死
func stopWithin(t *testing.T, g *XGin) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- g.Stop(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 该按调用方给的截止时间收手，3s 了还没返回")
		return nil
	}
}

func TestStop_HonorsCallerDeadline(t *testing.T) {
	// 回归用例。等多久只看调用方的 ctx，xone.Run 给的是服务那一段停止预算。
	// 这里曾经用 context.WithoutCancel 换掉它，于是会实打实地等满自己的上限——
	// 「所有组件共享一份预算」就成了一句空话
	g := servingWithHungRequest(t)
	if err := stopWithin(t, g); err == nil {
		t.Error("在途请求没做完就到点了，该如实报错")
	}
}

func TestStop_ForceClosesInFlightConnsAfterTimeout(t *testing.T) {
	// Shutdown 超时只返回错误，它不动那些连接。就这么走的话 handler 还在跑，
	// 而框架紧接着就去关数据库和缓存了——那些请求会摸到已经关掉的连接池。
	//
	// 只看端口连不连得上是测不出来的：Shutdown 一进去就把监听关了，
	// 连不上是两种情况共有的表现。要看的是那个在途请求有没有被断掉。
	g, hung := servingWithHungRequestDone(t)
	if err := stopWithin(t, g); err == nil {
		t.Fatal("在途请求没做完就到点了，该报错")
	}

	select {
	case <-hung:
	case <-time.After(2 * time.Second):
		t.Error("超时之后在途请求仍在继续——框架接着就去关数据库了，它会摸到已关闭的连接池")
	}
}

func TestStop_WaitsForHandlersAfterForceClose(t *testing.T) {
	// Close 只关连接、取消请求的 ctx，handler 所在的协程照跑。原先 Close 完就返回，
	// 看到 ctx 取消、正在收尾的 handler 还没返回，框架就接着去关数据库了。
	// 截止时间也不能被 Shutdown 用满：得给断连之后的这段收尾留出时间
	port := testkit.FreePort(t)
	var returned atomic.Bool
	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
		e.GET("/hang", func(c *gin.Context) {
			<-c.Request.Context().Done()
			time.Sleep(50 * time.Millisecond) // 收尾：回滚事务、写审计……
			returned.Store(true)
		})
	})
	go g.Start(context.Background())
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/ping")
	go func() {
		if resp, err := http.Get(base + "/hang"); err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond) // 等请求真的进了 handler

	// 预算的 20% 留给断连之后的收尾（shutdownCtx）：2s 的预算留 400ms，是这里 50ms 收尾的 8 倍。
	// 原先给 500ms、只留 100ms：-race 加上别的包并行跑时调度一慢就不够，-count=3 下三次挂两次。
	// 实测正常负载下 Stop 在 Shutdown 到点后约 50ms 返回；余量是留给调度抖动的
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := g.Stop(ctx)
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
	g := servingWithHungRequest(t)
	err := stopWithin(t, g)
	if err == nil || !strings.Contains(err.Error(), "1 handler(s) still running") {
		t.Errorf("该报出还有 1 个 handler 没返回，got=%v", err)
	}
}

// ---- 读配置文件 ----

func TestEngine_UsesFinalConfigEvenBeforeRun(t *testing.T) {
	// 使用者在 main 顶上建好 XGin、调 Engine()（比如为了挂 Swagger），之后才 xone.Run。
	// 曾经那时配置还没加载，TrustedProxies 和 MaxMultipartMemory 只好推迟到 Start
	// 才落到 engine 上，单独拿 Engine() 去用的只拿得到默认值。现在配置在第一次读的
	// 时候才加载：New 在前、配置文件就位在后，装配时读到的照样是最终值
	g := New().WithRoutes(echoClientIP)
	testkit.UseConfigEnv(t, "XGin:\n  TrustedProxies: [10.0.0.0/8]\n  MaxMultipartMemory: 1048576\n  Metric: false\n")

	if got := clientIPOf(t, g.Engine(), "10.0.0.5:1234"); got != "1.2.3.4" {
		t.Errorf("配置里的 TrustedProxies 没生效，client_ip=%q", got)
	}
	if got := g.Engine().MaxMultipartMemory; got != 1<<20 {
		t.Errorf("配置里的 MaxMultipartMemory 没生效，got=%d", got)
	}
	if w := doRequest(t, g.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("配置里关掉了指标，不该有 /metrics")
	}
}

func TestEngine_InvalidConfigFallsBackToSafeDefaults(t *testing.T) {
	// 照样给一个 engine（使用者可能在 Run 之前就拿了它），但按默认值装：
	// 解到一半的非法配置里，TrustedProxies 可能正是 0.0.0.0/0。错误由 Start 报，不监听
	testkit.UseConfigEnv(t, "XGin:\n  Port: 0\n  TrustedProxies: [0.0.0.0/0]\n")
	g := New().WithRoutes(echoClientIP)
	if got := clientIPOf(t, g.Engine(), "203.0.113.9:1234"); got != "203.0.113.9" {
		t.Errorf("配置不合法时该退回默认值（只信私有网段），公网对端的转发头不该认，client_ip=%q", got)
	}

	err := startErr(t, g)
	if xerror.Module(err) != "xgin" || !xerror.Is(err, "xconfig") || !strings.Contains(err.Error(), "Port") {
		t.Errorf("该是 xgin 报的、裹着配置文件那一块的错误，got=%v", err)
	}
}

func TestCurrentConfig_AlwaysReadsConfigSection(t *testing.T) {
	// 不用等 xone.Run：第一次读的时候才加载配置文件，读到的就是最终值
	testkit.UseConfigEnv(t, "XGin:\n  Port: 18080\n  Mode: debug\n  Log: false\n")
	c := CurrentConfig()
	if c.Port != 18080 || c.Mode != "debug" || c.Log {
		t.Errorf("该读到配置文件里的值，got=%+v", c)
	}
	if c.Host != "0.0.0.0" || !c.Metric {
		t.Errorf("没写的字段该保持默认值，got=%+v", c)
	}
}

func TestCurrentConfig_ReturnsDefaultsWhenSectionInvalid(t *testing.T) {
	// 错误由 xone.Run 在启动时报（见 TestLoadConfig_StartupFailsOnInvalidValue）。
	// 这里返回的东西会被拿去 WithConfig，必须是安全的：解到一半的非法配置不算
	testkit.UseConfigEnv(t, "XGin:\n  Port: 0\n  TrustedProxies: [0.0.0.0/0]\n")
	if got := CurrentConfig(); !reflect.DeepEqual(got, DefaultConfig()) {
		t.Errorf("该退回默认值，got=%+v", got)
	}
}

func TestLoadConfig_StartupFailsOnFieldTypo(t *testing.T) {
	// 拼错的字段在启动阶段就该被拦下，否则使用者会一直以为自己配上了
	testkit.UseConfigEnv(t, "XGin:\n  Prot: 8080\n")
	if err := loadConfig(context.Background()); err == nil {
		t.Fatal("字段拼错应当让启动失败")
	}
}

func TestLoadConfig_StartupFailsOnInvalidValue(t *testing.T) {
	// 端口越界、TLS 只配一半、指标路径写错，原先要等到服务 Start 才报。
	// Config 实现了 Validate，解码时就一起查了，于是 StageServer 的这个钩子让启动当场失败
	for name, yml := range map[string]string{
		"端口越界":        "XGin:\n  Port: 70000\n",
		"只配一半的 TLS":   "XGin:\n  TLS:\n    CertFile: cert.pem\n",
		"指标路径不以 / 开头": "XGin:\n  MetricPath: metrics\n",
	} {
		testkit.UseConfigEnv(t, yml)
		if err := loadConfig(context.Background()); err == nil {
			t.Errorf("%s 应当让启动失败", name)
		}
	}
}

func TestWithConfig_TwoInstancesListenOnOwnPorts(t *testing.T) {
	// 没有 WithConfig 时两个实例读的是同一块配置，只能监听同一个端口——
	// 「需要两套配置时也有出路」这条承诺对 xgin 就是假的。
	// 配置文件里那一块留在默认端口上，证明两个实例谁都没按它监听
	testkit.UseConfigEnv(t, "XGin:\n  Port: 8080\n")

	portA, portB := testkit.FreePort(t), testkit.FreePort(t)
	for _, s := range []struct {
		port int
		body string
	}{{portA, "A"}, {portB, "B"}} {
		c := CurrentConfig()
		c.Host, c.Port = "127.0.0.1", s.port
		quiet(&c)
		body := s.body
		g := New().WithConfig(c).WithRoutes(func(e *gin.Engine) {
			e.GET("/who", func(c *gin.Context) { c.String(200, body) })
		})
		serving(t, g, s.port)
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

func TestWithConfig_FollowsConfigFileWhenOmitted(t *testing.T) {
	// 默认路径：端口、开关都来自配置文件里的 XGin 块
	port := testkit.FreePort(t)
	testkit.UseConfigEnv(t, fmt.Sprintf("XGin:\n  Host: 127.0.0.1\n  Port: %d\n  Log: false\n  Metric: false\n", port))

	g := New().WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
	})
	serving(t, g, port)
}

// ---- 登记 ----

func TestRegister_OnlyReadsConfigNoStopHook(t *testing.T) {
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xgin" {
			got = &e
		}
	}
	if got == nil {
		t.Fatal("没有登记启动钩子，XGin 那一块就没人认领、配错了也要等到 Start 才报")
	}
	if got.Stage != hook.StageServer {
		t.Errorf("服务应在最后一档，got=%v", got.Stage)
	}
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xgin" {
			t.Error("服务由使用者交给 xone.Run 启停，不该在这里登记停止钩子")
		}
	}
}

func TestXGin_ImplementsRunnable(t *testing.T) {
	// 结构化满足即可，不 import 根包——「集成不依赖框架」这条要在编译层面成立
	var _ interface {
		Start(context.Context) error
		Stop(context.Context) error
	} = New()
}

func TestSensitiveFieldsAPI(t *testing.T) {
	// 打开 body 日志之前要能把自定义敏感字段补上
	middleware.AddSensitiveFields("x_custom")
	middleware.AddSensitiveHeaders("X-Custom")
}

// doRequest 对 engine 发一次请求
func doRequest(t *testing.T, e *gin.Engine, method, path string) *httptest.ResponseRecorder {
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
	// 回归用例。装配时如果就把 xmetric 的 registry 抓走，而那时 xmetric
	// 还没初始化，指标会被注册到一个永远不会被导出的兜底 registry 上：
	// 请求正常处理、指标正常记录、/metrics 里什么都没有，且没有任何迹象。
	g := New().WithConfig(DefaultConfig()).WithRoutes(func(e *gin.Engine) {
		e.GET("/x", func(c *gin.Context) { c.Status(200) })
	})
	e := g.Engine() // 使用者在 Run 之前拿一下 engine，很自然的写法

	// 此后框架才初始化 xmetric（StageTelemetry 在 StageServer 之前）
	m, closer, err := xmetric.New(xmetric.Config{Namespace: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), "demo_http_requests_total") {
		t.Errorf("指标应记在初始化之后的 registry 上\n实际=\n%s", w.Body.String())
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

	// 先打一个请求产出指标，再抓 /metrics
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/nope", nil))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))

	if !strings.Contains(w.Body.String(), "demo_http_requests_total") {
		t.Errorf("/metrics 应导出当前生效的 registry\n实际=\n%s", w.Body.String())
	}
}

func TestBuild_MetricsEndpointRunsUserMiddleware(t *testing.T) {
	// gin 在注册路由那一刻就把处理链定死了。指标端点原先注册在
	// e.Use(g.extra...) 之前，于是 WithMiddleware 挂的统一鉴权
	// 对业务路由生效、对 /metrics 不生效——一个以为被保护的端点其实敞着
	auth := func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) }

	e := New().WithConfig(configWith(func(c *Config) { c.Log = false })).
		WithMiddleware(auth).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/biz", func(c *gin.Context) { c.Status(200) })
		}).Engine()

	for _, path := range []string{"/biz", "/metrics"} {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s 该被鉴权中间件拦住，got=%d", path, w.Code)
		}
	}
}

// ---- applyConfig ----

func TestApplyConfig_FallsBackToTrustNoneOnProxyError(t *testing.T) {
	// gin 的默认是全都信，于是任何人发一个 X-Forwarded-For 就能决定
	// 访问日志里的 client_ip。它的解析行为是「解到出错为止、把已经解出来的
	// 留下」，所以列表前半段会被留着信任——必须整个退到安全的那一侧。
	//
	// 非法项前面要先放一个合法网段：只放非法项的话，gin 留下的是空列表，
	// 两种实现看起来一模一样，这条用例就什么都没在测
	e := gin.New()
	e.GET("/", func(c *gin.Context) { c.String(200, c.ClientIP()) })

	c := DefaultConfig()
	c.TrustedProxies = []string{"10.0.0.0/8", "not-an-ip"} // Validate 会拦，这里直接绕过它
	applyConfig(e, c)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:1234" // 落在前半段那个网段里
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if w.Body.String() != "10.0.0.1" {
		t.Errorf("设置出错时应当谁都不信、用直连地址，got=%q", w.Body.String())
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
func probeTrust(t *testing.T) func(e *gin.Engine, remote string) bool {
	t.Helper()
	old := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(old) })
	var got atomic.Bool
	otel.SetTextMapPropagator(trustProbe{got: &got})

	return func(e *gin.Engine, remote string) bool {
		got.Store(false)
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		e.ServeHTTP(httptest.NewRecorder(), req)
		return got.Load()
	}
}

func TestBuild_PassthroughHeadersOnlyFromTrustedProxies(t *testing.T) {
	// 回归用例。透传 Header 原先从任何入站请求里都照单全收：公网客户端发一个
	// X-Tenant-Id / X-Internal-Token，就被当成自己人给的，带进内网的每一次调用。
	// 「谁是自己人」用的是信任代理的那张表，不另设开关
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

func TestLoadConfig_EmptyTrustedProxiesInFileTrustsNone(t *testing.T) {
	// 默认值预填在结构体里；配置文件写 [] 得真的换成空列表，而不是解码时被当成「没写」留下默认的 private
	testkit.UseConfigEnv(t, "XGin:\n  TrustedProxies: []\n")
	if got := clientIPOf(t, New().WithRoutes(echoClientIP).Engine(), "10.0.0.5:1234"); got != "10.0.0.5" {
		t.Errorf("配置文件写了 [] 就该谁都不信，got=%q", got)
	}
}

func TestBuild_TrustedProxiesMixesPrivateWithOtherCIDRs(t *testing.T) {
	// 列表整体替换默认值：要在私有网段之外再加一段，把 private 一起写上
	trusted := probeTrust(t)
	e := New().WithConfig(configWith(quiet, func(c *Config) {
		c.TrustedProxies = []string{"private", "203.0.113.0/24"}
	})).Engine()
	for remote, want := range map[string]bool{"10.0.0.5:1": true, "203.0.113.9:1": true, "198.51.100.1:1": false} {
		if got := trusted(e, remote); got != want {
			t.Errorf("对端 %s 可信=%v，want %v", remote, got, want)
		}
	}
	// 写 [] 就谁都不信，透传 Header 也一样
	none := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = []string{} })).Engine()
	if trusted(none, "10.0.0.5:1") {
		t.Error("写了 [] 就该谁都不信")
	}
}

func TestBuild_TraceDisabledOnlySkipsSpan_StillPropagates(t *testing.T) {
	// 回归用例。XGin.Trace: false 原先连 Extract 一起摘掉：可信对端发来的
	// X-Request-Id、上游的 traceparent 都断在这一跳，而它本该只管 Span
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
	e = New().WithConfig(configWith(quiet, off)).WithRoutes(func(e *gin.Engine) {
		e.GET("/x", func(c *gin.Context) {
			seen = trace.SpanContextFromContext(c.Request.Context()).TraceID().String()
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
func servingH2C(t *testing.T, d time.Duration) (base string, g *XGin, entered, finished chan struct{}) {
	t.Helper()
	port := testkit.FreePort(t)

	entered, finished = make(chan struct{}), make(chan struct{})
	g = New().WithConfig(configWith(quiet, on(port), func(c *Config) { c.UseH2C = true })).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/ping", func(c *gin.Context) { c.Status(200) })
			e.GET("/slow", func(c *gin.Context) {
				close(entered)
				time.Sleep(d)
				close(finished)
				c.String(200, "done")
			})
		})
	go g.Start(context.Background())
	base = fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/ping")
	return base, g, entered, finished
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
	// 回归用例。h2c 原先靠 x/net 的 h2c.NewHandler：连接被它劫持走，
	// http.Server 从此不认识这些连接——实测 Shutdown 约 60µs 就返回 nil，
	// 而一个 2s 的在途请求还在跑；Close() 同样够不着它们。
	// 框架紧接着去关数据库，那个请求就摸到了已经关掉的连接池
	base, g, entered, finished := servingH2C(t, time.Second)
	got := h2cGet(base + "/slow")
	<-entered

	if err := g.Stop(context.Background()); err != nil {
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
	// 另一半：到点还没做完的 h2c 请求要被 Close() 断掉，
	// 而不是在框架关掉数据库之后继续跑
	base, g, entered, _ := servingH2C(t, 5*time.Second)
	got := h2cGet(base + "/slow")
	<-entered

	if err := stopWithin(t, g); err == nil {
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
	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
	})
	base := serving(t, g, port)

	if r := <-h2cGet(base + "/ping"); r.err == nil {
		t.Errorf("没开 UseH2C，明文 HTTP/2 不该成功，got=HTTP/%d", r.proto)
	}
}

func TestDefault404And405_SameAsGinAndBytesOutCounted(t *testing.T) {
	// gin 默认的 404 / 405 正文在中间件链跑完之后才写，访问日志的 bytes_out 原先记成 0；
	// 换成在链里写之后，响应得和 gin 自己的一个字节不差
	bare := gin.New()
	bare.HandleMethodNotAllowed = true
	bare.GET("/only-get", func(c *gin.Context) {})

	for _, c := range []struct{ name, method, path string }{
		{"404", "GET", "/nope"},
		{"405", "POST", "/only-get"},
	} {
		buf := captureLog(t)
		e := New().WithConfig(configWith(func(cfg *Config) { cfg.Metric, cfg.Trace = false, false })).
			WithRoutes(func(e *gin.Engine) { e.GET("/only-get", func(c *gin.Context) {}) }).Engine()
		got, want := httptest.NewRecorder(), httptest.NewRecorder()
		e.ServeHTTP(got, httptest.NewRequest(c.method, c.path, nil))
		bare.ServeHTTP(want, httptest.NewRequest(c.method, c.path, nil))

		if got.Code != want.Code || got.Body.String() != want.Body.String() ||
			got.Header().Get("Content-Type") != want.Header().Get("Content-Type") {
			t.Errorf("%s：响应该和 gin 默认的一样，got=%d %q %q want=%d %q %q", c.name,
				got.Code, got.Header().Get("Content-Type"), got.Body, want.Code, want.Header().Get("Content-Type"), want.Body)
		}
		if w := fmt.Sprintf(`"bytes_out":%d`, want.Body.Len()); !strings.Contains(buf.String(), w) {
			t.Errorf("%s：访问日志的 bytes_out 该是正文长度 %d\n%s", c.name, want.Body.Len(), buf.String())
		}
	}
}

func TestNoRoute_UserHandlerStillWins(t *testing.T) {
	// 框架的 NoRoute 注册在用户路由之前，WithRoutes 里自己设的要盖得过它
	e := New().WithConfig(configWith(func(cfg *Config) { cfg.Metric, cfg.Trace, cfg.Log = false, false, false })).
		WithRoutes(func(e *gin.Engine) { e.NoRoute(func(c *gin.Context) { c.String(404, "custom") }) }).Engine()
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/nope", nil))
	if w.Body.String() != "custom" {
		t.Errorf("用户自己的 NoRoute 该生效，got=%q", w.Body)
	}
}

func TestClientIP_SharedRuleMatchesGin(t *testing.T) {
	// xecho 没有 gin 可用，client_ip 按 internal/web 的 Proxies.ClientIP 算；它的规则照抄 gin。
	// 同一个服务换一个框架 client_ip 不该变，所以逐条比对 gin 自己算出来的
	for _, proxies := range [][]string{{web.TrustPrivate}, {}, {"10.0.0.0/8", "203.0.113.7"}, {web.TrustPrivate, "203.0.113.0/24"}} {
		e := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = proxies })).WithRoutes(echoClientIP).Engine()
		shared := web.ParseProxies(proxies)
		for _, remote := range []string{"203.0.113.9:1", "203.0.113.7:1", "10.0.0.5:1", "[::ffff:10.0.0.5]:1", "[::1]:1", "[2001:db8::1]:1", "junk"} {
			for _, h := range []map[string][]string{
				{},
				{"X-Forwarded-For": {"1.2.3.4"}},
				{"X-Forwarded-For": {"1.2.3.4, 10.0.0.9"}},
				{"X-Forwarded-For": {"1.2.3.4, 5.6.7.8, 10.0.0.9"}},
				{"X-Forwarded-For": {"10.0.0.7, 10.0.0.8"}},
				{"X-Forwarded-For": {"1.2.3.4", "5.6.7.8"}},
				{"X-Forwarded-For": {"junk, 10.0.0.9"}},
				{"X-Forwarded-For": {"1.2.3.4, junk"}},
				{"X-Forwarded-For": {"junk"}, "X-Real-Ip": {"9.9.9.9"}},
				{"X-Real-Ip": {"9.9.9.9"}},
				{"X-Forwarded-For": {" 1.2.3.4 ,10.0.0.9 "}},
			} {
				req := httptest.NewRequest("GET", "/client-ip", nil)
				req.RemoteAddr = remote
				for k, vs := range h {
					for _, v := range vs {
						req.Header.Add(k, v)
					}
				}
				w := httptest.NewRecorder()
				e.ServeHTTP(w, req)
				if got := shared.ClientIP(req); got != w.Body.String() {
					t.Errorf("proxies=%v remote=%s headers=%v：gin=%q web=%q", proxies, remote, h, w.Body.String(), got)
				}
			}
		}
	}
}

// ---- 零值 ----

func TestZeroValue_ReportsUnderModuleName(t *testing.T) {
	// &XGin{} 不经过 New 也要能用：错误和日志照样记在 xgin 名下，
	// 调用方的 xerror.Is(err, "xgin") 才成立，日志里也不会冒出 " listening" 这种没头的消息
	buf := captureLog(t)
	cert := filepath.Join(t.TempDir(), "missing.pem")
	err := startErr(t, (&XGin{}).WithConfig(configWith(quiet, on(testkit.FreePort(t)), func(c *Config) { c.TLS.CertFile, c.TLS.KeyFile = cert, cert })))
	if !xerror.Is(err, "xgin") || xerror.Module(err) != "xgin" {
		t.Fatalf("零值的 Start 报的错该算 xgin 的，got=%v", err)
	}

	z := &XGin{}
	if err := z.Stop(context.Background()); err != nil {
		t.Fatalf("Start 之前 Stop 该什么都不做，got=%v", err)
	}
	if err := z.Start(context.Background()); err != nil {
		t.Fatalf("Stop 之后的 Start 该直接返回 nil，got=%v", err)
	}
	if !strings.Contains(buf.String(), `"msg":"xgin received the shutdown signal before starting`) {
		t.Errorf("日志消息该以 xgin 开头，got=%s", buf.String())
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
		if strings.Contains(buf.String(), "xgin listening") {
			t.Errorf("%s: 没监听上就不该打 listening，got=%s", name, buf.String())
		}
	}
}

func TestLoadConfig_IPv4MappedProxyFailsStartup(t *testing.T) {
	// ::ffff:10.0.0.1 在 gin 和 xtrace 的可信判断里各是一个意思，只能拦住，并告诉使用者该怎么写
	err := loadErr(t, "XGin:\n  TrustedProxies: [private, \"::ffff:10.0.0.1\"]\n")
	if err == nil || !strings.Contains(err.Error(), "IPv4-mapped") || !strings.Contains(err.Error(), "write it as 10.0.0.1") {
		t.Fatalf("IPv4 映射写法该启动失败并给出 IPv4 写法，got=%v", err)
	}
}

// servingHijacked 起一个服务，/ws 劫持连接后一直等请求的 ctx（WebSocket 的读循环就是这样），
// 返回服务和「handler 已经劫持、正在等」的信号
func servingHijacked(t *testing.T) (*XGin, <-chan struct{}) {
	t.Helper()
	port := testkit.FreePort(t)
	entered, release := make(chan struct{}), make(chan struct{})
	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
		e.GET("/ws", func(c *gin.Context) {
			conn, _, err := c.Writer.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			close(entered)
			select {
			case <-c.Request.Context().Done():
			case <-release: // 测试失败时也让 Cleanup 里的 Stop 返回
			}
		})
	})
	base := serving(t, g, port)
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
	return g, entered
}

func TestStop_HijackedConnGetsCancelled(t *testing.T) {
	// 回归用例。被劫持的连接（WebSocket）Shutdown 不等、Close 也断不掉，请求的 ctx 原先永远不取消：
	// 等着它的 handler 不返回，Stop 把整份预算耗完再报 1 handler(s) still running
	g, _ := servingHijacked(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := g.Stop(ctx)
	if err != nil {
		t.Fatalf("劫持走的 handler 看到 ctx 取消就返回了，Stop 不该报错：%v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Stop 该在 Shutdown 返回之后马上取消劫持连接的 ctx，却用了 %v", d)
	}
}

func TestStart_RetryAfterListenFailure(t *testing.T) {
	// 回归用例。监听失败（端口被占）之后 s.srv 原先留着，之后每次 Start 都说 already running
	port := testkit.FreePort(t)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
	})
	if err := startErr(t, g); err == nil || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("端口被占时该报 listen 失败，got=%v", err)
	}
	ln.Close()

	done := make(chan error, 1)
	go func() { done <- g.Start(context.Background()) }()
	t.Cleanup(func() { _ = g.Stop(context.Background()) })
	select {
	case err := <-done:
		t.Fatalf("端口空出来之后该能再起来，got=%v", err)
	case <-time.After(300 * time.Millisecond):
	}
	waitServing(t, fmt.Sprintf("http://127.0.0.1:%d/ping", port))
}

func TestBuild_TrailingSlashGoesThroughMiddlewareAs404(t *testing.T) {
	// 回归用例。gin 的 RedirectTrailingSlash 默认开着（v1.12.0 gin.go 的 handleHTTPRequest）：
	// /users/ 对不上 /users 时直接回 301，整条中间件链都不跑——没有访问日志、指标和 Span。
	// 关掉之后它走 NoRoute，记成 404 unmatched
	buf := captureLog(t)
	route := func(e *gin.Engine) { e.GET("/users", func(c *gin.Context) { c.String(200, "ok") }) }
	e := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false })).WithRoutes(route).Engine()

	if w := doRequest(t, e, "GET", "/users/"); w.Code != 404 {
		t.Errorf("末尾多一个斜杠该是 404，不该被重定向，got=%d Location=%q", w.Code, w.Header().Get("Location"))
	}
	if out := buf.String(); !strings.Contains(out, `"route":"unmatched","path":"/users/","status":404`) {
		t.Errorf("该留下一条 404 unmatched 的访问日志：%s", out)
	}

	// 要重定向的使用者在 WithRoutes 里打开，它盖得过配置
	e = New().WithConfig(configWith(quiet)).WithRoutes(func(e *gin.Engine) { e.RedirectTrailingSlash = true }, route).Engine()
	if w := doRequest(t, e, "GET", "/users/"); w.Code != http.StatusMovedPermanently {
		t.Errorf("WithRoutes 里打开 RedirectTrailingSlash 该照 gin 的默认重定向，got=%d", w.Code)
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
	t.Cleanup(func() { slog.SetDefault(old); closer.Close() })
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

func TestBuild_GinContextAsContextKeepsSpanLogScopeAndCancel(t *testing.T) {
	// 回归用例。*gin.Context 本身就是 context.Context，业务里常直接传它：xlog.AddKV(c, ...)、db.WithContext(c)。
	// 不开 ContextWithFallback 时它的 Value 只查 c.Keys、Done 返回 nil：日志作用域、父 Span 全丢，取消也传不下去
	logs := xlogCapture(t)
	exp := tracetest.NewInMemoryExporter()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	var done <-chan struct{}
	e := New().WithConfig(configWith(func(c *Config) { c.Metric = false })).WithRoutes(func(e *gin.Engine) {
		e.GET("/a", func(c *gin.Context) {
			xlog.AddKV(c, "user_id", "u9")
			_, span := otel.Tracer("test").Start(c, "child")
			span.End()
			done = c.Done()
			c.Status(200)
		})
	}).Engine()
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/a", nil).WithContext(reqCtx))

	var server, child sdktrace.ReadOnlySpan
	for _, s := range exp.GetSpans().Snapshots() {
		if s.Name() == "child" {
			child = s
		} else {
			server = s
		}
	}
	if server == nil || child == nil || child.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Errorf("拿 *gin.Context 开的 Span 该挂在服务端 Span 下面")
	}
	if done == nil || done != reqCtx.Done() {
		t.Error("c.Done() 该是请求 ctx 的 Done，客户端走了才传得下去")
	}
	if out := logs(); !strings.Contains(out, `"user_id":"u9"`) {
		t.Errorf("xlog.AddKV(c, ...) 的字段该进访问日志：\n%s", out)
	}
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

func TestStart_GinUseH2CDoesNotBypassShutdown(t *testing.T) {
	// 回归用例。gin 的 engine.Handler() 在 UseH2C 开着时包一层 x/net 的 h2c.NewHandler：
	// 连接被劫持走，Shutdown 不等在途请求。服务交给 net/http 的应该是 engine 本身，
	// h2c 由 XGin.UseH2C 走标准库。这里在回调里打开 gin 自己的 UseH2C，服务端看到的请求不该是 h2c 包出来的
	port := testkit.FreePort(t)
	var proto int
	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.UseH2C = true
		e.GET("/ping", func(c *gin.Context) { proto = c.Request.ProtoMajor; c.Status(200) })
	})
	go func() { _ = g.Start(context.Background()) }()
	t.Cleanup(func() {
		// 带截止时间：包成 h2c 的连接被劫持走，不带的话这里会一直等那个协程
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = g.Stop(ctx)
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/ping")
	if r := <-h2cGet(base + "/ping"); r.err == nil {
		t.Errorf("XGin.UseH2C 没开，gin 自己的 UseH2C 不该让明文 HTTP/2 绕过 net/http 进来，got=HTTP/%d handler=HTTP/%d", r.proto, proto)
	}
}
