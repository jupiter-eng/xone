package e2e

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// 优雅退出这一类：真的进程、真的信号、真的 PG / Redis。
// 对照的承诺出自 docs/architecture.md「退出信号：从进程起步的第一毫秒就接管」「停止预算是一份」
// 两节，和 xgin/README.md 里 XGin 的停止说明、xone.WithStopTimeout 的注释。

// TestShutdown_SIGTERMUnderLoad_FinishesInFlight_RefusesNew_ExitsZero
//
// 文档的承诺（docs/architecture.md「停止预算是一份」、xgin/README.md XGin 一节）：
//   - Stop 等在途请求做完，最多等到服务那一段预算（WithStopTimeout 的 2/3）；
//   - 服务那一段（Stop + 等 Start 返回）结束之后才关数据库和缓存，在途请求摸不到已经关掉的连接池；
//   - 整个退出流程在 WithStopTimeout 之内结束，服务自己按要求退出是 0。
//
// 在途请求是 xgin.Stop 自己等的（Shutdown 之后再等 handler 返回）：xgin 的 Start 在
// Shutdown 一开始就返回了，所以这个用例测不到 Run 里「等 Start 返回」那一半，
// 那一半见 TestShutdown_StartReturnsBeforeDBAndCacheClose。
//
// 流量：/slow?ms=300 ×16（看 ctx 的慢请求）、读 PG 的 GET /users/:id?cache=off ×4、
// 写 PG 再删 Redis 的 PUT /users/:id ×2、不看 ctx 睡 300ms 再查库和 Redis 的 /stuck ×4。
// 最后这一路是「资源关早了」最灵敏的探针：它在信号之后 300ms 才去碰 PG / Redis，
// 框架要是没等 handler 返回就关了它们，它的日志里 db_error / redis_error 就不是 none。
func TestShutdown_SIGTERMUnderLoad_FinishesInFlight_RefusesNew_ExitsZero(t *testing.T) {
	harness.Require(t)
	const budget = 5 * time.Second
	p := harness.Start(t, harness.Options{Overlay: stopTimeout(budget)})

	var ids []int64
	for i := range 20 {
		ids = append(ids, createUser(t, p, fmt.Sprintf("u%d", i), fmt.Sprintf("u%d@example.com", i)).ID)
	}
	tr := startTraffic(
		lane{name: "slow", conc: 16, req: getReq(p.URL("/slow?ms=300"))},
		lane{name: "pg-read", conc: 4, req: func(i int) (*http.Request, error) {
			return http.NewRequest(http.MethodGet, p.URL(fmt.Sprintf("/users/%d?cache=off", ids[i%len(ids)])), nil)
		}},
		lane{name: "pg-write", conc: 2, req: func(i int) (*http.Request, error) {
			body := fmt.Sprintf(`{"name":"w%d","email":"w%d@example.com"}`, i, i)
			req, err := http.NewRequest(http.MethodPut, p.URL(fmt.Sprintf("/users/%d", ids[i%len(ids)])), strings.NewReader(body))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
			}
			return req, err
		}},
		lane{name: "stuck-db", conc: 4, req: getReq(p.URL("/stuck?ms=300&db=1&redis=1"))},
	)
	// 先跑满一秒：每一路都有请求在途，连接池、缓存都热了
	time.Sleep(time.Second)
	gauges := p.Metrics(t)
	slowInflight, stuckInflight := gauges.Sum("e2e_slow_inflight"), gauges.Sum("e2e_stuck_inflight")

	sigAt := time.Now()
	p.Signal(syscall.SIGTERM)
	probeCtx, stopProbe := context.WithCancel(context.Background())
	probesCh := probeNewConns(probeCtx, p.URL("/ping"), 5*time.Millisecond)

	exit, ok := p.Wait(budget + 5*time.Second)
	exitAt := time.Now()
	stopProbe()
	probes := <-probesCh
	shots := tr.stop()
	if !ok {
		t.Fatalf("文档说整个退出流程不超过 WithStopTimeout=%v，实际发出 SIGTERM %v 之后进程还活着", budget, budget+5*time.Second)
	}

	// ---- 信号那一刻的在途请求：每一个都拿到 200 ----
	//
	// guard：信号前最后这一小段发出的请求不算「已经开始」。net/http 的 Shutdown 一上来
	// 就关掉空闲的长连接，客户端刚写进这种连接、服务端还没读到的请求，在服务端看来
	// 根本没开始——客户端（GET 可重放）会换一条新连接重试，撞上已经关掉的监听，
	// 拿到 connection refused。这是 net/http 的语义，不是框架的承诺，所以这一段
	// 只要求「不挂住」，失败的个数单独打出来。信号从发出到 Shutdown 关掉监听和空闲连接
	// 是毫秒级（第一次被拒的探测在信号后 5~6ms，探测每 5ms 一次），20ms 是它的几倍。
	// 实测这 20ms 里每轮发出 150~170 个请求，一个都没失败过——竞态存在，只是很难撞上
	const guard = 20 * time.Millisecond
	type laneStat struct{ pre, inflight, window, windowBad, post, postOK int }
	stats := map[string]*laneStat{}
	var preBad, postBad, hung []reqShot
	var postErrs []string
	var lastInflightEnd time.Time
	for _, s := range shots {
		st := stats[s.stream]
		if st == nil {
			st = &laneStat{}
			stats[s.stream] = st
		}
		ok := s.err == nil && s.status == http.StatusOK
		if s.start.Before(sigAt) && s.end.After(sigAt) {
			st.inflight++
			lastInflightEnd = later(lastInflightEnd, s.end)
		}
		switch {
		case s.start.Before(sigAt.Add(-guard)):
			st.pre++
			if !ok {
				preBad = append(preBad, s)
			}
		case s.start.Before(sigAt):
			st.window++
			if !ok {
				st.windowBad++
			}
		default:
			st.post++
			switch {
			case ok:
				st.postOK++
			case s.err == nil:
				// 信号之后被接下的（端口还没关的那一瞬间）也要照常做完
				postBad = append(postBad, s)
			case len(postErrs) < 3 && !slices.Contains(postErrs, errKind(s.err)):
				postErrs = append(postErrs, errKind(s.err))
			}
		}
		// 最长的正常请求是 300ms 的 slow / stuck。超过 1.5s 的只能是挂住了
		if s.end.After(sigAt) && s.latency() > 1500*time.Millisecond {
			hung = append(hung, s)
		}
	}
	for _, name := range []string{"slow", "pg-read", "pg-write", "stuck-db"} {
		st := stats[name]
		if st == nil {
			t.Fatalf("%s 这一路一个请求都没发出去", name)
		}
		t.Logf("%-8s 信号前发出 %5d，信号时刻在途 %2d；信号前 %v 内发出 %3d（失败 %d）；信号后发出 %4d（200 的 %d 个，其余出错）",
			name, st.pre, st.inflight, guard, st.window, st.windowBad, st.post, st.postOK)
	}
	t.Logf("信号前一刻的指标：e2e_slow_inflight=%v e2e_stuck_inflight=%v；信号后出错的样子：%v", slowInflight, stuckInflight, postErrs)
	// 在途的太少，这个用例就没测到东西。16 / 4 个 worker 连着发 300ms 的请求，实测在途正好 16 / 4
	if stats["slow"].inflight < 8 || stats["stuck-db"].inflight < 2 {
		t.Fatalf("信号时刻在途的请求太少（slow %d、stuck-db %d），测不出优雅退出", stats["slow"].inflight, stats["stuck-db"].inflight)
	}
	if len(preBad) > 0 {
		t.Errorf("文档说 Stop 等在途请求做完，实际信号之前发出的请求里有 %d 个没拿到 200，前几个：%v", len(preBad), firstN(preBad, 5))
	}
	if len(postBad) > 0 {
		t.Errorf("信号之后被接下的请求应当照常做完（200），实际 %d 个不是，前几个：%v", len(postBad), firstN(postBad, 5))
	}
	if len(hung) > 0 {
		t.Errorf("信号之后不该有挂住的请求，实际 %d 个超过 1.5s，前几个：%v", len(hung), firstN(hung, 5))
	}

	// ---- 新连接：端口马上关掉，被拒而不是挂住 ----
	firstRefused := -1
	var probeMax time.Duration
	for i, pr := range probes {
		probeMax = max(probeMax, pr.latency)
		if firstRefused < 0 && pr.refused() {
			firstRefused = i
		}
		if firstRefused >= 0 && pr.err == nil {
			t.Errorf("端口关掉之后又有新连接拿到了响应（第 %d 次探测，status=%d）", i, pr.status)
		}
	}
	if firstRefused < 0 {
		t.Fatalf("信号之后的 %d 次新连接探测没有一次被拒（connection refused）：端口在进程退出之前一直开着", len(probes))
	}
	refusedAfter := probes[firstRefused].at.Sub(sigAt)
	t.Logf("新连接探测 %d 次：第一次被拒在信号后 %s，此前 %d 次拿到响应；单次最长 %s",
		len(probes), fmtMS(refusedAfter), firstRefused, fmtMS(probeMax))
	if probeMax > time.Second {
		t.Errorf("信号之后新连接应被拒或很快出错，实际有一次探测等了 %v", probeMax)
	}
	// 监听要在一开始就关，不是等在途请求做完、进程退出时才顺带关
	if !probes[firstRefused].at.Before(lastInflightEnd) {
		t.Errorf("端口应在在途请求做完之前就不再接新连接：第一次被拒在信号后 %s，最后一个在途请求在信号后 %s 才做完",
			fmtMS(refusedAfter), fmtMS(lastInflightEnd.Sub(sigAt)))
	}

	// ---- 在途请求碰到的 PG / Redis 都还开着 ----
	for _, pat := range afterCloseErrors {
		if strings.Contains(p.Output(), pat) {
			t.Errorf("文档说服务停干净之后才关数据库和缓存，实际输出里出现了 %q：\n%s", pat, grepLines(p.Output(), pat, 5))
		}
	}
	stuckDone := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "stuck request finished" })
	var stuckAfterSignal int
	for _, l := range stuckDone {
		if l.Str("db_error") != "none" || l.Str("redis_error") != "none" {
			t.Errorf("在途的 /stuck 睡完再碰 PG / Redis 应当成功，实际 db_error=%q redis_error=%q", l.Str("db_error"), l.Str("redis_error"))
		}
		if ts, err := time.Parse(time.RFC3339Nano, l.Str("time")); err == nil && ts.After(sigAt) {
			stuckAfterSignal++
		}
	}
	if stuckAfterSignal == 0 {
		t.Errorf("信号之后没有一个 /stuck 做完：在途的那几个要么没等就被关了，要么日志没写出来")
	}
	for _, l := range p.Logs() {
		if l.Level() == "ERROR" {
			t.Errorf("优雅退出期间不该有 ERROR 日志，实际：%s", l.Line)
		}
	}

	// ---- 退出码与耗时 ----
	sinceSig := exit.SinceSignal
	// 拆开看时间花在哪：第一条 stopping 说明服务那一段（Stop + 等 Start 返回）结束了，
	// 最后一条是 xlog 自己的
	if stops := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "stopping" }); len(stops) > 0 {
		first, _ := time.Parse(time.RFC3339Nano, stops[0].Str("time"))
		last, _ := time.Parse(time.RFC3339Nano, stops[len(stops)-1].Str("time"))
		t.Logf("服务那一段在信号后 %s 结束（比最后一个在途请求晚 %s），%d 个停止钩子从第一个到 xlog 用了 %s",
			fmtMS(first.Sub(sigAt)), fmtMS(first.Sub(lastInflightEnd)), len(stops), fmtMS(last.Sub(first)))
	}
	t.Logf("退出：%v；信号 → 最后一个在途请求做完 %s，→ 进程退出 %s（预算 %v）；信号后 /stuck 做完 %d 个",
		exit, fmtMS(lastInflightEnd.Sub(sigAt)), fmtMS(sinceSig), budget, stuckAfterSignal)
	if exit.Code != 0 || exit.Signal != nil {
		t.Errorf("按要求退出、在途请求都做完了，文档说以 0 退出，实际 %v\nstderr:\n%s", exit, lastLines(p.Stderr(), 20))
	}
	if sinceSig > budget {
		t.Errorf("文档说整个退出流程不超过 WithStopTimeout=%v，实际信号之后 %v 才退出", budget, sinceSig)
	}
	// 在途请求最长 300ms，做完就该往下走，而不是把预算等满。
	// 实测最后一个在途请求做完到进程退出 65~85ms：其中 60~80ms 是 net/http 的 Shutdown
	// 轮询「连接都空闲了没有」的间隔（从 1ms 起翻倍、封顶 500ms，等了 200ms 时一次要隔
	// 上百毫秒），6 个停止钩子一共 4~7ms。给 1s：真把预算等满的话这里是 3s 以上
	if tail := exitAt.Sub(lastInflightEnd); lastInflightEnd.IsZero() || tail > time.Second {
		t.Errorf("在途请求做完之后应当很快退出，实际最后一个在途请求做完 %v 之后进程才退出", tail)
	}
}

