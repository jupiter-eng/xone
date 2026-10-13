package xclient

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/jupiter-eng/xone/xerror"
)

// conn 一个假的连接池：记得自己关没关过
type conn struct {
	name string
	log  *log
	err  error
	boom bool
}

func (c *conn) Close() error {
	if c.boom {
		panic("close exploded")
	}
	c.log.add("close:" + c.name)
	return c.err
}

type log struct {
	mu  sync.Mutex
	seq []string
}

func (l *log) add(s string) { l.mu.Lock(); l.seq = append(l.seq, s); l.mu.Unlock() }
func (l *log) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.seq, " ")
}

type cfg struct {
	err  error
	boom bool
}

// builder 造一个 new 函数：按配置决定建成功、返回错误还是 panic。
// 日志里记的是 Build 交进来的实例名，顺带验证名字传对了
func builder(l *log) func(context.Context, string, cfg) (*conn, io.Closer, error) {
	return func(_ context.Context, name string, c cfg) (*conn, io.Closer, error) {
		if c.boom {
			l.add("panic:" + name)
			panic("new exploded")
		}
		if c.err != nil {
			l.add("fail:" + name)
			return nil, nil, c.err
		}
		l.add("new:" + name)
		v := &conn{name: name, log: l}
		return v, v, nil
	}
}

func newReg() *Registry[*conn] { return NewRegistry[*conn]("xfake", "XFake") }

func mustPanic(t *testing.T, f func()) any {
	t.Helper()
	var got any
	func() {
		defer func() { got = recover() }()
		f()
	}()
	if got == nil {
		t.Fatal("期望 panic，但没有")
	}
	return got
}

// publish 把一组现成的实例经 Build 发布出去：这些测试要的只是「注册表里有这些」
func publish(r *Registry[*conn], items map[string]*conn) {
	keep := func(_ context.Context, _ string, c *conn) (*conn, io.Closer, error) { return c, nil, nil }
	if err := Build(context.Background(), r, items, keep); err != nil {
		panic(err)
	}
}

// ---- 取实例 ----

func TestGet_NoArgReturnsDefault(t *testing.T) {
	r := newReg()
	want := &conn{name: "d"}
	publish(r, map[string]*conn{DefaultName: want})
	if got := r.Get(); got != want {
		t.Errorf("want %v, got %v", want, got)
	}
	if got := r.Get(DefaultName); got != want {
		t.Errorf("显式写 default 应当取到同一个，got %v", got)
	}
}

func TestGet_PanicsInsteadOfReturningZeroWhenMissing(t *testing.T) {
	// 返回 nil 只是把同一个 panic 推迟到调用方第一次用它的时候，
	// 那里的栈里只剩 "invalid memory address"，看不出根因是配置没配
	r := newReg()
	publish(r, map[string]*conn{"read": {}, "write": {}})

	msg := mustPanic(t, func() { r.Get("relay") })
	s, _ := msg.(string)
	for _, want := range []string{"xfake", `"relay"`, "read", "write"} {
		if !strings.Contains(s, want) {
			t.Errorf("panic 文案里应当有 %q，实际是：%s", want, s)
		}
	}
}

func TestGet_NamesConfigKeyWhenNoneConfigured(t *testing.T) {
	// 名字写错和整块没配是两个不同的问题，文案要能一眼分开：
	// 前者去查名字，后者去查有没有写这一块、有没有 import 对应的包
	r := newReg()
	publish(r, nil) // 这一块没配：启动钩子照样走一遍 Build，一个都不建
	s, _ := mustPanic(t, func() { r.Get() }).(string)
	if !strings.Contains(s, "XFake") {
		t.Errorf("一个都没配时要指明去看哪一块配置，实际是：%s", s)
	}
	if !strings.Contains(s, "none is configured") {
		t.Errorf("要说清是整块没配而不是名字写错，实际是：%s", s)
	}
}

func TestGet_SaysTooEarlyBeforeStartHook(t *testing.T) {
	// 从前这时候报的是「整块没配」，于是在 main 里、在 Run 之前取实例的人
	// 会去翻一份明明写对了的配置文件
	r := newReg()
	s, _ := mustPanic(t, func() { r.Get() }).(string)
	if !strings.Contains(s, "before xone.Run started") {
		t.Errorf("要说清是调早了，实际是：%s", s)
	}
	if strings.Contains(s, "none is configured") {
		t.Errorf("调早了不该被说成没配，实际是：%s", s)
	}
}

