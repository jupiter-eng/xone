package xflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
)

// TestMain 把流程日志丢掉：这些用例本来就会刷一屏
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// data 流程里贯穿全程的共享数据
type data struct {
	mu   sync.Mutex
	done []string // 正向执行过的步骤
	back []string // 回滚过的步骤
}

func (d *data) record(list *[]string, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	*list = append(*list, name)
}

func (d *data) doneList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.done...)
}

func (d *data) backList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.back...)
}

// step 一个可编排的测试步骤
type step struct {
	name     string
	dep      Dependency
	err      error // Process 返回它
	rbErr    error // Rollback 返回它
	panics   bool
	rbPanics bool
	delay    time.Duration
	rbDelay  time.Duration
}

func (s *step) Name() string           { return s.name }
func (s *step) Dependency() Dependency { return s.dep }

func (s *step) Process(ctx context.Context, d *data) error {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.panics {
		panic("步骤炸了：" + s.name)
	}
	if s.err != nil {
		return s.err // done 只记成功的，断言里才好读
	}
	d.record(&d.done, s.name)
	return nil
}

func (s *step) Rollback(ctx context.Context, d *data) error {
	if s.rbDelay > 0 {
		time.Sleep(s.rbDelay)
	}
	if s.rbPanics {
		panic("回滚炸了：" + s.name)
	}
	d.record(&d.back, s.name)
	return s.rbErr
}

func ok(name string) *step   { return &step{name: name, dep: Strong} }
func weak(name string) *step { return &step{name: name, dep: Weak} }

func failing(name string, dep Dependency) *step {
	return &step{name: name, dep: dep, err: errors.New(name + " 失败")}
}

// withConfig 换一份配置，测试结束还原。
// 句柄是只读的，换配置就是把包级句柄换成一个 Fixed 的
func withConfig(t *testing.T, mutate func(*Config)) {
	t.Helper()
	old := cfg
	t.Cleanup(func() { cfg = old })
	c := DefaultConfig()
	if mutate != nil {
		mutate(&c)
	}
	cfg = c
}

func TestExecute_AllSucceed(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	res := New("下单", ok("扣券"), ok("扣库存"), ok("扣款")).Execute(context.Background(), d)

	if !res.Success() {
		t.Fatalf("应当成功：%v", res)
	}
	if res.Rolled {
		t.Error("成功的流程不该回滚")
	}
	if got := d.doneList(); len(got) != 3 || got[0] != "扣券" || got[2] != "扣款" {
		t.Errorf("应按顺序执行，got=%v", got)
	}
	if len(d.backList()) != 0 {
		t.Errorf("不该回滚，got=%v", d.backList())
	}
}

func TestExecute_StrongDependencyFailureRollsBackInReverse(t *testing.T) {
	// 「谁做的事，谁负责撤销」，而且撤销顺序必须和执行顺序相反
	withConfig(t, nil)
	d := &data{}
	res := New("下单",
		ok("扣券"), ok("扣库存"), failing("扣款", Strong), ok("发通知"),
	).Execute(context.Background(), d)

	if res.Success() {
		t.Fatal("强依赖失败时流程应当失败")
	}
	if !res.Rolled {
		t.Error("应当触发回滚")
	}
	if got := d.doneList(); len(got) != 2 {
		t.Errorf("失败之后的步骤不该执行，got=%v", got)
	}
	if got := d.backList(); len(got) != 2 || got[0] != "扣库存" || got[1] != "扣券" {
		t.Errorf("应逆序回滚，got=%v", got)
	}

	var se *StepError
	if !errors.As(res.Err, &se) || se.Processor != "扣款" {
		t.Errorf("错误里应点名是哪一步，got=%v", res.Err)
	}
}

