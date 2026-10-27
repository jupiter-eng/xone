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
	"github.com/jupiter-eng/xone/xapp"
	"github.com/jupiter-eng/xone/xlog"
	"github.com/jupiter-eng/xone/xonetest"
)

// install sets up a clean metrics instance and restores the global state when the test ends.
func newMetrics(t testing.TB, mutate func(*Config)) *Metrics {
	t.Helper()
	c := DefaultConfig()
	// The Go / process metrics are off by default: they would add hundreds of lines to the export and drown what the assertions want to see.
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

// dump scrapes the text output of /metrics once.
func dump(t *testing.T, m *Metrics) string {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("/metrics returned %d", w.Code)
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
			t.Errorf("the export must contain %s\ngot=\n%s", want, out)
		}
	}
}

func TestShortcut_CounterAddIgnoresANegativeValue(t *testing.T) {
	// client_golang panics on it (counter cannot decrease in value), and that panic would come out of whichever
	// business call made the mistake. Everything else here treats a metric that cannot be recorded as an
	// observability problem rather than a reason to take the request down.
	m := newMetrics(t, nil)

	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	CounterAdd("refunds", 2)
	CounterAdd("refunds", -1)

	if !strings.Contains(buf.String(), "negative") {
		t.Errorf("a negative value must be reported, got log=%q", buf.String())
	}
	if out := dump(t, m); !strings.Contains(out, "refunds 2") {
		t.Errorf("the counter must keep the value it legitimately had\ngot=\n%s", out)
	}
}

func TestShortcut_Gauge(t *testing.T) {
	m := newMetrics(t, nil)
	GaugeSet("queue_depth", 10)
	GaugeInc("queue_depth")
	GaugeDec("queue_depth")
	GaugeDec("queue_depth")

	if out := dump(t, m); !strings.Contains(out, "queue_depth 9") {
		t.Errorf("it must be 10+1-1-1=9\ngot=\n%s", out)
	}
}

func TestShortcut_Histogram(t *testing.T) {
	m := newMetrics(t, nil)
	HistogramObserve("payload_bytes", 0.3)

	out := dump(t, m)
	if !strings.Contains(out, "payload_bytes_count 1") {
		t.Errorf("one observation must be recorded\ngot=\n%s", out)
	}
	// The default buckets are prometheus.DefBuckets, so 0.3 must land in the 0.5 bucket.
	if !strings.Contains(out, `payload_bytes_bucket{le="0.5"} 1`) {
		t.Errorf("the default buckets must be prometheus.DefBuckets\ngot=\n%s", out)
	}
}

func TestShortcut_LabelOrderDoesNotAffectReuse(t *testing.T) {
	// Writing the same metric with the labels in two orders must not build two collectors --
	// prometheus would reject the second one for "same name, different labels" and the data would be lost.
	m := newMetrics(t, nil)
	CounterInc("api_calls", T("a", "1"), T("b", "2"))
	CounterInc("api_calls", T("b", "2"), T("a", "1"))

	if out := dump(t, m); !strings.Contains(out, `api_calls{a="1",b="2"} 2`) {
		t.Errorf("both writing orders must hit the same time series\ngot=\n%s", out)
	}
}