func TestGet_SaysTooLateAfterClose(t *testing.T) {
	r := newReg()
	publish(r, map[string]*conn{DefaultName: {}})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	s, _ := mustPanic(t, func() { r.Get() }).(string)
	if !strings.Contains(s, "after xfake was closed") {
		t.Errorf("要说清是调晚了，实际是：%s", s)
	}
}

func TestLookup_ReturnsZeroAndFalseWhenMissing(t *testing.T) {
	r := newReg()
	if v, ok := r.Lookup("nope"); ok || v != nil {
		t.Errorf("want nil,false，got %v,%v", v, ok)
	}
}

func TestHas_ForOptionalDependencies(t *testing.T) {
	r := newReg()
	if r.Has() {
		t.Error("空注册表不该报告有 default")
	}
	publish(r, map[string]*conn{DefaultName: {}})
	if !r.Has() || r.Has("other") {
		t.Errorf("got Has()=%v Has(other)=%v", r.Has(), r.Has("other"))
	}
}

func TestNames_SortedByName(t *testing.T) {
	// 顺序稳定，日志和错误文案才可复现
	r := newReg()
	publish(r, map[string]*conn{"write": {}, "read": {}, "archive": {}})
	if got, want := strings.Join(r.Names(), " "), "archive read write"; got != want {
		t.Errorf("want %s, got %s", want, got)
	}
}

func TestNames_EmptyRegistryReturnsEmptySlice(t *testing.T) {
	if got := newReg().Names(); len(got) != 0 {
		t.Errorf("want 空，got %v", got)
	}
}

func TestAll_ReturnsEveryInstanceAsACopy(t *testing.T) {
	r := newReg()
	a, b := &conn{name: "a"}, &conn{name: "b"}
	publish(r, map[string]*conn{"a": a, "b": b})

	got := r.All()
	if len(got) != 2 || got["a"] != a || got["b"] != b {
		t.Fatalf("want a、b 两个实例，got %v", got)
	}
	// 快照发布之后不许改：调用方改的是自己那份
	delete(got, "a")
	got["c"] = &conn{}
	if !r.Has("a") || r.Has("c") {
		t.Errorf("改 All 的返回值不该改到注册表，got %v", r.Names())
	}
}

func TestAll_EmptyBeforeStartAndAfterClose(t *testing.T) {
	r := newReg()
	if got := r.All(); got == nil || len(got) != 0 {
		t.Errorf("启动前 want 非 nil 的空 map，got %v", got)
	}
	publish(r, map[string]*conn{"a": {}})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got := r.All(); got == nil || len(got) != 0 {
		t.Errorf("关闭后 want 非 nil 的空 map，got %v", got)
	}
}

// ---- 建实例 ----

func TestBuild_BuildsInNameOrder(t *testing.T) {
	// 建的顺序要可复现，否则「配了五个库，挂的是哪一个」每次都不一样
	l := &log{}
	r := newReg()
	err := Build(context.Background(), r, map[string]cfg{
		"write": {}, "archive": {}, "read": {},
	}, builder(l))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := l.String(), "new:archive new:read new:write"; got != want {
		t.Errorf("want %s\ngot  %s", want, got)
	}
	if got, want := strings.Join(r.Names(), " "), "archive read write"; got != want {
		t.Errorf("建完应当全部发布出去，want %s, got %s", want, got)
	}
}

func TestBuild_PublishesEmptyRegistryWithoutConfig(t *testing.T) {
	r := newReg()
	if err := Build(context.Background(), r, map[string]cfg{}, builder(&log{})); err != nil {
		t.Fatal(err)
	}
	if len(r.Names()) != 0 {
		t.Errorf("want 空，got %v", r.Names())
	}
}

