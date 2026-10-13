package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// 压测。只在 XONE_E2E_LOAD=1 时跑（scripts/e2e.sh --load），一轮十来分钟。
//
// 压测器、被测服务、PostgreSQL 挤在同一台机器上（本机 4 核），高并发下三者抢 CPU：
// 这里的 QPS 和延迟只能拿来在不同配置之间互相比，不代表独立部署时的绝对性能。
// 「服务 CPU µs/请求」受挤占的影响最小，比 QPS 更适合拿来算框架的开销。
//
// 开关（都是时长）：
//
//	XONE_E2E_LOAD_STEP    每档正式测多久，默认 5s
//	XONE_E2E_LOAD_WARMUP  每档先预热多久，默认 1s
//	XONE_E2E_SOAK         浸泡多久，默认 3m

// 对照组裸 gin，实验组 xone 服务的三种配置，/ping 和 /users/:id（读 PG）两个接口，
// 并发 1 / 16 / 64 / 256 各一档。
//
// 断言的只是压测下该守住的承诺，数字本身只打印：
//   - 一个请求都不失败（传输层错误、非 2xx 都算）
//   - 指标开着时，xgin 的请求计数和耗时直方图一个不多一个不少（xgin/README.md「指标」）
//   - 全开时每个请求一条访问日志，写进文件的每一行都是完整的 JSON（并发写不串行）
//   - 压完之后 SIGTERM 照样以 0 退出
func TestLoad_FrameworkOverheadVsBareGinByConcurrency(t *testing.T) {
	requireLoad(t)
	table := "e2e_load_" + harness.NewID()
	ids := seedUsers(t, table, 1000)
	eps := loadEndpoints(ids)
	concs := []int{1, 16, 64, 256}

	targets := []struct {
		name     string
		baseline bool
		m        mw
	}{
		{"裸 gin", true, mw{}},
		{"xone 全关", false, mwAllOff},
		{"xone 默认", false, mwDefault},
		{"xone 全开", false, mwAllOn},
	}

	var cells []cell
	for _, tg := range targets {
		var p *harness.Process
		if tg.baseline {
			p = harness.StartBaseline(t, harness.Options{Table: table, DiscardStdout: true})
		} else {
			p = harness.Start(t, tg.m.options(table))
		}
		for _, ep := range eps {
			for _, conc := range concs {
				var before harness.Metrics
				if tg.m.metric {
					before = p.Metrics(t)
				}
				c := measure(t, p, tg.name, ep, tg.baseline, conc)
				if tg.m.metric {
					checkRequestMetric(t, c, ep.route, before, p.Metrics(t))
				}
				cells = append(cells, c)
			}
		}

		if tg.m.export {
			// 只打印不断言：Span 丢不丢取决于使用者挂的 SpanProcessor（这里是 SDK 默认参数的
			// BatchSpanProcessor，队列满了静默丢），不是框架承诺的东西。采样率 1 这一条
			// 由对账的分母保证：每个请求都有一个服务端 Span 进了处理器
			for _, ep := range eps {
				exported, handled := waitSpansFlushed(t, p, ep.route)
				t.Logf("%s %s：导出服务端 Span %.0f 个 / 处理请求 %.0f 个（%.2f%%）", tg.name, ep.route, exported, handled, 100*exported/handled)
			}
		}
		var final harness.Metrics
		if tg.m.metric {
			final = p.Metrics(t)
		}

		exit := p.Terminate(t, 30*time.Second)
		if exit.Code != 0 || exit.Signal != nil {
			t.Errorf("%s 压完之后 SIGTERM 应以 0 退出，实际 %v\n%s", tg.name, exit, p.Stderr())
		}

		if tg.m.file {
			byRoute, bad, size := accessLogStats(t, p.Dir)
			lines := 0
			for _, ep := range eps {
				want := int(final.Sum("e2e_http_requests_total", "route", ep.route))
				if byRoute[ep.route] != want {
					t.Errorf("文档说每个请求一条访问日志；%s %s 处理了 %d 个请求，日志文件里只有 %d 条", tg.name, ep.route, want, byRoute[ep.route])
				}
				lines += byRoute[ep.route]
			}
			if bad > 0 {
				t.Errorf("日志文件里有 %d 行不是完整的 JSON：并发写入串行了（xlog/rotate.go 说 Write 加锁保护）", bad)
			}
			t.Logf("%s 日志文件 %.0fMB，%d 条访问日志，平均每条 %d 字节", tg.name, mb(size), lines, size/int64(max(lines, 1)))
		}
	}

	t.Logf("压测结果（每档预热 %v、正式 %v；压测器与服务同机，只作相对比较）：\n%s", loadWarmup(t), loadStep(t), cellTable(cells))
	t.Logf("相对裸 gin：\n%s", overheadTable(cells, "裸 gin"))
}