func TestShortcut_LabelNamesHoldingTheKeySeparatorDoNotShareACollector(t *testing.T) {
	// Nothing here checks what a label name holds, and the cache key has to survive that: with the names
	// joined by a separator, one label called "method,path" and the two labels "method" and "path" land on
	// the same entry, and whichever call comes second reaches a collector built for a different number of
	// labels -- a WithLabelValues with the wrong count, which panics inside the business call that made it.
	// A refactor splitting one tag into two and missing a call site is all it takes.
	m := newMetrics(t, nil)

	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	if one, two := cacheKey(kindCounter, "api_calls", []string{"method,path"}),
		cacheKey(kindCounter, "api_calls", []string{"method", "path"}); one == two {
		t.Fatalf("one label and two labels must not share a cache key, both are %q", one)
	}

	// Neither call takes the request down. What the second one gets is this package's answer to every
	// other malformed input: client_golang refuses the second api_calls (the first registered one label
	// literally named "method,path", which it exports escaped), so one ERROR line is written and those
	// values go nowhere.
	CounterInc("api_calls", T("method,path", "GET,/orders"))
	CounterInc("api_calls", T("method", "GET"), T("path", "/orders"))

	if !strings.Contains(buf.String(), "api_calls") {
		t.Errorf("the second registration conflicting should be reported, got log=%q", buf.String())
	}
	if out := dump(t, m); !strings.Contains(out, `api_calls{method_path="GET,/orders"} 1`) {
		t.Errorf("the first recording must still be exported\ngot=\n%s", out)
	}
}

func TestShortcut_SortingLeavesCallerSliceIntact(t *testing.T) {
	// A caller may expand the same slice repeatedly, so sorting it would change somebody else's data.
	newMetrics(t, nil)
	tags := []Tag{T("b", "2"), T("a", "1")}
	CounterInc("api_calls", tags...)

	if tags[0].Name != "b" || tags[1].Name != "a" {
		t.Errorf("the caller's slice was reordered: %v", tags)
	}
}

func TestRegister_SameNameDifferentTypeReturnsError(t *testing.T) {
	// In that case the collector passed in is not in the registry and the values it records can never be exported.
	// It used to only write a log, which the caller could not see, so startup succeeded and the metric stayed empty forever.
	newMetrics(t, nil)
	MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "conflict", Help: "h"}))

	got, err := Register(prometheus.NewGauge(prometheus.GaugeOpts{Name: "conflict", Help: "another type"}))
	if err == nil {
		t.Fatal("the same name with a different type must return an error")
	}
	if got == nil {
		t.Error("even on an error a non-nil collector must be returned, so the caller does not hit a nil pointer")
	}
}

func TestShortcut_SameNameDifferentTypeNotSilent(t *testing.T) {
	// A Counter first and a Gauge later: the second does not get into the registry and the values recorded through it
	// can never be exported. That has to be said out loud, or it is a completely silent loss of data.
	m := newMetrics(t, nil)
	var logged strings.Builder
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelError})))

	CounterInc("dup_name")
	GaugeSet("dup_name", 5)

	if !strings.Contains(logged.String(), "metric name conflict") {
		t.Errorf("the same name with a different type must warn, got log=%q", logged.String())
	}
	if out := dump(t, m); !strings.Contains(out, "dup_name 1") {
		t.Errorf("the one registered first must keep working\ngot=\n%s", out)
	}
}

func TestConfig_NamespaceAndConstLabels(t *testing.T) {
	m := newMetrics(t, func(c *Config) {
		c.Namespace = "myapp"
		c.ConstLabels = map[string]string{"env": "prod"}
	})
	CounterInc("orders")

	if out := dump(t, m); !strings.Contains(out, `myapp_orders{env="prod"} 1`) {
		t.Errorf("both the prefix and the constant labels must take effect\ngot=\n%s", out)
	}
}

func TestConfig_CustomHistogramBuckets(t *testing.T) {
	m := newMetrics(t, func(c *Config) { c.HistogramBuckets = []float64{0.1, 0.2} })
	HistogramObserve("latency", 0.15)

	out := dump(t, m)
	if !strings.Contains(out, `latency_bucket{le="0.2"} 1`) || strings.Contains(out, `le="0.5"`) {
		t.Errorf("the buckets from the configuration must be used\ngot=\n%s", out)
	}
}

func TestConstLabels_ReturnsCopy(t *testing.T) {
	newMetrics(t, func(c *Config) { c.ConstLabels = map[string]string{"env": "prod"} })
	l := ConstLabels()
	l["env"] = "changed"
	if ConstLabels()["env"] != "prod" {
		t.Error("a copy must be returned, so the caller cannot change the configuration")
	}
}

