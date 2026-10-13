package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// /metrics：
//   - xgin 的请求计数和耗时直方图按路由模板打标签（/users/:id 而不是 /users/42），
//     没匹配上的收敛成 unmatched，不认识的方法收敛成 OTHER
//   - 桶是 xmetric/README.md XMetric 写的默认值
//   - 业务的自定义指标（namespace e2e）、log_errors_total、xhttp 出站直方图、
//     xgorm / xredis 连接池指标、Go 运行时与进程指标都在
func TestFunctional_MetricsLabeledByRouteTemplate_WithCustomAndPoolMetrics(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	stub := harness.NewStub(t)
	p := harness.Start(t, harness.Options{Downstream: stub.URL})

	// 造流量。每一类的次数都是后面断言里的数字
	a := createUser(t, p, "m1", "m1@example.com")
	b := createUser(t, p, "m2", "m2@example.com")
	for _, id := range []int64{a.ID, b.ID, a.ID, b.ID} { // 两次 db、两次 local
		getUser(t, p, id)
	}
	p.Get(t, "/users/999999999") // 404，同一个路由模板
	for _, path := range []string{"/nope/1", "/nope/2", "/nope/abc/def"} {
		p.Get(t, path)
	}
	for _, m := range []string{"FOO", "BREW"} { // 自由 token 的方法
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

	m := waitMetrics(t, p, "请求计数里出现 /boom", func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", "/boom") == 1
	})

	t.Run("请求计数按路由模板打标签", func(t *testing.T) {
		if got := m.Sum("e2e_http_requests_total", "method", "GET", "route", "/users/:id", "status", "200"); got != 4 {
			t.Errorf("e2e_http_requests_total{GET,/users/:id,200} 应是 4，实际 %v", got)
		}
		if got := m.Sum("e2e_http_requests_total", "method", "GET", "route", "/users/:id", "status", "404"); got != 1 {
			t.Errorf("e2e_http_requests_total{GET,/users/:id,404} 应是 1，实际 %v", got)
		}
		// 标签里只能是注册过的路由模板，或者 unmatched：真实路径进标签的话时间序列随 id 无限增长
		allowed := []string{"/ping", "/users", "/users/:id", "/login", "/proxy", "/orders", "/slow", "/stuck", "/boom", "/upload", "/metrics", "unmatched"}
		for _, name := range []string{"e2e_http_requests_total", "e2e_http_request_duration_seconds_count"} {
			for _, s := range m.Find(name) {
				if !slices.Contains(allowed, s.Labels["route"]) {
					t.Errorf("%s 的 route 标签只该是路由模板或 unmatched，实际出现了 %q", name, s.Labels["route"])
				}
			}
		}
	})

	t.Run("未匹配的路由收敛成 unmatched", func(t *testing.T) {
		if got := m.Sum("e2e_http_requests_total", "method", "GET", "route", "unmatched", "status", "404"); got != 3 {
			t.Errorf("三个不存在的路径应合并计入 route=unmatched（404），实际 %v", got)
		}
	})

	t.Run("不认识的方法收敛成 OTHER", func(t *testing.T) {
		// .claude/CLAUDE.md：「指标的 method 标签收敛」；xgin/middleware/metric.go normalizeMethod
		known := []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "CONNECT", "OPTIONS", "TRACE", "OTHER"}
		for _, s := range m.Find("e2e_http_requests_total") {
			if !slices.Contains(known, s.Labels["method"]) {
				t.Errorf("method 标签只该是标准方法或 OTHER，实际出现了 %q", s.Labels["method"])
			}
		}
		if got := m.Sum("e2e_http_requests_total", "method", "OTHER"); got != 2 {
			t.Errorf("FOO、BREW 两个请求应计入 method=OTHER，实际 %v", got)
		}
	})

	t.Run("耗时直方图：同样的标签，桶是文档写的默认值", func(t *testing.T) {
		lbl := []string{"method", "GET", "route", "/users/:id", "status", "200"}
		if got := m.Sum("e2e_http_request_duration_seconds_count", lbl...); got != 4 {
			t.Errorf("耗时直方图 _count{GET,/users/:id,200} 应是 4，实际 %v", got)
		}
		if got := m.Sum("e2e_http_request_duration_seconds_sum", lbl...); got <= 0 {
			t.Errorf("耗时直方图 _sum 应大于 0，实际 %v", got)
		}
		// xmetric/README.md XMetric.HTTPDurationBuckets 的默认值
		want := "0.001,0.005,0.01,0.025,0.05,0.1,0.25,0.5,1,2.5,5,10,+Inf"
		if got := bucketBounds(m, "e2e_http_request_duration_seconds_bucket", lbl...); got != want {
			t.Errorf("HTTP 耗时的桶应是文档写的默认值 %s，实际 %s", want, got)
		}
		if got := m.Sum("e2e_http_request_duration_seconds_bucket", append(lbl, "le", "+Inf")...); got != 4 {
			t.Errorf("+Inf 那一档应等于 _count（4），实际 %v", got)
		}
	})

	t.Run("业务自定义指标", func(t *testing.T) {
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
				t.Errorf("/metrics 里应有 %s", c.name)
				continue
			}
			if got := m.Sum(c.name, c.labels...); got != c.want {
				t.Errorf("%s%v 应是 %v，实际 %v", c.name, c.labels, c.want, got)
			}
		}
		// xmetric/README.md XMetric.HistogramBuckets：快捷方法建的业务直方图，默认即 prometheus.DefBuckets
		want := "0.005,0.01,0.025,0.05,0.1,0.25,0.5,1,2.5,5,10,+Inf"
		if got := bucketBounds(m, "e2e_order_flow_seconds_bucket"); got != want {
			t.Errorf("业务直方图的桶应是文档写的默认值 %s，实际 %s", want, got)
		}
	})

	t.Run("log_errors_total 与 Error 级别的日志条数一致", func(t *testing.T) {
		// xmetric/README.md XMetric.LogErrorMetric：「Error 级别日志计入 log_errors_total」
		// Error 及以上（slog 写成 ERROR、ERROR+4……）；xmetric/log_counter.go 按级别打 level 标签
		errLogs := p.FindLogs(func(l harness.Log) bool { return strings.HasPrefix(l.Level(), "ERROR") })
		if len(errLogs) == 0 {
			t.Fatal("/boom 之后应至少有一条 Error 日志")
		}
		// 反过来也要成立：Warn 不算错误。fail_at=2 的订单打了一条 WARN（xflow step process failed），
		// 它要是被数进去，会多出一个 level="WARN" 的序列——只看 level="ERROR" 那一条的话看不出来，
		// 所以比的是全部 level 加起来的总数
		if len(p.FindLogs(func(l harness.Log) bool { return l.Level() == "WARN" })) == 0 {
			t.Fatal("这轮流量里应至少有一条 WARN 日志，否则分不出 Warn 有没有被数成错误")
		}
		for _, s := range m.Find("e2e_log_errors_total") {
			if !strings.HasPrefix(s.Labels["level"], "ERROR") {
				t.Errorf("文档说只有 Error 级别的日志计入 log_errors_total，实际出现了 level=%q：%v", s.Labels["level"], s.Value)
			}
		}
		if got := m.Sum("e2e_log_errors_total"); got != float64(len(errLogs)) {
			var msgs []string
			for _, l := range errLogs {
				msgs = append(msgs, l.Msg())
			}
			t.Errorf("e2e_log_errors_total（全部 level 加起来）应等于 Error 日志条数 %d（%s），实际 %v", len(errLogs), strings.Join(msgs, "; "), got)
		}
	})

	t.Run("xhttp 出站耗时直方图", func(t *testing.T) {
		got := m.Sum("e2e_http_client_request_duration_seconds_count", "method", "GET", "host", stub.Addr(), "status", "200")
		if got != 3 {
			t.Errorf("e2e_http_client_request_duration_seconds_count{GET,%s,200} 应是 3，实际 %v", stub.Addr(), got)
		}
	})

	t.Run("xgorm 与 xredis 的连接池指标", func(t *testing.T) {
		for _, name := range []string{
			"e2e_db_pool_open", "e2e_db_pool_in_use", "e2e_db_pool_idle", "e2e_db_pool_max_open",
			"e2e_db_pool_wait_total", "e2e_db_pool_wait_duration_seconds_total",
			"e2e_db_pool_closed_max_idle_total", "e2e_db_pool_closed_max_lifetime_total",
			"e2e_redis_pool_connections", "e2e_redis_pool_connections_idle", "e2e_redis_pool_connections_stale_total",
			"e2e_redis_pool_hits_total", "e2e_redis_pool_misses_total", "e2e_redis_pool_timeouts_total",
		} {
			if len(m.Find(name, "name", "default")) != 1 {
				t.Errorf("/metrics 里应有 %s{name=\"default\"}（xgorm/README.md、xredis/README.md：Metric: true 连接池指标按实例生效）", name)
			}
		}
		// xgorm/README.md XGorm.MaxOpenConns 默认 50
		if got := m.Sum("e2e_db_pool_max_open", "name", "default"); got != 50 {
			t.Errorf("e2e_db_pool_max_open 应是默认的 50，实际 %v", got)
		}
		if got := m.Sum("e2e_db_pool_open", "name", "default"); got < 1 {
			t.Errorf("跑过 SQL 之后 e2e_db_pool_open 应至少是 1，实际 %v", got)
		}
		if got := m.Sum("e2e_redis_pool_connections", "name", "default"); got < 1 {
			t.Errorf("跑过 Redis 命令之后 e2e_redis_pool_connections 应至少是 1，实际 %v", got)
		}
		t.Logf("数字：db open=%v idle=%v；redis conns=%v idle=%v hits=%v misses=%v",
			m.Sum("e2e_db_pool_open"), m.Sum("e2e_db_pool_idle"),
			m.Sum("e2e_redis_pool_connections"), m.Sum("e2e_redis_pool_connections_idle"),
			m.Sum("e2e_redis_pool_hits_total"), m.Sum("e2e_redis_pool_misses_total"))
	})

	t.Run("Go 运行时与进程指标默认开着", func(t *testing.T) {
		// xmetric/README.md XMetric：GoMetrics / ProcessMetrics 默认开
		for _, name := range []string{"go_goroutines", "go_memstats_heap_alloc_bytes", "process_resident_memory_bytes", "process_cpu_seconds_total"} {
			if !m.Has(name) {
				t.Errorf("/metrics 里应有 %s", name)
			}
		}
		t.Logf("数字：goroutines=%v RSS=%.1fMB，/metrics 共 %d 条样本",
			m.Sum("go_goroutines"), m.Sum("process_resident_memory_bytes")/1e6, len(m.Samples))
	})
}

// bucketBounds 直方图的桶上界，按 /metrics 文本的写法，从小到大逗号分隔
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