// overheadTable 每个实验组相对对照组的差：QPS 比、每请求多用的 CPU、p50 / p99 多出来的延迟
func overheadTable(cells []cell, base string) string {
	find := func(target, ep string, conc int) (cell, bool) {
		for _, c := range cells {
			if c.target == target && c.endpoint == ep && c.conc == conc {
				return c, true
			}
		}
		return cell{}, false
	}
	var b strings.Builder
	b.WriteString("| 接口 | 并发 | 目标 | QPS 比 | 服务 CPU µs/请求 | 多用 CPU µs/请求 | 多用 % | p50 多 ms | p99 多 ms |\n")
	b.WriteString("|---|---:|---|---:|---:|---:|---:|---:|---:|\n")
	for _, c := range cells {
		if c.target == base {
			continue
		}
		o, ok := find(base, c.endpoint, c.conc)
		if !ok || o.res.QPS == 0 || o.cpu == 0 {
			continue
		}
		d := c.cpuUS() - o.cpuUS()
		fmt.Fprintf(&b, "| %s | %d | %s | %.2f | %.1f | %+.1f | %+.0f%% | %+.2f | %+.2f |\n",
			c.endpoint, c.conc, c.target, c.res.QPS/o.res.QPS, c.cpuUS(), d, 100*d/o.cpuUS(),
			float64(c.res.P50-o.res.P50)/float64(time.Millisecond), float64(c.res.P99-o.res.P99)/float64(time.Millisecond))
	}
	return b.String()
}

// 从「全关」出发一次只开一个中间件，并发 16 压两个接口，看每请求的 CPU 各多多少。
//
// 「只开访问日志（级别 warn）」对的是 xgin/middleware/log.go 的承诺：级别关掉时直接放行，
// 缓存请求体、截响应、脱敏、序列化一样都不做——那它的开销应当接近零，远小于级别 info 时
func TestLoad_EnableMiddlewaresOneByOneToFindCost(t *testing.T) {
	requireLoad(t)
	table := "e2e_load_" + harness.NewID()
	ids := seedUsers(t, table, 1000)
	eps := loadEndpoints(ids)
	const conc = 16

	variants := []struct {
		name string
		m    mw
	}{
		{"全关", mwAllOff},
		{"只开访问日志", mw{log: true}},
		{"只开访问日志（级别 warn）", mw{log: true, level: "warn"}},
		{"只开访问日志（含 body）", mw{log: true, body: true}},
		{"只开链路", mw{trace: true}},
		// 采样率 0：照样生成、透传 TraceID（日志里的 trace_id、响应头的 X-Trace-Id 都在），
		// 只是 Span 不记录。和上一行的差就是「记录了却没有 exporter 收」的那一份
		{"只开链路（SampleRatio 0）", mw{trace: true, sample: "0"}},
		{"只开链路 + 批量导出", mw{trace: true, export: true}},
		{"只开指标", mw{metric: true}},
		{"默认（三个都开）", mwDefault},
		{"全开", mwAllOn},
	}

	var cells []cell
	for _, v := range variants {
		p := harness.Start(t, v.m.options(table))
		for _, ep := range eps {
			cells = append(cells, measure(t, p, v.name, ep, false, conc))
		}
		p.Terminate(t, 30*time.Second)
	}

	cpu := func(target, ep string) float64 {
		for _, c := range cells {
			if c.target == target && c.endpoint == ep {
				return c.cpuUS()
			}
		}
		t.Fatalf("没有 %s %s 这一档", target, ep)
		return 0
	}

	var b strings.Builder
	b.WriteString("| 配置 | 接口 | QPS | p50 ms | p99 ms | 服务 CPU µs/请求 | 相对全关 µs/请求 |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|\n")
	for _, c := range cells {
		fmt.Fprintf(&b, "| %s | %s | %.0f | %s | %s | %.1f | %+.1f |\n",
			c.target, c.endpoint, c.res.QPS, msf(c.res.P50), msf(c.res.P99), c.cpuUS(), c.cpuUS()-cpu("全关", c.endpoint))
	}
	for _, ep := range eps {
		off := cpu("全关", ep.name)
		sum := cpu("只开访问日志", ep.name) + cpu("只开链路", ep.name) + cpu("只开指标", ep.name) - 3*off
		fmt.Fprintf(&b, "\n%s：三个单独开的增量之和 %+.1fµs，三个一起开实测 %+.1fµs", ep.name, sum, cpu("默认（三个都开）", ep.name)-off)
	}
	t.Logf("中间件逐个打开（并发 %d，每档预热 %v、正式 %v）：\n%s", conc, loadWarmup(t), loadStep(t), b.String())

	// 级别关掉时直接放行：增量不超过级别 info 时的一半。
	//
	// 只在 /ping 上断言：/users/:id 每个请求还要等 PG，PG 的后端进程和被测服务抢同一批核，
	// 同一个配置三轮之间每请求 CPU 差到 5µs（全关 82.5～87.6µs），级别 warn 的增量量出来
	// 从 0 到 8.2µs 都有，和访问日志本身的增量（12.7～18.0µs）是一个量级。
	// /ping 上三轮量下来级别 info 的增量是 5.5～7.2µs、级别 warn 是 0.8～1.6µs（比例不超过 0.22），
	// 一半的线留足了余量
	off := cpu("全关", "/ping")
	info, warn := cpu("只开访问日志", "/ping")-off, cpu("只开访问日志（级别 warn）", "/ping")-off
	if warn > info/2 {
		t.Errorf("xgin/middleware/log.go 说级别关掉时访问日志中间件直接放行；/ping 上级别 info 多用 %.1fµs/请求，级别 warn 仍多用 %.1fµs/请求",
			info, warn)
	}
}