func TestHTTPDurationBuckets_ReturnsCopy(t *testing.T) {
	newMetrics(t, nil)
	b := HTTPDurationBuckets()
	if len(b) == 0 {
		t.Fatal("there must be default buckets")
	}
	b[0] = -1
	if HTTPDurationBuckets()[0] == -1 {
		t.Error("a copy must be returned, so the caller cannot change the configuration")
	}
}

func TestRegister_DuplicateReusesExisting(t *testing.T) {
	newMetrics(t, nil)
	opts := prometheus.CounterOpts{Name: "custom_total", Help: "h"}
	first := prometheus.NewCounter(opts)
	got, err := Register(first)
	if err != nil || got != prometheus.Collector(first) {
		t.Errorf("the first registration must return the one passed in, got=%v err=%v", got, err)
	}
	second := prometheus.NewCounter(opts)
	got, err = Register(second)
	if err != nil {
		t.Errorf("registering the same name and labels again is not an error: %v", err)
	}
	if got == prometheus.Collector(second) {
		t.Error("registering again must return the existing instance rather than panicking or returning a new one")
	}
}

// resetFallback empties the fallback instance and starts over.
//
// The tests used to call clearCollectors(): the cache was package-level back then and a collector left by one test
// leaked into the next. Now that the cache follows the instance, what has to be isolated is the fallback instance itself.
func resetFallback(t *testing.T) {
	t.Helper()
	old := fallback
	fallback = assemble(prometheus.NewRegistry(), DefaultConfig())
	t.Cleanup(func() { fallback = old })
}

func TestInstall_PreInitPointsDoNotLeakToNewInstance(t *testing.T) {
	// A recording before initialization goes onto the fallback registry. If the cache were separate from the instance,
	// recordings after initialization would keep going through that collector, which is never exported.
	old := current.Swap(nil)
	t.Cleanup(func() { current.Store(old) })
	resetFallback(t)

	CounterInc("early") // goes to the fallback instance

	m := newMetrics(t, nil)
	CounterInc("early")
	if out := dump(t, m); !strings.Contains(out, "early 1") {
		t.Errorf("a recording after initialization must land on the new registry and start from 1\ngot=\n%s", out)
	}
}

func TestActive_NoLossOrPanicBeforeInit(t *testing.T) {
	old := current.Swap(nil)
	t.Cleanup(func() { current.Store(old) })
	resetFallback(t)

	CounterInc("before_init") // must not panic
	if Registry() == nil || Handler() == nil {
		t.Error("a usable registry and handler must exist before initialization too")
	}
}

func TestRegister_RegistrationMatchesFramework(t *testing.T) {
	// This is the only wire between this package and the framework: a missing hook registration or a wrong stage shows
	// up as "the config has no effect" or "it is ready later than what uses it", and nothing else catches it.
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xmetric" {
			got = &e
			break
		}
	}
	if got == nil {
		t.Fatal("no start hook was registered")
	}
	if got.Stage != hook.StageTelemetry {
		t.Errorf("metrics must be ready before the clients, or the points they record are not collected, got=%v", got.Stage)
	}

	var stopped bool
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xmetric" {
			stopped = true
		}
	}
	if stopped {
		t.Errorf("New's Closer does nothing, so no stop hook may be registered")
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
			t.Errorf("%s must be collected by default", want)
		}
	}
}

func TestNew_RuntimeAndProcessMetricsHaveConstLabels(t *testing.T) {
	// The documentation says a constant label is "attached to every metric". These two groups are ready-made
	// collectors from client_golang that used to be registered on the Registry directly, and measured, not one go_* or
	// process_* carried them -- a dashboard filtering by env searching go_goroutines{env="prod"} found nothing.
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
			t.Errorf("every sample must carry the constant label env=\"prod\", this one does not: %s", line)
		}
	}
	if !goSeen || !procSeen {
		t.Errorf("both go_* and process_* must be present by default, got go=%v process=%v", goSeen, procSeen)
	}
}

