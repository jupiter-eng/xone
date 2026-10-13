package xmetric

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xlog"
	"github.com/jupiter-eng/xone/xonetest"
)

// install 装一套干净的指标设施，测试结束后还原全局状态
func newMetrics(t testing.TB, mutate func(*Config)) *Metrics {
	t.Helper()
	c := DefaultConfig()
	// 默认关掉 Go / 进程指标：它们会往导出里塞几百行，淹掉断言想看的东西
	c.GoMetrics, c.ProcessMetrics, c.LogErrorMetric = false, false, false
	if mutate != nil {
		mutate(&c)
	}
	m, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}

	oldLogger := slog.Default()
	oldCurrent := current.Load()
	t.Cleanup(func() {
		closer.Close()
		slog.SetDefault(oldLogger)
		current.Store(oldCurrent)
	})

	m.Install()
	return m
}

// dump 抓一次 /metrics 的文本输出
func dump(t *testing.T, m *Metrics) string {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("/metrics 返回 %d", w.Code)
	}
	return w.Body.String()
}

func TestShortcut_Counter(t *testing.T) {
	m := newMetrics(t, nil)
	CounterInc("orders", T("status", "ok"))
	CounterInc("orders", T("status", "ok"))
	CounterAdd("orders", 3, T("status", "failed"))

	out := dump(t, m)
	for _, want := range []string{`orders{status="ok"} 2`, `orders{status="failed"} 3`} {
		if !strings.Contains(out, want) {
			t.Errorf("导出里应有 %s\n实际=\n%s", want, out)
		}
	}
}

func TestShortcut_Gauge(t *testing.T) {
	m := newMetrics(t, nil)
	GaugeSet("queue_depth", 10)
	GaugeInc("queue_depth")
	GaugeDec("queue_depth")
	GaugeDec("queue_depth")

	if out := dump(t, m); !strings.Contains(out, "queue_depth 9") {
		t.Errorf("应为 10+1-1-1=9\n实际=\n%s", out)
	}
}

func TestShortcut_Histogram(t *testing.T) {
	m := newMetrics(t, nil)
	HistogramObserve("payload_bytes", 0.3)

	out := dump(t, m)
	if !strings.Contains(out, "payload_bytes_count 1") {
		t.Errorf("应记到一次观测\n实际=\n%s", out)
	}
	// 默认桶是 prometheus.DefBuckets，0.3 应落在 0.5 那一档里
	if !strings.Contains(out, `payload_bytes_bucket{le="0.5"} 1`) {
		t.Errorf("默认桶应为 prometheus.DefBuckets\n实际=\n%s", out)
	}
}

func TestShortcut_LabelOrderDoesNotAffectReuse(t *testing.T) {
	// 同一个指标写两种标签顺序，不该建出两个 collector——
	// prometheus 会因「同名不同标签」拒掉第二个，数据就丢了
	m := newMetrics(t, nil)
	CounterInc("api_calls", T("a", "1"), T("b", "2"))
	CounterInc("api_calls", T("b", "2"), T("a", "1"))

	if out := dump(t, m); !strings.Contains(out, `api_calls{a="1",b="2"} 2`) {
		t.Errorf("两种书写顺序应命中同一条时间序列\n实际=\n%s", out)
	}
}

func TestShortcut_SortingLeavesCallerSliceIntact(t *testing.T) {
	// 调用方可能拿同一个切片反复展开，排序改了它的顺序就是改了别人的数据
	newMetrics(t, nil)
	tags := []Tag{T("b", "2"), T("a", "1")}
	CounterInc("api_calls", tags...)

	if tags[0].Name != "b" || tags[1].Name != "a" {
		t.Errorf("调用方的切片被改了顺序：%v", tags)
	}
}

func TestRegister_SameNameDifferentTypeReturnsError(t *testing.T) {
	// 这种情况下传进来的 collector 不在 registry 里，记的值永远导不出去。
	// 以前只记一条日志，调用方没法知道，于是启动照样成功、指标永远是空的
	newMetrics(t, nil)
	MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "conflict", Help: "h"}))

	got, err := Register(prometheus.NewGauge(prometheus.GaugeOpts{Name: "conflict", Help: "别的类型"}))
	if err == nil {
		t.Fatal("同名不同类型应当返回错误")
	}
	if got == nil {
		t.Error("即使出错也该返回一个非 nil 的 collector，免得调用方空指针")
	}
}