func TestExecute_WeakDependencyFailureContinues(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	res := New("下单", ok("扣券"), failing("发通知", Weak), ok("写日志")).Execute(context.Background(), d)

	if !res.Success() {
		t.Fatalf("弱依赖失败不该中断流程：%v", res)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Processor != "发通知" {
		t.Errorf("应记下跳过的错误，got=%v", res.Skipped)
	}
	if got := d.doneList(); len(got) != 2 || got[1] != "写日志" {
		t.Errorf("后面的步骤应继续执行，got=%v", got)
	}
}

func TestExecute_FailedWeakDependencyIsAlsoRolledBack(t *testing.T) {
	// 它可能已经产生了副作用，只是后面没走下去而已
	withConfig(t, nil)
	d := &data{}
	New("下单", ok("扣券"), failing("发通知", Weak), failing("扣款", Strong)).
		Execute(context.Background(), d)

	got := d.backList()
	if len(got) != 2 || got[0] != "发通知" || got[1] != "扣券" {
		t.Errorf("失败的弱依赖也该纳入回滚范围，got=%v", got)
	}
}

func TestExecute_CtxCanceledStartsNoNewStepsButStillRollsBack(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	ctx, cancel := context.WithCancel(context.Background())

	cancelStep := &step{name: "取消", dep: Strong}
	res := New("下单", ok("扣券"), &cancelHook{step: cancelStep, cancel: cancel}, ok("扣款")).
		Execute(ctx, d)

	if res.Success() {
		t.Fatal("ctx 取消后流程应当失败")
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("错误里应能看出是取消，got=%v", res.Err)
	}
	if got := d.doneList(); len(got) != 2 {
		t.Errorf("取消之后不该再启动新步骤，got=%v", got)
	}
	if got := d.backList(); len(got) != 2 {
		t.Errorf("已完成的部分照常回滚，got=%v", got)
	}
}

// cancelHook 执行时顺手取消 ctx
type cancelHook struct {
	*step
	cancel context.CancelFunc
}

func (c *cancelHook) Process(ctx context.Context, d *data) error {
	err := c.step.Process(ctx, d)
	c.cancel()
	return err
}

func TestRollback_UsesCtxWithoutCancellation(t *testing.T) {
	// 补偿逻辑最需要执行的时机恰恰是请求超时之后。
	// 沿用已取消的 ctx，每个补偿调用一进去就被拒绝，资源就真的漏掉了
	withConfig(t, nil)
	d := &data{}

	var rbCtxErr error
	checker := &ctxChecker{step: &step{name: "检查", dep: Strong}, got: &rbCtxErr}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 一开始就取消

	New("下单", checker, ok("后面这步不会执行")).Execute(ctx, d)

	if rbCtxErr != nil {
		t.Errorf("回滚用的 ctx 不该是已取消的，got=%v", rbCtxErr)
	}
}

// ctxChecker 在回滚时记下 ctx 的状态
type ctxChecker struct {
	*step
	got *error
}

func (c *ctxChecker) Rollback(ctx context.Context, d *data) error {
	*c.got = ctx.Err()
	return c.step.Rollback(ctx, d)
}

func TestRollback_CtxValuesPreserved(t *testing.T) {
	// 剥的是取消和超时，不是 value：补偿逻辑常常要用到 ctx 里的租户、链路标识
	withConfig(t, nil)
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "租户A")

	var seen any
	checker := &valueChecker{step: &step{name: "检查", dep: Strong}, key: key{}, got: &seen}
	New("下单", checker, failing("扣款", Strong)).Execute(ctx, &data{})

	if seen != "租户A" {
		t.Errorf("回滚时应还能读到 ctx 里的值，got=%v", seen)
	}
}

type valueChecker struct {
	*step
	key any
	got *any
}

func (c *valueChecker) Rollback(ctx context.Context, d *data) error {
	*c.got = ctx.Value(c.key)
	return c.step.Rollback(ctx, d)
}

