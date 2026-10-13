// Package xflow 把一串步骤编排成一个流程，强依赖失败时自动逆序回滚。
//
//	flow := xflow.New("下单",
//		&扣券{}, &扣库存{}, &扣款{}, &发通知{},
//	)
//	result := flow.Execute(ctx, data)
//	if !result.Success() { ... }
//
// 一个步骤只要写 Process 和 Rollback。默认是强依赖：失败就中断并回滚；
// 弱依赖（失败只记一笔、继续往下走）再写一个 Dependency() 返回 Weak。
//
// 本包零第三方依赖，所以留在核心模块里。
package xflow

import (
	"cmp"
	"context"
	"fmt"
	"reflect"
	"runtime/debug"
	"time"

	"github.com/jupiter-eng/xone/xerror"
)

// Dependency 步骤的依赖强弱
type Dependency int

const (
	// Strong 强依赖：失败就中断流程并回滚
	Strong Dependency = iota
	// Weak 弱依赖：失败只记一笔，继续往下走
	Weak
)

func (d Dependency) String() string {
	switch d {
	case Strong:
		return "strong"
	case Weak:
		return "weak"
	default:
		return "unknown"
	}
}

// Processor 流程里的一步。
//
// 类型参数 T 是贯穿整个流程的共享数据，建议用指针（如 *OrderData）：
// 入参、出参、各步骤之间的中间结果都放进这一个结构体，
// 于是每一步的方法签名里只出现业务自己的类型，不必重复框架的泛型。
//
// Process 和 Rollback 要写在同一个类型上：谁做的事，谁负责撤销。
// 拆开写迟早会出现「加了一步正向逻辑，忘了配套的补偿」。
//
// 另有两个可选的方法，写了才用，构建流程时读一次：
//
//	Name() string            步骤名，用于日志和错误信息。不写就是类型名：&扣券{} 叫「扣券」
//	Dependency() Dependency  强依赖还是弱依赖。不写就是 Strong
type Processor[T any] interface {
	// Process 正向逻辑
	Process(ctx context.Context, data T) error

	// Rollback 补偿逻辑，强依赖失败时逆序调用已成功的步骤。
	//
	// 两件事要留意：
	//   - 不能假设 Process 完全成功。弱依赖失败之后仍会被回滚，
	//     所以这里要做幂等处理。
	//   - 用的 ctx 已经剥掉了原来的取消和超时，改由 RollbackTimeout 限时
	//     （见 rollback 的说明）。预算到点还没返回的，框架不再等它、记一笔超时，
	//     但它的协程还在跑，之后仍可能读写 data——要看 ctx，别让它悬着。
	Rollback(ctx context.Context, data T) error
}

// StepError 某一步出的错
type StepError struct {
	Processor  string
	Dependency Dependency
	Err        error
}

func (e *StepError) Error() string {
	return fmt.Sprintf("step %q (%s dependency) failed: %v", e.Processor, e.Dependency, e.Err)
}

func (e *StepError) Unwrap() error { return e.Err }

// PanicError 步骤的 Process 或 Rollback panic 了，它会出现在 StepError.Err 里。
//
// 调用栈放在 Stack 里而不是错误消息里：消息会进告警标题、进日志检索，
// 几十行的栈塞进去，一条错误就被拆成了几十行。默认监控把它记成单独的
// stack 字段；自己实现 Monitor 的用 errors.As 取。
type PanicError struct {
	Value any    // recover() 拿到的值
	Stack []byte // panic 那一刻的调用栈
}

func (e *PanicError) Error() string { return fmt.Sprintf("panicked: %v", e.Value) }

// Unwrap panic 出来的本身是 error 时返回它，errors.Is / errors.As 才问得出根因
func (e *PanicError) Unwrap() error {
	err, _ := e.Value.(error)
	return err
}

// Result 一次执行的结果。
//
// 业务数据不在这里——它在调用方传进 Execute 的那个 T 里。
// 流程失败时，此前各步写进去的内容依然保留，便于排查和人工补偿。
type Result struct {
	// Err 致命错误：强依赖失败，或者 ctx 被取消。nil 表示流程走完了。
	Err error

	// Skipped 弱依赖失败的记录，不影响流程继续
	Skipped []*StepError

	// RollbackErrors 回滚过程中出的错。
	//
	// 这个列表非空意味着有资源没能补偿回来，需要人工介入。
	RollbackErrors []*StepError

	// Rolled 是否真的回滚过至少一步。
	//
	// 第一步开始前就被取消的流程没有可回滚的，这里是 false。
	Rolled bool
}

// Success 流程是否走完了（没有强依赖失败）
func (r *Result) Success() bool { return r.Err == nil }

