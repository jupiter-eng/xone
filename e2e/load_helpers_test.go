package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// What the load tests share: the switches, the configurations under test, measuring one step,
// reconciling and rendering the tables.
//
// The load generator (this test process), the service under test and PostgreSQL fight over the CPU
// of one machine, so these numbers can only be compared with each other and are not this
// framework's absolute performance on a machine of its own.

// requireLoad: the load tests are slow and do not run by default; XONE_E2E_LOAD=1 runs them (scripts/e2e.sh --load)
func requireLoad(t *testing.T) {
	t.Helper()
	harness.Require(t)
	if os.Getenv("XONE_E2E_LOAD") != "1" {
		t.Skip("load tests are slow and off by default: run scripts/e2e.sh --load, or set XONE_E2E_LOAD=1")
	}
}

// loadKnob reads a duration switch, falling back to the default when it is unset or malformed
func loadKnob(t *testing.T, name string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		t.Fatalf("%s=%q is no positive duration", name, v)
	}
	return d
}

// Each step warms up for warmup first and is then measured for step. It uses LoadSpec.Duration
// rather than a fixed number of requests: with 20,000 fixed, /ping is through in 0.7s at
// concurrency 256 before the connections have spread out, and the p99 is all ramp-up
func loadStep(t *testing.T) time.Duration   { return loadKnob(t, "XONE_E2E_LOAD_STEP", 5*time.Second) }
func loadWarmup(t *testing.T) time.Duration { return loadKnob(t, "XONE_E2E_LOAD_WARMUP", time.Second) }

// minLoadRequests is how many requests each step measures at the least, see measure
const minLoadRequests = 20000

// mw is one configuration of the service under test: which middleware is on and where the logs go
type mw struct {
	log    bool   // XGin.Log, the access log
	body   bool   // XGin.LogRequestBody + LogResponseBody
	trace  bool   // XTrace.Enable + XGin.Trace + the Trace of both XGorm instances: the whole tracing pipeline
	export bool   // a BatchSpanProcessor hung on with an exporter that only counts (Service.SpanDiscard)
	metric bool   // XGin.Metric, with no /metrics when it is off
	file   bool   // the logs go to a file rather than to standard output
	level  string // XLog.Level, empty being info
	sample string // XTrace.SampleRatio, empty being 1
}

var (
	// mwDefault is service/application.yml itself, which is the framework's defaults:
	// the access log to standard output, tracing sampled at 100% with no exporter, and the metrics on
	mwDefault = mw{log: true, trace: true, metric: true}
	// mwAllOff turns off everything that can be turned off: no access log, a noop tracing provider and no request metrics
	mwAllOff = mw{}
	// mwAllOn: the access log to a file (with the request and response bodies), tracing sampled at 100% and exported through a BatchSpanProcessor, and the metrics on
	mwAllOn = mw{log: true, body: true, trace: true, export: true, metric: true, file: true}
)

// options turns the configuration into harness.Options.
//
// Standard output always goes to /dev/null: the access log runs to tens of thousands of lines a
// second, and collecting that into the harness's memory both takes room and slows the process under
// test down. In a real deployment standard output has a pipe and a collector behind it, which costs
// more than /dev/null and is not what is measured here
func (m mw) options(table string) harness.Options {
	xlog := fmt.Sprintf("XLog:\n  Console: %v\n", !m.file)
	if m.level != "" {
		xlog += "  Level: " + m.level + "\n"
	}
	xtrace := fmt.Sprintf("XTrace:\n  Enable: %v\n", m.trace)
	if m.sample != "" {
		xtrace += "  SampleRatio: " + m.sample + "\n"
	}
	return harness.Options{
		Table:         table,
		DiscardStdout: true,
		LogFile:       m.file,
		Overlay: fmt.Sprintf("XGin:\n  Log: %v\n  LogRequestBody: %v\n  LogResponseBody: %v\n  Trace: %v\n  Metric: %v\n",
			m.log, m.body, m.body, m.trace, m.metric) + xlog + xtrace +
			fmt.Sprintf("Service:\n  SpanDiscard: %v\n", m.export) +
			// XGorm comes last: chLoadOptions appends a ch instance to it
			fmt.Sprintf("XGorm:\n  Clients:\n    default:\n      Trace: %v\n    mysql:\n      Trace: %v\n", m.trace, m.trace),
	}
}

// endpoint is an endpoint under load. The paths differ slightly between baseline and the xone
// service: xone's /users/:id needs ?cache=off to read PG directly, as baseline does
type endpoint struct {
	name  string // the name in the table, and the route label in xgin's metrics
	path  func(i int, baseline bool) string
	route string
}