func TestRollback_RecordsRemainingStepsWhenBudgetExhausted(t *testing.T) {
	// 调用方得知道还有哪些资源悬着，否则只能人工翻日志猜
	withConfig(t, func(c *Config) { c.RollbackTimeout = 30 * time.Millisecond })
	d := &data{}

	slow := &step{name: "慢补偿", dep: Strong, rbDelay: 60 * time.Millisecond}
	res := New("下单", ok("第一步"), ok("第二步"), slow, failing("扣款", Strong)).
		Execute(context.Background(), d)

	if len(res.RollbackErrors) == 0 {
		t.Fatal("预算耗尽时应把没补偿的记下来")
	}
	var names []string
	for _, e := range res.RollbackErrors {
		names = append(names, e.Processor)
	}
	if !strings.Contains(strings.Join(names, ","), "第一步") {
		t.Errorf("没轮到的步骤也该记下来，got=%v", names)
	}
	if !strings.Contains(res.String(), "failed to roll back") {
		t.Errorf("结果摘要里该看得出回滚没做完，got=%s", res.String())
	}
}

func TestWithRollbackTimeout_OverridesConfigForThisFlow_OriginalUnchanged(t *testing.T) {
	// 不跑 xone.Run、单独用 xflow 时，配置文件没人读，改回滚预算只有这一条路
	withConfig(t, nil) // 配置里是默认的 30s
	slow := &step{name: "慢补偿", dep: Strong, rbDelay: 60 * time.Millisecond}
	base := New("下单", ok("第一步"), slow, failing("扣款", Strong))
	short := base.WithRollbackTimeout(20 * time.Millisecond)

	if res := short.Execute(context.Background(), &data{}); len(res.RollbackErrors) == 0 {
		t.Error("回滚预算 20ms、补偿要 60ms，该记下没做完的补偿")
	}
	// 返回的是新流程：原来那个还跟着配置的 30s 走，补偿全部做完
	if res := base.Execute(context.Background(), &data{}); len(res.RollbackErrors) != 0 {
		t.Errorf("原来的流程不该被改掉，got=%v", res.RollbackErrors)
	}
}

func TestWithRollbackTimeout_PanicsOnNonPositive(t *testing.T) {
	// 0 会让回滚一进去就判超时、补偿全被跳过，而流程看起来一切正常
	for _, d := range []time.Duration{0, -time.Second} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("WithRollbackTimeout(%v) 该 panic", d)
				}
			}()
			New("下单", ok("第一步")).WithRollbackTimeout(d)
		}()
	}
}

func TestRollback_CompensationIgnoringCtxCannotHangExecute(t *testing.T) {
	// 预算只在步骤之间查的话，一个不看 ctx 的 Rollback 能把 Execute 挂住：
	// 实测 50ms 的预算等了 2s，而且挂住的那一步不在 RollbackErrors 里
	withConfig(t, func(c *Config) { c.RollbackTimeout = 50 * time.Millisecond })
	d := &data{}

	// 挂 3s：挂得住的话 Execute 至少等 3s，上界 1.5s 离 50ms 的预算和 3s 都远
	hung := &step{name: "不看ctx的补偿", dep: Strong, rbDelay: 3 * time.Second}
	start := time.Now()
	res := New("下单", ok("第一步"), hung, failing("扣款", Strong)).Execute(context.Background(), d)

	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("回滚预算 50ms，Execute 却等了 %v", took)
	}
	if len(res.RollbackErrors) != 2 {
		t.Fatalf("挂住的那一步和没轮到的那一步都该记下来，got=%v", res.RollbackErrors)
	}
	if e := res.RollbackErrors[0]; e.Processor != "不看ctx的补偿" || !errors.Is(e, context.DeadlineExceeded) {
		t.Errorf("挂住的那一步该记成超时，got=%v", e)
	}
	if e := res.RollbackErrors[1]; e.Processor != "第一步" {
		t.Errorf("没轮到的步骤也该记下来，got=%v", e)
	}
}

func TestExecute_CanceledBeforeFirstStepIsNotRolledBack(t *testing.T) {
	// 一步都没做就没有可回滚的：报成 rolled back 会让人去查一次并不存在的补偿
	withConfig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := New("下单", ok("扣券")).Execute(ctx, &data{})

	if res.Success() {
		t.Fatal("ctx 取消后流程应当失败")
	}
	if res.Rolled {
		t.Error("一步都没执行，Rolled 不该是 true")
	}
	if strings.Contains(res.String(), "rolled back") {
		t.Errorf("摘要不该说回滚过，got=%s", res.String())
	}
}