// TestShutdown_StartReturnsBeforeDBAndCacheClose
//
// 文档（docs/architecture.md「停止预算是一份」、xone.Runnable 与 Run 的注释）：服务那一段是
// 「Stop + 等 Start 返回」；Stop 返回不等于服务已经停干净，框架等 Start 真正返回
// 才关其余组件，不然还在跑的活会摸到已经关掉的数据库和缓存。
//
// 服务配 Service.Drain：Start 在 xgin 返回之后再收 drain 这么久的尾（不看 ctx），
// 然后查一次 PG、Ping 一次 Redis，记一条 drain finished（见 service/drain.go）。
// xgin.Stop 没有在途请求要等，一上来就返回——此后还没关组件，全靠 Run 在等 Start。
// 框架要是 Stop 一返回就去关组件，收尾要么摸到关掉的连接池，要么进程先退出、它根本没做完
func TestShutdown_StartReturnsBeforeDBAndCacheClose(t *testing.T) {
	harness.Require(t)
	const (
		budget = 5 * time.Second
		drain  = 500 * time.Millisecond
	)
	p := harness.Start(t, harness.Options{Overlay: fmt.Sprintf("Service:\n  StopTimeout: %v\n  Drain: %v\n", budget, drain)})

	exit := p.Terminate(t, budget+5*time.Second)
	t.Logf("退出：%v（收尾 %v，预算 %v）", exit, drain, budget)
	if exit.Code != 0 || exit.Signal != nil {
		t.Errorf("收尾做完、组件关干净，应以 0 退出，实际 %v\nstderr:\n%s", exit, lastLines(p.Stderr(), 20))
	}

	drains := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "drain finished" })
	if len(drains) != 1 {
		t.Fatalf("文档说框架等 Start 真正返回才关其余组件，实际 Start 的收尾没做完进程就退出了（drain finished %d 条），退出在信号后 %v\n%s",
			len(drains), exit.SinceSignal, lastLines(p.Output(), 20))
	}
	d := drains[0]
	if d.Str("db_error") != "none" || d.Str("mysql_error") != "none" || d.Str("redis_error") != "none" {
		t.Errorf("Start 返回之前 PG / MySQL / Redis 应当都还开着，实际收尾时 db_error=%q mysql_error=%q redis_error=%q",
			d.Str("db_error"), d.Str("mysql_error"), d.Str("redis_error"))
	}
	drainAt, _ := time.Parse(time.RFC3339Nano, d.Str("time"))
	stops := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "stopping" })
	if len(stops) == 0 {
		t.Fatalf("没有一条 stopping 日志\n%s", lastLines(p.Output(), 30))
	}
	firstStop, _ := time.Parse(time.RFC3339Nano, stops[0].Str("time"))
	t.Logf("收尾做完在第一个停止钩子（%s）之前 %s", stops[0].Str("hook"), fmtMS(firstStop.Sub(drainAt)))
	if !firstStop.After(drainAt) {
		t.Errorf("停止钩子应在 Start 返回之后才开始，实际 %s 在收尾做完之前 %s 就开始了", stops[0].Str("hook"), fmtMS(drainAt.Sub(firstStop)))
	}
	if exit.SinceSignal < drain || exit.SinceSignal > budget {
		t.Errorf("退出应在收尾（%v）之后、预算（%v）之内，实际信号之后 %v", drain, budget, exit.SinceSignal)
	}
}