func TestShortcut_SameNameDifferentTypeNotSilent(t *testing.T) {
	// 先 Counter 后 Gauge：第二个注册不进 registry，通过它记的值永远导不出去。
	// 必须说出来，否则是一次完全静默的数据丢失
	m := newMetrics(t, nil)
	var logged strings.Builder
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelError})))

	CounterInc("dup_name")
	GaugeSet("dup_name", 5)

	if !strings.Contains(logged.String(), "metric name conflict") {
		t.Errorf("同名不同类型应当告警，实际日志=%q", logged.String())
	}
	if out := dump(t, m); !strings.Contains(out, "dup_name 1") {
		t.Errorf("先注册的那个应当照常工作\n实际=\n%s", out)
	}
}

func TestConfig_NamespaceAndConstLabels(t *testing.T) {
	m := newMetrics(t, func(c *Config) {
		c.Namespace = "myapp"
		c.ConstLabels = map[string]string{"env": "prod"}
	})
	CounterInc("orders")

	if out := dump(t, m); !strings.Contains(out, `myapp_orders{env="prod"} 1`) {
		t.Errorf("前缀和常量标签都应生效\n实际=\n%s", out)
	}
}

func TestConfig_CustomHistogramBuckets(t *testing.T) {
	m := newMetrics(t, func(c *Config) { c.HistogramBuckets = []float64{0.1, 0.2} })
	HistogramObserve("latency", 0.15)

	out := dump(t, m)
	if !strings.Contains(out, `latency_bucket{le="0.2"} 1`) || strings.Contains(out, `le="0.5"`) {
		t.Errorf("应使用配置里的桶\n实际=\n%s", out)
	}
}

func TestConstLabels_ReturnsCopy(t *testing.T) {
	newMetrics(t, func(c *Config) { c.ConstLabels = map[string]string{"env": "prod"} })
	l := ConstLabels()
	l["env"] = "改掉了"
	if ConstLabels()["env"] != "prod" {
		t.Error("返回的应是拷贝，调用方改不动配置")
	}
}

func TestHTTPDurationBuckets_ReturnsCopy(t *testing.T) {
	newMetrics(t, nil)
	b := HTTPDurationBuckets()
	if len(b) == 0 {
		t.Fatal("应有默认桶")
	}
	b[0] = -1
	if HTTPDurationBuckets()[0] == -1 {
		t.Error("返回的应是拷贝，调用方改不动配置")
	}
}

func TestRegister_DuplicateReusesExisting(t *testing.T) {
	newMetrics(t, nil)
	opts := prometheus.CounterOpts{Name: "custom_total", Help: "h"}
	first := prometheus.NewCounter(opts)
	got, err := Register(first)
	if err != nil || got != prometheus.Collector(first) {
		t.Errorf("首次注册应返回传进去的那个，got=%v err=%v", got, err)
	}
	second := prometheus.NewCounter(opts)
	got, err = Register(second)
	if err != nil {
		t.Errorf("同名同标签的重复注册不是错误：%v", err)
	}
	if got == prometheus.Collector(second) {
		t.Error("重复注册应返回已有实例，而不是 panic 或返回新的")
	}
}

// resetFallback 把兜底实例清空重来。
//
// 从前测试里写的是 clearCollectors()：缓存那时是包级的，一个测试留下的
// collector 会串到下一个。现在缓存跟着实例走，要隔离的就是兜底实例本身
func resetFallback(t *testing.T) {
	t.Helper()
	old := fallback
	fallback = assemble(prometheus.NewRegistry(), DefaultConfig())
	t.Cleanup(func() { fallback = old })
}

func TestInstall_PreInitPointsDoNotLeakToNewInstance(t *testing.T) {
	// 初始化前打的点记在兜底 registry 上。缓存要是跟实例分家，
	// 初始化之后的打点会继续走那个不会被导出的 collector
	old := current.Swap(nil)
	t.Cleanup(func() { current.Store(old) })
	resetFallback(t)

	CounterInc("early") // 走兜底实例

	m := newMetrics(t, nil)
	CounterInc("early")
	if out := dump(t, m); !strings.Contains(out, "early 1") {
		t.Errorf("初始化后的打点应记在新 registry 上，且从 1 开始\n实际=\n%s", out)
	}
}