func TestRollback_OneFailureDoesNotBlockOtherSteps(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	bad := &step{name: "补偿会失败", dep: Strong, rbErr: errors.New("补偿失败")}

	res := New("下单", ok("第一步"), bad, failing("扣款", Strong)).Execute(context.Background(), d)

	if len(res.RollbackErrors) != 1 {
		t.Errorf("应记下那一步的回滚错误，got=%v", res.RollbackErrors)
	}
	// 两步的补偿都跑过了：一步失败不该拦住另一步
	if got := d.backList(); len(got) != 2 {
		t.Errorf("其余步骤的回滚应照常进行，got=%v", got)
	}
}

func TestExecute_StepPanicBecomesOrdinaryFailure(t *testing.T) {
	// 一步炸了不该把整个进程打穿，前面几步还得有机会回滚
	withConfig(t, nil)
	d := &data{}
	res := New("下单", ok("扣券"), &step{name: "炸", dep: Strong, panics: true}).
		Execute(context.Background(), d)

	if res.Success() {
		t.Fatal("panic 应当变成流程失败")
	}
	if !strings.Contains(res.Err.Error(), "panic") {
		t.Errorf("错误里应看得出是 panic，got=%v", res.Err)
	}
	if got := d.backList(); len(got) != 1 || got[0] != "扣券" {
		t.Errorf("前面的步骤仍应回滚，got=%v", got)
	}
}

// errPanicStep panic 出一个 error 值的步骤
type errPanicStep struct{ err error }

func (errPanicStep) Name() string           { return "炸出error" }
func (errPanicStep) Dependency() Dependency { return Strong }
func (s errPanicStep) Process(context.Context, *data) error {
	panic(s.err)
}
func (errPanicStep) Rollback(context.Context, *data) error { return nil }

func TestExecute_StepPanicWrappedOnceAndKeepsErrorChain(t *testing.T) {
	// 一个模块边界一个 xerror：panic 在 safeProcess 里包一次、在 Execute 里
	// 又包一次的话，文本就套成 xone xflow execute failed, err=[... xone xflow execute failed ...]。
	// panic 出来的是 error 时要用 %w 接住，errors.Is 才问得出根因；
	// 调用栈是几十行，塞进错误消息就把告警标题和日志检索全搅乱了
	withConfig(t, nil)
	cause := errors.New("连接池炸了")
	res := New[*data]("下单", errPanicStep{cause}).Execute(context.Background(), &data{})

	if res.Success() {
		t.Fatal("panic 应当变成流程失败")
	}
	msg := res.Err.Error()
	if n := strings.Count(msg, "xone xflow"); n != 1 {
		t.Errorf("该只有一层 xflow 错误，got %d 层：%s", n, msg)
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("错误消息里不该有多行的调用栈：%s", msg)
	}
	if !errors.Is(res.Err, cause) {
		t.Errorf("panic 出来的 error 该还在链上，got=%v", res.Err)
	}
	var pe *PanicError
	if !errors.As(res.Err, &pe) || !strings.Contains(string(pe.Stack), "goroutine") {
		t.Errorf("调用栈该挂在 PanicError.Stack 上，got=%+v", pe)
	}
}

func TestMonitor_DefaultRecordsPanicStackAsSeparateField(t *testing.T) {
	// 栈不进错误消息，就得有个地方看得到它：默认监控把它放进 stack 字段
	withConfig(t, nil)
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	New("下单", &step{name: "炸", dep: Strong, panics: true}).Execute(context.Background(), &data{})

	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "xflow step process failed") {
			line = l
		}
	}
	if !strings.Contains(line, `"stack":"goroutine`) {
		t.Errorf("失败那一步的日志该带 stack 字段，got=%s", line)
	}
}

func TestRollback_PanicDoesNotBlockOtherCompensations(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	res := New("下单",
		ok("第一步"),
		&step{name: "补偿会炸", dep: Strong, rbPanics: true},
		failing("扣款", Strong),
	).Execute(context.Background(), d)

	if len(res.RollbackErrors) != 1 {
		t.Errorf("应记下那一步的 panic，got=%v", res.RollbackErrors)
	}
	if got := d.backList(); len(got) != 1 || got[0] != "第一步" {
		t.Errorf("其余补偿应照常进行，got=%v", got)
	}
}

