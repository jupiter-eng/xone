// Package xone 统一读配置、按阶段初始化所有已登记的组件、逆序关闭。
//
// import 想用的集成包（xgin、xgorm、xredis……），写一份配置文件，然后调一次 Run：
//
//	func main() {
//		xone.MustRun(xgin.New().WithRoutes(routes))
//	}
//
// Run 做的事，按顺序：
//
//   - 接管 SIGINT / SIGTERM（在读配置之前）；
//   - 加载配置（WithConfigPath > --config > XONE_CONFIG > conf/application.yml 等约定路径）；
//   - 按档位跑全部启动钩子（xhook.BeforeStart），日志 → 链路/指标 → 客户端 → 业务 → 服务；
//   - 调 Runnable.Start，阻塞到它返回或收到退出信号；
//   - 退出：取消 Start 的 ctx，调可选的 Stop，等 Start 返回，再逆序跑停止钩子。
//
// Runnable 只要写 Start；光靠 ctx 停不下来的再加一个 Stop(context.Context) error。
// 整个退出流程只有一份预算 WithStopTimeout（默认 15s），服务最多用其中 2/3。
// 第一个信号之后再发一次信号，进程立即终止。
//
// 用法和设计理由见仓库的 docs/guide.md 与 docs/architecture.md。
package xone

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"syscall"
	"time"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xerror"

	// XApp 块跟着框架一起来：只 import 了 xone 的程序也能写应用名。它只读配置，不改任何全局状态。
	// xlog 不在这里：它会换掉 slog.Default，只用 xhook、xgorm 的程序不该被它接管日志。
	// 只有 xgin / xecho / xcron 会把它带进来（xtrace、xmetric 不 import 它），别的程序要用就匿名 import 它
	_ "github.com/jupiter-eng/xone/xapp"
)

// Runnable 需要持续运行的东西，通常就是你的服务器。
//
// Start 收到的 ctx 在退出信号到达时被取消，Start 应当据此返回——消费者、
// 定时任务、一次性任务，做到这一条就够了。
//
// 光靠 ctx 停不下来的（比如 http.Server 要调 Shutdown），再实现一个
// Stop(context.Context) error：框架退出时先调它，再等 Start 返回。
// Stop 收到的是一个独立的 ctx，带着服务那一段停止预算（见 WithStopTimeout）——
// 它不继承那次取消，否则每个关闭动作一进去就被拒绝，等于没有优雅退出。
type Runnable interface {
	Start(context.Context) error
}

// Func 把一个函数当成 Runnable：fn 返回，Run 就走退出流程，fn 的错误就是 Run 的错误。
//
// 一次性任务（迁移、批处理、只想借框架把组件建起来干一件事）：干完 return。
// 自己写循环的消费者：循环到 ctx 被取消，把在途的做完再 return——
// 框架在 fn 返回之后才关数据库和缓存（见 docs/guide.md「非 Web 服务」）。
//
//	xone.MustRun(xone.Func(func(ctx context.Context) error {
//		return migrate(ctx, xgorm.CWithCtx(ctx))
//	}))
func Func(fn func(ctx context.Context) error) Runnable {
	if fn == nil {
		panic("xone: Func needs a function, got nil")
	}
	return funcRunnable(fn)
}

type funcRunnable func(context.Context) error

func (f funcRunnable) Start(ctx context.Context) error { return f(ctx) }

// UntilSignal 一个什么都不做、一直阻塞到退出信号的 Runnable。
//
// 活全在钩子里的进程用它：比如 SDK 自带协程的推送式消费者，在 xhook.BeforeStart 里启动、
// xhook.BeforeStop 里关；Run 跑完启动钩子就停在这里，收到信号再逆序跑停止钩子。
//
//	xone.MustRun(xone.UntilSignal())
func UntilSignal() Runnable {
	return Func(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})
}

// stopper Runnable 可选的那个 Stop
type stopper interface {
	Stop(context.Context) error
}