// TestShutdown_CloseOrderIsReverseStartOrder_XLogLast
//
// 文档：「按阶段初始化、逆序关闭」；docs/architecture.md「停止预算是一份」里「排在最后的 xlog 总还有时间关文件」。
// xlog 最后关，前面每个组件的关闭日志才写得出去——所以除了 stdout 里的顺序，
// 还核对日志文件：每一条 stopping（包括 xlog 自己那条）都要落进文件
func TestShutdown_CloseOrderIsReverseStartOrder_XLogLast(t *testing.T) {
	harness.Require(t)
	p := harness.Start(t, harness.Options{LogFile: true})
	createUser(t, p, "order", "order@example.com")

	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 || exit.Signal != nil {
		t.Fatalf("SIGTERM 之后应以 0 退出，实际 %v\n%s", exit, lastLines(p.Output(), 30))
	}

	starts, stops := hookSeq(p, "starting"), hookSeq(p, "stopping")
	t.Logf("启动顺序：%v", starts)
	t.Logf("关闭顺序：%v", stops)
	if len(stops) == 0 {
		t.Fatalf("没有一条 stopping 日志\n%s", lastLines(p.Output(), 30))
	}

	// 每个停止钩子配对的启动钩子，在启动顺序里的位置必须严格递减
	prev := len(starts)
	for _, s := range stops {
		i, ok := startIndex(starts, s)
		if !ok {
			t.Errorf("停止钩子 %s 所在的包没有启动过", s)
			continue
		}
		if i >= prev {
			t.Errorf("文档说逆序关闭，实际 %s 比启动得更晚的组件先关：关闭顺序 %v", s, stops)
		}
		prev = i
	}
	if want := expectedStops(starts); !slices.Equal(pkgsOf(stops), want) {
		t.Errorf("关闭的应当正好是启动过的、有停止钩子的那些，按逆序：want %v，got %v"+
			"（框架给别的包加了停止钩子的话，把它补进 stopHookPkgs）", want, pkgsOf(stops))
	}
	if last := stops[len(stops)-1]; pkgOf(last) != "xlog" {
		t.Errorf("文档说 xlog 排在最后关，实际最后一个是 %s", last)
	}

	// 日志文件里要有全部的 stopping，包括 xlog 自己那条：它关的时候文件还开着
	var inFile []string
	for _, l := range p.FileLogs(t) {
		if l.Msg() == "stopping" {
			inFile = append(inFile, l.Str("hook"))
		}
	}
	if !slices.Equal(inFile, stops) {
		t.Errorf("xlog 最后关，前面每个组件的关闭日志都该落进文件：stdout 里是 %v，文件里是 %v", stops, inFile)
	}
}