func TestNew_NilStepPanics(t *testing.T) {
	// 它只会在执行到那一步时炸成空指针，那时错误早已脱离构建现场
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("传 nil 步骤应当 panic")
		}
		if !strings.Contains(fmt.Sprint(r), "step 1") {
			t.Errorf("应指出是第几步，got=%v", r)
		}
	}()
	New[*data]("下单", ok("第一步"), nil)
}

func TestFlow_CanExecuteConcurrently(t *testing.T) {
	// 构建之后字段不再变化
	withConfig(t, nil)
	flow := New("下单", ok("扣券"), weak("发通知"), ok("扣款"))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := flow.Execute(context.Background(), &data{}); !res.Success() {
				t.Errorf("并发执行应当都成功：%v", res)
			}
		}()
	}
	wg.Wait()
}

func TestExecute_nil_CtxDoesNotPanic(t *testing.T) {
	withConfig(t, nil)
	//nolint:staticcheck // 故意传 nil
	if res := New("下单", ok("第一步")).Execute(nil, &data{}); !res.Success() {
		t.Errorf("nil ctx 应当兜住，got=%v", res)
	}
}

func TestDependencyString(t *testing.T) {
	if Strong.String() != "strong" || Weak.String() != "weak" || Dependency(9).String() != "unknown" {
		t.Error("依赖类型的文案不对")
	}
}

func TestResultString(t *testing.T) {
	for _, c := range []struct {
		res  *Result
		want string
	}{
		{&Result{}, "flow succeeded"},
		{&Result{Skipped: []*StepError{{}}}, "1 weak step(s) skipped"},
		{&Result{Err: errors.New("炸了")}, "flow failed"},
		{&Result{Err: errors.New("炸了"), Rolled: true}, "rolled back"},
		{&Result{Err: errors.New("炸了"), Rolled: true, RollbackErrors: []*StepError{{}}}, "1 step(s) failed to roll back"},
	} {
		if got := c.res.String(); !strings.Contains(got, c.want) {
			t.Errorf("摘要里应含 %q，got=%q", c.want, got)
		}
	}
}

func TestStepError(t *testing.T) {
	cause := errors.New("根因")
	e := &StepError{Processor: "扣款", Dependency: Strong, Err: cause}
	if !strings.Contains(e.Error(), "扣款") || !strings.Contains(e.Error(), "strong") {
		t.Errorf("错误信息应带上步骤名和依赖类型，got=%s", e.Error())
	}
	if !errors.Is(e, cause) {
		t.Error("应当能 errors.Is 出根因")
	}
}

func TestFlowName(t *testing.T) {
	if got := New[*data]("下单").Name(); got != "下单" {
		t.Errorf("Name 不对，got=%q", got)
	}
}

// ---- 配置与登记 ----

func TestConfig_LoadsFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(path, []byte("XFlow:\n  Monitor: false\n  RollbackTimeout: 5s\n"), 0o644)

	c := DefaultConfig()
	if err := config.LoadInto(path, ConfigKey, &c); err != nil {
		t.Fatal(err)
	}
	if c.Monitor || c.RollbackTimeout != 5*time.Second {
		t.Errorf("配置没生效，got=%+v", c)
	}
}

func TestValidate_ZeroRollbackBudgetFails(t *testing.T) {
	// 配成 0 会让每次回滚一进去就判超时、所有补偿被跳过，
	// 而流程本身看起来一切正常——这种配错必须在启动时拦住
	c := DefaultConfig()
	c.RollbackTimeout = 0
	if err := c.Validate(); err == nil {
		t.Fatal("回滚预算为 0 应当报错")
	}
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("默认配置应当合法：%v", err)
	}
}