// 默认配置、64 并发，读写混合地持续压几分钟，每 10 秒采一次 RSS、goroutine 数、堆、fd。
//
// 泄漏的样子是「随请求数单调涨」：每请求漏一个 goroutine 的话几分钟就是上百万个，
// 每连接漏一个的话压测的连接数固定，所以还要看压完之后能不能回落到压测前的水平
func TestLoad_DefaultConfig64ConcurrencySoakNoLeak(t *testing.T) {
	requireLoad(t)
	soak := loadKnob(t, "XONE_E2E_SOAK", 3*time.Minute)
	const (
		conc     = 64
		interval = 10 * time.Second
	)
	table := "e2e_load_" + harness.NewID()
	ids := seedUsers(t, table, 1000)
	p := harness.Start(t, mwDefault.options(table))

	// 读写混合：一半走三级缓存读，两成直接读库，两成 /ping，一成改用户（删两级缓存，
	// 下一次读就又落到库上），PG、Redis、本地缓存、访问日志、链路、指标全都在转
	newReq := func(ctx context.Context, i int) (*http.Request, error) {
		// 连续 10 个请求用同一个 id：按 i 打散的话 i%10 定了请求类型也就定了 id 的余数，
		// 改的那一成 id 和读的那几成永远不重合，删缓存就测了个寂寞
		id := ids[(i/10*7919)%len(ids)]
		switch i % 10 {
		case 0, 1, 2, 3, 4:
			return http.NewRequestWithContext(ctx, http.MethodGet, p.URL(fmt.Sprintf("/users/%d", id)), nil)
		case 5, 6:
			return http.NewRequestWithContext(ctx, http.MethodGet, p.URL(fmt.Sprintf("/users/%d?cache=off", id)), nil)
		case 7, 8:
			return http.NewRequestWithContext(ctx, http.MethodGet, p.URL("/ping"), nil)
		default:
			body := fmt.Sprintf(`{"name":"soak-%d","email":"soak-%d@example.com"}`, i, i)
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.URL(fmt.Sprintf("/users/%d", id)), strings.NewReader(body))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
			}
			return req, err
		}
	}

	// 预热 10 秒再记空闲水位：连接池、缓存、各个 sync.Pool 都建起来之后的 goroutine 数才是基线
	warm := harness.Load(t, harness.LoadSpec{Concurrency: conc, Duration: 10 * time.Second, NewRequest: newReq})
	if warm.Errors > 0 || warm.OK() != warm.Requests {
		t.Fatalf("预热就有失败：%v %v", warm, warm.ErrorSamples)
	}
	idle := waitIdle(t, p, 0)
	t.Logf("预热之后空闲：%s", idle)

	samples := make(chan soakSample, 64)
	stop := make(chan struct{})
	go func() {
		defer close(samples)
		start := time.Now()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				samples <- sampleSoak(p, time.Since(start))
			}
		}
	}()
	res := harness.Load(t, harness.LoadSpec{Concurrency: conc, Duration: soak, NewRequest: newReq})
	close(stop)
	var got []soakSample
	for s := range samples {
		got = append(got, s)
	}
	after := waitIdle(t, p, idle.goroutines)

	var b strings.Builder
	b.WriteString("| 时刻 | 累计请求 | 这 10s 的 QPS | RSS MB | goroutine | heap_inuse MB | fd | 线程 |\n")
	b.WriteString("|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	fmt.Fprintf(&b, "| 空闲（压前） | %.0f | | %.1f | %.0f | %.1f | %.0f | %d |\n", idle.requests, mb(idle.rss), idle.goroutines, mb(int64(idle.heap)), idle.fds, idle.threads)
	prev := idle.requests
	for _, s := range got {
		if s.err != "" {
			t.Errorf("%v 那次采样失败：%s", s.at, s.err)
			continue
		}
		fmt.Fprintf(&b, "| %v | %.0f | %.0f | %.1f | %.0f | %.1f | %.0f | %d |\n",
			s.at.Round(time.Second), s.requests, (s.requests-prev)/interval.Seconds(), mb(s.rss), s.goroutines, mb(int64(s.heap)), s.fds, s.threads)
		prev = s.requests
	}
	fmt.Fprintf(&b, "| 空闲（压后） | %.0f | | %.1f | %.0f | %.1f | %.0f | %d |\n", after.requests, mb(after.rss), after.goroutines, mb(int64(after.heap)), after.fds, after.threads)
	t.Logf("浸泡 %v（默认配置，并发 %d）：%v\n%s", soak, conc, res, b.String())

	if res.Errors > 0 || res.OK() != res.Requests {
		t.Errorf("浸泡期间不该有失败的请求：%d 个里传输层失败 %d 个、非 2xx %d 个，状态码 %v，错误样本 %v",
			res.Requests, res.Errors, res.Requests-res.Errors-res.OK(), res.Status, res.ErrorSamples)
	}
	checkSoak(t, conc, idle, after, got)

	exit := p.Terminate(t, 30*time.Second)
	if exit.Code != 0 || exit.Signal != nil {
		t.Errorf("浸泡之后 SIGTERM 应以 0 退出，实际 %v\n%s", exit, p.Stderr())
	}
}

