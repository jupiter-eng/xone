# xcron

进程内的定时任务：`xcron.Add` 登记，框架起停。没有配置块，全部写在代码里。

- 每次执行一个根 Span（`cron <name>`），ctx 里带着 `job` 日志字段和 `trace_id`
- 上一次还没跑完时这一次跳过并记 WARN，不越积越多（`AllowOverlap` 改成并发）
- panic 被接住，记 ERROR、带栈，别的任务和进程照常
- 退出时取消在途执行的 ctx，等它们返回之后才关数据库；到点没返回的点名报出来
- 默认按 UTC 解释 spec；`WithLocation` 换时区
- `RunOnStartAndWait`：起来先跑一次，跑不成功服务就不启动

## 快速上手

**Web 服务里带几个后台任务**——任务在后台跑，服务照常：

```go
import (
	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/xcron"
	"github.com/jupiter-eng/xone/xgin"
)

func main() {
	if err := xcron.Add("*/5 * * * *", syncOrders); err != nil { // 每 5 分钟（UTC）
		log.Fatal(err)
	}
	xcron.Add("@every 30s", refreshRates, xcron.RunOnStart()) // 起来先跑一次，不等它
	xone.MustRun(xgin.New().WithRoutes(routes))
}

func syncOrders(ctx context.Context) error {
	// ctx 退出时被取消；往下游传它
	return xgorm.CWithCtx(ctx).Exec("...").Error
}
```

**只跑定时任务的进程**：

```go
func main() {
	xcron.Add("0 3 * * *", cleanup, xcron.WithTimeout(30*time.Minute))
	xone.MustRun(xone.UntilSignal())
}
```

**启动前跑一次、失败就不启动**（迁移、预热）不需要 xcron：写成 `xhook.BeforeStart(migrate)`，
它在客户端建好之后、服务开始监听之前跑，返回错误启动就失败。跑一次就退出的进程用 `xone.Func`。

## API

| 函数 | 说明 |
|---|---|
| `Add(spec, fn, opts...) error` | 登记一个任务。当场校验，错误是 `*xerror.Error`（模块 `xcron`）：spec 写错、`fn` 是 nil、名字为空或重复是 `config`，退出开始之后再 Add 是 `register` |

`spec`：标准的 5 段 `分 时 日 月 周`，或者 `@yearly` / `@annually` / `@monthly` / `@weekly` / `@daily` / `@hourly` / `@every <时长>`。
`<时长>` 是 `time.ParseDuration` 的写法（`90s`、`1h30m`），必须大于 0。

| Option | 默认 | 说明 |
|---|---|---|
| `WithName(name)` | 函数名，见下 | 日志的 `job`、Span 名、报错里都用它；同一个进程里不能重复 |
| `WithTimeout(d)` | 不限时 | 每一次执行的超时，到点取消 ctx；也管 `RunOnStart*` 的那第一次 |
| `AllowOverlap()` | 不允许 | 上一次没跑完也开始下一次，两次并发 |
| `WithLocation(loc)` | UTC | 按哪个时区解释 spec |
| `RunOnStart()` | 不跑 | 调度器起来时先在后台跑一次，不等它；失败只记日志 |
| `RunOnStartAndWait()` | 不跑 | 调度器起来时先跑一次，启动等它；失败启动就失败 |

**默认名字**是函数名，去掉 import path 前缀和方法值的 `-fm` 后缀（Go 1.25 实测）：

| 传进去的 | 默认名字 |
|---|---|
| 普通函数 `cleanup` | `main.cleanup` |
| 方法值 `svc.Sync`（值接收者） | `main.Service.Sync` |
| 方法值 `svc.Sync`（指针接收者） | `main.(*Service).Sync` |
| 闭包 | `main.main.func1` |

闭包的名字是编译器按出现顺序编的号，挪一下代码就变，看板和告警跟着断——传闭包时写 `WithName`。
同一个函数登记两次（比如两个 spec）会撞名，第二个要 `WithName`。

## 什么时候跑、什么时候停

调度器在 `StageServer` 那一档起来：客户端（xgorm、xredis……）和你的业务钩子都已就绪，任务里直接 `xgorm.C()`。
在 `xone.Run` 之前 `Add` 的，调度器起来时一起开始；之后（比如在某个钩子里）`Add` 的，当场开始。

- **`RunOnStartAndWait`** 的任务在启动阶段并行跑完第一次，全部成功了启动才继续；任何一个失败，
  启动失败，报错里点名是哪几个（`xone xcron start failed, err=[first run failed: job "load-config": ...]`）。
  启动期间收到退出信号时它们的 ctx 被取消。调度器起来之后再 `Add` 的，`Add` 等第一次跑完、返回它的错误，失败的不登记。