func TestBuild_ClosesBuiltOnesOnMidwayFailure(t *testing.T) {
	// 启动钩子返回错误时这一包的停止钩子不会被执行，不自己收拾
	// 就会漏掉前面那几个连接池
	l := &log{}
	boom := errors.New("dial refused")
	err := Build(context.Background(), newReg(), map[string]cfg{
		"a": {}, "b": {}, "c": {err: boom},
	}, builder(l))

	if err == nil {
		t.Fatal("want 错误")
	}
	if !errors.Is(err, boom) {
		t.Errorf("要能顺着 errors.Is 找到底层错误，got %v", err)
	}
	if got, want := l.String(), "new:a new:b fail:c close:b close:a"; got != want {
		t.Errorf("已建好的要逆序关掉\nwant %s\ngot  %s", want, got)
	}
}

func TestBuild_RegistryUnchangedOnFailure(t *testing.T) {
	// 半套实例发布出去比一个都没有更糟：C() 取得到 a 取不到 b，
	// 而启动其实已经失败了
	l := &log{}
	r := newReg()
	old := &conn{name: "old"}
	publish(r, map[string]*conn{DefaultName: old})

	err := Build(context.Background(), r, map[string]cfg{
		"a": {}, "b": {err: errors.New("nope")},
	}, builder(l))
	if err == nil {
		t.Fatal("want 错误")
	}
	if got, ok := r.Lookup(DefaultName); !ok || got != old {
		t.Errorf("注册表被半套实例污染了，got %v", r.Names())
	}
}

func TestBuild_ErrorNamesInstance(t *testing.T) {
	err := Build(context.Background(), newReg(), map[string]cfg{
		"replica": {err: errors.New("dial refused")},
	}, builder(&log{}))
	if err == nil || !strings.Contains(err.Error(), `"replica"`) {
		t.Errorf("错误里要说清是哪个实例，got %v", err)
	}
	if !xerror.Is(err, "xfake") {
		t.Errorf("错误应归到本模块名下，got %v", err)
	}
}

func TestBuild_OwnModuleErrorNotRewrappedAndKeepsOp(t *testing.T) {
	// 一个模块边界一个 xerror。New 已经报了 connect，再按 new 包一层的话
	// 文本里模块名出现两次，errors.As 取出来的 op 永远是 new，
	// 调用方再也分不清是配置错了还是连不上
	root := errors.New("dial refused")
	err := Build(context.Background(), newReg(), map[string]cfg{
		"replica": {err: xerror.Newf("xfake", "connect", "cannot reach h:1: %w", root)},
	}, builder(&log{}))

	var xe *xerror.Error
	if !errors.As(err, &xe) {
		t.Fatalf("应当是 xerror，got %v", err)
	}
	if xe.Module != "xfake" || xe.Op != "connect" {
		t.Errorf("该保留 New 报的 op，got module=%s op=%s", xe.Module, xe.Op)
	}
	var inner *xerror.Error
	if errors.As(xe.Err, &inner) {
		t.Errorf("同一个模块只该有一层 xerror，got %v", err)
	}
	if strings.Count(err.Error(), "xfake") != 1 {
		t.Errorf("模块名只该出现一次，got %v", err)
	}
	if !strings.Contains(err.Error(), `"replica"`) || !errors.Is(err, root) {
		t.Errorf("实例名和根因都得留着，got %v", err)
	}
}

func TestBuild_OtherErrorsWrappedOnceAsNew(t *testing.T) {
	err := Build(context.Background(), newReg(), map[string]cfg{
		"a": {err: xerror.New("xother", "connect", errors.New("x"))},
	}, builder(&log{}))
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Module != "xfake" || xe.Op != "new" {
		t.Errorf("外来的错误要归到本模块的 new 名下，got %v", err)
	}
	if !xerror.Is(err, "xother") {
		t.Errorf("里层的模块要留着，got %v", err)
	}
}

func TestBuild_StopsBuildingOnShutdownSignal(t *testing.T) {
	// 配了五个库、第一个就要重试到超时的话，收到退出信号应当就此打住，
	// 而不是把剩下四个也挨个试一遍
	l := &log{}
	ctx, cancel := context.WithCancel(context.Background())
	var n int
	err := Build(ctx, newReg(), map[string]cfg{
		"a": {}, "b": {}, "c": {},
	}, func(_ context.Context, name string, _ cfg) (*conn, io.Closer, error) {
		n++
		if n == 2 {
			cancel()
		}
		l.add("new:" + name)
		v := &conn{name: name, log: l}
		return v, v, nil
	})

	if err == nil {
		t.Fatal("want 错误：被打断的启动不能报成功")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("要能看出是被取消的，got %v", err)
	}
	if got, want := l.String(), "new:a new:b close:b close:a"; got != want {
		t.Errorf("已建好的要关掉，且不该再建 c\nwant %s\ngot  %s", want, got)
	}
}

