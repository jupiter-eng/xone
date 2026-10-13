# xflow

把一串步骤编排成一个流程（核心模块）：强依赖的步骤失败时，已经做过的步骤逆序回滚。

- 每一步只写 `Process` 和 `Rollback`，写在同一个类型上：谁做的事，谁负责撤销
- 强依赖失败就中断并回滚；弱依赖失败只记一笔，继续往下走
- 回滚沿用调用方 ctx 里的值（链路、日志字段、压测标、泳道标……），只去掉它的取消和截止时间，另给一份总预算
- 默认监控写 slog：每一步一条（成功 INFO、失败 WARN），每次执行再一条结果日志；可以换成自己的实现或关掉
- 不跑 `xone.Run` 也能用：核心 module，只依赖 yaml

## 快速上手

```go
// Order 贯穿整个流程的数据：入参、各步的中间结果都放在这里
type Order struct {
	ID, SKU  string
	Amount   int64
	chargeID string // 扣款成功后记下，回滚时退款用
}

type reserveStock struct{}

func (reserveStock) Process(ctx context.Context, o *Order) error  { return inventory.Reserve(ctx, o.SKU, 1) }
func (reserveStock) Rollback(ctx context.Context, o *Order) error { return inventory.Release(ctx, o.SKU, 1) }

type charge struct{}

func (charge) Process(ctx context.Context, o *Order) (err error) {
	o.chargeID, err = payment.Charge(ctx, o.ID, o.Amount)
	return err
}
func (charge) Rollback(ctx context.Context, o *Order) error {
	if o.chargeID == "" {
		return nil // 没扣成功就没什么可退；回滚要写成幂等的
	}
	return payment.Refund(ctx, o.chargeID)
}

type notify struct{}

func (notify) Process(ctx context.Context, o *Order) error { return sms.Send(ctx, o.ID) }
func (notify) Rollback(context.Context, *Order) error      { return nil }
func (notify) Dependency() xflow.Dependency                { return xflow.Weak } // 弱依赖

var placeOrder = xflow.New[*Order]("place_order", reserveStock{}, charge{}, notify{})

func Place(ctx context.Context, o *Order) error {
	res := placeOrder.Execute(ctx, o)
	if len(res.RollbackErrors) > 0 { // 有资源没补偿回来，要人工介入
		slog.ErrorContext(ctx, "order compensation failed", "order_id", o.ID, "result", res.String())
	}
	return res.Err // 强依赖失败或 ctx 取消时非 nil
}
```

`charge` 失败时 `reserveStock` 被回滚、`notify` 不执行；`notify` 失败只记进 `res.Skipped`，订单照常成功。

## 配置

```yaml
XFlow:
  Monitor: true            # 关掉之后 Execute 一次监控回调都不走
  RollbackTimeout: 30s     # 回滚全部步骤的总预算，必须 > 0
```

- `XFlow` 块只在框架启动时读；不跑 `xone.Run` 就是上面的默认值，要改写在代码里（见[下文](#单独使用不跑-xonerun)）。

## API

| 函数 | 说明 |
|---|---|
| `New[T any](name string, steps ...Processor[T]) *Flow[T]` | 按传入顺序构建流程。传 nil 步骤直接 panic |
| `(*Flow[T]) Execute(ctx, data T) *Result` | 执行一次。`data` 贯穿全程供各步读写 |
| `(*Flow[T]) WithRollbackTimeout(d) *Flow[T]` | 给这个流程单独定回滚预算，压过 `XFlow.RollbackTimeout`。返回新流程，不影响原流程；`d <= 0` 直接 panic |
| `(*Flow[T]) Name() string` | 流程名 |
| `SetMonitor(m Monitor)` | 换掉默认的监控实现；传 nil 关掉 |

步骤实现 `Processor[T]`：必须有 `Process` / `Rollback`；可选 `Name() string`（不写就是类型名）和
`Dependency() Dependency`（`xflow.Strong` / `xflow.Weak`，不写就是 `Strong`）。

`Result` 的字段：`Err`（强依赖失败或 ctx 取消）、`Skipped`（弱依赖失败的记录）、`RollbackErrors`（非空要人工介入）、
`Rolled`（是否真的回滚过至少一步）；方法 `Success()`（`Err == nil`）、`String()`。步骤 panic 时错误里是 `*xflow.PanicError`，调用栈在它的 `Stack` 字段。

## 注意事项

- **回滚的 ctx 是调用方的 ctx 去掉取消**（`context.WithoutCancel`）：trace、baggage、`xlog.AddKV` 的字段、压测标、泳道标都在，
  补偿请求照样透传给下游。去掉取消和截止时间是因为请求一超时补偿最需要执行，而那时调用方的 ctx 已经取消了。
  回滚改由 `XFlow.RollbackTimeout`（默认 30s）限时，个别流程用 `WithRollbackTimeout` 单独定。
- **预算到点就不再等**：不看 ctx 的 `Rollback` 也会被放弃，那一步和没轮到的步骤记进 `RollbackErrors`，`Execute` 随即返回；
  被放弃的那一步的协程仍在后台跑、仍可能读写 `data`，所以 `Rollback` 要看 ctx。
- **`Rollback` 不能假设 `Process` 成功过**：失败了的弱依赖步骤在后面的强依赖失败时同样会被回滚，要写成幂等的。
- xflow 不开 Span，要链路就在步骤里自己 `otel.Tracer(...).Start`。

## 单独使用：不跑 `xone.Run`

上面的代码不调 `xone.Run` 也原样能跑（`go get github.com/jupiter-eng/xone`），配置就是默认值（监控开着、回滚预算 30s）。要改就写在代码里：

```go
var placeOrder = xflow.New[*Order]("place_order", reserveStock{}, charge{}, notify{}).
	WithRollbackTimeout(10 * time.Second) // 只管这个流程，压过 XFlow.RollbackTimeout

func init() { xflow.SetMonitor(nil) } // 不要每一步的监控日志；换成自己的实现就传它
```

## 可观测

### 日志

默认的监控（`SetMonitor` 没换掉、`XFlow.Monitor` 没关）写这些：

| 消息 | 级别 | 字段 |
|---|---|---|
| `xflow flow done` / `xflow flow failed` | INFO / WARN | `flow`、`elapsed_ms`（毫秒）、`result` |
| `xflow rollback did not complete, resources may be left dangling` | ERROR | 同上，加 `uncompensated_steps` |
| `xflow step process failed` / `xflow step rollback failed` | WARN | `flow`、`step`、`dependency`、`elapsed_ms`、`error`；panic 时加 `stack` |
| `xflow step process done` / `xflow step rollback done` | INFO | `flow`、`step`、`dependency`、`elapsed_ms` |

### 链路

| 来源 | Span 名 | 关键属性 |
|---|---|---|
| xflow | —— | xflow 不开 Span，步骤里自己用 `otel.Tracer(...)` |

链路的全貌、传播与信任边界见 [`docs/observability.md`「链路」](../docs/observability.md#链路)。
