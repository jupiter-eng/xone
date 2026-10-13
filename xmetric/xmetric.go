package xmetric

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	promcollectors "github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
)

// Metrics 一份配置装配出来的指标设施。
//
// New 只构造、不安装；装成进程级的是 Install 的事。
// 分开是为了让测试能拿到一套独立的 Registry，而不必动全局状态。
type Metrics struct {
	// Registry 原生的 Prometheus Registry，需要完整控制时直接用它
	Registry *prometheus.Registry

	// Handler /metrics 的 HTTP handler
	Handler http.Handler

	cfg Config

	// logCounter 日志错误计数器，未开启该特性时为 nil
	logCounter *prometheus.CounterVec

	// collectors 快捷方法（CounterInc / GaugeSet / …）建过的 collector，
	// 按「类型 + 指标名 + 标签名」缓存，见 shortcut.go 的说明。
	//
	// 跟着实例走而不是做成包级的：collector 是注册在本实例的 Registry 上的，
	// 换了实例，旧的那些就再也导不出去了。缓存是包级的时候，
	// Install 必须记得去清另一处的全局状态，忘了就是静默的数据丢失。
	collectors sync.Map   // map[string]prometheus.Collector
	createMu   sync.Mutex // 保证「创建 + 注册 + 入缓存」是一步
}

// New 按配置构造指标设施，不触碰任何全局变量。
//
// 成功时返回的 io.Closer 永不为 nil（目前什么都不做，留着是为了签名与其它
// 集成一致）；出错时三个返回值里只有 error 有意义。
//
// 桶为 nil 的补上默认值，见 Config.withDefaults。
func New(cfg Config) (*Metrics, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, xerror.Newf("xmetric", "config", "invalid config: %w", err)
	}
	cfg = cfg.withDefaults()
	reg := prometheus.NewRegistry()

	// 框架自带的和快捷方法建的指标都在各自的 Opts.ConstLabels 里填了常量标签，
	// 这两组是 client_golang 现成的 collector，没有地方填，只能在注册这一侧包一层。
	// 不包的话实测 go_* / process_* 全都没有常量标签，按 env 过滤的看板查不到它们
	withLabels := prometheus.WrapRegistererWith(cfg.ConstLabels, reg)
	if cfg.GoMetrics {
		if err := withLabels.Register(promcollectors.NewGoCollector()); err != nil {
			return nil, nil, xerror.Newf("xmetric", "register", "register Go runtime metrics: %w", err)
		}
	}
	if cfg.ProcessMetrics {
		if err := withLabels.Register(promcollectors.NewProcessCollector(promcollectors.ProcessCollectorOpts{})); err != nil {
			return nil, nil, xerror.Newf("xmetric", "register", "register process metrics: %w", err)
		}
	}

	return assemble(reg, cfg), noopCloser{}, nil
}

// Install 把这套设施装成进程级的：此后快捷方法和 Handler() 都走它。
//
// 开启 LogErrorMetric 时还会把当前的 slog 默认 logger 包一层，
// 让 Error 及以上级别的日志自动计入 log_errors_total。
// 计数走的是 xlog 的观察者（internal/logext），所以它只统计经 xlog 写出去的日志：
// 自己另起一套 slog handler 的话，这个指标是空的。
func (m *Metrics) Install() {
	// 先把 logCounter 填好，最后才发布 —— 顺序反过来的话，current 已经指向
	// 本实例、而 logCounter 还在被写，这中间每一条错误日志都在读一个
	// 正在被写的字段。不只是 -race 会报：观察者是在赋值之前就挂上的，
	// 所以那段窗口里的错误日志一条都计不进去，面板上看不出任何区别。
	//
	// 发布是一次原子写，读的一侧 active() 是原子读，赋值因此对读者可见。
	if m.cfg.LogErrorMetric {
		m.logCounter = newLogCounter(m)
	}

	prev := current.Swap(m)
	if prev == nil {
		prev = fallback // 之前的打点都落在兜底实例上
	}

	// 换实例之前记的点是记在上一个 Registry 上的，不会出现在 /metrics 里。
	// 缓存已经跟着实例走，不需要清任何东西——但这件事仍然要说出来，
	// 否则就是一次完全静默的数据丢失。
	if prev != m {
		if n := prev.cachedCount(); n > 0 {
			slog.Warn("metrics were recorded before xmetric was initialized; those values went to a temporary registry and will not be exported",
				"metrics", n)
		}
	}
}

// ---- 全局状态 ----

