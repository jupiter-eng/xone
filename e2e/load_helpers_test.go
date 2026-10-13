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

// 压测的公共部分：开关、被测配置、一档的测量、对账和出表。
//
// 压测器（本测试进程）、被测服务、PostgreSQL 在同一台机器上抢 CPU，
// 所以这里的数字只能拿来互相比，不是这个框架在独立机器上的绝对性能。

// requireLoad 压测慢，默认不跑：XONE_E2E_LOAD=1 才跑（scripts/e2e.sh --load）
func requireLoad(t *testing.T) {
	t.Helper()
	harness.Require(t)
	if os.Getenv("XONE_E2E_LOAD") != "1" {
		t.Skip("load tests are slow and off by default: run scripts/e2e.sh --load, or set XONE_E2E_LOAD=1")
	}
}

// loadKnob 读一个时长开关，没设或写错时用默认值
func loadKnob(t *testing.T, name string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		t.Fatalf("%s=%q 不是正的时长", name, v)
	}
	return d
}

// 每档先预热 warmup，再正式测 step。用 LoadSpec.Duration 而不是固定请求数：
// 固定 2 万个的话，/ping 在 256 并发下 0.7s 就跑完了，连接都没摊开，p99 全是爬坡
func loadStep(t *testing.T) time.Duration   { return loadKnob(t, "XONE_E2E_LOAD_STEP", 5*time.Second) }
func loadWarmup(t *testing.T) time.Duration { return loadKnob(t, "XONE_E2E_LOAD_WARMUP", time.Second) }

// minLoadRequests 每档至少测这么多个请求，见 measure
const minLoadRequests = 20000

// mw 被测服务的一种配置：哪些中间件开着、日志往哪写
type mw struct {
	log    bool   // XGin.Log 访问日志
	body   bool   // XGin.LogRequestBody + LogResponseBody
	trace  bool   // XTrace.Enable + XGin.Trace + 两个 XGorm 实例的 Trace：整套链路
	export bool   // 挂 BatchSpanProcessor + 只计数的 exporter（Service.SpanDiscard）
	metric bool   // XGin.Metric，关掉时没有 /metrics
	file   bool   // 日志写文件、不写标准输出
	level  string // XLog.Level，空串是 info
	sample string // XTrace.SampleRatio，空串是 1
}

var (
	// mwDefault 就是 service/application.yml 本身，也就是框架的默认值：
	// 访问日志写标准输出、链路 100% 采样但没有 exporter、指标开
	mwDefault = mw{log: true, trace: true, metric: true}
	// mwAllOff 能关的都关：没有访问日志、链路是 noop provider、没有请求指标
	mwAllOff = mw{}
	// mwAllOn 访问日志写文件（连同请求体、响应体）、链路 100% 采样并经 BatchSpanProcessor 导出、指标开
	mwAllOn = mw{log: true, body: true, trace: true, export: true, metric: true, file: true}
)

// options 把配置翻成 harness.Options。
//
// 标准输出一律接 /dev/null：访问日志一秒几万行，收进 harness 的内存既占地方又会拖慢
// 被测进程。真实部署里标准输出后面是一根管道和采集器，比 /dev/null 贵，这里量不到
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
			// XGorm 放最后：chLoadOptions 往后接一个 ch 实例
			fmt.Sprintf("XGorm:\n  Clients:\n    default:\n      Trace: %v\n    mysql:\n      Trace: %v\n", m.trace, m.trace),
	}
}

// endpoint 被压的接口。baseline 和 xone 服务的路径略有不同：
// xone 的 /users/:id 要带 ?cache=off 才直接读 PG，和 baseline 一样
type endpoint struct {
	name  string // 表里的名字，也是 xgin 指标里的 route 标签
	path  func(i int, baseline bool) string
	route string
}

func loadEndpoints(ids []int64) []endpoint {
	return []endpoint{
		{name: "/ping", route: "/ping", path: func(int, bool) string { return "/ping" }},
		{name: "/users/:id", route: "/users/:id", path: func(i int, baseline bool) string {
			// 乘一个质数打散：相邻的请求读不同的行，不全压在同一个页上
			id := ids[(i*7919)%len(ids)]
			if baseline {
				return fmt.Sprintf("/users/%d", id)
			}
			return fmt.Sprintf("/users/%d?cache=off", id)
		}},
	}
}

