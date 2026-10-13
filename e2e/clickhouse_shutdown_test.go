package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// chRunning ClickHouse 服务端此刻有几条 query 里带着 marker 的查询在跑（不算查 system.processes 自己的这条）
func chRunning(t *testing.T, marker string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n uint64
	if err := harness.CH(t).QueryRowContext(ctx,
		`SELECT count() FROM system.processes WHERE query LIKE ? AND query NOT LIKE '%system.processes%'`, "%"+marker+"%").Scan(&n); err != nil {
		t.Fatalf("读 system.processes：%v", err)
	}
	return int(n)
}

// SIGTERM 时 ClickHouse 上正在执行的查询：README「按阶段初始化、逆序关闭」——xgorm 在 StageClient，
// 比服务（xgin）先起、后关；docs/architecture.md「停止预算是一份」：先停服务、等在途请求做完，
// 然后才轮到客户端类组件。所以在途的 SELECT sleep 要在服务端睡完、拿到 200，
// 不看 ctx 的 handler 睡完再查一次 ClickHouse 也要查得到（ch_error=none），
// 这些都发生在 xgorm 的停止钩子关掉连接池之前
func TestClickHouse_SIGTERMFinishesInFlightQueriesBeforeClosingPool(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	const (
		budget = 8 * time.Second
		sleeps = 6
		stucks = 2
		sleep  = 1500 * time.Millisecond
	)
	p := chStart(t, harness.Options{Overlay: stopTimeout(budget)})

	type shot struct {
		path string
		r    harness.Response
		err  error
	}
	var mu sync.Mutex
	var shots []shot
	var wg sync.WaitGroup
	fire := func(path string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := p.Request(context.Background(), http.MethodGet, path, nil)
			mu.Lock()
			shots = append(shots, shot{path, r, err})
			mu.Unlock()
		}()
	}
	for range sleeps {
		fire(fmt.Sprintf("/ch/sleep?ms=%d", sleep.Milliseconds()))
	}
	for range stucks {
		fire(fmt.Sprintf("/stuck?ms=%d&ch=1", sleep.Milliseconds()))
	}
	waitGauge(t, p, "e2e_ch_sleep_inflight", sleeps, 5*time.Second)
	waitGauge(t, p, "e2e_stuck_inflight", stucks, 5*time.Second)
	// sleep 已经发到 ClickHouse 上了：system.processes 里看得见它们，信号到的时候它们正在服务端执行
	deadline := time.Now().Add(2 * time.Second)
	// 参数由驱动代进语句再发出去：clickhouse-go v2.48.0 把 1.5 写成 cast(1.5, 'Float64')（v2.30.0 时是裸的 1.5）
	const marker = "sleep(cast(1.5, 'Float64'))"
	running := chRunning(t, marker)
	for running < sleeps && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		running = chRunning(t, marker)
	}
	if running < sleeps {
		t.Fatalf("发信号之前 ClickHouse 上应有 %d 条正在执行的 SELECT sleep，system.processes 里只有 %d 条", sleeps, running)
	}
	sigAt := time.Now()
	p.Signal(syscall.SIGTERM)
	exit, ok := p.Wait(budget + 5*time.Second)
	if !ok {
		t.Fatalf("文档说整个退出流程不超过 WithStopTimeout=%v，实际信号之后 %v 进程还活着", budget, budget+5*time.Second)
	}
	wg.Wait()
	t.Logf("退出：%v", exit)
	if exit.Code != 0 || exit.Signal != nil {
		t.Errorf("在途请求都在预算内做完，应以 0 退出，实际 %v\nstderr:\n%s", exit, lastLines(p.Stderr(), 10))
	}
	for _, s := range shots {
		if s.err != nil || s.r.Status != http.StatusOK {
			t.Errorf("信号那一刻在途的 %s 应做完拿到 200，实际 err=%v %v", s.path, s.err, s.r)
		}
	}
	if exit.SinceSignal < sleep-500*time.Millisecond {
		t.Errorf("在途的查询要睡 %v，进程应等它们做完再退出，实际信号之后 %v 就退出了", sleep, exit.SinceSignal)
	}

	stops := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "stopping" && l.Str("hook") == "xgorm.closeXGorm" })
	if len(stops) != 1 {
		t.Fatalf("应有一条 xgorm.closeXGorm 的 stopping 日志，实际 %d 条", len(stops))
	}
	closeAt := mysqlLogTime(stops[0])
	var last time.Time
	done := p.FindLogs(func(l harness.Log) bool {
		return l.Msg() == "ch sleep finished" || l.Msg() == "stuck request finished"
	})
	if len(done) != sleeps+stucks {
		t.Errorf("%d 个在途请求都该做完并记一条日志，实际 %d 条", sleeps+stucks, len(done))
	}
	for _, l := range done {
		if l.Msg() == "stuck request finished" && l.Str("ch_error") != "none" {
			t.Errorf("不看 ctx 的 handler 睡完再查 ClickHouse 时连接池应还开着，实际 ch_error=%q", l.Str("ch_error"))
		}
		last = later(last, mysqlLogTime(l))
	}
	if !closeAt.After(last) {
		t.Errorf("xgorm 的连接池应在在途的 ClickHouse 查询全部做完之后才关：最后一个做完于信号后 %s，关池于信号后 %s",
			fmtMS(last.Sub(sigAt)), fmtMS(closeAt.Sub(sigAt)))
	}
	t.Logf("数字：信号后 %s 最后一个在途 ClickHouse 查询做完，%s 关 xgorm 连接池，%v 进程退出",
		fmtMS(last.Sub(sigAt)), fmtMS(closeAt.Sub(sigAt)), exit.SinceSignal.Round(time.Millisecond))
}