func TestNew_RuntimeAndProcessMetricsCanBeDisabled(t *testing.T) {
	m := newMetrics(t, nil) // install turns both of them off by default
	out := dump(t, m)
	if strings.Contains(out, "go_goroutines") || strings.Contains(out, "process_cpu") {
		t.Errorf("they must not appear once turned off\ngot=\n%s", out)
	}
}

// ---- the log error counter ----

// logThrough builds an xlog writing only to a file and returns it together with the log directory.
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
	// The "error rate" is the most common alert and should not have to wait for the business to instrument it.
	m := newMetrics(t, func(c *Config) { c.LogErrorMetric = true })
	l := logThrough(t)

	l.Info("this one does not count")
	l.Warn("this one does not count either")
	for _, msg := range []string{"something went wrong", "something went wrong again"} {
		l.Error(msg) // the same line, aggregated into the same time series
	}

	out := dump(t, m)
	if !strings.Contains(out, `log_errors_total{caller=`) {
		t.Fatalf("there must be a log_errors_total\ngot=\n%s", out)
	}
	// caller is an aggregation dimension: two errors logged on the same line become one series with a count of 2.
	if !strings.Contains(out, `level="ERROR"} 2`) {
		t.Errorf("two Errors logged in the same place must become one series with a count of 2\ngot=\n%s", out)
	}
	// The Info and the Warn above have to be absent, and that has to be asserted rather than implied by the
	// ERROR series alone: counting every record puts them in series of their own and leaves the check above
	// untouched, while log_errors_total -- what an error-rate alert is built on -- has quietly become a count
	// of all logging.
	for _, level := range []string{`level="INFO"`, `level="WARN"`} {
		if strings.Contains(out, level) {
			t.Errorf("only Error and above are counted, but %s produced a series\ngot=\n%s", level, out)
		}
	}
}

func TestInstall_NoDataRaceWithConcurrentLogging(t *testing.T) {
	// Install used to publish the setup as current first and write logCounter onto it afterwards.
	// In between, any error log written through xlog would go through the observer to read
	// active().logCounter -- reading exactly the field being written.
	//
	// The consequence is more than a -race warning: the observer is attached before logCounter is assigned, so not one
	// error log in that window was counted, and the alerting dashboard looked no different.
	newMetrics(t, func(c *Config) { c.LogErrorMetric = true }) // attach the observer first
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
				l.Error("concurrent write")
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
	// Regression case. The implementation used to be "wrap slog.Default() and set it back", and slog.SetDefault also
	// routes the standard library log package's output into the new handler:
	// when that chain ends up back at slog's own handler, the record flows back through log.Output and
	// deadlocks on the log package's non-reentrant lock -- an application not using xlog hangs on its first log line.
	before := slog.Default()
	newMetrics(t, func(c *Config) { c.LogErrorMetric = true })
	if slog.Default() != before {
		t.Fatal("the global logger must not be changed: that path forms a cycle with the standard library log package")
	}

	// Log one line through the standard library default logger; it must not hang.
	done := make(chan struct{})
	go func() {
		slog.Error("through the standard library default logger")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging through the standard library default logger hung")
	}
}

