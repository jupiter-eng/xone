// Package xcron 进程内的定时任务：Add 登记，框架起停。
//
// Web 服务里带几个后台任务：
//
//	func main() {
//		xcron.Add("*/5 * * * *", syncOrders)                // 每 5 分钟（UTC）
//		xcron.Add("@every 30s", refresh, xcron.RunOnStart()) // 起来先跑一次
//		xone.MustRun(xgin.New().WithRoutes(routes))
//	}
//
// 只跑定时任务的进程：
//
//	xone.MustRun(xone.UntilSignal())
//
// 调度器在 StageServer 那一档起来——客户端（xgorm、xredis……）和你的业务钩子都已就绪，
// 任务里直接 xgorm.C()；停的时候它最先停，等在途的执行返回之后才轮到关客户端。
//
// 每次执行有自己的根 Span（cron <name>），ctx 里带着 job=<name> 这个日志字段，
// 结束记一行 cron job finished / failed；panic 被接住，记 ERROR，不影响别的任务和进程。
//
// 多副本部署时每个副本都会跑，只想一个副本跑的要自己抢锁，见 README「多副本」。
package xcron

import (
	"context"

	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"

	// 用了 xcron 就有链路：xtrace 装好全局的 TracerProvider，每次执行的 Span 经 otel 的全局 API 记下来
	_ "github.com/jupiter-eng/xone/xtrace"
)

// Add 登记一个定时任务。spec 是标准的 5 段 cron（分 时 日 月 周），或者
// @yearly / @monthly / @weekly / @daily / @hourly / @every <时长>；默认按 UTC 解释，见 WithLocation。
//
//	err := xcron.Add("0 3 * * *", cleanup, xcron.WithTimeout(10*time.Minute))
//
// 当场校验，返回 *xerror.Error（模块 xcron）：spec 写错、fn 是 nil、名字为空或重复是 config；
// 退出流程已经开始之后再 Add 是 register。
//
// 什么时候调都行：在 xone.Run 之前登记的，调度器起来时一起开始；之后（比如在某个钩子里）登记的，
// 当场开始。带 RunOnStartAndWait 的在调度器起来之后 Add，会等第一次跑完，失败就返回它的错误（op execute），
// 这个任务也不登记。
//
// fn 收到的 ctx 在退出时被取消（WithTimeout 到点也是）。它返回的错误只记日志，不影响下一次。
func Add(spec string, fn func(ctx context.Context) error, opts ...Option) error {
	if fn == nil {
		return xerror.Newf("xcron", "config", "job with spec %q has a nil function", spec)
	}
	o := buildOptions(fn, opts)
	if err := o.validate(); err != nil {
		return xerror.Newf("xcron", "config", "job %q: %w", o.name, err)
	}
	sched, err := parse(spec)
	if err != nil {
		return xerror.Newf("xcron", "config", "job %q: bad spec %q: %w", o.name, spec, err)
	}
	return std.add(&job{name: o.name, sched: sched, fn: fn, o: o})
}

// ---- 登记 ----

// 一对钩子起停整个调度器。StageServer：客户端和业务钩子都起来了它才起，
// 停的时候它先停——在途的任务还在用数据库，等它们返回之后才轮到关数据库
func init() {
	xhook.BeforeStart(startXCron, xhook.At(xhook.StageServer))
	xhook.BeforeStop(stopXCron) // 档位跟着上面那个启动钩子
}

func startXCron(ctx context.Context) error { return std.start(ctx) }

func stopXCron(ctx context.Context) error { return std.stop(ctx) }