// TestShutdown_CtxIgnoringHandlerInFlight_ReportsRunningCount_ExitsInBudget
//
// 文档（xgin/README.md XGin 一节）：
//  1. Shutdown 只用到截止时间前的一截（留出剩余时间的 20%，最多 1s），到那时还有请求就 Close() 断开所有连接；
//  2. 留出来的那一截等 handler 真正返回，看到 ctx 取消就收尾的 handler 在这里做完；
//  3. 到截止时间还有 handler 没返回时，错误里写明还剩几个（N handler(s) still running…）；
//  4. 不看 ctx 的 handler 框架停不下来，但退出流程照样在 WithStopTimeout 之内走完。
//
// WithStopTimeout=3s：服务那一段是 2s，Close 落在约 2s − 0.4s = 1.6s。
// 两个 /stuck（不看 ctx）加一个 /slow（看 ctx），错误里应当正好是 2 个
func TestShutdown_CtxIgnoringHandlerInFlight_ReportsRunningCount_ExitsInBudget(t *testing.T) {
	harness.Require(t)
	const (
		budget     = 3 * time.Second
		serverPart = budget - budget/3         // 2s
		closeAt    = serverPart - serverPart/5 // 1.6s：剩余时间的 20% 是 0.4s，不到 1s
		tolerance  = 300 * time.Millisecond    // 实测误差在 20ms 以内（信号投递 + 10ms 的轮询间隔）
	)
	p := harness.Start(t, harness.Options{Overlay: stopTimeout(budget)})

	type result struct {
		path string
		at   time.Time
		resp harness.Response
		err  error
	}
	results := make(chan result, 3)
	for _, path := range []string{"/stuck?ms=20000", "/stuck?ms=20000", "/slow?ms=20000"} {
		go func() {
			r, err := p.Request(context.Background(), http.MethodGet, path, nil)
			results <- result{path, time.Now(), r, err}
		}()
	}
	waitGauge(t, p, "e2e_stuck_inflight", 2, 5*time.Second)
	waitGauge(t, p, "e2e_slow_inflight", 1, 5*time.Second)

	sigAt := time.Now()
	p.Signal(syscall.SIGTERM)
	exit, ok := p.Wait(budget + 5*time.Second)
	if !ok {
		t.Fatalf("文档说不看 ctx 的 handler 也拖不住退出流程（WithStopTimeout=%v），实际信号之后 %v 进程还活着",
			budget, budget+5*time.Second)
	}
	t.Logf("退出：%v（预算 %v，服务那一段 %v）", exit, budget, serverPart)

	// 1. 到 Close 那一刀，三个请求的连接都被断开，客户端拿不到响应
	for range 3 {
		r := <-results
		cut := r.at.Sub(sigAt)
		t.Logf("%s：信号后 %s 结束，err=%v status=%d", r.path, fmtMS(cut), r.err, r.resp.Status)
		if r.err == nil {
			t.Errorf("%s 到点还没做完，文档说 Close() 断开所有连接，实际客户端拿到了响应 %v", r.path, r.resp)
		}
		if cut < closeAt-tolerance || cut > closeAt+tolerance {
			t.Errorf("%s：文档说 Shutdown 用到截止时间前 20%%（约信号后 %v）就断开连接，实际信号后 %v", r.path, closeAt, cut)
		}
	}

	// 2. 看 ctx 的 /slow 在留出来的那一截里收尾
	if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "slow request cancelled" })) != 1 {
		t.Errorf("断连之后看 ctx 的 handler 应当收尾，实际没有 slow request cancelled 这条日志")
	}

	// 3. 错误里写明还剩几个：两个 /stuck，不含已经收尾的 /slow
	const want = "2 handler(s) still running when the shutdown deadline passed"
	if !strings.Contains(p.Stderr(), want) {
		t.Errorf("文档说错误里写明还剩几个（%q），实际 stderr：\n%s", want, lastLines(p.Stderr(), 10))
	}
	// MustRun 把 Run 的错误打到 stderr 并以 1 退出
	if exit.Code != 1 {
		t.Errorf("Stop 返回了错误，MustRun 应以 1 退出，实际 %v", exit)
	}

	// 4. 在预算内退出，而且确实等满了服务那一段（没有一上来就放弃）
	if exit.SinceSignal > budget {
		t.Errorf("文档说整个退出流程不超过 WithStopTimeout=%v，实际信号之后 %v 才退出", budget, exit.SinceSignal)
	}
	if exit.SinceSignal < serverPart-tolerance {
		t.Errorf("文档说 Stop 最多等到服务那一段（%v）的截止时间，实际信号之后 %v 就退出了", serverPart, exit.SinceSignal)
	}
	// 服务没停干净也照样关组件：xlog 最后关
	stops := hookSeq(p, "stopping")
	if want := expectedStops(hookSeq(p, "starting")); !slices.Equal(pkgsOf(stops), want) {
		t.Errorf("服务那一段超时之后，组件照样逆序关：want %v，got %v", want, stops)
	}
}

