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

// mysqlLogTime 日志的 time 字段
func mysqlLogTime(l harness.Log) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, l.Str("time"))
	return at
}

// SIGTERM 时 MySQL 上正在执行的查询：README「按阶段初始化、逆序关闭」——xgorm 在 StageClient，
// 比服务（xgin）先起、后关；docs/architecture.md「停止预算是一份」：先停服务、等在途请求做完，
// 然后才轮到客户端类组件。所以在途的 SELECT SLEEP 要在服务端睡完、拿到 200，
// 不看 ctx 的 handler 睡完再查一次 MySQL 也要查得到（mysql_error=none），
// 这些都发生在 xgorm 的停止钩子关掉连接池之前
func TestMySQL_SIGTERMFinishesInFlightQueriesBeforeClosingPool(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const (
		budget = 8 * time.Second
		sleeps = 6
		stucks = 2
		sleep  = 1500 * time.Millisecond
	)
	p := harness.Start(t, harness.Options{Overlay: stopTimeout(budget)})

	type shot struct {
		path string
		r    harness.Response
		err  error
		at   time.Time
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
			shots = append(shots, shot{path, r, err, time.Now()})
			mu.Unlock()
		}()
	}
	for range sleeps {
		fire(fmt.Sprintf("/mysql/sleep?ms=%d", sleep.Milliseconds()))
	}
	for range stucks {
		fire(fmt.Sprintf("/stuck?ms=%d&mysql=1", sleep.Milliseconds()))
	}
	waitGauge(t, p, "e2e_mysql_sleep_inflight", sleeps, 5*time.Second)
	waitGauge(t, p, "e2e_stuck_inflight", stucks, 5*time.Second)
	// SELECT SLEEP 已经发到 MySQL 上了：在 processlist 里看得见它们，信号到的时候它们正在服务端执行
	var running int
	if err := harness.MySQL(t).QueryRow(`SELECT COUNT(*) FROM information_schema.processlist WHERE info LIKE 'SELECT SLEEP(%'`).Scan(&running); err != nil {
		t.Fatalf("读 processlist：%v", err)
	}
	if running < sleeps {
		t.Fatalf("发信号之前 MySQL 上应有 %d 条正在执行的 SELECT SLEEP，processlist 里只有 %d 条", sleeps, running)
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
	if exit.SinceSignal < sleep-200*time.Millisecond {
		t.Errorf("在途的查询要睡 %v，进程应等它们做完再退出，实际信号之后 %v 就退出了", sleep, exit.SinceSignal)
	}

	stops := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "stopping" && l.Str("hook") == "xgorm.closeXGorm" })
	if len(stops) != 1 {
		t.Fatalf("应有一条 xgorm.closeXGorm 的 stopping 日志，实际 %d 条", len(stops))
	}
	closeAt := mysqlLogTime(stops[0])
	var last time.Time
	done := p.FindLogs(func(l harness.Log) bool {
		return l.Msg() == "mysql sleep finished" || l.Msg() == "stuck request finished"
	})
	if len(done) != sleeps+stucks {
		t.Errorf("%d 个在途请求都该做完并记一条日志，实际 %d 条", sleeps+stucks, len(done))
	}
	for _, l := range done {
		if l.Msg() == "stuck request finished" && l.Str("mysql_error") != "none" {
			t.Errorf("不看 ctx 的 handler 睡完再查 MySQL 时连接池应还开着，实际 mysql_error=%q", l.Str("mysql_error"))
		}
		last = later(last, mysqlLogTime(l))
	}
	if !closeAt.After(last) {
		t.Errorf("xgorm 的连接池应在在途的 MySQL 查询全部做完之后才关：最后一个做完于信号后 %s，关池于信号后 %s",
			fmtMS(last.Sub(sigAt)), fmtMS(closeAt.Sub(sigAt)))
	}
	t.Logf("数字：信号后 %s 最后一个在途 MySQL 查询做完，%s 关 xgorm 连接池，%v 进程退出",
		fmtMS(last.Sub(sigAt)), fmtMS(closeAt.Sub(sigAt)), exit.SinceSignal.Round(time.Millisecond))
}