var (
	// current 生效中的设施，由 Install 发布。每次打点都要读它，所以用原子指针而不是锁
	current atomic.Pointer[Metrics]

	// fallback 未 Install 时用的兜底设施。
	//
	// 打点代码可能在初始化之前就跑起来（比如另一个组件 Init 里打的点），
	// 那时既不该 panic 也不该把数据丢进空气里。它不采集 Go / 进程指标——
	// 那些由真正的实例负责，这里只是给早到的打点一个不会丢的落点。
	// 包变量直接建好：一个空 Registry，没有会失败的步骤，也不读任何配置。
	fallback = assemble(prometheus.NewRegistry(), DefaultConfig())
)

// assemble 把 Registry 和配置装成一套设施
func assemble(reg *prometheus.Registry, cfg Config) *Metrics {
	return &Metrics{
		Registry: reg,
		Handler:  promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}),
		cfg:      cfg,
	}
}

// active 返回当前生效的设施，未 Install 时返回兜底实例
func active() *Metrics {
	if m := current.Load(); m != nil {
		return m
	}
	return fallback
}

// Registry 返回当前生效的 Prometheus Registry
func Registry() *prometheus.Registry { return active().Registry }

// Handler 返回 /metrics 的 HTTP handler
func Handler() http.Handler { return active().Handler }

// MustRegister 把自定义 collector 注册到当前 Registry，重复注册会 panic
func MustRegister(cs ...prometheus.Collector) { Registry().MustRegister(cs...) }

// Register 注册 collector，已注册过同名同标签的则复用已有实例而不是 panic。
//
// 返回实际生效的那个 collector——可能不是传进来的这个，务必用返回值。
//
// 同名但类型或标签不同时返回错误。那种情况下传进来的这个 collector
// 不在 registry 里，通过它记的值永远导不出去；调用方要么让启动失败，
// 要么至少知道自己在往空气里写。
func Register(c prometheus.Collector) (prometheus.Collector, error) {
	return register(Registry(), c)
}

// ConstLabels 返回配置的全局常量标签（拷贝）。
//
// 给自己建指标的包用：把它填进 prometheus.Opts.ConstLabels，
// 你的指标就和框架内置指标带上同样的环境/集群标签。
func ConstLabels() prometheus.Labels { return maps.Clone(active().cfg.ConstLabels) }

// HTTPDurationBuckets 返回 HTTP 耗时直方图的桶边界（拷贝）
func HTTPDurationBuckets() []float64 { return slices.Clone(active().cfg.HTTPDurationBuckets) }

// Namespace 返回配置的指标名前缀
func Namespace() string { return active().cfg.Namespace }

// register 注册 collector，重复注册时复用已有实例
func register(reg *prometheus.Registry, c prometheus.Collector) (prometheus.Collector, error) {
	err := reg.Register(c)
	if err == nil {
		return c, nil
	}
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		return are.ExistingCollector, nil
	}
	// 同名不同标签之类的冲突：这个 collector 不在 registry 里，
	// 通过它记的值永远导不出去
	return c, xerror.Newf("xmetric", "register", "metric registration failed, values recorded through it will not be exported: %w", err)
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// ---- 登记 ----

// init 指标要早于各类客户端就绪，否则它们打的点收不到
func init() {
	xhook.BeforeStart(initXMetric, xhook.At(xhook.StageTelemetry))
}

// initXMetric 读配置，然后装好指标设施
func initXMetric(context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return err
	}
	return install(c)
}

// install 按配置建好指标设施并装成全局默认实例
func install(c Config) error {
	// 不登记停止钩子：New 返回的 Closer 目前什么都不做（见 New），
	// 为调一个空操作去存一个全局、挂一个钩子不值得。哪天它真要收尾了再加回来
	m, _, err := New(c)
	if err != nil {
		return err
	}
	m.Install()
	return nil
}

// RegisterAs 注册 c 并把实际生效的那个断言回 T。
//
// 比 Register 好用的地方：调用方几乎总是需要具体类型（*CounterVec 之类）
// 才能打点，而 Register 返回的是接口，每个调用点都要重复一遍
// 「注册 → 判错 → 类型断言 → 断言失败怎么办」。
//
//	total, err := xmetric.RegisterAs(prometheus.NewCounterVec(opts, labels))
//
// 出错时返回传进来的那个（可以照常打点，只是导不出去），
// 调用方据此决定是让启动失败还是记一条日志继续。
func RegisterAs[T prometheus.Collector](c T) (T, error) {
	registered, err := Register(c)
	if err != nil {
		return c, err
	}
	typed, ok := registered.(T)
	if !ok {
		return c, xerror.Newf("xmetric", "register", "metric name is already registered as %T, values recorded through it will not be exported", registered)
	}
	return typed, nil
}