- **退出**：Web 服务先停（等在途请求），然后 xcron 最先停——不再发起新的执行、取消在途执行的 ctx、
  在它那一份停止预算里等它们返回，之后才轮到关数据库和缓存。到点还有没返回的，停止钩子的错误和一条 WARN 里点名是哪几个任务。
  服务停下来的那段时间里任务照常调度。
- **任务要停得下来就得看 ctx**：Go 没有从外面停下一个协程的办法，取消 ctx 是能做的全部。
  xcron 是第一个停止钩子，能等的时间是「退出预算（`WithStopTimeout`，默认 15s）减去服务停下来用掉的，
  再给排在后面的每个停止钩子各留最多 1s」：只跑定时任务的进程里服务不占时间，接了 xgorm、xredis、xlog 等 6 个组件时约 9s；
  Web 服务用满它那 2/3 时只剩几百毫秒。规则见 [`docs/architecture.md`「停止预算是一份」](../docs/architecture.md#停止预算是一份不是每个组件一份)。

## 多副本

**每个副本都会跑。** xcron 是进程内的调度器，不知道别的副本在不在；部署 3 个副本，`0 3 * * *` 每天就跑 3 次。
只想一个副本跑的，自己抢锁，比如用 xredis 的 `SetNX`：

```go
xcron.Add("0 3 * * *", func(ctx context.Context) error {
	// 锁的有效期：比任务最长的耗时长，比两次之间的间隔短。
	// 跑完不删锁：副本之间的时钟差着几毫秒，删早了，晚到的那个副本会再抢到、再跑一遍
	got, err := xredis.C().SetNX(ctx, "lock:cleanup", hostname, 30*time.Minute).Result()
	if err != nil || !got {
		return err // 别的副本拿到了，这一轮不跑
	}
	return cleanup(ctx)
}, xcron.WithName("cleanup"), xcron.WithTimeout(20*time.Minute))
```

`WithTimeout` 要小于锁的有效期：否则任务还在跑、锁先过期，下一个时间点别的副本就能再抢到。

## 可观测

### 日志

| 消息 | 级别 | 字段 |
|---|---|---|
| `cron job started` | DEBUG | `job`、`trace_id` |
| `cron job finished` | INFO | `job`、`trace_id`、`elapsed_ms` |
| `cron job failed` | WARN | `job`、`trace_id`、`elapsed_ms`、`error` |
| `cron job panicked` | ERROR | `job`、`trace_id`、`elapsed_ms`、`error`、`stack` |
| `cron job skipped, previous run still running` | WARN | `job` |
| `xcron ready` | INFO | `jobs` |
| `xcron jobs still running when the stop budget ran out` | WARN | `jobs` |

- `job` 在任务的 ctx 里，任务里用 `slog.InfoContext(ctx, …)` 记的每一行都带着；任务里 `xlog.AddKV(ctx, …)` 的字段也进结束那一行。
- 开始那一行记 DEBUG：`@every 1s` 的任务记 INFO 就是每秒两行，而结束那一行带着 `elapsed_ms`，开始时刻倒推得出来。
  卡住不返回的任务看不到结束那一行，退出时的报错会点名它。
- 用了 xcron 就有 xlog（同 xgin）：只跑定时任务的进程也是 JSON 日志、带 `trace_id`。

日志的全局约定见 [`docs/observability.md`](../docs/observability.md#日志)。

### 链路

| 来源 | Span 名 | 说明 |
|---|---|---|
| xcron | `cron <name>` | 每次执行一个根 Span，`SpanKindInternal`；失败和 panic 时状态标成 Error，描述是 `cron job failed` / `cron job panicked`，不记错误原文（它常包着带参数值的 SQL、Redis 错误，原文在日志里）。任务里的数据库、Redis、出站 HTTP 的 Span 都挂在它下面 |

用了 xcron 就有链路（它带着 xtrace），不用另外 import。

### 指标

xcron 不导出指标：只跑定时任务的进程没有 `/metrics` 端点，为它带上 Prometheus 的依赖不值得。
要按任务看耗时，在任务里自己打点：

```go
start := time.Now()
err := cleanup(ctx)
xmetric.ObserveDuration("cron_job_duration", time.Since(start), xmetric.T("job", "cleanup"), xmetric.T("status", status(err)))
```

## 行为与实测

实测环境和跨模块的总表见 [`docs/behavior.md`](../docs/behavior.md)。

hashicorp/cronexpr v1.1.3（只用来解析和算下一个时间点，调度循环是 xcron 自己的）、Go 1.25。升级 cronexpr 之后
`schedule_test.go` 里 `TestCronexpr_*` 那一组哪一条红了，这里对应的那条就要重量。

**cronexpr 收的比 5 段多，这里只收 5 段。**

| spec | cronexpr 自己 | xcron |
|---|---|---|
| 6 段 `0 30 2 * * *` | 多出来的那段是**年**，不是秒：报 `syntax error in hour field: '30'`；`30 2 * * * 2027` 从 2027 年才开始跑 | `want 5 fields` |
| 7 段 | 开头多一段秒、末尾一段年 | `want 5 fields` |
| 8 段及以上 | 第 7 段之后的悄悄丢掉 | `want 5 fields` |
| `CRON_TZ=Europe/Berlin 30 2 * * *` | `syntax error in minute field: 'CRON_TZ=Europe/Berlin'` | 指向 `WithLocation` |
| `@midnight`、`@reboot`、`@every 1m` | 一律 `missing field(s)` | `@every` 自己实现；其余报 `unknown descriptor` |
| `0 0 30 2 *`（2 月 30 日） | 解析成功，`Next` 返回零值：任务登记上了却一次都不跑 | `spec never fires` |

`L`、`W`、`#`、`?`、月份和星期的英文缩写（`MON-FRI`、`JAN`）它也认，xcron 不拦。

**时区**：cronexpr 按传进去的时刻的时区算下一个时间点——同一个 `30 2 * * *`，传柏林时间得到柏林的 02:30，
传 UTC 得到 UTC 的 02:30。`WithLocation` 就是把「现在」换到那个时区再交给它；不写是 UTC，和机器的 `TZ` 无关。

**夏令时**（`WithLocation(America/New_York)`，2026-03-08 02:00 拨快到 03:00，2026-11-01 02:00 拨回 01:00）：

| spec | 拨快那天 | 拨回那天 |
|---|---|---|
| `30 2 * * *` | 02:30 不存在，**整天跳过**（3-07 之后下一次是 3-09），不挪到 03:30 | 跑一次（EST 02:30，UTC 07:30） |
| `30 1 * * *` | 照常 | **跑两次**：EDT 01:30 和 EST 01:30，相隔一小时 |
| `0 * * * *` | 01:00 之后是 03:00 | 01:00 跑两次 |

有夏令时的时区里，落在 01:00–03:00 的任务要么某天不跑、要么某天跑两次。不想要这个就用默认的 UTC。

**下一个时间点严格在「现在」之后**：从 10:01:00 整算 `* * * * *` 得到 10:02:00。墙上时钟被往回拨（NTP 校时）时，
xcron 从上一个时间点往后算，不会把刚跑过的那个再跑一次。

**`@every <d>` 不对齐**：从每次触发的那一刻往后数 `d`——10:00:17.5 起的 `@every 1m` 下一次是 10:01:17.5，
不对齐到整分，也不截掉毫秒。第一次在调度器起来之后 `d` 才跑（要立刻跑一次写 `RunOnStart`）。
每次都从实际触发的时刻往后数，计时器的延迟会累积：实测 `@every 10ms` 跑 2s，每次比间隔晚约 0.32ms
（4 核 Linux）。`@every 1m` 一天累积约 0.5s，要对齐整点的用 5 段写法。

**到了时间点而上一次还在跑**：默认跳过，记 `cron job skipped, previous run still running`，下一个时间点照常；
所以一个比间隔还慢的任务，实际节奏是「跑完之后的下一个时间点」。`AllowOverlap` 时每个时间点都发起一次。

## 排错

### `xone xcron config failed, err=[job "main.main.func1": bad spec "...": want 5 fields ...]`

spec 不是 5 段。秒级的写法（6 段）不收，要秒级用 `@every 10s`；cronexpr 的 6 段是「带年份」，见上面的表。

### `duplicate job name "main.cleanup": give one of them xcron.WithName`

同一个函数登记了两次，或者两个闭包恰好同名。给其中一个写 `WithName`。

### `xone xcron register failed, err=[cannot add job ...: the scheduler is shutting down]`

退出流程已经开始之后才 `Add`。多半是在 `Start` 返回之后、或者停止钩子里登记的。

### `xone xcron stop failed, err=[jobs [cleanup] still running when the stop budget ran out: context deadline exceeded]`

这些任务在停止预算用完时还没返回：没看 ctx，或者收尾比预算长。让任务把 ctx 传给它调的每一个阻塞操作，
或者调大 `xone.WithStopTimeout`。