// checkRunnable 在跑任何钩子之前拦住几种一跑起来才会暴露的错。
//
// 写了 Stop 却调不到它：Stop 是可选的，编译器不会拦，它就永远不会被调到——
// 服务收到信号停不下来，只能等停止预算耗尽。调不到有两种写法：签名不对
// （比如少了 ctx），或者 Stop 写在指针上、传进来的却是值（Run(App{})）。
func checkRunnable(r Runnable) error {
	if r == nil {
		return xerror.Newf("xone", "config", "Run needs a Runnable, got nil")
	}
	if _, ok := r.(stopper); ok {
		return nil
	}
	t := reflect.TypeOf(r)
	if m, ok := t.MethodByName("Stop"); ok {
		return xerror.Newf("xone", "config",
			"%T has a Stop method of type %v, but Run only calls Stop(context.Context) error: "+
				"fix the signature, or rename the method if it is not meant for shutdown", r, m.Type)
	}
	if t.Kind() != reflect.Pointer {
		if _, ok := reflect.PointerTo(t).MethodByName("Stop"); ok {
			return xerror.Newf("xone", "config",
				"%T has Stop on its pointer receiver, so Run cannot call it on a value: pass a pointer (&%T{...})", r, r)
		}
	}
	return nil
}

// defaultStopTimeout 停止预算的默认值：从开始退出到 Run 返回，一共这么久
const defaultStopTimeout = 15 * time.Second

// hookReserve 每个还没轮到的停止钩子，至少替它留这么多（见 runStop）。
//
// 没有这一份，一个关不掉的连接池吃光预算，后面每个钩子一进去就判超时——
// 最吃亏的是日志：它排在最后关，正是为了让前面所有组件的关闭日志都写得出去。
// 1s 对一个没卡住的 Close / Flush 绰绰有余；钩子多、时间少时改按人头均分。
const hookReserve = time.Second

// Run 读配置、初始化全部组件、启动 r，阻塞到退出信号，然后逆序关闭。
func Run(r Runnable, opts ...Option) error {
	o := options{stopTimeout: defaultStopTimeout}
	for _, f := range opts {
		f(&o)
	}
	// 0 在这里不是「不限时」而是「一点都不等」：Stop 会拿到一个已经过期的
	// context，服务当场被切断，后面每个组件的关闭也都在超时状态下跑。
	// 让它在做任何事之前就失败，好过退出时才发现没有优雅退出这回事
	if o.stopTimeout <= 0 {
		return xerror.Newf("xone", "config", "stop budget must be > 0 (0 is not unlimited, it is no wait at all), got=%v", o.stopTimeout)
	}

	// 退出信号在做任何事之前就接管，配置加载和初始化都在它的保护之内。
	// 装在初始化之后的话，启动期间（连库、连 Redis、Ping 重试）收到 SIGTERM
	// 走的是系统默认处置：进程当场暴毙，已经建好的资源一个都来不及注销——
	// 注册中心里那条记录、那把分布式锁，只能等对端超时过期。
	ctx, stopSignals := notifyShutdown(o)
	defer stopSignals()
	printBanner(stderr, isTerminal(stderr))

	// 提前有人读过配置的话，这里沿用那一份。配置跟着这次 Run 走，
	// 不论怎么结束都交还——连加载失败也算：否则那个失败会一直留着，
	// 同一个进程里的下一次 Run 读不到它自己的那一份
	defer config.Reset()

	// Runnable 写错、配置读不出来：一个启动钩子都不跑，但停止阶段照样走一遍。
	// started 是空的，于是只有之前没有启动钩子的那些停止钩子会执行——文档说它们
	// 总会执行，提前返回的话这一条就只在启动走得够远时才成立
	if err := checkRunnable(r); err != nil {
		return errors.Join(err, stopWithin(o, nil))
	}
	if err := config.Ensure(o.configPath, o.log()); err != nil {
		return errors.Join(err, stopWithin(o, nil))
	}

	debugHooks(hook.Start())
	started, err := runStart(ctx, o)
	switch {
	case ctx.Err() != nil:
		// 启动期间收到退出信号：不启动服务，把已经起来的逆序关干净。
		// 即便 runStart 带回了错误也不往上报——被取消的建连必然失败，
		// 那是按要求退出的结果而不是故障。报上去的话，每次滚动更新
		// 撞上这个窗口都会在面板上留一条「启动失败」。
		attrs := []any{"ready", len(started)}
		if err != nil {
			attrs = append(attrs, "interrupted_start", err)
		}
		o.log().Info("shutdown signal received during startup, not starting the server", attrs...)
		return stopWithin(o, started)
	case err != nil:
		return errors.Join(err, stopWithin(o, started))
	}

	// 全部启动钩子都跑完了，此时还没人读过的顶层 key 就是没人要的。
	// 多半是拼错了，或者忘了 import 对应的集成包——两种都会让人配了半天
	// 才发现不生效，而配置文件是使用者唯一的操作界面。还有一种是读得太晚：
	// 只在 Start 里才读的 key 此时同样没人读过，报错里要说清该挪到哪里
	if orphan := config.Unclaimed(); len(orphan) > 0 {
		hint := ""
		if slices.Contains(orphan, "XLog") {
			// XLog 不跟着根包来（见 import 处），只用核心和数据类集成的程序最容易踩到
			hint = `; XLog needs import _ "github.com/jupiter-eng/xone/xlog" (xgin, xecho and xcron bring it along)`
		}
		return errors.Join(xerror.Newf("xone", "config",
			"config keys %v are not read by anyone: check the spelling, or whether the matching package is imported; "+
				"a key first read after startup (inside Start) is too late to count, read it in main or a BeforeStart hook%s", orphan, hint),
			stopWithin(o, started))
	}

	runErr := make(chan error, 1)
	go func() { runErr <- safe("start", func() error { return r.Start(ctx) }) }()

	var first error
	var serverExited bool
	select {
	case <-ctx.Done(): // 信号已经由 notifyShutdown 记过日志了
	case e := <-runErr:
		first, serverExited = e, true
	}

	// 整个退出流程共用这一份预算。服务（Stop + 等 Start 返回）只能用前 2/3，
	// 后 1/3 留给停止钩子：不肯退出的服务吃不掉它；服务早早退出了，剩下的全归钩子。
	// 默认 15s 时服务那一段是 10s，xgin 的 Stop 就按这个截止时间等在途请求
	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), o.stopTimeout)
	defer stopCancel()
	serverCtx, serverCancel := context.WithTimeout(stopCtx, o.stopTimeout-o.stopTimeout/3)
	defer serverCancel()
	if s, ok := r.(stopper); ok {
		first = errors.Join(first, stopServer(serverCtx, o, s))
	}

	// 等 Start 真正返回再关其余组件。
	//
	// Stop 返回不等于服务已经停干净：Stop 只负责「让它停」，
	// 有没有等在处理的请求做完是各实现自己的事。不等就往下关的话，
	// 还在跑的请求会摸到已经关掉的数据库和缓存。
	// 同时这也是唯一能拿到 Start 错误的地方——走信号分支时它还没被读过。
	//
	// 服务自己退出时上面那次 select 已经把 runErr 取走了，这里不能再取：
	// channel 里没有第二个值，等下去就是白等满服务那一段。
	if !serverExited {
		select {
		case e := <-runErr:
			first = errors.Join(first, e)
		case <-serverCtx.Done():
			// 守规矩的 Stop 看着截止时间返回，Start 紧跟着返回：两边同时就绪时
			// select 随机挑，挑中这一支就把 Start 的错误丢了。所以先不阻塞地看一眼
			select {
			case e := <-runErr:
				first = errors.Join(first, e)
			default:
				// 服务用完了它那一段还没退出。不再等它：剩下的是留给组件的——
				// 继续等下去，关不成的就是注册中心那条记录、那把分布式锁。
				// 不肯退出的服务不该顺带让每个资源都漏着
				o.log().Warn("server did not exit within its share of the stop budget, closing the rest",
					"budget", o.stopTimeout)
			}
		}
	}

	return errors.Join(first, runStop(stopCtx, o, started))
}