func loadEndpoints(ids []int64) []endpoint {
	return []endpoint{
		{name: "/ping", route: "/ping", path: func(int, bool) string { return "/ping" }},
		{name: "/users/:id", route: "/users/:id", path: func(i int, baseline bool) string {
			// Multiplied by a prime to spread them out: neighboring requests read different rows rather than all landing on one page
			id := ids[(i*7919)%len(ids)]
			if baseline {
				return fmt.Sprintf("/users/%d", id)
			}
			return fmt.Sprintf("/users/%d?cache=off", id)
		}},
	}
}

// seedUsers creates the user table, fills in n users and returns their ids.
// The table is the same as baseline and service/store create (both of them CREATE TABLE IF NOT EXISTS)
func seedUsers(t *testing.T, table string, n int) []int64 {
	t.Helper()
	db := harness.DB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+table+` (
		id         BIGSERIAL PRIMARY KEY,
		name       TEXT NOT NULL,
		email      TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("creating the table %s: %v", table, err)
	}
	// The table is created by the test itself, and the harness cleans up none of it when the start fails before the process is up
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS ` + table + `, ` + table + `_orders`) })
	if _, err := db.ExecContext(ctx, `INSERT INTO `+table+` (name, email)
		SELECT 'load-' || g, 'load-' || g || '@example.com' FROM generate_series(1, $1) g`, n); err != nil {
		t.Fatalf("filling in the data: %v", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM `+table+` ORDER BY id`)
	if err != nil {
		t.Fatalf("reading the ids: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("reading the ids: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil || len(ids) != n {
		t.Fatalf("filled in %d users and read %d back (%v)", n, len(ids), err)
	}
	return ids
}

// cell is one step's result
type cell struct {
	target   string
	endpoint string
	conc     int
	res      harness.LoadResult
	// warm is how many requests the warmup sent, which counts when reconciling
	warm int
	// cpu / loaderCPU are the CPU the service process and the load process used per request
	cpu, loaderCPU time.Duration
	peakRSS        int64
	threads        int
}

func (c cell) cpuUS() float64 { return float64(c.cpu) / float64(time.Microsecond) }

// measure runs one step: a warmup at the same concurrency, the peak memory reset, and then the
// measured run, reading /proc before and after.
//
// The warmup counts separately: a process that has just come up builds its connection pools and
// allocates its buffers, and that one-off, JIT-like cost should not be spread over the per-request
// CPU
func measure(t *testing.T, p *harness.Process, target string, ep endpoint, baseline bool, conc int) cell {
	t.Helper()
	spec := harness.LoadSpec{
		Concurrency: conc,
		NewRequest: func(ctx context.Context, i int) (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, p.URL(ep.path(i, baseline)), nil)
		},
	}

	warm := spec
	warm.Duration = loadWarmup(t)
	wr := harness.Load(t, warm)
	if wr.Errors > 0 || wr.OK() != wr.Requests {
		t.Fatalf("%s %s at concurrency %d had failures in the warmup already: %v %v", target, ep.name, conc, wr, wr.ErrorSamples)
	}

	// At least step long, and long enough for 20,000 requests at the warmup's QPS: /users/:id does
	// two or three thousand a second at concurrency 1, so 5 seconds is barely over ten thousand and
	// the p99 rests on a hundred samples. It stretches to 3 x step at most
	spec.Duration = loadStep(t)
	if need := time.Duration(float64(minLoadRequests) / wr.QPS * 1.1 * float64(time.Second)); need > spec.Duration {
		spec.Duration = min(need, 3*loadStep(t))
	}

	p.ResetPeakRSS(t)
	before, selfBefore := p.Proc(t), readSelf(t)
	res := harness.Load(t, spec)
	after, selfAfter := p.Proc(t), readSelf(t)

	c := cell{target: target, endpoint: ep.name, conc: conc, res: res, warm: wr.Requests,
		peakRSS: after.PeakRSS, threads: after.Threads}
	if res.Requests > 0 {
		c.cpu = (after.CPU - before.CPU) / time.Duration(res.Requests)
		c.loaderCPU = (selfAfter.CPU - selfBefore.CPU) / time.Duration(res.Requests)
	}
	t.Logf("%-10s %-10s concurrency %-3d %v  service CPU %.1fus/request  peak RSS %.1fMB", target, ep.name, conc, res, c.cpuUS(), mb(c.peakRSS))

	// Not one request may fail under load: a transport error (a refused connection, a reset, a timeout) and anything that is not 2xx both count
	if res.Errors > 0 || res.OK() != res.Requests {
		t.Errorf("%s %s at concurrency %d: of %d requests, %d failed in the transport and %d were not 2xx, with the status codes %v and the error samples %v",
			target, ep.name, conc, res.Requests, res.Errors, res.Requests-res.Errors-res.OK(), res.Status, res.ErrorSamples)
	}
	if res.Requests < minLoadRequests {
		t.Logf("note: %s %s at concurrency %d measured only %d requests (fewer than %d), so the percentiles are shakier", target, ep.name, conc, res.Requests, minLoadRequests)
	}
	return c
}

func readSelf(t *testing.T) harness.ProcStat {
	t.Helper()
	s, err := harness.ReadProc(os.Getpid())
	if err != nil {
		t.Fatalf("reading the load process's own /proc: %v", err)
	}
	return s
}

// requestCount is the number of 200 requests on one route in /metrics (once from the counter and once from the histogram's _count)
func requestCount(m harness.Metrics, route string) (total, hist float64) {
	return m.Sum("e2e_http_requests_total", "route", route, "status", "200"),
		m.Sum("e2e_http_request_duration_seconds_count", "route", route, "status", "200")
}

// checkRequestMetric reconciles the metrics: xgin's request metrics have to grow by exactly what
// this step (the warmup and the measured run) sent.
//
// xgin/README.md, XGin: "Metric: true # the request metrics"; metric.go in xgin/middleware records
// them in a defer, so every request counts. A count lost or counted twice under concurrency shows
// up here
func checkRequestMetric(t *testing.T, c cell, route string, before, after harness.Metrics) {
	t.Helper()
	want := float64(c.warm + c.res.Requests)
	t0, h0 := requestCount(before, route)
	t1, h1 := requestCount(after, route)
	if t1-t0 != want || h1-h0 != want {
		t.Errorf("the documentation says every request counts into e2e_http_requests_total and the duration histogram; %s %s at concurrency %d sent %.0f 200s, the counter grew by %.0f and the histogram's _count by %.0f",
			c.target, c.endpoint, c.conc, want, t1-t0, h1-h0)
	}
}

// accessLogStats streams through every app.log.* file in the log directory and counts: the access
// log lines per route, the lines that are not whole JSON, and the bytes in all.
//
// Not Process.FileLogs: one round with everything on writes millions of lines, and reading them all
// into memory and decoding them into maps is too heavy
func accessLogStats(t *testing.T, dir string) (byRoute map[string]int, bad int, size int64) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "app.log.*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no log file was found in %s (%v)", dir, err)
	}
	byRoute = map[string]int{}
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			t.Fatalf("opening %s: %v", name, err)
		}
		st, _ := f.Stat()
		size += st.Size()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
		for sc.Scan() {
			var l struct {
				Msg   string `json:"msg"`
				Route string `json:"route"`
			}
			if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
				bad++
				continue
			}
			if l.Msg == "request completed" {
				byRoute[l.Route]++
			}
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
	}
	return byRoute, bad, size
}

// waitSpansFlushed waits for the BatchSpanProcessor to export what it holds: the export count
// catches up with the request count, or it stops changing for 7 seconds in a row (its default
// BatchTimeout being 5s, by which time the rest of the queue has gone out at the latest)
func waitSpansFlushed(t *testing.T, p *harness.Process, route string) (exported, handled float64) {
	t.Helper()
	last, stable := -1.0, time.Now()
	for {
		m := p.Metrics(t)
		exported = m.Sum("e2e_spans_exported_total", "kind", "server", "route", route)
		handled = m.Sum("e2e_http_requests_total", "route", route)
		if exported >= handled || time.Since(stable) > 7*time.Second {
			return exported, handled
		}
		if exported != last {
			last, stable = exported, time.Now()
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func mb(b int64) float64 { return float64(b) / (1 << 20) }

// msf is milliseconds with two decimals
func msf(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 2, 64)
}

// cellTable renders a set of steps as a Markdown table
func cellTable(cells []cell) string {
	var b strings.Builder
	b.WriteString("| target | endpoint | concurrency | requests | QPS | p50 ms | p90 ms | p99 ms | max ms | errors | service CPU us/request | loader CPU us/request | peak RSS MB | threads |\n")
	for _, c := range cells {
		fail := c.res.Errors + (c.res.Requests - c.res.Errors - c.res.OK())
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %.0f | %s | %s | %s | %s | %d | %.1f | %.1f | %.1f | %d |\n",
			c.target, c.endpoint, c.conc, c.res.Requests, c.res.QPS, msf(c.res.P50), msf(c.res.P90), msf(c.res.P99), msf(c.res.Max),
			fail, c.cpuUS(), float64(c.loaderCPU)/float64(time.Microsecond), mb(c.peakRSS), c.threads)
	}
	return b.String()
}