func TestRegister_OnlyReadsConfigBuildsNothing(t *testing.T) {
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xflow" {
			got = &e
		}
	}
	if got == nil {
		t.Fatal("没有登记启动钩子，XFlow 那一块就没人读、Validate 也没人调")
	}
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xflow" {
			t.Error("本包没有要关的资源，登记停止钩子会让它出现在退出序列里")
		}
	}
	// Config 要实现 Validate()，框架才会在启动时替它拦住配错的配置
	var c any = &Config{}
	if _, ok := c.(interface{ Validate() error }); !ok {
		t.Error("Config 必须实现 Validate()，否则 RollbackTimeout 配成 0 没人拦")
	}
}

func TestLoad_InvalidConfigFailsStartup(t *testing.T) {
	// RollbackTimeout 配成 0 会让每次回滚一进去就判超时、补偿全被跳过，
	// 而流程本身看起来一切正常——这种配错只有加载时的 Validate 能拦
	path := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(path, []byte("XFlow:\n  RollbackTimeout: 0s\n"), 0o644)

	c := DefaultConfig()
	err := config.LoadInto(path, ConfigKey, &c)
	if err == nil {
		t.Fatal("非法配置应当让加载失败")
	}
	if !strings.Contains(err.Error(), "RollbackTimeout") {
		t.Errorf("错误里应点名是哪个字段，got=%v", err)
	}
}

// panicMonitor 每个回调都炸，用来确认监控实现的故障不会打断业务流程
type panicMonitor struct{}

func (panicMonitor) OnStep(context.Context, *StepEvent) { panic("监控的 OnStep 炸了") }
func (panicMonitor) OnFlow(context.Context, *FlowEvent) { panic("监控的 OnFlow 炸了") }

func TestMonitor_CallbackPanicIsolated(t *testing.T) {
	// 观测出问题只该丢一次观测，不该把业务流程打断。
	// 隔离是用 defer recoverNotify() 做的——recover 必须由被 defer 的那个
	// 函数直接调用才生效，包一层就失效了，所以这条要钉住
	withConfig(t, nil)
	SetMonitor(panicMonitor{})
	t.Cleanup(func() { SetMonitor(slogMonitor{}) })

	d := &data{}
	res := New("下单", ok("扣券"), ok("扣款")).Execute(context.Background(), d)

	if !res.Success() {
		t.Fatalf("监控炸了不该让流程失败：%v", res)
	}
	if got := d.doneList(); len(got) != 2 {
		t.Errorf("每一步都该照常执行，got=%v", got)
	}
}

func TestMonitor_CallbackPanicIsolatedDuringRollback(t *testing.T) {
	withConfig(t, nil)
	SetMonitor(panicMonitor{})
	t.Cleanup(func() { SetMonitor(slogMonitor{}) })

	d := &data{}
	res := New("下单", ok("扣券"), failing("扣款", Strong)).Execute(context.Background(), d)

	if res.Success() {
		t.Fatal("强依赖失败时流程应当失败")
	}
	if got := d.backList(); len(got) != 1 || got[0] != "扣券" {
		t.Errorf("监控炸了不该拦住回滚，got=%v", got)
	}
}

func TestSlogMonitor_StepLogsAtInfoLevel(t *testing.T) {
	// 开着 Monitor 就是想看每一步：成功的步骤记 INFO，默认级别下就看得到，流程结果也在
	withConfig(t, nil)
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(old) })

	New("下单", ok("扣券"), ok("扣款")).Execute(context.Background(), &data{})

	got := buf.String()
	for _, want := range []string{`"level":"INFO","msg":"xflow step process done"`, "扣券", "扣款", `"msg":"xflow flow done"`} {
		if !strings.Contains(got, want) {
			t.Errorf("默认的 info 级别下该看得到每一步和流程结果，缺 %q\n实际=\n%s", want, got)
		}
	}
}

func TestSlogMonitor_NoStepLogsAtWarnLevel(t *testing.T) {
	// 日志级别设成 warn 时成功的步骤不写；失败的步骤照样写
	withConfig(t, nil)
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })

	New("下单", ok("扣券")).Execute(context.Background(), &data{})
	if strings.Contains(buf.String(), "xflow step") {
		t.Errorf("warn 级别下不该有成功步骤的日志\n实际=\n%s", buf.String())
	}
}