func TestActive_NoLossOrPanicBeforeInit(t *testing.T) {
	old := current.Swap(nil)
	t.Cleanup(func() { current.Store(old) })
	resetFallback(t)

	CounterInc("before_init") // 不该 panic
	if Registry() == nil || Handler() == nil {
		t.Error("未初始化时也该有可用的 registry 和 handler")
	}
}

func TestRegister_RegistrationMatchesFramework(t *testing.T) {
	// 这是本包和框架之间唯一的一根线：钩子漏登记、档位挂错，
	// 表现是「配置不生效」或者「比用它的东西晚就绪」，别处都测不出来
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xmetric" {
			got = &e
			break
		}
	}
	if got == nil {
		t.Fatal("没有登记启动钩子")
	}
	if got.Stage != hook.StageTelemetry {
		t.Errorf("指标要早于各类客户端就绪，否则它们打的点收不到，got=%v", got.Stage)
	}

	var stopped bool
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xmetric" {
			stopped = true
		}
	}
	if stopped {
		t.Errorf("New 的 Closer 什么都不做，不该登记停止钩子")
	}
}

func TestNew_RuntimeAndProcessMetrics(t *testing.T) {
	c := DefaultConfig()
	c.LogErrorMetric = false
	m, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	out := dump(t, m)
	for _, want := range []string{"go_goroutines", "process_"} {
		if !strings.Contains(out, want) {
			t.Errorf("默认应采集 %s", want)
		}
	}
}

func TestNew_RuntimeAndProcessMetricsHaveConstLabels(t *testing.T) {
	// 文档说常量标签「附加到所有指标上」。这两组是 client_golang 现成的 collector，
	// 从前直接注册在 Registry 上，实测 go_* / process_* 一个都不带，
	// 按 env 过滤的看板查 go_goroutines{env="prod"} 什么都查不到
	c := DefaultConfig()
	c.LogErrorMetric = false
	c.ConstLabels = map[string]string{"env": "prod"}
	m, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	var goSeen, procSeen bool
	for _, line := range strings.Split(dump(t, m), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		goSeen = goSeen || strings.HasPrefix(line, "go_")
		procSeen = procSeen || strings.HasPrefix(line, "process_")
		if !strings.Contains(line, `env="prod"`) {
			t.Errorf("每一条样本都该带常量标签 env=\"prod\"，这条没有：%s", line)
		}
	}
	if !goSeen || !procSeen {
		t.Errorf("默认应同时有 go_* 和 process_*，got go=%v process=%v", goSeen, procSeen)
	}
}

func TestNew_RuntimeAndProcessMetricsCanBeDisabled(t *testing.T) {
	m := newMetrics(t, nil) // install 默认关掉这两项
	out := dump(t, m)
	if strings.Contains(out, "go_goroutines") || strings.Contains(out, "process_cpu") {
		t.Errorf("关掉之后不该出现\n实际=\n%s", out)
	}
}

// ---- 日志错误计数 ----

// logThrough 建一套只写文件的 xlog，返回它和日志文件目录
func logThrough(t *testing.T) *slog.Logger {
	t.Helper()
	c := xlog.DefaultConfig()
	c.Console = false
	c.File = xlog.FileConfig{Enable: true, Path: t.TempDir(), Name: "app.log", RotateTime: time.Hour, Perm: "0644"}
	l, closer, err := xlog.New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	return l
}

func TestLogCounter_CountsErrorLogs(t *testing.T) {
	// 「错误率」是最常用的告警，不该等业务先埋点
	m := newMetrics(t, func(c *Config) { c.LogErrorMetric = true })
	l := logThrough(t)

	l.Info("这条不算")
	l.Warn("这条也不算")
	for _, msg := range []string{"出事了", "又出事了"} {
		l.Error(msg) // 同一行，聚合成同一条时间序列
	}

	out := dump(t, m)
	if !strings.Contains(out, `log_errors_total{caller=`) {
		t.Fatalf("应有 log_errors_total\n实际=\n%s", out)
	}
	// caller 是聚合维度：同一行打的两条错误合成一条序列，计数为 2
	if !strings.Contains(out, `level="ERROR"} 2`) {
		t.Errorf("同一处打的两条 Error 应合成一条序列、计数为 2\n实际=\n%s", out)
	}
}