// soakSample 浸泡期间的一次采样
type soakSample struct {
	at                    time.Duration
	requests              float64 // xgin 请求计数的总和，除掉 /metrics 自己
	rss                   int64
	threads               int
	goroutines, heap, fds float64
	err                   string
}

func (s soakSample) String() string {
	return fmt.Sprintf("goroutine %.0f、RSS %.1fMB、heap_inuse %.1fMB、fd %.0f、线程 %d", s.goroutines, mb(s.rss), mb(int64(s.heap)), s.fds, s.threads)
}

// sampleClient 采样用的客户端：服务卡住时采样也不跟着卡住
var sampleClient = &http.Client{Timeout: 5 * time.Second}

// sampleSoak 采一次样。在压测协程之外的协程里跑，所以不带 t：出错记进 err
func sampleSoak(p *harness.Process, at time.Duration) soakSample {
	s := soakSample{at: at}
	st, err := harness.ReadProc(p.Pid())
	if err != nil {
		s.err = err.Error()
		return s
	}
	s.rss, s.threads = st.RSS, st.Threads
	resp, err := sampleClient.Get(p.URL("/metrics"))
	if err != nil {
		s.err = err.Error()
		return s
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		s.err = err.Error()
		return s
	}
	m, err := harness.ParseMetrics(bytes.NewReader(body))
	if err != nil {
		s.err = err.Error()
		return s
	}
	s.goroutines = m.Sum("go_goroutines")
	s.heap = m.Sum("go_memstats_heap_inuse_bytes")
	s.fds = m.Sum("process_open_fds")
	s.requests = m.Sum("e2e_http_requests_total") - m.Sum("e2e_http_requests_total", "route", "/metrics")
	return s
}

// waitIdle 压测停下之后等 goroutine 数回落：压测器关掉连接之后，服务端每条连接的协程
// 要读到 EOF 才退出。等到不超过 want + idleSlack（want 为 0 时只等它连续两次不再变），最多 15 秒
func waitIdle(t *testing.T, p *harness.Process, want float64) soakSample {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last soakSample
	for {
		time.Sleep(time.Second)
		s := sampleSoak(p, 0)
		if s.err != "" {
			t.Fatalf("采样失败：%s", s.err)
		}
		settled := want == 0 && last.goroutines == s.goroutines && last.fds == s.fds
		if settled || (want > 0 && s.goroutines <= want+idleSlack) || time.Now().After(deadline) {
			return s
		}
		last = s
	}
}