// TestShutdown_SecondSignalKillsStuckShutdown
//
// 文档（docs/architecture.md「退出信号」第 3 条）：第一个信号之后把默认处置还回去，框架卡在
// 不看 ctx 的调用里时，再发一次信号进程立即终止；「第 3 条同样覆盖关闭阶段：Stop 卡住时，第二次 Ctrl+C 有用」。
//
// 卡住的办法：WithStopTimeout=60s，一个 /stuck?ms=60000 在途——Stop 要等 40s 才放弃
func TestShutdown_SecondSignalKillsStuckShutdown(t *testing.T) {
	harness.Require(t)
	cases := []struct {
		name          string
		first, second syscall.Signal
	}{
		{"SIGTERM之后再SIGTERM", syscall.SIGTERM, syscall.SIGTERM},
		{"SIGTERM之后再SIGINT", syscall.SIGTERM, syscall.SIGINT},
		{"SIGINT之后再SIGINT", syscall.SIGINT, syscall.SIGINT},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := harness.Start(t, harness.Options{Overlay: stopTimeout(time.Minute)})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { _, _ = p.Request(ctx, http.MethodGet, "/stuck?ms=60000", nil) }()
			waitGauge(t, p, "e2e_stuck_inflight", 1, 5*time.Second)

			p.Signal(c.first)
			l := p.WaitLog(t, 5*time.Second, func(l harness.Log) bool {
				return strings.HasPrefix(l.Msg(), "shutdown signal received, closing gracefully")
			})
			if got := l.Str("signal"); got != c.first.String() {
				t.Errorf("日志里的 signal 应是 %q，实际 %q", c.first.String(), got)
			}
			// 确认它真的卡在 Stop 里了：不然第二个信号测的就不是「卡住时」
			if e, exited := p.Wait(500 * time.Millisecond); exited {
				t.Fatalf("在途的 /stuck 应当让 Stop 卡住（最多 40s），实际第一个信号之后就退出了：%v", e)
			}

			secondAt := time.Now()
			p.Signal(c.second)
			e, ok := p.Wait(5 * time.Second)
			took := time.Since(secondAt)
			if !ok {
				t.Fatalf("文档说卡住时再发一次信号进程立即终止，实际第二个 %v 之后 5s 还活着", c.second)
			}
			t.Logf("第二个 %v → 进程终止 %s；%v", c.second, fmtMS(took), e)
			if e.Signal != c.second {
				t.Errorf("第二个信号应走系统默认处置、由 %v 直接终止进程，实际 %v", c.second, e)
			}
			// 实测 1~3ms，给到 2s：它要么立刻死，要么就是没还回默认处置（那会等满 40s）
			if took > 2*time.Second {
				t.Errorf("文档说再发一次信号进程立即终止，实际等了 %v", took)
			}
		})
	}
}

