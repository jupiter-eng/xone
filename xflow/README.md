# xflow

Orchestrates a series of steps into one flow (a core module): when a strongly depended-on step fails, the steps already done are rolled back in reverse.

- Each step writes only `Process` and `Rollback`, on the same type: whoever did something is responsible for undoing it
- A failing strong dependency interrupts the flow and rolls it back; a failing weak dependency is only recorded and the flow continues
- A rollback keeps the values in the caller's ctx (traces, log fields, load-test markers, lane markers) and only drops its cancellation and deadline, with a budget of its own instead
- The default monitor writes to slog: one line per step (INFO on success, WARN on failure) plus one result line per execution; it can be replaced with an implementation of your own or turned off
- It works without `xone.Run`: a core module depending only on yaml

## Quick start

```go
// Order is the data carried through the whole flow: the inputs and every step's intermediate results go here
type Order struct {
	ID, SKU  string
	Amount   int64
	chargeID string // recorded once the charge succeeds, used for the refund on rollback
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
		return nil // nothing was charged, so there is nothing to refund; a rollback is written to be idempotent
	}
	return payment.Refund(ctx, o.chargeID)
}

type notify struct{}

func (notify) Process(ctx context.Context, o *Order) error { return sms.Send(ctx, o.ID) }
func (notify) Rollback(context.Context, *Order) error      { return nil }
func (notify) Dependency() xflow.Dependency                { return xflow.Weak } // a weak dependency

var placeOrder = xflow.New[*Order]("place_order", reserveStock{}, charge{}, notify{})

func Place(ctx context.Context, o *Order) error {
	res := placeOrder.Execute(ctx, o)
	if len(res.RollbackErrors) > 0 { // a resource was not compensated and needs a human
		slog.ErrorContext(ctx, "order compensation failed", "order_id", o.ID, "result", res.String())
	}
	return res.Err // non-nil when a strong dependency failed or the ctx was cancelled
}
```

When `charge` fails, `reserveStock` is rolled back and `notify` does not run; when `notify` fails it is only recorded in
`res.Skipped` and the order succeeds as usual.

## Configuration

```yaml
XFlow:
  Monitor: true            # with it off, Execute makes no monitoring callback at all
  RollbackTimeout: 30s     # the total budget for rolling every step back, must be > 0
```

- The `XFlow` block is only read when the framework starts; without `xone.Run` the defaults above apply and changing them
  means writing it in code (see [below](#standalone-use-without-xonerun)).

## API

| Function | Description |
|---|---|
| `New[T any](name string, steps ...Processor[T]) *Flow[T]` | Builds a flow in the order the steps are passed. A nil step panics outright |
| `(*Flow[T]) Execute(ctx, data T) *Result` | Runs it once. `data` is carried through for every step to read and write |
| `(*Flow[T]) WithRollbackTimeout(d) *Flow[T]` | Sets this flow's own rollback budget, overriding `XFlow.RollbackTimeout`. It returns a new flow and leaves the original untouched; `d <= 0` panics outright |
| `(*Flow[T]) Name() string` | The flow name |
| `SetMonitor(m Monitor)` | Replaces the default monitoring implementation; nil turns it off |

A step implements `Processor[T]`: `Process` / `Rollback` are required, while `Name() string` (without it, the type name)
and `Dependency() Dependency` (`xflow.Strong` / `xflow.Weak`; without it, `Strong`) are optional.

`Result`'s fields: `Err` (a strong dependency failed or the ctx was cancelled), `Skipped` (the failed weak dependencies),
`RollbackErrors` (non-empty means a human is needed) and `Rolled` (whether at least one step was really rolled back);
its methods are `Success()` (`Err == nil`) and `String()`. When a step panics the error holds an `*xflow.PanicError`,
with the call stack in its `Stack` field.

## Notes

- **The rollback ctx is the caller's ctx with cancellation removed** (`context.WithoutCancel`): the trace, the baggage,
  the fields from `xlog.AddKV`, load-test markers and lane markers are all still there, so compensation requests still
  propagate downstream. Cancellation and the deadline are removed because compensation is needed most once a request has
  timed out, by which point the caller's ctx is already cancelled. The rollback is limited by `XFlow.RollbackTimeout`
  (30s by default) instead, with `WithRollbackTimeout` for an individual flow.
- **Once the budget is reached nothing is waited for**: a `Rollback` that ignores the ctx is abandoned too, that step
  and the ones that never got a turn are recorded in `RollbackErrors`, and `Execute` returns right away; the abandoned
  step's goroutine keeps running and may still read and write `data`, which is why a `Rollback` should watch the ctx.
- **`Rollback` cannot assume `Process` succeeded**: a weak dependency that failed is still rolled back when a later
  strong dependency fails, so it has to be idempotent.
- xflow opens no Span; for tracing, call `otel.Tracer(...).Start` inside the step.

## Standalone use: without `xone.Run`

The code above runs unchanged without calling `xone.Run` (`go get github.com/jupiter-eng/xone`), with the default
configuration (monitoring on, a 30s rollback budget). Changing it means writing it in code:

```go
var placeOrder = xflow.New[*Order]("place_order", reserveStock{}, charge{}, notify{}).
	WithRollbackTimeout(10 * time.Second) // only this flow, overriding XFlow.RollbackTimeout

func init() { xflow.SetMonitor(nil) } // no per-step monitoring logs; pass an implementation of your own instead
```

## Observability

### Logs

The default monitor (with `SetMonitor` not replaced and `XFlow.Monitor` not turned off) writes these:

| Message | Level | Fields |
|---|---|---|
| `xflow flow done` / `xflow flow failed` | INFO / WARN | `flow`, `elapsed_ms` (milliseconds), `result` |
| `xflow rollback did not complete, resources may be left dangling` | ERROR | the same, plus `uncompensated_steps` |
| `xflow step process failed` / `xflow step rollback failed` | WARN | `flow`, `step`, `dependency`, `elapsed_ms`, `error`; plus `stack` on a panic |
| `xflow step process done` / `xflow step rollback done` | INFO | `flow`, `step`, `dependency`, `elapsed_ms` |

### Tracing

| Source | Span name | Key attributes |
|---|---|---|
| xflow | — | xflow opens no Span; use `otel.Tracer(...)` inside the step |

The whole tracing picture, propagation and the trust boundary are in
[`docs/observability.md` "Tracing"](../docs/observability.md#tracing).