// idleSlack 下面几个判据共用的余量（个）。三轮浸泡量下来，压完之后的 goroutine 数
// 和压前完全一样（都是 13），10 是给偶尔起落的后台协程留的
const idleSlack = 10

// checkSoak 判有没有泄漏。泄漏的两种样子：
//
//   - 随请求数涨：浸泡几分钟是两百多万个请求，每十万个漏一个也有二十几个。
//     看压完之后 goroutine 能不能回落到压前 + idleSlack 以内
//   - 随连接数涨：压测的连接数固定是 conc，所以压的过程中数不会涨，要看压完之后
//     那 conc 条连接占的 fd 有没有还回来：压完的 fd ≤ 浸泡末尾的 fd − conc + idleSlack。
//     不和压前比：PG / Redis 的连接池在浸泡中途还会再涨几条（量到过压前 96、压后 99），
//     它们有上限，不算漏
//
// 压的过程中另有几条上限，挡的是「涨到把进程拖垮」：
//
//   - goroutine ≤ 压前 + 3×conc：每条连接一个 serve 协程，handler 在跑时 net/http
//     再起一个后台读（server.go startBackgroundRead），此外只有请求前后一闪而过的
//     （pgx 的后台读、定时器回调；pgx v5.10 的 ctx 监视用的是 context.AfterFunc，不起协程）。
//     三轮量下来是压前 + 64～135，也就是每条连接最多 2.1 个，所以上限按每条连接 3 个给
//   - 稳定段（浸泡一分钟之后，连接池、sync.Pool、ristretto 的缓冲都摊开了）后半段
//     RSS 的最大值不超过前半段最大值的 1.15 倍加 8MB。三轮量下来 RSS 在 56.6～61.5MB，
//     一轮之内晃不出 3MB
//   - heap_inuse 看每半段的最小值，也就是 GC 刚做完时的「底」：采样落在 GC 周期的哪一刻
//     是随机的，最大值在 18～26MB 之间来回跳，底只在 18.3～19.6MB。
//     后半段的底不超过前半段的底的 1.25 倍加 4MB
func checkSoak(t *testing.T, conc int, idle, after soakSample, got []soakSample) {
	t.Helper()
	var ok, steady []soakSample
	for _, s := range got {
		if s.err != "" {
			continue
		}
		ok = append(ok, s)
		if s.at >= time.Minute {
			steady = append(steady, s)
		}
	}
	if len(steady) < 4 {
		t.Fatalf("稳定段只采到 %d 个样本，判不了有没有泄漏（XONE_E2E_SOAK 至少给 2m）", len(steady))
	}

	if after.goroutines > idle.goroutines+idleSlack {
		t.Errorf("压完之后 goroutine 没有回落：压前空闲 %.0f 个，压后空闲 %.0f 个", idle.goroutines, after.goroutines)
	}
	if last := ok[len(ok)-1]; after.fds > last.fds-float64(conc)+idleSlack {
		t.Errorf("压完之后 %d 条连接的 fd 没有还回来：浸泡末尾 %.0f 个，压后空闲 %.0f 个", conc, last.fds, after.fds)
	}
	limit := idle.goroutines + float64(3*conc)
	for _, s := range ok {
		if s.goroutines > limit {
			t.Errorf("%v 时 goroutine %.0f 个，超过了压前 %.0f + 每连接 3 个的上限 %.0f", s.at, s.goroutines, idle.goroutines, limit)
		}
	}

	half := len(steady) / 2
	fold := func(ss []soakSample, f func(soakSample) float64, pick func(a, b float64) float64) float64 {
		m := f(ss[0])
		for _, s := range ss[1:] {
			m = pick(m, f(s))
		}
		return m
	}
	hi := func(a, b float64) float64 { return max(a, b) }
	lo := func(a, b float64) float64 { return min(a, b) }
	rss := func(s soakSample) float64 { return float64(s.rss) }
	heap := func(s soakSample) float64 { return s.heap }
	if a, b := fold(steady[:half], rss, hi), fold(steady[half:], rss, hi); b > a*1.15+8<<20 {
		t.Errorf("RSS 在稳定段里还在涨：前半段最高 %.1fMB，后半段最高 %.1fMB", a/(1<<20), b/(1<<20))
	}
	if a, b := fold(steady[:half], heap, lo), fold(steady[half:], heap, lo); b > a*1.25+4<<20 {
		t.Errorf("heap_inuse 的底在稳定段里还在涨：前半段最低 %.1fMB，后半段最低 %.1fMB", a/(1<<20), b/(1<<20))
	}
}