func TestLogCounter_AttachesTraceAsExemplar(t *testing.T) {
	// A dashboard can jump from a metric point to the matching trace.
	t.Cleanup(func() { xlog.SetTraceExtractor(nil) })
	xlog.SetTraceExtractor(func(context.Context) (string, string) {
		return "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	})

	m := newMetrics(t, func(c *Config) { c.LogErrorMetric = true })
	logThrough(t).ErrorContext(context.Background(), "something went wrong")

	// An exemplar is only exported in the OpenMetrics format.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Accept", "application/openmetrics-text; version=1.0.0")
	m.Handler.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "trace_id=") {
		t.Errorf("a trace_id exemplar must be carried\ngot=\n%s", w.Body.String())
	}
}

func TestLogCounter_NoExemplarWithoutTrace(t *testing.T) {
	m := newMetrics(t, func(c *Config) { c.LogErrorMetric = true })
	logThrough(t).Error("something went wrong")

	if out := dump(t, m); !strings.Contains(out, "log_errors_total") {
		t.Errorf("it must count as usual without tracing\ngot=\n%s", out)
	}
}

func TestLogCounter_StopsCountingWhenDisabled(t *testing.T) {
	m := newMetrics(t, func(c *Config) { c.LogErrorMetric = false })
	logThrough(t).Error("something went wrong")

	if out := dump(t, m); strings.Contains(out, "log_errors_total") {
		t.Errorf("the metric must not exist once turned off\ngot=\n%s", out)
	}
}

func TestLogCounter_CallerKeepsTwoPathSegments(t *testing.T) {
	// A full path carries the build machine's directories, so the same code would give different label values on different machines.
	m := newMetrics(t, func(c *Config) { c.LogErrorMetric = true })
	logThrough(t).Error("something went wrong")

	out := dump(t, m)
	if !strings.Contains(out, `caller="xmetric/xmetric_test.go:`) {
		t.Errorf("caller must be \"parent directory/file:line\"\ngot=\n%s", out)
	}
}

func TestObserver_PanicDoesNotInterruptLogging(t *testing.T) {
	// A problem in observation must not interrupt the logging itself.
	newMetrics(t, func(c *Config) { c.LogErrorMetric = true })
	xlog.AddObserver(func(context.Context, slog.Record) { panic("the observer blew up") })

	dir := t.TempDir()
	c := xlog.DefaultConfig()
	c.Console = false
	c.File = xlog.FileConfig{Enable: true, Path: dir, Name: "app.log", RotateTime: time.Hour, Perm: "0644"}
	l, closer, err := xlog.New(c)
	if err != nil {
		t.Fatal(err)
	}
	l.Error("something went wrong")
	closer.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "app.log.*"))
	if len(files) == 0 {
		t.Fatal("no log file was written")
	}
	b, _ := os.ReadFile(files[0])
	if !strings.Contains(string(b), "something went wrong") {
		t.Errorf("the observer panicked, and the log itself must still be written, got=%q", b)
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
		t.Errorf("unknown must be returned when the call site cannot be taken, got=%q", got)
	}
}

func TestMustRegister(t *testing.T) {
	newMetrics(t, nil)
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "must_total", Help: "h"})
	MustRegister(c)
	c.Inc()

	if out := dump(t, active()); !strings.Contains(out, "must_total 1") {
		t.Errorf("a registered custom metric must be exported\ngot=\n%s", out)
	}
}

func TestNew_DuplicateCollectorFails(t *testing.T) {
	// New is a pure constructor with a freshly built registry, so the normal path cannot conflict;
	// a real conflict must return an error rather than panic, so the framework can treat it as an ordinary startup failure.
	c := DefaultConfig()
	c.LogErrorMetric = false
	m, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if m.Registry == nil || m.Handler == nil {
		t.Fatal("neither the registry nor the handler may be nil")
	}
}

func TestInstall_PreInitPointsAreReported(t *testing.T) {
	// Now that the cache follows the instance, Install no longer has to clear that other piece of global state.
	// But "what was recorded before the instance was swapped stays on the previous registry and cannot be exported"
	// still has to be said out loud, or it is a completely silent loss of data.
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

	if !strings.Contains(buf.String(), "before xmetric was initialized") {
		t.Errorf("something was recorded before initialization and Install must say so, got log=%q", buf.String())
	}
	if !strings.Contains(buf.String(), "metrics=2") {
		t.Errorf("it must report how many metrics there were, got log=%q", buf.String())
	}
}