// ClickHouse 不回话时，传了请求 ctx 的查询在 ctx 被取消（不是到截止时间）时停不停得下来。
//
// xgorm/clickhouse/README.md「行为与实测」：「查询和执行认 ctx 的取消：读回包放在协程里，ctx 取消时当场返回
// context canceled，同时给服务端发 Cancel 包、关掉这条连接（不还回池里）」。池里的连接上量：
//
//   - 客户端断开：请求的 ctx 被取消，handler 当场返回，不等 read_timeout（默认 300s）；
//   - SIGTERM 之后到点断连：同样是取消，卡住的查询在断连那一刻返回，进程在预算内退出
func TestClickHouse_StuckQuery_ReturnsOnRequestCtxCancel_NoReadTimeoutWait(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()

	t.Run("客户端断开", func(t *testing.T) {
		t.Parallel()
		const giveUp = 500 * time.Millisecond
		chp := harness.NewProxy(t, harness.CHAddr())
		p := chStart(t, harness.Options{CHAddr: chp.Addr()})
		chFillPool(t, p, 2)
		chp.SetDelay(time.Hour)
		r := chDep(p, "", giveUp)
		if r.ReqErr == nil {
			t.Fatalf("不给截止时间、read_timeout 300s：%v 内查询不该返回，实际 %v", giveUp, r)
		}
		gaveUpAt := time.Now()
		l, ok := p.LookForLog(10*time.Second, func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("path") == "/dep" })
		if !ok {
			t.Fatalf("客户端断开之后请求的 ctx 被取消，查询应跟着返回：10s 内 handler 仍没返回")
		}
		after := mysqlLogTime(l).Sub(gaveUpAt)
		if after > faultSlack {
			t.Errorf("客户端断开之后查询应当场返回（文档：ctx 取消时当场返回 context canceled），实际 %v 后 handler 才返回", after)
		}
		t.Logf("数字：ClickHouse 不回话、客户端 %v 放弃之后 %s，handler 返回（status=%s）", giveUp, fmtMS(after), l.Str("status"))
	})

	t.Run("SIGTERM之后到点断连", func(t *testing.T) {
		t.Parallel()
		const (
			budget     = 3 * time.Second
			serverPart = budget - budget/3         // 2s
			closeAt    = serverPart - serverPart/5 // 1.6s
		)
		chp := harness.NewProxy(t, harness.CHAddr())
		p := chStart(t, harness.Options{CHAddr: chp.Addr(), Overlay: stopTimeout(budget)})
		ids, _ := chInsert(t, p, "stalled", 1)
		chp.SetDelay(time.Hour)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = p.Request(context.Background(), http.MethodGet, fmt.Sprintf("/ch/events/%d", ids[0]), nil)
		}()
		time.Sleep(200 * time.Millisecond) // 进 handler 到发出查询是微秒级，200ms 足够它卡进读里

		sigAt := time.Now()
		p.Signal(syscall.SIGTERM)
		exit, ok := p.Wait(budget + 5*time.Second)
		<-done
		if !ok {
			t.Fatalf("文档说整个退出流程不超过 WithStopTimeout=%v，实际进程还活着", budget)
		}
		if exit.SinceSignal > budget {
			t.Errorf("文档说整个退出流程不超过 WithStopTimeout=%v，实际信号之后 %v 才退出", budget, exit.SinceSignal)
		}
		stderr := p.Stderr()
		if strings.Contains(stderr, "handler(s) still running") {
			t.Errorf("传了请求 ctx 的 ClickHouse 查询断连之后应停得下来，实际 Stop 报 handler 没返回：\n%s", lastLines(stderr, 5))
		}
		ls := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "read ch event failed" })
		if len(ls) != 1 {
			t.Fatalf("卡住的查询被取消后 handler 应记一条 read ch event failed，实际 %d 条\n%s", len(ls), lastLines(p.Output(), 20))
		}
		at := mysqlLogTime(ls[0]).Sub(sigAt)
		if at > serverPart+faultSlack {
			t.Errorf("断连约在信号后 %v，查询应跟着返回，实际信号后 %s 才返回", closeAt, fmtMS(at))
		}
		if !strings.Contains(ls[0].Str("error"), "context canceled") {
			t.Errorf("被取消的查询应报 context canceled，实际 %q", ls[0].Str("error"))
		}
		t.Logf("数字：SIGTERM（预算 %v，断连约在 %v）之后 %s 卡住的 ClickHouse 查询返回：%s；进程 %v 退出",
			budget, closeAt, fmtMS(at), ls[0].Str("error"), exit.SinceSignal.Round(time.Millisecond))
	})
}