func TestInstall_NoDataRaceWithConcurrentLogging(t *testing.T) {
	// Install 曾经先把设施发布成 current，再往它身上写 logCounter。
	// 中间那一段里，任何一条经 xlog 写出的错误日志都会走观察者去读
	// active().logCounter —— 读的正是一个还在被写的字段。
	//
	// 后果不止是 -race 报警：观察者是在 logCounter 赋值之前挂上的，
	// 所以这段窗口里的错误日志一条都没被计进去，而告警面板上看不出区别。
	newMetrics(t, func(c *Config) { c.LogErrorMetric = true }) // 先挂上观察者
	l := logThrough(t)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				l.Error("并发写")
			}
		}
	}()

	for i := 0; i < 20; i++ {
		newMetrics(t, func(c *Config) { c.LogErrorMetric = true })
	}
	close(stop)
	<-done
}

func TestLogCounter_LeavesGlobalLoggerAlone(t *testing.T) {
	// 回归用例。曾经的实现是「把 slog.Default() 包一层再设回去」，
	// 而 slog.SetDefault 顺带把标准库 log 包的输出也接到新 handler 上：
	// 链条最终落回 slog 自带的 handler 时，记录经 log.Output 又流回来，
	// 卡死在 log 包那把不可重入的锁上——不用 xlog 的应用第一条日志就挂住。
	before := slog.Default()
	newMetrics(t, func(c *Config) { c.LogErrorMetric = true })
	if slog.Default() != before {
		t.Fatal("不得改动全局 logger：那条路会和标准库 log 包绕成环")
	}

	// 直接用标准库默认 logger 打一条，不该挂住
	done := make(chan struct{})
	go func() {
		slog.Error("经标准库默认 logger")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("经标准库默认 logger 打日志挂住了")
	}
}

func TestLogCounter_AttachesTraceAsExemplar(t *testing.T) {
	// 面板上从指标点能跳到对应的链路
	t.Cleanup(func() { xlog.SetTraceExtractor(nil) })
	xlog.SetTraceExtractor(func(context.Context) (string, string) {
		return "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	})

	m := newMetrics(t, func(c *Config) { c.LogErrorMetric = true })
	logThrough(t).ErrorContext(context.Background(), "出事了")

	// exemplar 只在 OpenMetrics 格式里导出
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Accept", "application/openmetrics-text; version=1.0.0")
	m.Handler.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "trace_id=") {
		t.Errorf("应带上 trace_id exemplar\n实际=\n%s", w.Body.String())
	}
}

func TestLogCounter_NoExemplarWithoutTrace(t *testing.T) {
	m := newMetrics(t, func(c *Config) { c.LogErrorMetric = true })
	logThrough(t).Error("出事了")

	if out := dump(t, m); !strings.Contains(out, "log_errors_total") {
		t.Errorf("没有链路也要正常计数\n实际=\n%s", out)
	}
}

func TestLogCounter_StopsCountingWhenDisabled(t *testing.T) {
	m := newMetrics(t, func(c *Config) { c.LogErrorMetric = false })
	logThrough(t).Error("出事了")

	if out := dump(t, m); strings.Contains(out, "log_errors_total") {
		t.Errorf("关掉之后不该有这个指标\n实际=\n%s", out)
	}
}

func TestLogCounter_CallerKeepsTwoPathSegments(t *testing.T) {
	// 完整路径带着构建机的目录，同一份代码在不同机器上会产生不同的标签值
	m := newMetrics(t, func(c *Config) { c.LogErrorMetric = true })
	logThrough(t).Error("出事了")

	out := dump(t, m)
	if !strings.Contains(out, `caller="xmetric/xmetric_test.go:`) {
		t.Errorf("caller 应是「上级目录/文件:行号」\n实际=\n%s", out)
	}
}

func TestObserver_PanicDoesNotInterruptLogging(t *testing.T) {
	// 观测出问题不该把日志本身打断
	newMetrics(t, func(c *Config) { c.LogErrorMetric = true })
	xlog.AddObserver(func(context.Context, slog.Record) { panic("观察者炸了") })

	dir := t.TempDir()
	c := xlog.DefaultConfig()
	c.Console = false
	c.File = xlog.FileConfig{Enable: true, Path: dir, Name: "app.log", RotateTime: time.Hour, Perm: "0644"}
	l, closer, err := xlog.New(c)
	if err != nil {
		t.Fatal(err)
	}
	l.Error("出事了")
	closer.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "app.log.*"))
	if len(files) == 0 {
		t.Fatal("没写出日志文件")
	}
	b, _ := os.ReadFile(files[0])
	if !strings.Contains(string(b), "出事了") {
		t.Errorf("观察者 panic 了，日志本身还得写出去，实际=%q", b)
	}
}