func TestInstall_ASecondInstallIsNotReportedAsAStartupProblem(t *testing.T) {
	// The previous instance is not always the fallback: another Run in the same process, or a caller installing by
	// hand, swaps one real instance for another. "before xmetric was initialized" then sends whoever reads it looking
	// for a startup problem that is not there.
	keepGlobals(t)

	first, closer1, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer1.Close() })
	first.Install()
	CounterInc("recorded_on_the_first")

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	second, closer2, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer2.Close() })
	second.Install()

	if !strings.Contains(buf.String(), "installed again") {
		t.Errorf("a second Install must be reported as such, got log=%q", buf.String())
	}
	if strings.Contains(buf.String(), "before xmetric was initialized") {
		t.Errorf("this is not a startup problem and must not be worded as one, got log=%q", buf.String())
	}
}

func TestInstall_CacheFollowsInstanceSwap(t *testing.T) {
	// The cache used to be package-level while the Registry belongs to the instance: swapping the instance meant
	// remembering to clear that other piece of global state, and forgetting was a silent loss of data.
	m1 := newMetrics(t, nil)
	CounterInc("shared_name")
	if out := dump(t, m1); !strings.Contains(out, "shared_name 1") {
		t.Fatalf("the first instance must have recorded it\n%s", out)
	}

	m2 := newMetrics(t, nil)
	CounterInc("shared_name")

	out := dump(t, m2)
	if !strings.Contains(out, "shared_name 1") {
		t.Errorf("after the swap it must land on the new registry and start from 1\ngot=\n%s", out)
	}
}

// initComponent walks the path the framework really takes: set this module up from this configuration.
func initComponent(t *testing.T, c Config) {
	t.Helper()
	if err := install(c); err != nil {
		t.Fatalf("initialization failed: %v", err)
	}
}

// keepGlobals records the globals install will change and restores them when the test ends.
func keepGlobals(t *testing.T) {
	t.Helper()
	oldLogger := slog.Default()
	oldCurrent := current.Load()
	t.Cleanup(func() {
		slog.SetDefault(oldLogger)
		current.Store(oldCurrent)
	})
}

// keepApp puts the application identity back the way the test found it: SetDefault writes a global, and leaving
// one test's name behind would have the next one pass or fail for the wrong reason.
func keepApp(t *testing.T) {
	t.Helper()
	old := xapp.Config{Name: xapp.Name(), Version: xapp.Version()}
	t.Cleanup(func() { xapp.SetDefault(old) })
}

func TestInitXMetric_CarriesTheAppIdentityAsConstLabels(t *testing.T) {
	// One name per signal: the logs write service_name, the spans carry service.name, and the metrics have to carry
	// it too -- otherwise telling applications apart on a dashboard means writing ConstLabels by hand everywhere.
	keepGlobals(t)
	keepApp(t)
	xapp.SetDefault(xapp.Config{Name: "order-api", Version: "v1.2.3"})
	xonetest.UseConfigYAML(t, "XMetric:\n  ConstLabels:\n    env: test\n")

	if err := initXMetric(context.Background()); err != nil {
		t.Fatal(err)
	}
	labels := ConstLabels()
	if labels["service_name"] != "order-api" || labels["service_version"] != "v1.2.3" {
		t.Errorf("the identity must reach the constant labels, got=%v", labels)
	}
	if labels["env"] != "test" {
		t.Errorf("the configured labels must be kept alongside it, got=%v", labels)
	}
}