func (r *Result) String() string {
	if r.Err == nil {
		if len(r.Skipped) > 0 {
			return fmt.Sprintf("flow succeeded, %d weak step(s) skipped after failing", len(r.Skipped))
		}
		return "flow succeeded"
	}
	msg := fmt.Sprintf("flow failed: %v", r.Err)
	if r.Rolled {
		msg += ", rolled back"
	}
	if n := len(r.RollbackErrors); n > 0 {
		msg += fmt.Sprintf(", %d step(s) failed to roll back", n)
	}
	return msg
}

// Flow 一个编排好的流程。
//
// 构建之后字段不再变化，可以并发 Execute。
type Flow[T any] struct {
	name  string
	steps []entry[T]

	// rollbackTimeout 这个流程自己的回滚预算，0 表示跟着 XFlow.RollbackTimeout 走
	rollbackTimeout time.Duration
}

// entry 一个步骤，连同构建时就定下来的名字和依赖强弱
type entry[T any] struct {
	Processor[T]
	name string
	dep  Dependency
}

// entryOf 读出步骤可选的 Name / Dependency，没写的用默认值
func entryOf[T any](p Processor[T]) entry[T] {
	s := entry[T]{Processor: p, name: typeName(p), dep: Strong}
	if n, ok := p.(interface{ Name() string }); ok {
		s.name = n.Name()
	}
	if d, ok := p.(interface{ Dependency() Dependency }); ok {
		s.dep = d.Dependency()
	}
	return s
}

// fail 把这一步出的错记成 StepError
func (s entry[T]) fail(err error) *StepError {
	return &StepError{Processor: s.name, Dependency: s.dep, Err: err}
}

// typeName 没写 Name 的步骤用它的类型名：&扣券{} → 扣券
func typeName(p any) string {
	t := reflect.TypeOf(p)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if n := t.Name(); n != "" {
		return n
	}
	return t.String()
}

// New 按传入顺序构建流程。
//
// 传 nil 步骤直接 panic：它只会在执行到那一步时炸成空指针，
// 而那时错误早就脱离了构建现场，越早暴露越好。
func New[T any](name string, steps ...Processor[T]) *Flow[T] {
	out := make([]entry[T], len(steps))
	for i, p := range steps {
		if p == nil {
			panic(fmt.Sprintf("xflow: step %d of flow %q is nil", i, name))
		}
		out[i] = entryOf(p)
	}
	return &Flow[T]{name: name, steps: out}
}

// Name 流程名
func (f *Flow[T]) Name() string { return f.name }

// WithRollbackTimeout 给这个流程单独定回滚的总预算，压过配置里的 XFlow.RollbackTimeout。
//
// 不跑 xone.Run、单独用 xflow 时，这是改回滚预算的办法：配置文件只在框架启动时才读。
// 返回一个新的 Flow，原来那个不变，所以已经在别处并发 Execute 的流程不受影响。
// d 必须为正：0 会让回滚一进去就判超时、所有补偿被跳过，传进来就 panic。
func (f *Flow[T]) WithRollbackTimeout(d time.Duration) *Flow[T] {
	if d <= 0 {
		panic(fmt.Sprintf("xflow: rollback timeout of flow %q must be > 0, got=%v", f.name, d))
	}
	g := *f
	g.rollbackTimeout = d
	return &g
}