// 客户端放弃之后，服务端那条查询停没停。xgorm/clickhouse/README.md：取消时驱动「给服务端发 Cancel 包」。
// ClickHouse 收到 Cancel 之后在处理下一个数据块时停下：
//
//   - 一行一个数据块、每行之间睡 100ms 的查询：客户端放弃之后很快就从 system.processes 里消失；
//   - SELECT sleep(2.5) 是一个函数调用、没有块的边界，Cancel 打断不了它：服务端照样睡满（实测 2.5s 后才消失）。
//     调用方那一侧照样当场返回、连接照样关掉，只是服务端的这点资源要等它自己跑完
func TestClickHouse_ClientGivesUp_DriverSendsCancel_ServerStopsPerBlock(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	p := chStart(t, harness.Options{})
	for _, c := range []struct {
		name, path, marker string
		gone               time.Duration // 服务端最多多久之后不再有这条查询
	}{
		// numbers(N) 的 N 当标记：带参数的查询在服务端看到的是代进去之后的文本。
		// 整数原样代进去，浮点数 clickhouse-go v2.48.0 写成 cast(2.5, 'Float64')（v2.30.0 时是裸的 2.5）
		{"逐块的查询", "/ch/stream?rows=4711&ms=100", "numbers(4711)", time.Second},
		{"单个sleep", "/ch/sleep?ms=2500", "sleep(cast(2.5, 'Float64'))", 3 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			const giveUp = 300 * time.Millisecond
			r := chCall(p, c.path, giveUp)
			if r.ReqErr == nil {
				t.Fatalf("这条查询要跑好几秒，%v 内不该返回，实际 %v", giveUp, r.faultDepResult)
			}
			gaveUpAt := time.Now()
			var left time.Duration
			for {
				if chRunning(t, c.marker) == 0 {
					left = time.Since(gaveUpAt)
					break
				}
				if time.Since(gaveUpAt) > 5*time.Second {
					t.Fatalf("客户端放弃 5s 之后服务端还在跑 %s", c.marker)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if left > c.gone {
				t.Errorf("客户端放弃之后服务端应在 %v 内停下，实际 %v", c.gone, left)
			}
			// 标记对不上的话上面第一眼就是 0，这条断言什么都没验：驱动换了代参数的写法时就会这样
			if n := chQueryStarts(t, c.marker); n == 0 {
				t.Fatalf("前提：服务端的 query_log 里应有带 %q 的查询，实际没有（驱动代参数的写法变了？）", c.marker)
			}
			t.Logf("数字：%s：客户端 %v 放弃之后 %s 服务端不再有这条查询", c.name, giveUp, fmtMS(left))
		})
	}
}