// cancelingStep 在自己执行期间取消父 ctx，并把取消如实返回
type cancelingStep struct {
	name   string
	dep    Dependency
	cancel context.CancelFunc
}

func (s *cancelingStep) Name() string           { return s.name }
func (s *cancelingStep) Dependency() Dependency { return s.dep }
func (s *cancelingStep) Process(ctx context.Context, d *data) error {
	s.cancel()
	return ctx.Err()
}
func (s *cancelingStep) Rollback(_ context.Context, d *data) error {
	d.record(&d.back, s.name)
	return nil
}

func TestExecute_CanceledLastStepMustNotReportSuccess(t *testing.T) {
	// 取消只在每步开始前查一次的话，最后一步撞上取消就查不到了：
	// 它的 context.Canceled 走进「弱依赖跳过」分支，循环随即结束，
	// 于是一个被取消的流程报成了 Success，还一步都没回滚
	withConfig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	d := &data{}

	res := New("下单", ok("扣券"), &cancelingStep{name: "发通知", dep: Weak, cancel: cancel}).
		Execute(ctx, d)

	if res.Success() {
		t.Fatalf("流程被取消了，不该报成功：%v", res)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("该如实说是被取消，got=%v", res.Err)
	}
	if !res.Rolled {
		t.Error("取消之后已执行的步骤要回滚")
	}
	if got := d.backList(); len(got) != 2 {
		t.Errorf("失败的弱依赖也在回滚范围内，got=%v", got)
	}
}

func TestExecute_CanceledMiddleWeakStepAlsoAborts(t *testing.T) {
	withConfig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	d := &data{}

	res := New("下单",
		ok("扣券"),
		&cancelingStep{name: "发通知", dep: Weak, cancel: cancel},
		ok("不该跑到"),
	).Execute(ctx, d)

	if res.Success() {
		t.Fatal("流程被取消了，不该报成功")
	}
	if got := d.doneList(); strings.Contains(strings.Join(got, ","), "不该跑到") {
		t.Errorf("取消之后不该再往下走，got=%v", got)
	}
}

func TestLoadConfig_ReadsConfigFlowActuallyUses(t *testing.T) {
	// 解进一个局部变量也能让配置「加载成功」，但 Execute 读的是包级的 cfg——
	// 那样 RollbackTimeout 和 Monitor 配了等于没配，且没有任何迹象
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = DefaultConfig()

	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte("XFlow:\n  Monitor: false\n  RollbackTimeout: 3s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	if err := config.Load(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.Reset)

	if err := loadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.Monitor || cfg.RollbackTimeout != 3*time.Second {
		t.Errorf("配置没落到包级变量上，got=%+v", cfg)
	}
}

func TestLoadConfig_InvalidConfigFailsStartup(t *testing.T) {
	old := cfg
	t.Cleanup(func() { cfg = old })

	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte("XFlow:\n  RollbackTimeout: 0s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	if err := config.Load(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.Reset)

	cfg = DefaultConfig()
	if err := loadConfig(context.Background()); err == nil {
		t.Fatal("回滚预算为 0 应当让启动失败")
	}
	if cfg.RollbackTimeout != DefaultConfig().RollbackTimeout {
		t.Errorf("校验失败的值不该留在包级配置上，got=%v", cfg.RollbackTimeout)
	}
}

func TestLoadConfig_UnsetFieldsResetToDefaultsNotPreviousValues(t *testing.T) {
	// 同一进程里跑两次 Run：第二次的配置文件没写 RollbackTimeout，
	// 就该是默认的 30s，而不是上一次配的 3s
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = Config{Monitor: false, RollbackTimeout: 3 * time.Second} // 上一次 Run 留下的

	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte("XFlow:\n  Monitor: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	if err := config.Load(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.Reset)

	if err := loadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.RollbackTimeout != DefaultConfig().RollbackTimeout {
		t.Errorf("没写的字段该回到默认值，got=%v", cfg.RollbackTimeout)
	}
}