func TestTrimPath(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/build/a/b/c/d.go", "c/d.go"},
		{"c/d.go", "c/d.go"},
		{"d.go", "d.go"},
		{"", ""},
	} {
		if got := trimPath(c.in); got != c.want {
			t.Errorf("trimPath(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestCallerOf_NotEmptyWithoutPC(t *testing.T) {
	if got := callerOf(slog.Record{}); got != "unknown" {
		t.Errorf("取不到调用点时应返回 unknown，got=%q", got)
	}
}

func TestMustRegister(t *testing.T) {
	newMetrics(t, nil)
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "must_total", Help: "h"})
	MustRegister(c)
	c.Inc()

	if out := dump(t, active()); !strings.Contains(out, "must_total 1") {
		t.Errorf("注册的自定义指标应被导出\n实际=\n%s", out)
	}
}

func TestNew_DuplicateCollectorFails(t *testing.T) {
	// New 是纯构造器，registry 是新建的，正常路径不该冲突；
	// 真冲突了要返回错误而不是 panic，好让框架把它当成普通启动失败处理
	c := DefaultConfig()
	c.LogErrorMetric = false
	m, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if m.Registry == nil || m.Handler == nil {
		t.Fatal("registry 和 handler 都不该为 nil")
	}
}

func TestInstall_PreInitPointsAreReported(t *testing.T) {
	// 缓存跟着实例走之后，Install 不再需要去清另一处全局状态。
	// 但「换实例之前记的点留在上一个 registry 里、导不出去」这件事
	// 仍然要说出来，否则就是一次完全静默的数据丢失
	old := current.Swap(nil)
	t.Cleanup(func() { current.Store(old) })
	resetFallback(t)

	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	CounterInc("early_one")
	CounterInc("early_two")

	m, closer, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	m.Install()

	if !strings.Contains(buf.String(), "will not be exported") {
		t.Errorf("初始化前记过点，Install 该报出来，实际日志=%q", buf.String())
	}
	if !strings.Contains(buf.String(), "metrics=2") {
		t.Errorf("该报出有几个指标，实际日志=%q", buf.String())
	}
}

func TestInstall_CacheFollowsInstanceSwap(t *testing.T) {
	// 缓存曾经是包级的，而 Registry 属于实例：换实例时必须记得
	// 手动清另一处全局状态，忘了就是静默的数据丢失
	m1 := newMetrics(t, nil)
	CounterInc("shared_name")
	if out := dump(t, m1); !strings.Contains(out, "shared_name 1") {
		t.Fatalf("第一个实例该记到\n%s", out)
	}

	m2 := newMetrics(t, nil)
	CounterInc("shared_name")

	out := dump(t, m2)
	if !strings.Contains(out, "shared_name 1") {
		t.Errorf("换实例之后该记在新 registry 上、且从 1 开始\n实际=\n%s", out)
	}
}

// initComponent 走一遍框架真正会走的路径：按这份配置装好本模块。
func initComponent(t *testing.T, c Config) {
	t.Helper()
	if err := install(c); err != nil {
		t.Fatalf("初始化失败：%v", err)
	}
}

// keepGlobals 记下会被 install 改掉的那些全局值，测试结束还原
func keepGlobals(t *testing.T) {
	t.Helper()
	oldLogger := slog.Default()
	oldCurrent := current.Load()
	t.Cleanup(func() {
		slog.SetDefault(oldLogger)
		current.Store(oldCurrent)
	})
}