// stopServer 在 ctx 的截止时间之前调服务的 Stop，到点就不再等它。
//
// 和 runWithin 同一个道理：Stop 收了 ctx，但它未必真的看。同步调的话，
// 一个不看 ctx 的 Stop 能把 Run 永远挂住，后面一个停止钩子都轮不到。
// 超时只告警、不算错误：和下面「等 Start 返回」超时一样，是服务没停利索，
// 剩下的组件照样要关。
func stopServer(ctx context.Context, o options, s stopper) error {
	done := make(chan error, 1)
	go func() { done <- safe("stop", func() error { return s.Stop(ctx) }) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	// 到点之后再给它一小截：守规矩的 Stop 恰恰是看着同一个截止时间返回的
	// （xgin 等在途 handler 等到截止时间，再带着「N handler(s) still running」返回），
	// 和这里的 ctx.Done 几乎同时发生。不留这一截，select 就可能先看到 Done，
	// 把 Stop 如实报出的错误丢掉，进程以 0 退出（e2e 两个退出用例撞上过）
	select {
	case err := <-done:
		return err
	case <-time.After(stopGrace):
		o.log().Warn("server Stop did not return within its share of the stop budget, closing the rest",
			"budget", o.stopTimeout)
		return nil
	}
}

// stopGrace 服务的 Stop 到点之后还等它多久，见 stopServer。
// 只用来接住「看着截止时间返回」的那个结果，从停止钩子的份额里借，量级要远小于 hookReserve
const stopGrace = 50 * time.Millisecond

// stopWithin 启动阶段失败时的关闭，自己开一份停止预算。
//
// 这一支没有 stopCtx——服务还没起来，那个 ctx 还没造出来。
// 但预算同样要有：启动到一半失败时，已经建好的那几个照样可能关不掉。
func stopWithin(o options, started map[int]bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), o.stopTimeout)
	defer cancel()
	return runStop(ctx, o, started)
}

