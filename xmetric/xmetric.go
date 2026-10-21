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

// Metrics is the metrics setup assembled from one configuration.
//
// New only constructs it and does not install it; making it process-wide is Install's job.
// They are separate so a test can get a Registry of its own without touching global state.
type Metrics struct {
	// Registry is the native Prometheus Registry, used directly when full control is needed.
	Registry *prometheus.Registry

	// Handler is the HTTP handler for /metrics.
	Handler http.Handler

	cfg Config

	// logCounter is the log error counter, nil when that feature is off.
	logCounter *prometheus.CounterVec

	// collectors are the collectors the shortcuts (CounterInc / GaugeSet / ...) have built,
	// cached by "type + metric name + label names"; see shortcut.go for the details.
	//
	// It follows the instance rather than being package-level: a collector is registered on this instance's Registry,
	// and once the instance is swapped the old ones can never be exported again. With a package-level cache, Install
	// would have to remember to clear that other piece of global state, and forgetting is a silent loss of data.
	collectors sync.Map   // map[string]prometheus.Collector
	createMu   sync.Mutex // keeps "create + register + cache" a single step
}

// New builds the metrics setup from the configuration and touches no globals.
//
// On success the io.Closer returned is never nil (it currently does nothing and exists to keep the signature
// consistent with the other integrations); on failure only the error of the three return values is meaningful.
//
// Buckets that are nil get the defaults; see Config.withDefaults.
func New(cfg Config) (*Metrics, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, xerror.Newf("xmetric", "config", "invalid config: %w", err)
	}
	cfg = cfg.withDefaults()
	reg := prometheus.NewRegistry()

	// The framework's own metrics and those built by the shortcuts fill the constant labels into their own
	// Opts.ConstLabels. These two groups are ready-made collectors from client_golang with nowhere to fill them in, so
	// they are wrapped on the registration side. Measured without that wrapping, go_* / process_* carried no constant labels at all and a dashboard filtering by env found none of them.
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

// Install makes this setup the process-wide one: from then on the shortcuts and Handler() go through it.
//
// With LogErrorMetric on it also wraps the current slog default logger, so logs at Error level and above are counted
// into log_errors_total automatically.
// The counting goes through xlog's observer (internal/logext), so it only counts logs written through xlog:
// with a separate slog handler of your own, this metric stays empty.
func (m *Metrics) Install() {
	// logCounter is filled in first and published last -- the other way round, current would already point at this
	// instance while logCounter was still being written, and every error log in between would read a field being
	// written. It is not only -race that would report it: the observer is attached before the assignment, so not one
	// error log in that window would be counted, and nothing on the dashboard would look any different.
	//
	// Publishing is one atomic write and the reading side active() is an atomic read, so the assignment is visible to readers.
	if m.cfg.LogErrorMetric {
		m.logCounter = newLogCounter(m)
	}

	prev := current.Swap(m)
	if prev == nil {
		prev = fallback // everything recorded so far landed on the fallback instance
	}

	// What was recorded before the instance was swapped is on the previous Registry and does not appear in /metrics.
	// The cache already follows the instance, so nothing has to be cleared -- but this still has to be said out loud,
	// or it is a completely silent loss of data.
	if prev != m {
		if n := prev.cachedCount(); n > 0 {
			slog.Warn("metrics were recorded before xmetric was initialized; those values went to a temporary registry and will not be exported",
				"metrics", n)
		}
	}
}

// ---- global state ----

var (
	// current is the setup in effect, published by Install. Every recording reads it, so it is an atomic pointer rather than a lock.
	current atomic.Pointer[Metrics]

	// fallback is the setup used before Install.
	//
	// Recording code may run before initialization (a recording inside another component's Init, say), and at that
	// point it must neither panic nor throw the data away. It collects no Go or process metrics -- those are the real
	// instance's job, and this is only somewhere for early recordings to land without being lost.
	// It is built directly as a package variable: an empty Registry, with no step that can fail and no configuration read.
	fallback = assemble(prometheus.NewRegistry(), DefaultConfig())
)

// assemble turns a Registry and a configuration into one setup.
func assemble(reg *prometheus.Registry, cfg Config) *Metrics {
	return &Metrics{
		Registry: reg,
		Handler:  promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}),
		cfg:      cfg,
	}
}

// active returns the setup in effect, or the fallback instance before Install.
func active() *Metrics {
	if m := current.Load(); m != nil {
		return m
	}
	return fallback
}

// Registry returns the Prometheus Registry currently in effect.
func Registry() *prometheus.Registry { return active().Registry }

// Handler returns the HTTP handler for /metrics.
func Handler() http.Handler { return active().Handler }

// MustRegister registers a custom collector on the current Registry; registering the same one twice panics.
func MustRegister(cs ...prometheus.Collector) { Registry().MustRegister(cs...) }

// Register registers a collector, reusing an existing instance with the same name and labels instead of panicking.
//
// It returns the collector actually in effect -- which may not be the one passed in, so always use the return value.
//
// A same-named collector with a different type or different labels returns an error. The collector passed in is then
// not in the registry and the values recorded through it can never be exported; the caller either fails startup or at
// least knows it is writing into thin air.
func Register(c prometheus.Collector) (prometheus.Collector, error) {
	return register(Registry(), c)
}

// ConstLabels returns the configured global constant labels (a copy).
//
// It is for packages building metrics of their own: filling it into prometheus.Opts.ConstLabels gives those metrics
// the same environment and cluster labels as the framework's built-in ones.
func ConstLabels() prometheus.Labels { return maps.Clone(active().cfg.ConstLabels) }

// HTTPDurationBuckets returns the bucket boundaries of the HTTP duration histograms (a copy).
func HTTPDurationBuckets() []float64 { return slices.Clone(active().cfg.HTTPDurationBuckets) }

// Namespace returns the configured metric name prefix.
func Namespace() string { return active().cfg.Namespace }

// register registers a collector, reusing an existing instance when it is already registered.
func register(reg *prometheus.Registry, c prometheus.Collector) (prometheus.Collector, error) {
	err := reg.Register(c)
	if err == nil {
		return c, nil
	}
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		return are.ExistingCollector, nil
	}
	// A conflict such as the same name with different labels: this collector is not in the registry and the values
	// recorded through it can never be exported.
	return c, xerror.Newf("xmetric", "register", "metric registration failed, values recorded through it will not be exported: %w", err)
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// ---- registration ----

// init puts metrics before the clients are ready, or the points they record are not collected.
func init() {
	xhook.BeforeStart(initXMetric, xhook.At(xhook.StageTelemetry))
}

// initXMetric reads the configuration and then assembles the metrics setup.
func initXMetric(context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return err
	}
	return install(c)
}

// install builds the metrics setup from the configuration and makes it the global default instance.
func install(c Config) error {
	// No stop hook is registered: the Closer New returns currently does nothing (see New), and storing a global and
	// attaching a hook to call a no-op is not worth it. It can come back if it ever has cleanup to do.
	m, _, err := New(c)
	if err != nil {
		return err
	}
	m.Install()
	return nil
}

// RegisterAs registers c and asserts the one actually in effect back to T.
//
// What it gives over Register: a caller almost always needs the concrete type (a *CounterVec, say) in order to record,
// while Register returns an interface, so every call site would repeat "register, check the error, type-assert, decide
// what to do when the assertion fails".
//
//	total, err := xmetric.RegisterAs(prometheus.NewCounterVec(opts, labels))
//
// On an error it returns the one passed in (which can be recorded to as usual, it just cannot be exported), so the
// caller decides whether to fail startup or log a line and carry on.
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
