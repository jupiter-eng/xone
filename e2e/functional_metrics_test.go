package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// /metrics:
//   - xgin's request count and duration histogram are labeled by route template (/users/:id and
//     not /users/42), an unmatched path collapses into unmatched and an unknown method into OTHER
//   - the buckets are the defaults xmetric/README.md, XMetric writes down
//   - the business's own metrics (the e2e namespace), log_errors_total, xhttp's outbound
//     histogram, the xgorm / xredis pool metrics and the Go runtime and process metrics are all
//     there
func TestFunctional_MetricsLabeledByRouteTemplate_WithCustomAndPoolMetrics(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	stub := harness.NewStub(t)
	p := harness.Start(t, harness.Options{Downstream: stub.URL})

	// Make some traffic. The count of each kind is the number the assertions below go by
	a := createUser(t, p, "m1", "m1@example.com")
	b := createUser(t, p, "m2", "m2@example.com")
	for _, id := range []int64{a.ID, b.ID, a.ID, b.ID} { // twice db, twice local
		getUser(t, p, id)
	}
	p.Get(t, "/users/999999999") // a 404 on the same route template
	for _, path := range []string{"/nope/1", "/nope/2", "/nope/abc/def"} {
		p.Get(t, path)
	}
	for _, m := range []string{"FOO", "BREW"} { // free-token methods
		p.Do(t, m, "/ping", nil)
	}
	p.PostJSON(t, "/login", map[string]string{"username": "x"})
	p.PostJSON(t, "/login", map[string]string{"username": "y"})
	p.PostJSON(t, "/orders", map[string]any{"user_id": a.ID, "amount": 1})
	p.PostJSON(t, "/orders", map[string]any{"user_id": a.ID, "amount": 1, "fail_at": 2})
	for range 3 {
		p.Get(t, "/proxy?token=m")
	}
	p.Get(t, "/slow?ms=5")
	p.Get(t, "/boom")

	m := waitMetrics(t, p, "/boom turning up in the request count", func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", "/boom") == 1
	})

	t.Run("the request count is labeled by route template", func(t *testing.T) {
		if got := m.Sum("e2e_http_requests_total", "method", "GET", "route", "/users/:id", "status", "200"); got != 4 {
			t.Errorf("e2e_http_requests_total{GET,/users/:id,200} should be 4, got %v", got)
		}
		if got := m.Sum("e2e_http_requests_total", "method", "GET", "route", "/users/:id", "status", "404"); got != 1 {
			t.Errorf("e2e_http_requests_total{GET,/users/:id,404} should be 1, got %v", got)
		}
		// A label may hold a registered route template or unmatched alone: with the real path in the label the time series would grow without bound with the ids
		allowed := []string{"/ping", "/users", "/users/:id", "/login", "/proxy", "/orders", "/slow", "/stuck", "/boom", "/upload", "/metrics", "unmatched"}
		for _, name := range []string{"e2e_http_requests_total", "e2e_http_request_duration_seconds_count"} {
			for _, s := range m.Find(name) {
				if !slices.Contains(allowed, s.Labels["route"]) {
					t.Errorf("%s's route label should be a route template or unmatched alone, and %q turned up", name, s.Labels["route"])
				}
			}
		}
	})

	t.Run("an unmatched route collapses into unmatched", func(t *testing.T) {
		if got := m.Sum("e2e_http_requests_total", "method", "GET", "route", "unmatched", "status", "404"); got != 3 {
			t.Errorf("the three paths that do not exist should count together under route=unmatched (404), got %v", got)
		}
	})

	t.Run("an unknown method collapses into OTHER", func(t *testing.T) {
		// .claude/CLAUDE.md: "the method label of a metric collapses"; normalizeMethod in xgin/middleware/metric.go
		known := []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "CONNECT", "OPTIONS", "TRACE", "OTHER"}
		for _, s := range m.Find("e2e_http_requests_total") {
			if !slices.Contains(known, s.Labels["method"]) {
				t.Errorf("the method label should be a standard method or OTHER alone, and %q turned up", s.Labels["method"])
			}
		}
		if got := m.Sum("e2e_http_requests_total", "method", "OTHER"); got != 2 {
			t.Errorf("the FOO and BREW requests should count under method=OTHER, got %v", got)
		}
	})

	t.Run("the duration histogram: the same labels, with the documented default buckets", func(t *testing.T) {
		lbl := []string{"method", "GET", "route", "/users/:id", "status", "200"}
		if got := m.Sum("e2e_http_request_duration_seconds_count", lbl...); got != 4 {
			t.Errorf("the duration histogram's _count{GET,/users/:id,200} should be 4, got %v", got)
		}
		if got := m.Sum("e2e_http_request_duration_seconds_sum", lbl...); got <= 0 {
			t.Errorf("the duration histogram's _sum should be above 0, got %v", got)
		}
		// The default of XMetric.HTTPDurationBuckets in xmetric/README.md
		want := "0.001,0.005,0.01,0.025,0.05,0.1,0.25,0.5,1,2.5,5,10,+Inf"
		if got := bucketBounds(m, "e2e_http_request_duration_seconds_bucket", lbl...); got != want {
			t.Errorf("the HTTP duration buckets should be the documented defaults %s, got %s", want, got)
		}
		if got := m.Sum("e2e_http_request_duration_seconds_bucket", append(lbl, "le", "+Inf")...); got != 4 {
			t.Errorf("the +Inf bucket should equal _count (4), got %v", got)
		}
	})

	t.Run("the business's own metrics", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			labels []string
			want   float64
		}{
			{"e2e_users_created_total", nil, 2},
			{"e2e_user_reads_total", []string{"source", "db"}, 2},
			{"e2e_user_reads_total", []string{"source", "local"}, 2},
			{"e2e_logins_total", nil, 2},
			{"e2e_orders_total", []string{"result", "success"}, 1},
			{"e2e_orders_total", []string{"result", "failed"}, 1},
			{"e2e_order_flow_seconds_count", nil, 2},
			{"e2e_downstream_calls_total", []string{"status", "200"}, 3},
			{"e2e_slow_inflight", nil, 0},
		} {
			if !m.Has(c.name) {
				t.Errorf("/metrics should hold %s", c.name)
				continue
			}
			if got := m.Sum(c.name, c.labels...); got != c.want {
				t.Errorf("%s%v should be %v, got %v", c.name, c.labels, c.want, got)
			}
		}
		// XMetric.HistogramBuckets in xmetric/README.md: a business histogram made with the shortcut defaults to prometheus.DefBuckets
		want := "0.005,0.01,0.025,0.05,0.1,0.25,0.5,1,2.5,5,10,+Inf"
		if got := bucketBounds(m, "e2e_order_flow_seconds_bucket"); got != want {
			t.Errorf("a business histogram's buckets should be the documented defaults %s, got %s", want, got)
		}
	})

	t.Run("log_errors_total matches the number of Error log lines", func(t *testing.T) {
		// XMetric.LogErrorMetric in xmetric/README.md: "a log line at Error counts into log_errors_total"
		// Error and above (which slog writes as ERROR, ERROR+4 and so on); xmetric/log_counter.go labels them with level
		errLogs := p.FindLogs(func(l harness.Log) bool { return strings.HasPrefix(l.Level(), "ERROR") })
		if len(errLogs) == 0 {
			t.Fatal("there should be at least one Error log line after /boom")
		}
		// And the other way around: a Warn is not an error. The order with fail_at=2 logged one WARN (xflow step process failed),
		// and were it counted there would be one more series with level="WARN" -- which looking at level="ERROR" alone would not show,
		// so what is compared is the total over every level
		if len(p.FindLogs(func(l harness.Log) bool { return l.Level() == "WARN" })) == 0 {
			t.Fatal("this round of traffic should hold at least one WARN log line, or whether a Warn is counted as an error cannot be told")
		}
		for _, s := range m.Find("e2e_log_errors_total") {
			if !strings.HasPrefix(s.Labels["level"], "ERROR") {
				t.Errorf("the documentation says log lines at Error alone count into log_errors_total, and level=%q turned up: %v", s.Labels["level"], s.Value)
			}
		}
		if got := m.Sum("e2e_log_errors_total"); got != float64(len(errLogs)) {
			var msgs []string
			for _, l := range errLogs {
				msgs = append(msgs, l.Msg())
			}
			t.Errorf("e2e_log_errors_total (summed over every level) should equal the %d Error log lines (%s), got %v", len(errLogs), strings.Join(msgs, "; "), got)
		}
	})

	t.Run("xhttp's outbound duration histogram", func(t *testing.T) {
		got := m.Sum("e2e_http_client_request_duration_seconds_count", "method", "GET", "host", stub.Addr(), "status", "200")
		if got != 3 {
			t.Errorf("e2e_http_client_request_duration_seconds_count{GET,%s,200} should be 3, got %v", stub.Addr(), got)
		}
	})

	t.Run("the xgorm and xredis pool metrics", func(t *testing.T) {
		for _, name := range []string{
			"e2e_db_pool_open", "e2e_db_pool_in_use", "e2e_db_pool_idle", "e2e_db_pool_max_open",
			"e2e_db_pool_wait_total", "e2e_db_pool_wait_duration_seconds_total",
			"e2e_db_pool_closed_max_idle_total", "e2e_db_pool_closed_max_lifetime_total",
			"e2e_redis_pool_connections", "e2e_redis_pool_connections_idle", "e2e_redis_pool_connections_stale_total",
			"e2e_redis_pool_hits_total", "e2e_redis_pool_misses_total", "e2e_redis_pool_timeouts_total",
		} {
			if len(m.Find(name, "name", "default")) != 1 {
				t.Errorf("/metrics should hold %s{name=\"default\"} (xgorm/README.md and xredis/README.md: Metric: true, the pool metrics taking per instance)", name)
			}
		}
		// XGorm.MaxOpenConns in xgorm/README.md is 50 by default
		if got := m.Sum("e2e_db_pool_max_open", "name", "default"); got != 50 {
			t.Errorf("e2e_db_pool_max_open should be the default 50, got %v", got)
		}
		if got := m.Sum("e2e_db_pool_open", "name", "default"); got < 1 {
			t.Errorf("after SQL has run e2e_db_pool_open should be at least 1, got %v", got)
		}
		if got := m.Sum("e2e_redis_pool_connections", "name", "default"); got < 1 {
			t.Errorf("after a Redis command has run e2e_redis_pool_connections should be at least 1, got %v", got)
		}
		t.Logf("numbers: db open=%v idle=%v; redis conns=%v idle=%v hits=%v misses=%v",
			m.Sum("e2e_db_pool_open"), m.Sum("e2e_db_pool_idle"),
			m.Sum("e2e_redis_pool_connections"), m.Sum("e2e_redis_pool_connections_idle"),
			m.Sum("e2e_redis_pool_hits_total"), m.Sum("e2e_redis_pool_misses_total"))
	})

	t.Run("the Go runtime and process metrics are on by default", func(t *testing.T) {
		// XMetric in xmetric/README.md: GoMetrics / ProcessMetrics are on by default
		for _, name := range []string{"go_goroutines", "go_memstats_heap_alloc_bytes", "process_resident_memory_bytes", "process_cpu_seconds_total"} {
			if !m.Has(name) {
				t.Errorf("/metrics should hold %s", name)
			}
		}
		t.Logf("numbers: goroutines=%v RSS=%.1fMB, %d samples in /metrics",
			m.Sum("go_goroutines"), m.Sum("process_resident_memory_bytes")/1e6, len(m.Samples))
	})
}

// bucketBounds are a histogram's bucket upper bounds as the /metrics text writes them, comma
// separated from smallest to largest
func bucketBounds(m harness.Metrics, name string, labels ...string) string {
	type b struct {
		le  string
		val float64
	}
	var bs []b
	seen := map[string]bool{}
	for _, s := range m.Find(name, labels...) {
		le := s.Labels["le"]
		if seen[le] {
			continue
		}
		seen[le] = true
		var v float64
		if le == "+Inf" {
			v = 1e308
		} else {
			fmt.Sscan(le, &v)
		}
		bs = append(bs, b{le, v})
	}
	slices.SortFunc(bs, func(x, y b) int {
		switch {
		case x.val < y.val:
			return -1
		case x.val > y.val:
			return 1
		}
		return 0
	})
	out := make([]string, len(bs))
	for i, x := range bs {
		out[i] = x.le
	}
	return strings.Join(out, ",")
}