// runStart 按档位执行全部启动钩子，返回跑成功了的那些（按登记序号）。
//
// 记的是每一个钩子而不是每一个包：一个包可以登记好几对钩子，
// 停止钩子只认和它配对的那一个启动钩子。见 runStop。
func runStart(ctx context.Context, o options) (map[int]bool, error) {
	started := map[int]bool{}
	for _, e := range hook.Start() {
		// 每个钩子之前看一眼：已经决定退出了就别再去连三个库。
		// 打断不了正在跑的那一个——那要靠它自己把 ctx 传下去，
		// 所以钩子的签名里有 ctx
		if ctx.Err() != nil {
			o.log().Warn("shutdown signal received, skipping the remaining start hooks", "ready", len(started))
			return started, nil
		}
		// 每次重新取 logger：xlog 就在这个循环里把全局默认 logger 换掉，
		// 它之后的钩子应该用新的那个
		o.log().Info("starting", "hook", e.Name)
		if err := safeHook(ctx, e); err != nil {
			return started, xerror.Newf("xone", "start", "%s: %w", e.Name, err)
		}
		started[e.Seq] = true
	}
	return started, nil
}

// runStop 逆序执行停止钩子，全部在 ctx 的截止时间之前结束。
//
// 停止钩子只在和它配对的启动钩子成功了之后才执行（配对在登记时就定了，
// 见 hook.AddStop：同一个包里在它之前最近登记的那个）。启动到一半失败时，后面那些资源根本
// 没建起来，调它们的停止钩子只会在一堆空值上出错——所以停止钩子里不必处理
// 「还没建起来」。之前没有启动钩子的停止钩子照常执行，它本来就不依赖启动。
//
// 钩子之间给后面的留一份：第 i 个钩子最多用到
// 「截止时间 − 排在它后面的钩子数 × reserve」，reserve = min(hookReserve, 剩余时间 ÷ 钩子数)。
// 卡住的钩子吃不掉后面的；前面的做得快，省下来的顺延给后面。
// 于是每个钩子一进去手上都还有时间，不会揣着一个已经过期的 ctx 当场被判超时。
//
// 一个失败不影响其余：退出阶段要尽量把能关的都关掉。
func runStop(ctx context.Context, o options, started map[int]bool) error {
	todo := hook.StopAfter(started)
	if len(todo) == 0 {
		return nil
	}

	end, _ := ctx.Deadline() // 调用方总会给截止时间：Run 的 stopCtx，或 stopWithin 那一份
	reserve := min(hookReserve, time.Until(end)/time.Duration(len(todo)))

	var errs []error
	for i, e := range todo {
		o.log().Info("stopping", "hook", e.Name)
		hookCtx, cancel := context.WithDeadline(ctx, end.Add(-time.Duration(len(todo)-1-i)*reserve))
		err := runWithin(hookCtx, e)
		cancel()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// runWithin 在 ctx 的截止时间之前执行一个停止钩子，到点就不再等它。
//
// 钩子收了 ctx，但它未必真的看——里面可能是一个不吃 ctx 的第三方 Close()。
// 所以另起一个协程去等。超时之后那个协程还挂在原地，这是有意的：强行放弃它，
// 好过让后面每一个钩子、以及进程本身，都排在一个关不掉的资源后面。
// 进程马上就要退出了，漏一个协程没有下文。
func runWithin(ctx context.Context, e hook.Entry) error {
	done := make(chan error, 1)
	go func() { done <- safeHook(ctx, e) }()

	select {
	case err := <-done:
		if err != nil {
			return xerror.Newf("xone", "stop", "%s: %w", e.Name, err)
		}
		return nil
	case <-ctx.Done():
		return xerror.Newf("xone", "stop", "%s did not finish within its share of the stop budget: %w", e.Name, ctx.Err())
	}
}

// safeHook 执行一个钩子并隔离 panic。
//
// 一个钩子炸了，不该把整个进程打穿——它应该变成一个普通的启动/停止错误，
// 让已经起来的部分有机会被逆序关闭。
//
// 返回普通 error 而不是 xerror：调用方（runStart / runWithin）会以
// start / stop 包一次，这里再包就成了两层 xone。
func safeHook(ctx context.Context, e hook.Entry) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicked(r)
		}
	}()
	return e.Run(ctx)
}