// TestShutdown_SIGTERMDuringStartup_NoServe_ClosesBuiltInReverse_ExitsZero
//
// 文档（docs/architecture.md「退出信号」第 1、2 条）：信号在读配置之前就接管；钩子收 ctx，收到信号后
// 不再跑剩下的启动钩子，已建好的逆序关干净，服务不再启动，然后以 0 退出——按要求退出
// 不是故障，不报成启动失败。被打断的那个钩子靠它自己把 ctx 传下去：xgorm/README.md 说
// PostgreSQL 的建连受 ctx 管；xredis.New 的注释说「收到退出信号就该当场放弃」，
// ping 的注释说「parent 取消时立即放弃」。
//
// 让建连慢下来：PG 或 Redis 经 TCP 代理，代理把对端的回话压住不放（SetDelay(time.Hour)），
// TCP 秒连、握手永远等不到回复。等日志里出现那个钩子的 starting、代理收到了连接，再发信号。
//
// 这两例里被打断的建连看到 ctx 取消就返回错误，runStart 走的是出错那条路——第 2 条的
// 「框架在每个钩子之前再查一次」一次都轮不到（把那次复查删掉，这两例照样全过）。
// 所以还有第三例：一个不看 ctx 的启动钩子（warmup，Service.StartStall）睡着时收到信号，
// 睡完照样成功返回；它算建好了，框架得自己在下一个钩子之前停下
func TestShutdown_SIGTERMDuringStartup_NoServe_ClosesBuiltInReverse_ExitsZero(t *testing.T) {
	harness.Require(t)
	// immediate「当场放弃」的上限。PG 实测信号之后 2~3ms 退出（pgx 的读在 ctx 取消时立刻返回），
	// 200ms 是它的几十倍，又远小于 Redis 默认 500ms 的 ReadTimeout
	const immediate = 200 * time.Millisecond
	// stall warmup 不看 ctx 睡多久。它打断不了，退出的上限是睡满再加 immediate
	const stall = 500 * time.Millisecond
	cases := []struct {
		name   string
		hook   string // 卡住的那个启动钩子
		target string // 经代理压住回话的对端；空表示不用代理
		opts   func(addr string) harness.Options
		stall  time.Duration // warmup 启动钩子不看 ctx 地睡多久（Service.StartStall），0 是不睡
		// completes 卡住的那个钩子不看 ctx、睡完照样成功返回：它算建好了，要跟着关
		completes bool
		limit     time.Duration // 信号之后多久之内退出，0 表示 immediate
	}{
		{name: "PG建连中", hook: "xgorm.initXGorm", target: harness.PGAddr(),
			opts: func(a string) harness.Options { return harness.Options{PGAddr: a} }},
		{name: "不看ctx的启动钩子", hook: "warmup.preload", completes: true, limit: stall + immediate, stall: stall,
			opts: func(string) harness.Options { return harness.Options{} }},
		{name: "Redis建连中", hook: "xredis.initXRedis", target: harness.RedisAddr(),
			// go-redis 只认 ctx 的截止时间、取消叫不醒阻塞在读上的 Ping，xredis 的探测因此放在协程里跑、
			// 这边看着 ctx：从前信号要等这次读撞上 ReadTimeout 才生效（默认 500ms 时约 450ms，配 3s 时约 2.95s）。
			// ReadTimeout 配成 3s：比 immediate 大一个量级，退回老样子一眼看得出来
			opts: func(a string) harness.Options {
				return harness.Options{RedisAddr: a, Overlay: "XRedis:\n  ReadTimeout: 3s\n"}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var proxy *harness.Proxy
			var o harness.Options
			if c.target != "" {
				proxy = harness.NewProxy(t, c.target)
				proxy.SetDelay(time.Hour)
				o = c.opts(proxy.Addr())
			} else {
				o = c.opts("")
			}
			o.NoWait = true
			o.Overlay += fmt.Sprintf("Service:\n  StopTimeout: 5s\n  StartStall: %v\n", c.stall)
			p := harness.Start(t, o)

			p.WaitLog(t, 15*time.Second, func(l harness.Log) bool { return l.Msg() == "starting" && l.Str("hook") == c.hook })
			if proxy != nil {
				deadline := time.Now().Add(5 * time.Second)
				for proxy.Accepted() == 0 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if proxy.Accepted() == 0 {
					t.Fatalf("%s 开始了 5s，代理还没收到连接", c.hook)
				}
			}
			time.Sleep(50 * time.Millisecond) // 让它进到握手里等回话 / 进到 warmup 的 Sleep 里

			p.Signal(syscall.SIGTERM)
			exit, ok := p.Wait(10 * time.Second)
			if !ok {
				t.Fatalf("启动期间收到 SIGTERM，10s 之后进程还活着\n%s", lastLines(p.Output(), 30))
			}
			if proxy != nil {
				t.Logf("退出：%v（信号在 %s 卡住时发出，代理收到 %d 个连接）", exit, c.hook, proxy.Accepted())
			} else {
				t.Logf("退出：%v（信号在 %s 卡住时发出）", exit, c.hook)
			}

			if exit.Code != 0 || exit.Signal != nil {
				t.Errorf("文档说启动期间按要求退出以 0 退出，实际 %v\nstderr:\n%s", exit, lastLines(p.Stderr(), 20))
			}

			starts := hookSeq(p, "starting")
			if len(starts) == 0 || starts[len(starts)-1] != c.hook {
				t.Fatalf("信号之后不该再跑别的启动钩子：最后一个 starting 应是 %s，实际 %v", c.hook, starts)
			}
			started := starts[:len(starts)-1] // 最后那个被打断了，没建起来
			if c.completes {
				started = starts // 不看 ctx 的那个睡完照样成功，算建好了
			}

			// 「不启动服务」：xgin 从没监听
			if ls := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgin listening" }); len(ls) > 0 {
				t.Errorf("文档说启动期间收到信号服务不再启动，实际日志里有 %s", ls[0].Line)
			}
			// 框架说明了为什么没启动，并且不当成失败：INFO 级别，ready 是建好了几个
			ls := p.FindLogs(func(l harness.Log) bool {
				return l.Msg() == "shutdown signal received during startup, not starting the server"
			})
			if len(ls) != 1 {
				t.Errorf("应当有一条 shutdown signal received during startup 的日志，实际 %d 条", len(ls))
			} else {
				t.Logf("框架的说明：%s", ls[0].Line)
				if ls[0].Level() != "INFO" {
					t.Errorf("按要求退出不是故障，这条日志应是 INFO，实际 %s", ls[0].Level())
				}
				if got, want := ls[0].Str("ready"), fmt.Sprint(len(started)); got != want {
					t.Errorf("ready 应是已建好的 %s 个，实际 %s", want, got)
				}
			}

			// 「已建好的逆序关干净」：正好是建好了的那些，逆序；被打断的、没轮到的都不关
			stops := hookSeq(p, "stopping")
			t.Logf("已建好：%v；关闭：%v", started, stops)
			if want := expectedStops(started); !slices.Equal(pkgsOf(stops), want) {
				t.Errorf("文档说已建好的逆序关干净：want %v，got %v", want, pkgsOf(stops))
			}

			// 「不报成启动失败」：没有 MustRun 打出来的错误，也没有 ERROR 日志
			for _, line := range strings.Split(p.Stderr(), "\n") {
				if strings.Contains(line, "failed, err=[") {
					t.Errorf("文档说不报成启动失败，实际 stderr 里有：%s", line)
				}
			}
			for _, l := range p.Logs() {
				if l.Level() == "ERROR" {
					t.Errorf("按要求退出不该有 ERROR 日志，实际：%s", l.Line)
				}
			}

			// 被打断的建连当场放弃；不看 ctx 的那个睡满就走，不再跑别的
			limit := cmp.Or(c.limit, immediate)
			if exit.SinceSignal > limit {
				t.Errorf("文档说收到退出信号建连当场放弃、不再跑剩下的启动钩子，实际信号之后 %v 才退出（上限 %v）", exit.SinceSignal, limit)
			}
		})
	}
}