// seedUsers 建用户表、灌 n 个用户，返回它们的 id。
// 表结构和 baseline、service/store 建的一样（它们都是 CREATE TABLE IF NOT EXISTS）
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
		t.Fatalf("建表 %s：%v", table, err)
	}
	// 表是测试自己建的，进程还没起来就失败的话 harness 不会替它收尾
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS ` + table + `, ` + table + `_orders`) })
	if _, err := db.ExecContext(ctx, `INSERT INTO `+table+` (name, email)
		SELECT 'load-' || g, 'load-' || g || '@example.com' FROM generate_series(1, $1) g`, n); err != nil {
		t.Fatalf("灌数据：%v", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM `+table+` ORDER BY id`)
	if err != nil {
		t.Fatalf("读 id：%v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("读 id：%v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil || len(ids) != n {
		t.Fatalf("灌了 %d 个用户，读回 %d 个（%v）", n, len(ids), err)
	}
	return ids
}

// cell 一档的结果
type cell struct {
	target   string
	endpoint string
	conc     int
	res      harness.LoadResult
	// warm 预热发出去的请求数，对账时要算上
	warm int
	// cpu / loaderCPU 服务进程、压测进程每个请求用掉的 CPU
	cpu, loaderCPU time.Duration
	peakRSS        int64
	threads        int
}

func (c cell) cpuUS() float64 { return float64(c.cpu) / float64(time.Microsecond) }

// measure 压一档：同样的并发先预热，重置峰值内存，再正式测，前后各读一次 /proc。
//
// 预热单独算：刚起来的进程要建连接池、分配各种缓冲，JIT 式的一次性开销不该摊进每请求 CPU 里
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
		t.Fatalf("%s %s 并发 %d 预热就有失败：%v %v", target, ep.name, conc, wr, wr.ErrorSamples)
	}

	// 至少跑 step，并且按预热的 QPS 估够 2 万个请求：/users/:id 在并发 1 下一秒只有两三千个，
	// 5 秒才一万出头，p99 只由一百来个样本决定。最多拉长到 3 倍 step
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
	t.Logf("%-10s %-10s 并发 %-3d %v  服务 CPU %.1fµs/请求  峰值 RSS %.1fMB", target, ep.name, conc, res, c.cpuUS(), mb(c.peakRSS))

	// 压测期间一个请求都不该失败：传输层错误（连接被拒、被重置、超时）和非 2xx 都算
	if res.Errors > 0 || res.OK() != res.Requests {
		t.Errorf("%s %s 并发 %d：%d 个请求里传输层失败 %d 个、非 2xx %d 个，状态码 %v，错误样本 %v",
			target, ep.name, conc, res.Requests, res.Errors, res.Requests-res.Errors-res.OK(), res.Status, res.ErrorSamples)
	}
	if res.Requests < minLoadRequests {
		t.Logf("注意：%s %s 并发 %d 这一档只有 %d 个请求（不足 %d），分位数的抖动会大一些", target, ep.name, conc, res.Requests, minLoadRequests)
	}
	return c
}

func readSelf(t *testing.T) harness.ProcStat {
	t.Helper()
	s, err := harness.ReadProc(os.Getpid())
	if err != nil {
		t.Fatalf("读压测进程自己的 /proc：%v", err)
	}
	return s
}

// requestCount /metrics 里某个路由的 200 请求数（计数器和直方图的 _count 各一份）
func requestCount(m harness.Metrics, route string) (total, hist float64) {
	return m.Sum("e2e_http_requests_total", "route", route, "status", "200"),
		m.Sum("e2e_http_request_duration_seconds_count", "route", route, "status", "200")
}

// checkRequestMetric 指标对账：这一档（预热加正式）发出去多少个，xgin 的请求指标就该多多少。
//
// xgin/README.md XGin：「Metric: true # 请求指标」；xgin/middleware/metric.go 用 defer 记，
// 每个请求都计入。并发下丢计数、重复计数都会在这里现形
func checkRequestMetric(t *testing.T, c cell, route string, before, after harness.Metrics) {
	t.Helper()
	want := float64(c.warm + c.res.Requests)
	t0, h0 := requestCount(before, route)
	t1, h1 := requestCount(after, route)
	if t1-t0 != want || h1-h0 != want {
		t.Errorf("文档说每个请求都计进 e2e_http_requests_total 和耗时直方图；%s %s 并发 %d 发了 %.0f 个 200，计数器涨了 %.0f，直方图 _count 涨了 %.0f",
			c.target, c.endpoint, c.conc, want, t1-t0, h1-h0)
	}
}

// accessLogStats 流式数一遍日志目录下的全部 app.log.* 文件：
// 每个路由有几条访问日志、有几行不是完整的 JSON、一共多少字节。
//
// 不用 Process.FileLogs：一轮全开的压测写下上百万行，全读进内存解析成 map 太重
func accessLogStats(t *testing.T, dir string) (byRoute map[string]int, bad int, size int64) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "app.log.*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("在 %s 里没找到日志文件（%v）", dir, err)
	}
	byRoute = map[string]int{}
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			t.Fatalf("打开 %s：%v", name, err)
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
			t.Fatalf("读 %s：%v", name, err)
		}
	}
	return byRoute, bad, size
}

// waitSpansFlushed 等 BatchSpanProcessor 把手上的 Span 导完：导出计数追上请求计数，
// 或者连续 7 秒不再变（它的默认 BatchTimeout 是 5s，队列里剩下的最迟那时候发出去）
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

// msf 毫秒，两位小数
func msf(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 2, 64)
}

// cellTable 把若干档渲染成 Markdown 表格
func cellTable(cells []cell) string {
	var b strings.Builder
	b.WriteString("| 目标 | 接口 | 并发 | 请求数 | QPS | p50 ms | p90 ms | p99 ms | max ms | 错误 | 服务 CPU µs/请求 | 压测端 CPU µs/请求 | 峰值 RSS MB | 线程 |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, c := range cells {
		fail := c.res.Errors + (c.res.Requests - c.res.Errors - c.res.OK())
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %.0f | %s | %s | %s | %s | %d | %.1f | %.1f | %.1f | %d |\n",
			c.target, c.endpoint, c.conc, c.res.Requests, c.res.QPS, msf(c.res.P50), msf(c.res.P90), msf(c.res.P99), msf(c.res.Max),
			fail, c.cpuUS(), float64(c.loaderCPU)/float64(time.Microsecond), mb(c.peakRSS), c.threads)
	}
	return b.String()
}