// safe 执行服务的 Start / Stop 并隔离 panic，op 即 start 或 stop。
//
// 服务自己返回的错误原样交出去：那是使用者的错误，不归框架包装。
func safe(op string, f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = xerror.New("xone", op, fmt.Errorf("server %w", panicked(r)))
		}
	}()
	return f()
}

// panicked 把 recover 到的值变成 error。
//
// panic 出来的本身是 error 时用 %w 接住：用 %v 的话它就只剩一段文本，
// errors.Is / errors.As 到此为止。
func panicked(r any) error {
	if err, ok := r.(error); ok {
		return fmt.Errorf("panicked: %w", err)
	}
	return fmt.Errorf("panicked: %v", r)
}

// shutdownSignals 触发优雅退出的信号
var shutdownSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}

// notifyShutdown 接管退出信号，返回一个收到信号时被取消的 context。
//
// 与 signal.NotifyContext 的差别在于第一个信号之后会把默认处置还回去，
// 于是再发一次信号由系统直接终止进程。这一手是必须的：注册信号处理本身
// 就取消了系统原本的「收到就死」，而框架在初始化和关闭阶段都可能卡在
// 某个不看 ctx 的第三方调用里——没有这条逃生口，那些情况下进程会变成
// 只有 kill -9 才能收掉，K8s 得等满整个终止宽限期。
//
// 返回的 stop 用于正常退出时注销，它会等接管协程真正退出再返回，
// 保证 Run 返回之后信号已经回到默认处置（同一进程里反复 Run 的测试依赖这点）。
func notifyShutdown(o options) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, shutdownSignals...)

	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case s := <-ch:
			// 先还原默认处置再取消：这中间要是又来一个信号，
			// 要的就是它直接把进程终止掉，而不是被一个已经没人看的 handler 收走
			signal.Stop(ch)
			o.log().Info("shutdown signal received, closing gracefully; send it again to terminate now",
				"signal", s.String())
			cancel()
		case <-ctx.Done():
			signal.Stop(ch)
		}
	}()

	return ctx, func() {
		cancel()
		<-done
	}
}

type options struct {
	configPath  string
	stopTimeout time.Duration

	// logger 留空表示每次取 slog.Default()。
	//
	// 必须延迟到用的时候才取：xlog 是在 StageLog 初始化时才调用
	// slog.SetDefault 的，Run 开头捕获一次的话，后面所有框架日志
	// 都还写在初始化之前那个默认 logger 上。
	logger *slog.Logger
}

// log 取当前该用的 logger。未显式指定时每次都取标准库的全局默认值，
// 这样 xlog 初始化完成后，框架自己的日志会自动跟着走新的那个。
func (o options) log() *slog.Logger {
	if o.logger != nil {
		return o.logger
	}
	return slog.Default()
}

type Option func(*options)

// WithConfigPath 指定配置文件，优先于 --config、XONE_CONFIG 和约定路径。
//
// 在 Run 之前就读过配置的话，配置已经按那几种方式加载过了，这里再点名
// 另一个文件会让 Run 直接报错——那时要改用 --config 或 XONE_CONFIG。
func WithConfigPath(p string) Option { return func(o *options) { o.configPath = p } }

// WithStopTimeout 设置停止预算，默认 15s：从开始退出（收到信号，或服务自己返回）
// 到 Run 返回，一共就这么久，部署环境的终止宽限期留得比它长就够了。
// 必须大于 0——0 不是「不限时」而是「一点都不等」，Run 会直接返回错误。
//
// 框架在内部把它分开用：服务最多占前 2/3，其余留给停止钩子，钩子之间再给排在
// 后面的各留一份——不肯退出的服务、关不掉的连接池都吃不掉别人那份。
// 服务的 Stop 不按它收到的 ctx 返回的话，到了服务那一段的截止时间就不再等它。
func WithStopTimeout(d time.Duration) Option { return func(o *options) { o.stopTimeout = d } }

// WithLogger 只给框架自己的启停日志换一个 logger，主要用于测试里静音；
// 业务代码和各集成照样写 slog.Default()。要换整个进程的日志后端，用 xlog.UseHandler。
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// MustRun 同 Run，出错直接退出。
func MustRun(r Runnable, opts ...Option) {
	if err := Run(r, opts...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