func TestBuild_BuildsNothingWhenAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := &log{}
	err := Build(ctx, newReg(), map[string]cfg{"a": {}}, builder(l))
	if err == nil {
		t.Fatal("want 错误")
	}
	if l.String() != "" {
		t.Errorf("一个都不该建，got %s", l.String())
	}
}

func TestBuild_new_PanicDoesNotLeakBuiltOnes(t *testing.T) {
	// 不隔离的话 panic 会穿过 Build 往上抛，而已经建好的那几个 Closer
	// 还只存在于 Build 这一帧的局部变量里——栈一展开就找不回来了
	l := &log{}
	err := Build(context.Background(), newReg(), map[string]cfg{
		"a": {}, "b": {boom: true},
	}, builder(l))

	if err == nil {
		t.Fatal("panic 要变成普通错误，而不是打穿整个进程")
	}
	if !strings.Contains(err.Error(), "panicked") || !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("错误里要说清是哪个实例炸的，got %v", err)
	}
	if got, want := l.String(), "new:a panic:b close:a"; got != want {
		t.Errorf("want %s\ngot  %s", want, got)
	}
}

func TestBuild_new_NilCloserIsHarmless(t *testing.T) {
	// 有的实例没有要关的东西，返回 nil Closer 是合法的
	r := newReg()
	err := Build(context.Background(), r, map[string]cfg{"a": {}},
		func(context.Context, string, cfg) (*conn, io.Closer, error) {
			return &conn{name: "a"}, nil, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("关一个没有 Closer 的实例不该出错，got %v", err)
	}
}

// ---- 关实例 ----

func TestClose_ClosesInReverseOrder(t *testing.T) {
	l := &log{}
	r := newReg()
	if err := Build(context.Background(), r, map[string]cfg{
		"a": {}, "b": {}, "c": {},
	}, builder(l)); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := l.String(), "new:a new:b new:c close:c close:b close:a"; got != want {
		t.Errorf("want %s\ngot  %s", want, got)
	}
}

func TestClose_UnpublishesBeforeClosing(t *testing.T) {
	// 反过来的话，关到一半时 C() 还能取到正在被关闭的实例
	r := newReg()
	var namesDuringClose []string
	err := Build(context.Background(), r, map[string]cfg{"a": {}},
		func(context.Context, string, cfg) (*conn, io.Closer, error) {
			return &conn{name: "a"}, closerFunc(func() error {
				namesDuringClose = r.Names()
				return nil
			}), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if len(namesDuringClose) != 0 {
		t.Errorf("关闭进行中时实例就不该还取得到了，got %v", namesDuringClose)
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func TestClose_OneFailureDoesNotAffectOthers(t *testing.T) {
	// 退出阶段要尽量把能关的都关掉
	l := &log{}
	boom := errors.New("close failed")
	r := newReg()
	r.state.Store(&snapshot[*conn]{
		items: map[string]*conn{"a": {}, "b": {}, "c": {}},
		closers: []io.Closer{
			&conn{name: "a", log: l},
			&conn{name: "b", log: l, err: boom},
			&conn{name: "c", log: l},
		},
	})

	err := r.Close()
	if !errors.Is(err, boom) {
		t.Errorf("失败要汇总上来，got %v", err)
	}
	if got, want := l.String(), "close:c close:b close:a"; got != want {
		t.Errorf("其余的照常关\nwant %s\ngot  %s", want, got)
	}
}

func TestClose_OnePanicDoesNotAffectOthers(t *testing.T) {
	l := &log{}
	r := newReg()
	r.state.Store(&snapshot[*conn]{closers: []io.Closer{
		&conn{name: "a", log: l},
		&conn{name: "b", boom: true},
		&conn{name: "c", log: l},
	}})

	err := r.Close()
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("panic 要变成普通错误，got %v", err)
	}
	if got, want := l.String(), "close:c close:a"; got != want {
		t.Errorf("其余的照常关\nwant %s\ngot  %s", want, got)
	}
}

func TestClose_RepeatedCallIsNoop(t *testing.T) {
	// 停止钩子可能被重试，关第二次不该把同一个连接池再关一遍
	l := &log{}
	r := newReg()
	if err := Build(context.Background(), r, map[string]cfg{"a": {}}, builder(l)); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("第二次关不该出错，got %v", err)
	}
	if got, want := l.String(), "new:a close:a"; got != want {
		t.Errorf("only closed once expected\nwant %s\ngot  %s", want, got)
	}
}

func TestBuild_new_panic_ErrorStaysInChain(t *testing.T) {
	// 用 %v 接住的话它只剩一段文本，调用方再也问不出根因是什么
	cause := errors.New("driver exploded")
	explode := func(context.Context, string, cfg) (*conn, io.Closer, error) { panic(cause) }
	err := Build(context.Background(), newReg(), map[string]cfg{"a": {}}, explode)
	if !errors.Is(err, cause) {
		t.Fatalf("panic 出来的 error 应当还在错误链上，got %v", err)
	}
	if !strings.Contains(err.Error(), `instance "a" panicked: driver exploded`) {
		t.Errorf("文案应当说清是哪个实例 panic 了，got %v", err)
	}
}

func TestClose_panic_ErrorStaysInChain(t *testing.T) {
	cause := errors.New("close exploded")
	r := newReg()
	r.state.Store(&snapshot[*conn]{closers: []io.Closer{closerFunc(func() error { panic(cause) })}})

	err := r.Close()
	if !errors.Is(err, cause) {
		t.Fatalf("panic 出来的 error 应当还在错误链上，got %v", err)
	}
	if !strings.Contains(err.Error(), "close panicked: close exploded") {
		t.Errorf("got %v", err)
	}
}

func TestClose_InstancesGoneAfterClose(t *testing.T) {
	r := newReg()
	publish(r, map[string]*conn{DefaultName: {}})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if r.Has() {
		t.Error("关完还取得到实例，调用方会摸到一个已经关掉的连接池")
	}
}

func TestRegistry_NoDataRaceUnderConcurrentAccess(t *testing.T) {
	r := newReg()
	publish(r, map[string]*conn{DefaultName: {}})

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				r.Has()
			case 1:
				r.Names()
			case 2:
				r.Lookup(DefaultName)
			case 3:
				publish(r, map[string]*conn{DefaultName: {}})
			}
		}(i)
	}
	wg.Wait()
}