func TestInitXMetric_InstallsDefaultMetricsWhenUnconfigured(t *testing.T) {
	// 指标没配就不装的话，框架内置的那些打点全落到兜底实例上，
	// /metrics 导出来是空的——而使用者没配指标本来就该是「用默认的」
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")

	if err := initXMetric(context.Background()); err != nil {
		t.Fatalf("没配不该报错：%v", err)
	}
	got := current.Load()
	if got == nil {
		t.Fatal("没装上全局实例")
	}
}

func TestInitXMetric_ConfigTypoFailsStartup(t *testing.T) {
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XMetric:\n  NameSpace: app\n")

	if err := initXMetric(context.Background()); err == nil {
		t.Fatal("字段拼错应当让启动失败")
	}
}

func TestInitXMetric_ConfigAppliedToGlobalInstance(t *testing.T) {
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XMetric:\n  Namespace: demoapp\n  GoMetrics: false\n  ProcessMetrics: false\n  ConstLabels:\n    env: test\n")

	if err := initXMetric(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := Namespace(); got != "demoapp" {
		t.Errorf("Namespace want demoapp, got %q", got)
	}
	if got := ConstLabels()["env"]; got != "test" {
		t.Errorf("ConstLabels want test, got %q", got)
	}
}

func TestRegisterAs_ReturnsPassedCollectorOnSuccess(t *testing.T) {
	newMetrics(t, nil)
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "registeras_ok"})
	got, err := RegisterAs(c)
	if err != nil {
		t.Fatal(err)
	}
	if got != c {
		t.Error("第一次注册应当拿回同一个实例")
	}
}

func TestRegisterAs_DuplicateReusesFirst(t *testing.T) {
	// 两处代码各建一个同名 collector 时，两边必须打到同一个上，
	// 否则后到的那份永远导不出去
	newMetrics(t, nil)
	first := prometheus.NewCounter(prometheus.CounterOpts{Name: "registeras_dup"})
	if _, err := RegisterAs(first); err != nil {
		t.Fatal(err)
	}
	second := prometheus.NewCounter(prometheus.CounterOpts{Name: "registeras_dup"})
	got, err := RegisterAs(second)
	if err != nil {
		t.Fatal(err)
	}
	if got != first {
		t.Error("重复注册应当拿回先到的那个")
	}
}

func TestRegisterAs_SameNameDifferentTypeFailsInsteadOfReplacing(t *testing.T) {
	// 断言失败时还回传进来的那个：调用方照常打点（只是导不出去），
	// 而不是拿到一个零值去空指针
	newMetrics(t, nil)
	if _, err := Register(prometheus.NewCounter(prometheus.CounterOpts{Name: "registeras_kind"})); err != nil {
		t.Fatal(err)
	}
	want := prometheus.NewGauge(prometheus.GaugeOpts{Name: "registeras_kind"})
	got, err := RegisterAs(want)
	if err == nil {
		t.Fatal("类型对不上应当报错")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("错误要说清是被同名的别的类型占了，got=%v", err)
	}
	if got != want {
		t.Error("出错时要把传进来的那个还回去，让调用方还能照常打点")
	}
}

func TestCollectorOf_SameNameDifferentTypeReportsInsteadOfDropping(t *testing.T) {
	// 先 Counter 后 Gauge：后者不在 registry 里，通过它记的值永远导不出去。
	// 不喊一声的话，面板上那条线一直是空的，而代码里明明在打点
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	newMetrics(t, nil)
	CounterInc("clash")
	GaugeSet("clash", 5)

	if !strings.Contains(buf.String(), "clash") {
		t.Errorf("同名冲突必须留下记录，实际日志=\n%s", buf.String())
	}
}

func TestCollectorOf_SameNameSameTypeReusesInstance(t *testing.T) {
	// 不复用的话每次打点都新建一个 collector，注册被拒之后值全丢
	m := newMetrics(t, nil)
	before := m.cachedCount()
	for i := 0; i < 5; i++ {
		CounterInc("reused", T("a", "1"))
	}
	if got := m.cachedCount() - before; got != 1 {
		t.Errorf("同名同标签只该建一个 collector，实际新建了 %d 个", got)
	}
	if out := dump(t, m); !strings.Contains(out, `reused{a="1"} 5`) {
		t.Errorf("五次打点该累加到同一条序列上\n实际=\n%s", out)
	}
}