// MySQL 不回话时，传了请求 ctx 的查询在 ctx 被取消（不是到截止时间）时停不停得下来。
//
// xgorm/README.md 的 XGorm 一节原先没写 MySQL 的取消（只写了 PG「Web 请求里用请求自带的 ctx 也行：
// 客户端断开时它被取消，查询跟着返回」）。量下来：go-sql-driver 在 ctx 结束时关掉那条连接，
// 阻塞在读上的查询当场返回（context canceled），不等 ReadTimeout。量出来的数写进了
// xgorm/README.md「MySQL」，这里照它断言。
//
// ReadTimeout 配成 10s：默认 3s 的话，断连之后留的那一截里读超时可能自己先到，测不出取消管不管用
func TestMySQL_StuckQuery_ReturnsOnRequestCtxCancel_NoReadTimeoutWait(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const readTimeout = 10 * time.Second
	overlay := mysqlOverlay("MySQL:\n  ReadTimeout: 10s")

	t.Run("客户端断开", func(t *testing.T) {
		t.Parallel()
		const giveUp = 500 * time.Millisecond
		my := harness.NewProxy(t, harness.MySQLAddr())
		p := harness.Start(t, harness.Options{MySQLAddr: my.Addr(), Overlay: overlay})
		my.SetDelay(time.Hour)
		r := mysqlDep(p, "", giveUp)
		if r.ReqErr == nil {
			t.Fatalf("不给截止时间、ReadTimeout 10s：%v 内查询不该返回，实际 %v", giveUp, r)
		}
		gaveUpAt := time.Now()
		l, ok := p.LookForLog(readTimeout, func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("path") == "/dep" })
		if !ok {
			t.Fatalf("客户端断开之后请求的 ctx 被取消，查询应跟着返回：%v 内 handler 仍没返回", readTimeout)
		}
		after := mysqlLogTime(l).Sub(gaveUpAt)
		if after > faultSlack {
			t.Errorf("客户端断开之后查询应当场返回（文档：go-sql-driver 在 ctx 结束时关掉连接），实际 %v 后 handler 才返回", after)
		}
		t.Logf("数字：MySQL 不回话、客户端 %v 放弃之后 %s，handler 返回（status=%s）", giveUp, fmtMS(after), l.Str("status"))
	})

	t.Run("SIGTERM之后到点断连", func(t *testing.T) {
		t.Parallel()
		const (
			budget     = 3 * time.Second
			serverPart = budget - budget/3         // 2s
			closeAt    = serverPart - serverPart/5 // 1.6s
		)
		my := harness.NewProxy(t, harness.MySQLAddr())
		p := harness.Start(t, harness.Options{MySQLAddr: my.Addr(), Overlay: overlay + stopTimeout(budget)})
		u, _ := mysqlCreate(t, p, "stalled", "stalled@example.com")
		my.SetDelay(time.Hour)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = p.Request(context.Background(), http.MethodGet, fmt.Sprintf("/mysql/users/%d", u.ID), nil)
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
			t.Errorf("传了请求 ctx 的 MySQL 查询断连之后应停得下来，实际 Stop 报 handler 没返回：\n%s", lastLines(stderr, 5))
		}
		if !strings.Contains(stderr, "graceful shutdown timed out, connections were force closed") {
			t.Errorf("到点还有请求时应当强制断开并报出来，实际 stderr：\n%s", lastLines(stderr, 5))
		}
		ls := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "read mysql user failed" })
		if len(ls) != 1 {
			t.Fatalf("卡住的查询被取消后 handler 应记一条 read mysql user failed，实际 %d 条\n%s", len(ls), lastLines(p.Output(), 20))
		}
		at := mysqlLogTime(ls[0]).Sub(sigAt)
		if at > serverPart+faultSlack {
			t.Errorf("断连约在信号后 %v，查询应跟着返回，实际信号后 %s 才返回", closeAt, fmtMS(at))
		}
		t.Logf("数字：SIGTERM（预算 %v，断连约在 %v）之后 %s 卡住的 MySQL 查询返回：%s；进程 %v 退出",
			budget, closeAt, fmtMS(at), ls[0].Str("error"), exit.SinceSignal.Round(time.Millisecond))
	})
}

// 启动时 MySQL 不回话，这时收到 SIGTERM。xgorm/README.md XGorm：MySQL 的建连（包括查版本）全部在受 ctx 管的
// 建连验证里，启动期间的退出信号当场生效，不等握手那一读撞上 ReadTimeout。
//
// 这条原先是 KNOWN BUG：信号之后约 2.95s 才退出（= 握手那一读等满 ReadTimeout 3s，减去发信号前已经等掉的那一截）。
// 那一读发生在 gorm.Open 里 Dialector.Initialize 查版本时，用的是 context.Background()；
// 文档还把它的上限写成了 DialTimeout，而对端 TCP 秒连、握手不回话时管这一读的是 readTimeout
func TestMySQL_SIGTERMWhileSilentAtStartup_ExitsImmediately(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const immediate = 200 * time.Millisecond // 同 TestShutdown_SIGTERMDuringStartup_NoServe_ClosesBuiltInReverse_ExitsZero 的「当场放弃」
	my := harness.NewProxy(t, harness.MySQLAddr())
	my.SetDelay(time.Hour)
	p := harness.Start(t, harness.Options{MySQLAddr: my.Addr(), NoWait: true, Overlay: stopTimeout(5 * time.Second)})
	p.WaitLog(t, 15*time.Second, func(l harness.Log) bool { return l.Msg() == "starting" && l.Str("hook") == "xgorm.initXGorm" })
	deadline := time.Now().Add(5 * time.Second)
	for my.Accepted() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if my.Accepted() == 0 {
		t.Fatal("xgorm 开始了 5s，MySQL 的代理还没收到连接")
	}
	time.Sleep(50 * time.Millisecond)
	p.Signal(syscall.SIGTERM)
	exit, ok := p.Wait(15 * time.Second)
	if !ok {
		t.Fatalf("启动期间收到 SIGTERM，15s 之后进程还活着\n%s", lastLines(p.Output(), 30))
	}
	t.Logf("数字：启动时 MySQL 不回话、信号之后 %v 退出（%v）；stderr 最后一行：%s", exit.SinceSignal.Round(time.Millisecond), exit, lastLine(p.Stderr()))
	if ls := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgin listening" }); len(ls) > 0 {
		t.Errorf("启动期间收到信号服务不该再启动，实际日志里有 %s", ls[0].Line)
	}
	if exit.SinceSignal > immediate {
		t.Errorf("文档说启动期间的退出信号当场生效，实际信号之后 %v 才退出（ReadTimeout %v）", exit.SinceSignal.Round(time.Millisecond), mysqlReadTimeout)
	}
}