// Execute 按顺序执行各步骤，data 贯穿全程供各步读写。
//
// 强依赖失败或 ctx 被取消时中断并逆序回滚已执行的步骤；
// 弱依赖失败记进 Skipped 后继续，但同样会被纳入回滚范围——
// 它可能已经产生了副作用，只是后面没走下去而已。
func (f *Flow[T]) Execute(ctx context.Context, data T) *Result {
	if ctx == nil {
		ctx = context.Background()
	}

	m := activeMonitor()
	res := &Result{}
	start := now(m)

	// 用 defer 而不是在每个出口各写一遍：出口有三个（取消、失败、正常走完），
	// 以后再加一个分支时漏掉通知，表现是监控上这次执行凭空消失，
	// 而流程本身照常返回——不会有人发现
	defer func() { f.notifyFlow(ctx, m, res, start) }()

	// 要回滚的总是已执行的那一段前缀：成功的、失败的弱依赖都算在内，
	// 只有失败的强依赖不算——而它一定是最后执行的那一个。所以记个数就够了
	n := 0

	for _, s := range f.steps {
		// 调用方已经不等了，就不再启动新步骤；但已经做完的仍要回滚
		if err := ctx.Err(); err != nil {
			res.Err = xerror.Newf("xflow", "execute", "flow %q canceled before step %q: %w", f.name, s.name, err)
			f.rollback(ctx, data, f.steps[:n], res, m)
			return res
		}

		stepStart := now(m)
		err := safeProcess(ctx, s.Processor, data)
		f.notifyStep(ctx, m, false, s, err, stepStart)

		if err == nil {
			n++
			continue
		}

		se := s.fail(err)

		if s.dep == Weak {
			res.Skipped = append(res.Skipped, se)
			n++ // 失败的弱依赖也可能留下了副作用，同样要回滚

			// 弱依赖失败只记一笔继续走——除非它是被取消带下水的。
			//
			// 取消只在每步开始前查一次的话，最后一步撞上取消就查不到了：
			// 它的 context.Canceled 走进这个「跳过」分支，循环随即结束，
			// 于是一个被取消的流程报成了 Success，还一步都没回滚。
			if ctx.Err() == nil {
				continue
			}
			res.Err = xerror.Newf("xflow", "execute", "flow %q canceled while running step %q: %w",
				f.name, s.name, ctx.Err())
		} else {
			// 强依赖失败：这一步没成，不纳入回滚范围。
			//
			// 包一层 xerror：不包的话 res.Err 是个裸的 *StepError，
			// xerror.Is(err, "xflow") 认不出它，而别处的错误都认得出。
			// 包了之后 errors.As(err, &stepErr) 照样能拿到里面那个
			res.Err = xerror.New("xflow", "execute", se)
		}

		f.rollback(ctx, data, f.steps[:n], res, m)
		return res
	}
	return res
}

// rollback 逆序回滚已执行的步骤。
//
// 回滚用的 ctx 剥掉了原来的取消和超时：补偿逻辑（退款、还库存、解冻额度）
// 最需要执行的时机恰恰是请求超时之后，沿用已经取消的 ctx 会让每个补偿调用
// 一进去就被拒绝，资源就真的漏掉了。
// 剥掉之后由 RollbackTimeout 单独限时，免得补偿无限期挂住退出流程。
func (f *Flow[T]) rollback(ctx context.Context, data T, done []entry[T], res *Result, m Monitor) {
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cmp.Or(f.rollbackTimeout, cfg.RollbackTimeout))
	defer cancel()

	for i := len(done) - 1; i >= 0; i-- {
		s := done[i]

		// 预算耗尽也要把剩下的逐个记下来：调用方得知道还有哪些资源悬着
		if err := rbCtx.Err(); err != nil {
			res.RollbackErrors = append(res.RollbackErrors,
				s.fail(fmt.Errorf("rollback budget exhausted, this step never ran: %w", err)))
			continue
		}

		res.Rolled = true
		stepStart := now(m)
		err := rollbackWithin(rbCtx, s.Processor, data)
		f.notifyStep(rbCtx, m, true, s, err, stepStart)

		if err != nil {
			res.RollbackErrors = append(res.RollbackErrors, s.fail(err))
		}
	}
}

// rollbackWithin 在回滚预算之内执行一步 Rollback，到点就不再等它。
//
// Rollback 收了 ctx，但它未必真的看——里面可能是一个不吃 ctx 的第三方调用。
// 只在步骤之间查预算的话，这样一步能把 Execute 无限期挂住（实测 50ms 的预算
// 等了 2s），而且它自己不会出现在 RollbackErrors 里。所以另起一个协程去等，
// 做法和 xone 的 runWithin 一样。超时之后那个协程还挂在原地、照样会读写 data，
// 这是有意的：强行放弃它，好过让 Execute 连同调用方一起陪它挂着。
// 预算这时已经耗尽，还没轮到的步骤由 rollback 逐个记成「没执行」。
func rollbackWithin[T any](ctx context.Context, p Processor[T], data T) error {
	done := make(chan error, 1)
	go func() { done <- safeRollback(ctx, p, data) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("rollback did not finish within the rollback budget, abandoned while still running: %w", ctx.Err())
	}
}

// safeProcess 执行 Process 并隔离 panic
//
// 一步炸了不该把整个进程打穿：它应该变成一个普通的步骤失败，
// 好让前面几步有机会被回滚。
//
// 返回的是 *PanicError 而不是 xerror：它会被装进 StepError，再由 Execute
// 在模块边界包一次 xerror。这里再包就是两层 xflow。
func safeProcess[T any](ctx context.Context, p Processor[T], data T) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return p.Process(ctx, data)
}

// safeRollback 执行 Rollback 并隔离 panic
//
// 尤其重要：一步补偿炸了不该拦住其余步骤的补偿。
func safeRollback[T any](ctx context.Context, p Processor[T], data T) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return p.Rollback(ctx, data)
}