// TestShutdown_AfterDisconnect_PGCallWithCtxStops_RedisWaitsForPoolClose
//
// 文档（xgin/README.md XGin 一节）：到点还没做完的请求，Close() 断开连接、取消请求的 ctx；
// 「handler 里的慢操作（查库、调下游）要传 c.Request.Context()，断连之后才停得下来」，
// 留出来的那一截就是等这种 handler 收尾的。
//
// Redis 是那一节写明的例外（xredis/README.md「行为与实测」）：go-redis 只认 ctx 的
// 截止时间，取消叫不醒阻塞在读上的命令，它要等到 ReadTimeout、ctx 的截止时间或 xredis 的停止钩子
// 关掉连接池才返回。所以 Redis 那一例断言的是这个：handler 在 closeXRedis 之后才返回，
// Stop 如实报出还有 handler 没返回。
//
// 做法：服务经 TCP 代理连 PG / Redis，起来之后让代理把回话压住（SetDelay(time.Hour)），
// 读用户的请求就卡在 PG / Redis 的读上——handler 传的是请求的 ctx。WithStopTimeout=3s：
// 约 1.6s 断连，2.0s 服务那一段到点。handler 看 ctx 的话断连之后立刻收尾，Stop 报的是
// 「超时后强制断开」，而不是「还有 N 个 handler 没返回」。
//
// Redis 那一例把 ReadTimeout 配成 10s：默认的 500ms 比断连之后留的那一截（0.4s）还短，
// 一次读自己就超时了，测不出 ctx 管不管用
func TestShutdown_AfterDisconnect_PGCallWithCtxStops_RedisWaitsForPoolClose(t *testing.T) {
	harness.Require(t)
	const (
		budget     = 3 * time.Second
		serverPart = budget - budget/3         // 2s
		closeAt    = serverPart - serverPart/5 // 1.6s
	)
	cases := []struct {
		name   string
		target string
		opts   func(addr string) harness.Options
		path   string // %d 是用户 id
		// untilPoolClosed 取消叫不醒这个依赖上阻塞的调用，handler 等到它的停止钩子关掉连接池才返回
		untilPoolClosed string
	}{
		{name: "PG", target: harness.PGAddr(), path: "/users/%d?cache=off",
			opts: func(a string) harness.Options { return harness.Options{PGAddr: a} }},
		{name: "Redis", target: harness.RedisAddr(), path: "/users/%d",
			opts: func(a string) harness.Options {
				return harness.Options{RedisAddr: a, Overlay: "XRedis:\n  ReadTimeout: 10s\n"}
			},
			// 实测断连在信号后 1.6s，handler 直到 2.0s 服务那一段到点、xredis 的停止钩子
			// 把连接池关掉才返回
			untilPoolClosed: "xredis.closeXRedis"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proxy := harness.NewProxy(t, c.target)
			o := c.opts(proxy.Addr())
			o.Overlay += stopTimeout(budget)
			p := harness.Start(t, o)
			u := createUser(t, p, "slowcall", "slowcall@example.com")
			path := fmt.Sprintf(c.path, u.ID)

			proxy.SetDelay(time.Hour)
			type result struct {
				at   time.Time
				resp harness.Response
				err  error
			}
			done := make(chan result, 1)
			go func() {
				r, err := p.Request(context.Background(), http.MethodGet, path, nil)
				done <- result{time.Now(), r, err}
			}()
			// 进 handler 到发出查询是微秒级，200ms 足够它卡进读里
			time.Sleep(200 * time.Millisecond)

			sigAt := time.Now()
			p.Signal(syscall.SIGTERM)
			exit, ok := p.Wait(budget + 5*time.Second)
			if !ok {
				t.Fatalf("文档说整个退出流程不超过 WithStopTimeout=%v，实际信号之后 %v 进程还活着", budget, budget+5*time.Second)
			}
			r := <-done
			t.Logf("退出：%v；客户端在信号后 %s 结束（err=%v）", exit, fmtMS(r.at.Sub(sigAt)), r.err)
			if exit.SinceSignal > budget {
				t.Errorf("文档说整个退出流程不超过 WithStopTimeout=%v，实际信号之后 %v 才退出", budget, exit.SinceSignal)
			}

			// handler 什么时候返回的、返回前碰到了什么：访问日志在 handler 返回之后才写，
			// 它的 path 不带查询串
			route, _, _ := strings.Cut(path, "?")
			var completedAt, poolClosedAt time.Time
			for _, l := range p.FindLogs(func(l harness.Log) bool {
				return (l.Msg() == "request completed" && l.Str("path") == route) ||
					l.Msg() == "redis read failed, falling back to the database" || l.Msg() == "read user failed" ||
					(l.Msg() == "stopping" && (l.Str("hook") == "xredis.closeXRedis" || l.Str("hook") == "xgorm.closeXGorm"))
			}) {
				at, _ := time.Parse(time.RFC3339Nano, l.Str("time"))
				switch {
				case l.Msg() == "request completed":
					completedAt = at
				case l.Msg() == "stopping" && l.Str("hook") == c.untilPoolClosed:
					poolClosedAt = at
				}
				t.Logf("信号后 %s（断连约在 %v）：%s %s%s%s", fmtMS(at.Sub(sigAt)), closeAt, l.Msg(),
					l.Str("hook"), l.Str("status"), l.Str("error"))
			}

			stderr := p.Stderr()
			if c.untilPoolClosed != "" {
				// handler 在连接池关掉之后才醒，这时离进程退出只剩几十微秒：它的访问日志
				// 有时来不及写出来就退出了（全量 e2e 里撞上过一次）。没写出来同样说明它没被断连叫醒，
				// 证据由下面的「1 handler(s) still running」给；写出来了就必须晚于关池
				if poolClosedAt.IsZero() || (!completedAt.IsZero() && completedAt.Before(poolClosedAt)) {
					t.Errorf("文档说取消叫不醒阻塞的 Redis 命令、handler 等到 %s 关掉连接池才返回：实际 handler 返回于 %v，%s 于 %v。"+
						"handler 先返回了的话是 go-redis 开始听取消了，xredis/README.md XRedis 那句限制该删了",
						c.untilPoolClosed, completedAt, c.untilPoolClosed, poolClosedAt)
				}
				if !strings.Contains(stderr, "1 handler(s) still running") {
					t.Errorf("handler 过了服务那一段的截止时间才返回，Stop 应如实报出，实际 stderr：\n%s", lastLines(stderr, 5))
				}
				return
			}
			if strings.Contains(stderr, "handler(s) still running") {
				t.Errorf("文档说传了请求 ctx 的慢操作断连之后停得下来，实际 Stop 报 handler 没返回：\n%s", lastLines(stderr, 5))
			}
			// handler 都在截止时间前返回了，Stop 报的才是「强制断开」；还有 handler 没返回时报的是上面那句
			if !strings.Contains(stderr, "graceful shutdown timed out, connections were force closed") {
				t.Errorf("到点还有请求时应当强制断开并报出来，实际 stderr：\n%s", lastLines(stderr, 5))
			}
		})
	}
}