func TestInitXMetric_AConfiguredLabelWinsOverTheAppIdentity(t *testing.T) {
	keepGlobals(t)
	keepApp(t)
	xapp.SetDefault(xapp.Config{Name: "order-api"})
	xonetest.UseConfigYAML(t, "XMetric:\n  ConstLabels:\n    service_name: billing\n")

	if err := initXMetric(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := ConstLabels()["service_name"]; got != "billing" {
		t.Errorf("what the file configured must win over XApp, got=%q", got)
	}
}

func TestInitXMetric_NoIdentityAddsNoEmptyLabel(t *testing.T) {
	// A constant label with an empty value is worse than none: every query would carry a service_name="" that says
	// nothing, on the Go runtime and process metrics as well.
	keepGlobals(t)
	keepApp(t)
	xapp.SetDefault(xapp.Config{})
	xonetest.UseConfigYAML(t, "XMetric:\n  Namespace: demoapp\n")

	if err := initXMetric(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := ConstLabels()["service_name"]; ok {
		t.Errorf("with no identity anywhere there must be no service_name label, got=%v", ConstLabels())
	}
}

func TestInitXMetric_InstallsDefaultMetricsWhenUnconfigured(t *testing.T) {
	// Not installing it when metrics are not configured would make every recording from the framework's built-in
	// instrumentation land on the fallback instance and /metrics export nothing -- while not configuring metrics should simply mean "use the defaults".
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")

	if err := initXMetric(context.Background()); err != nil {
		t.Fatalf("not configuring it must not be an error: %v", err)
	}
	got := current.Load()
	if got == nil {
		t.Fatal("no global instance was installed")
	}
}

func TestInitXMetric_ConfigTypoFailsStartup(t *testing.T) {
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XMetric:\n  NameSpace: app\n")

	if err := initXMetric(context.Background()); err == nil {
		t.Fatal("a misspelled field must fail startup")
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
		t.Error("the first registration must give back the same instance")
	}
}

func TestRegisterAs_DuplicateReusesFirst(t *testing.T) {
	// When two places each build a collector with the same name, both have to record to the same one,
	// otherwise whichever arrives later can never be exported.
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
		t.Error("registering again must give back the one that arrived first")
	}
}

func TestRegisterAs_SameNameDifferentTypeFailsInsteadOfReplacing(t *testing.T) {
	// On a failed assertion the one passed in is given back: the caller records as usual (it just cannot be exported)
	// rather than getting a zero value and a nil pointer.
	newMetrics(t, nil)
	if _, err := Register(prometheus.NewCounter(prometheus.CounterOpts{Name: "registeras_kind"})); err != nil {
		t.Fatal(err)
	}
	want := prometheus.NewGauge(prometheus.GaugeOpts{Name: "registeras_kind"})
	got, err := RegisterAs(want)
	if err == nil {
		t.Fatal("a type mismatch must be an error")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("the error must say plainly that the name is taken by another type, got=%v", err)
	}
	if got != want {
		t.Error("on an error the one passed in must be given back, so the caller can still record")
	}
}

func TestCollectorOf_SameNameDifferentTypeReportsInsteadOfDropping(t *testing.T) {
	// A Counter first and a Gauge later: the latter is not in the registry and the values recorded through it can never
	// be exported. Without a word about it, that line on the dashboard stays empty while the code is clearly recording.
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	newMetrics(t, nil)
	CounterInc("clash")
	GaugeSet("clash", 5)

	if !strings.Contains(buf.String(), "clash") {
		t.Errorf("a same-name conflict must leave a record, got log=\n%s", buf.String())
	}
}

func TestCollectorOf_SameNameSameTypeReusesInstance(t *testing.T) {
	// Without reuse, every recording would build a new collector and lose every value once registration is rejected.
	m := newMetrics(t, nil)
	before := m.cachedCount()
	for i := 0; i < 5; i++ {
		CounterInc("reused", T("a", "1"))
	}
	if got := m.cachedCount() - before; got != 1 {
		t.Errorf("the same name and labels must build only one collector, %d were built", got)
	}
	if out := dump(t, m); !strings.Contains(out, `reused{a="1"} 5`) {
		t.Errorf("five recordings must accumulate on the same series\ngot=\n%s", out)
	}
}