// C() 每次数据访问都要走一遍 Get
func BenchmarkGet_Default(b *testing.B) {
	r := newReg()
	publish(r, map[string]*conn{DefaultName: {}, "b": {}})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Get()
	}
}

func BenchmarkGet_Default_Parallel(b *testing.B) {
	r := newReg()
	publish(r, map[string]*conn{DefaultName: {}, "b": {}})
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.Get()
		}
	})
}

func TestBuild_CloseErrorsOfBuiltOnesJoinedIntoBuildError(t *testing.T) {
	// 建到一半失败、回头关已经建好的那几个时，关不掉的错误从前被丢掉：
	// 漏着的连接池在日志里一个字都没有
	closeErr := errors.New("close a failed")
	boom := errors.New("dial refused")
	for name, fail := range map[string]func(context.CancelFunc, string) error{
		"new 失败": func(_ context.CancelFunc, n string) error {
			if n == "b" {
				return boom
			}
			return nil
		},
		"收到退出信号": func(cancel context.CancelFunc, n string) error {
			if n == "a" {
				cancel()
			}
			return nil
		},
	} {
		l := &log{}
		ctx, cancel := context.WithCancel(context.Background())
		err := Build(ctx, newReg(), map[string]cfg{"a": {}, "b": {}}, func(_ context.Context, n string, _ cfg) (*conn, io.Closer, error) {
			if err := fail(cancel, n); err != nil {
				return nil, nil, err
			}
			v := &conn{name: n, log: l, err: closeErr}
			return v, v, nil
		})
		cancel()
		if !errors.Is(err, closeErr) {
			t.Errorf("%s：关不掉的错误要并进 Build 的错误，got %v", name, err)
		}
		if !errors.Is(err, boom) && !errors.Is(err, context.Canceled) {
			t.Errorf("%s：原来的错误要留着，got %v", name, err)
		}
	}
}
