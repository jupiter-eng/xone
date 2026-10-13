package xone

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
)

// ---- 测试替身：不用 mock，都是普通类型 ----

type recorder struct {
	mu  sync.Mutex
	seq []string
}

func (r *recorder) add(s string) { r.mu.Lock(); r.seq = append(r.seq, s); r.mu.Unlock() }
func (r *recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.seq, " → ")
}

type closer struct {
	name string
	r    *recorder
	err  error
}

func (c *closer) Close() error { c.r.add("close:" + c.name); return c.err }

// pair 一对钩子，模拟一个集成包登记的那两个
type pair struct{ start, stop hook.Entry }

// comp 造一对钩子：启动时记一笔，停止时记一笔
func comp(name string, stage hook.Stage, r *recorder, startErr error) pair {
	return pair{
		start: hook.Entry{Name: name + ".init", Pkg: name, Stage: stage, Run: func(context.Context) error {
			r.add("init:" + name)
			return startErr
		}},
		stop: hook.Entry{Name: name + ".close", Pkg: name, Stage: stage, Run: func(context.Context) error {
			r.add("close:" + name)
			return nil
		}},
	}
}

// comps 把若干对钩子登记到全局登记板上，测试结束时清空。
//
// 按传入顺序登记，每一对先启动、后停止——和集成包在 init 里登记的顺序一样，
// 停止钩子因此配上的正是同一对里的那个启动钩子
func comps(t *testing.T, ps ...pair) {
	t.Helper()
	hook.Reset()
	t.Cleanup(hook.Reset)
	for _, p := range ps {
		if p.start.Run != nil {
			hook.AddStart(p.start)
		}
		if p.stop.Run != nil {
			hook.AddStop(p.stop)
		}
	}
}

type server struct {
	r       *recorder
	stopped chan struct{}
	startEr error
}

func newServer(r *recorder) *server { return &server{r: r, stopped: make(chan struct{})} }

func (s *server) Start(context.Context) error {
	s.r.add("start:server")
	if s.startEr != nil {
		return s.startEr
	}
	<-s.stopped
	return nil
}

func (s *server) Stop(context.Context) error {
	s.r.add("stop:server")
	close(s.stopped)
	return nil
}

// signalOnStart 包一层 r：Start 被调到之后再过 after，给自己发 sig。
//
// 不能在调 Run 之前起一个协程按固定时间发：负载重时（test.sh 并行跑十几个模块、带 -race）
// 那一刻 Run 可能还没接管信号，或者已经出错返回、把信号还给了默认处置——SIGTERM
// 按默认处置就是把整个测试进程杀掉，报出来的是一个毫不相干的用例 signal: terminated。
// 从 Start 里发，信号一定落在 Run 接管着的那一段里；Run 提前失败的话根本不发
func signalOnStart(r Runnable, sig syscall.Signal, after time.Duration) Runnable {
	k := signaler{sig: sig, after: after}
	if s, ok := r.(stopper); ok {
		return &signalingServer{signalingRunnable{r, k}, s}
	}
	return &signalingRunnable{r, k} // 不凭空多出一个 Stop：只写 Start 的 Runnable 照旧只有 Start
}

type signaler struct {
	sig   syscall.Signal
	after time.Duration
}

type signalingRunnable struct {
	r Runnable
	k signaler
}

func (s *signalingRunnable) Start(ctx context.Context) error {
	go func() { time.Sleep(s.k.after); syscall.Kill(syscall.Getpid(), s.k.sig) }()
	return s.r.Start(ctx)
}

type signalingServer struct {
	signalingRunnable
	s stopper
}

func (s *signalingServer) Stop(ctx context.Context) error { return s.s.Stop(ctx) }

func emptyConf(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(p, []byte(""), 0o600)
	return p
}

// ---- 用例 ----

func TestRun_ZeroStopBudgetFailsFast(t *testing.T) {
	// 0 在这里不是「不限时」而是「一点都不等」：Stop 拿到的是一个已经过期的
	// context，服务当场被切断，后面每个组件的关闭也都在超时状态下跑。
	// 这种配错必须在做任何事之前就拦住
	started := false
	r := &lateRunnable{
		start: func(context.Context) error { started = true; return nil },
		stop:  func(context.Context) error { return nil },
	}
	err := Run(r, WithLogger(quietLogger()), WithStopTimeout(0))
	if err == nil {
		t.Fatal("停止预算为 0 应当直接报错")
	}
	if started {
		t.Error("报错之前不该已经把服务启动起来")
	}
}

func TestRun_ClosesInReverseOrder(t *testing.T) {
	r := &recorder{}

	comps(t, comp("a", hook.StageClient, r, nil), comp("b", hook.StageClient, r, nil))
	err := Run(signalOnStart(newServer(r), syscall.SIGTERM, 120*time.Millisecond), WithConfigPath(emptyConf(t)))
	if err != nil {
		t.Fatalf("正常退出不该有错误: %v", err)
	}

	want := "init:a → init:b → start:server → stop:server → close:b → close:a"
	if got := r.String(); got != want {
		t.Errorf("关闭顺序不对\n got=%s\nwant=%s", got, want)
	}
}

func TestRun_StageDecidesOrderNotRegistration(t *testing.T) {
	// 这是整套设计的核心主张：init() 的执行顺序（也就是登记顺序）不影响结果
	r := &recorder{}

	// 故意按「反的」顺序登记
	comps(t,
		comp("client", hook.StageClient, r, nil),
		comp("trace", hook.StageTelemetry, r, nil),
		comp("log", hook.StageLog, r, nil),
	)
	err := Run(signalOnStart(newServer(r), syscall.SIGTERM, 120*time.Millisecond), WithConfigPath(emptyConf(t)))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(r.String(), "init:log → init:trace → init:client") {
		t.Errorf("应按 Stage 升序初始化，与登记顺序无关，got=%s", r.String())
	}
	if !strings.HasSuffix(r.String(), "close:client → close:trace → close:log") {
		t.Errorf("关闭应是初始化的严格逆序，got=%s", r.String())
	}
}

func TestRun_KeepsRegistrationOrderWithinStage(t *testing.T) {
	r := &recorder{}

	comps(t,
		comp("first", hook.StageClient, r, nil),
		comp("second", hook.StageClient, r, nil),
	)
	_ = Run(signalOnStart(newServer(r), syscall.SIGTERM, 120*time.Millisecond), WithConfigPath(emptyConf(t)))
	if !strings.HasPrefix(r.String(), "init:first → init:second") {
		t.Errorf("同一档内应保持登记顺序（稳定排序），got=%s", r.String())
	}
}

func TestRun_InitFailureRollsBackInitialized(t *testing.T) {
	r := &recorder{}
	boom := errors.New("连不上")

	comps(t,
		comp("ok", hook.StageClient, r, nil),
		comp("bad", hook.StageClient, r, boom),
	)
	err := Run(newServer(r), WithConfigPath(emptyConf(t)))

	if !errors.Is(err, boom) {
		t.Fatalf("应返回初始化失败的原始错误，got=%v", err)
	}
	if got := r.String(); got != "init:ok → init:bad → close:ok" {
		t.Errorf("失败时应逆序关闭已初始化的部分，且不启动 server，got=%s", got)
	}
}

func TestRun_ServiceStartFails(t *testing.T) {
	r := &recorder{}
	boom := errors.New("端口被占用")
	s := newServer(r)
	s.startEr = boom

	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(s, WithConfigPath(emptyConf(t)))
	if !errors.Is(err, boom) {
		t.Fatalf("应返回服务启动错误，got=%v", err)
	}
	if !strings.Contains(r.String(), "close:a") {
		t.Errorf("服务起不来时组件也要被关掉，got=%s", r.String())
	}
}

func TestRun_CloseErrorsAggregatedNotSwallowed(t *testing.T) {
	r := &recorder{}
	closeErr := errors.New("关不掉")

	comps(t, pair{
		start: hook.Entry{Name: "stuck.init", Pkg: "stuck", Run: func(context.Context) error { return nil }},
		stop:  hook.Entry{Name: "stuck.close", Pkg: "stuck", Run: func(context.Context) error { return closeErr }},
	})
	err := Run(signalOnStart(newServer(r), syscall.SIGTERM, 120*time.Millisecond), WithConfigPath(emptyConf(t)))

	if !errors.Is(err, closeErr) {
		t.Fatalf("关闭错误应被返回，got=%v", err)
	}
}

// demoConf 复刻使用者的配置块：一个结构体，加默认值
type demoConf struct {
	Name string `yaml:"Name"`
}

func TestRun_InitSeesFileValuesNotDefaults(t *testing.T) {
	// 集成在 BeforeStart 钩子里读配置。「框架什么时候加载文件」这一步
	// 使用者不需要知道——这里守的就是钩子里读到的确实是文件里的值
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte("Demo:\n  Name: from-yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := demoConf{Name: "default"}
	var seen string
	r := &recorder{}
	s := newServer(r)
	s.startEr = errors.New("stop right away") // 本用例只关心启动阶段

	comps(t, pair{start: hook.Entry{Name: "demo.init", Pkg: "demo", Run: func(context.Context) error {
		if err := xconfig.Unmarshal("Demo", &cfg); err != nil {
			return err
		}
		seen = cfg.Name
		return nil
	}}})
	_ = Run(s, WithConfigPath(p), WithLogger(quietLogger()))

	if seen != "from-yaml" {
		t.Fatalf("Init 该读到配置文件里的值，got=%q", seen)
	}
}

// earlyConf 写一份配置并让它经 XONE_CONFIG 被找到，模拟「在 main 里、Run 之前读配置」
func earlyConf(t *testing.T, yml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	t.Cleanup(config.Reset)
	t.Setenv(config.EnvKey, p)
	return p
}

func TestRun_ReadingConfigBeforeRunGetsFileValues(t *testing.T) {
	// 要防的是：读早了静默拿到空值，服务带着一套默认配置正常起来，
	// 直到有人发现连的库不是预期那个。现在第一次读就先加载：
	// main 里读到的和 Run 里生效的是同一份
	earlyConf(t, "Demo:\n  Name: from-yaml\n")

	var early demoConf
	if err := xconfig.Unmarshal("Demo", &early); err != nil {
		t.Fatal(err)
	}
	if early.Name != "from-yaml" {
		t.Fatalf("Run 之前读到的应是文件里的值，got=%q", early.Name)
	}

	// Run 沿用这一份：Demo 已经被认领，不会被报成没人读的 key
	r := &recorder{}
	s := newServer(r)
	s.startEr = errors.New("stop right away")
	err := Run(s, WithLogger(quietLogger()))
	if err == nil || !strings.Contains(err.Error(), "stop right away") {
		t.Fatalf("提前读过的块应当算认领过，服务照常启动，got=%v", err)
	}
}

func TestRun_FailsWhenOtherFileNamedAfterEarlyRead(t *testing.T) {
	// 悄悄换一份的话，main 里读到的值和组件里读到的对不上
	earlyConf(t, "Demo:\n  Name: a\n")
	var c demoConf
	if err := xconfig.Unmarshal("Demo", &c); err != nil {
		t.Fatal(err)
	}

	r := &recorder{}
	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(newServer(r), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if err == nil || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("应当报错，并告诉使用者改用 --config / XONE_CONFIG，got=%v", err)
	}
	if strings.Contains(r.String(), "init:") {
		t.Errorf("配置对不上就一个钩子都不该跑，got=%s", r.String())
	}
}

func TestRun_UnreadConfigBlockIsError(t *testing.T) {
	// 多半是拼错了，或者忘了 import 对应的包。静默忽略的话，
	// 使用者会盯着一份「明明配了」的文件查半天
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte("XGrom:\n  DSN: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &recorder{}
	s := newServer(r)
	s.startEr = errors.New("server started") // 检查生效的话根本轮不到它
	err := Run(s, WithConfigPath(p), WithLogger(quietLogger()))

	if err == nil {
		t.Fatal("没人读过的顶层 key 应当让启动失败")
	}
	if !strings.Contains(err.Error(), "XGrom") {
		t.Errorf("错误里应点名是哪个 key，got=%v", err)
	}
	// 另一种常见原因是读得太晚：Start 里才读的 key，启动检查时还没人读过
	if !strings.Contains(err.Error(), "read it in main or a BeforeStart hook") {
		t.Errorf("错误里应说清该在哪里读，got=%v", err)
	}
	if strings.Contains(r.String(), "start:server") {
		t.Errorf("既然启动失败就不该把服务起起来，got=%s", r.String())
	}
}

func TestRun_ComponentPanicIsIsolated(t *testing.T) {
	r := &recorder{}
	comps(t, pair{start: hook.Entry{Name: "panicky.init", Pkg: "panicky",
		Run: func(context.Context) error { panic("初始化炸了") }}})
	err := Run(newServer(r), WithConfigPath(emptyConf(t)))

	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("组件 panic 应被转成错误而不是打穿进程，got=%v", err)
	}
}

func TestRun_MissingExplicitConfigFileIsError(t *testing.T) {
	r := &recorder{}
	// 断言英文原文，不断言中文：临时目录的路径里带着测试名，
	// 「不存在」三个字从那里就能匹配上，这条断言原先因此永远成立
	err := Run(newServer(r), WithConfigPath(filepath.Join(t.TempDir(), "nope.yml")))
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("点名要的配置文件找不到应当场失败，got=%v", err)
	}
}

func TestRun_StartsWithDefaultsWhenNoConfigFile(t *testing.T) {
	// 组件必须真的去读配置：原先的假组件一行配置都不读，于是「没有配置文件时
	// 每个 Unmarshal 都报读早了、服务根本起不来」这个 bug 一直没被发现
	r := &recorder{}
	chdir(t, t.TempDir()) // 约定路径下什么都没有
	os.Args = []string{"svc"}
	config.Reset()
	t.Setenv(config.EnvKey, "")

	cfg := demoConf{Name: "default"}
	reads := pair{start: hook.Entry{Name: "demo.init", Pkg: "demo", Run: func(context.Context) error {
		return xconfig.Unmarshal("Demo", &cfg)
	}}}
	comps(t, comp("a", hook.StageClient, r, nil), reads)
	if err := Run(signalOnStart(newServer(r), syscall.SIGTERM, 120*time.Millisecond), WithLogger(quietLogger())); err != nil {
		t.Fatalf("没有配置文件应该只告警、用默认值起，got=%v", err)
	}
	if !strings.Contains(r.String(), "init:a") {
		t.Errorf("组件仍应被初始化，got=%s", r.String())
	}
	if cfg.Name != "default" {
		t.Errorf("没有配置文件时读到的应是默认值，got=%q", cfg.Name)
	}
}

// chdir 切换工作目录，测试结束后切回。见 internal/config 里同名助手的说明：
// testing.T.Chdir 要 Go 1.24，核心模块的下限不为一个测试助手上抬。
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func TestRun_WaitsForServiceExitBeforeClosing(t *testing.T) {
	// 回归用例。Stop 返回不等于服务已经停干净——Stop 只负责「让它停」，
	// 等不等在处理的请求做完是各实现自己的事。不等就往下关的话，
	// 还在跑的请求会摸到已经关掉的数据库和缓存。
	var closedAt, startReturnedAt time.Time
	var mu sync.Mutex

	probe := pair{
		start: hook.Entry{Name: "probe.init", Pkg: "probe", Stage: hook.StageClient,
			Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: "probe.close", Pkg: "probe", Stage: hook.StageClient,
			Run: func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				closedAt = time.Now()
				return nil
			}},
	}

	// Stop 只发个信号就返回，Start 还要再跑一会儿才退出
	stop := make(chan struct{})
	r := &lateRunnable{
		start: func(context.Context) error {
			<-stop
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			startReturnedAt = time.Now()
			return nil
		},
		stop: func(context.Context) error { close(stop); return nil },
	}

	comps(t, probe)
	if err := Run(signalOnStart(r, syscall.SIGINT, 50*time.Millisecond), WithLogger(quietLogger())); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if startReturnedAt.IsZero() || closedAt.IsZero() {
		t.Fatal("两边都该跑到")
	}
	if closedAt.Before(startReturnedAt) {
		t.Errorf("组件在服务退出之前就被关了：关闭=%v 服务退出=%v", closedAt, startReturnedAt)
	}
}

func TestRun_ServiceExitErrorIsKept(t *testing.T) {
	// 走信号分支时 Start 的返回值此前从没被读过
	wantErr := errors.New("监听挂了")
	stop := make(chan struct{})
	r := &lateRunnable{
		start: func(context.Context) error { <-stop; return wantErr },
		stop:  func(context.Context) error { close(stop); return nil },
	}

	err := Run(signalOnStart(r, syscall.SIGINT, 50*time.Millisecond), WithLogger(quietLogger()))
	if !errors.Is(err, wantErr) {
		t.Errorf("服务退出时的错误应当被带出来，got=%v", err)
	}
}

type lateRunnable struct {
	start func(context.Context) error
	stop  func(context.Context) error
}

func (l *lateRunnable) Start(ctx context.Context) error { return l.start(ctx) }
func (l *lateRunnable) Stop(ctx context.Context) error  { return l.stop(ctx) }

// startOnly 只有 Start 的 Runnable：消费者、定时任务的常见形状
type startOnly struct{ r *recorder }

func (s *startOnly) Start(ctx context.Context) error {
	s.r.add("start:job")
	<-ctx.Done()
	s.r.add("done:job")
	return nil
}

func TestRun_StartOnlyRunnableWorks(t *testing.T) {
	// 靠 ctx 就停得下来的 Runnable 不该被迫写一个空的 Stop
	r := &recorder{}

	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(signalOnStart(&startOnly{r: r}, syscall.SIGTERM, 120*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.String(), "init:a → start:job → done:job → close:a"; got != want {
		t.Errorf("got=%s\nwant=%s", got, want)
	}
}

func TestFunc_RunEndsWhenFuncReturns_ErrorPassedThrough(t *testing.T) {
	// 一次性任务：钩子建好组件，函数干完活返回，Run 逆序关掉组件、把函数的错误交回来
	r := &recorder{}
	comps(t, comp("a", hook.StageClient, r, nil))
	boom := errors.New("migrate failed")
	err := Run(Func(func(context.Context) error { r.add("job"); return boom }),
		WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if !errors.Is(err, boom) {
		t.Fatalf("Run 该交回函数的错误，got=%v", err)
	}
	if got, want := r.String(), "init:a → job → close:a"; got != want {
		t.Errorf("got=%s\nwant=%s", got, want)
	}
}

func TestFunc_NilPanics(t *testing.T) {
	// 不拦的话，nil 要等启动钩子全跑完、调 Start 的那一刻才炸
	defer func() {
		if recover() == nil {
			t.Error("Func(nil) 该 panic")
		}
	}()
	Func(nil)
}

func TestUntilSignal_BlocksUntilSignalThenClosesInReverse(t *testing.T) {
	// 活全在钩子里的进程：Run 跑完启动钩子就停住，收到信号才去跑停止钩子
	r := &recorder{}
	comps(t, comp("a", hook.StageClient, r, nil))
	start := time.Now()
	if err := Run(signalOnStart(UntilSignal(), syscall.SIGTERM, 120*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Errorf("该一直阻塞到信号（120ms），却 %v 就返回了", took)
	}
	if got, want := r.String(), "init:a → close:a"; got != want {
		t.Errorf("got=%s\nwant=%s", got, want)
	}
}

// wrongStop 的 Stop 少了 ctx：编译得过，但不是框架认的那个签名
type wrongStop struct{ started bool }

func (w *wrongStop) Start(context.Context) error { w.started = true; return nil }
func (w *wrongStop) Stop() error                 { return nil }

func TestRun_StopWithWrongSignatureFails(t *testing.T) {
	// Stop 是可选的，签名写错编译器不拦，它就永远不会被调到——
	// 服务收到信号停不下来，只能等停止预算耗尽。要在做任何事之前说清楚
	w := &wrongStop{}
	err := Run(w, WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if err == nil || !strings.Contains(err.Error(), "Stop(context.Context) error") {
		t.Fatalf("应当报错并说明框架要的签名，got=%v", err)
	}
	if w.started {
		t.Error("报错之前不该已经把服务启动起来")
	}
}

// valueStart Start 在值上、Stop 在指针上：传值进去能编译，但值的方法集里没有 Stop
type valueStart struct{ started *bool }

func (v valueStart) Start(context.Context) error { *v.started = true; return nil }
func (v *valueStart) Stop(context.Context) error { return nil }

func TestRun_StopOnPointerButValuePassedFails(t *testing.T) {
	// 传值的话 Stop 不在方法集里，框架调不到它，服务收到信号停不下来
	started := false
	err := Run(valueStart{started: &started}, WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if err == nil || !strings.Contains(err.Error(), "pointer receiver") {
		t.Fatalf("应当报错并提示传指针，got=%v", err)
	}
	if started {
		t.Error("报错之前不该已经把服务启动起来")
	}
}

func TestRun_NilFails(t *testing.T) {
	// 不拦的话，nil 要等到启动钩子全跑完、调 Start 的那一刻才炸
	if err := Run(nil, WithLogger(quietLogger())); err == nil {
		t.Fatal("没有 Runnable 应当报错")
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// syscallSelfInterrupt 给自己发一个 SIGINT，模拟收到退出信号
func syscallSelfInterrupt(t *testing.T) {
	t.Helper()
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Error(err)
		return
	}
	if err := p.Signal(syscall.SIGINT); err != nil {
		t.Error(err)
	}
}

func TestRun_ReturnsWhenServiceExitsOnItsOwn(t *testing.T) {
	// 回归用例。服务自己退出时，上面那次 select 已经把 runErr 取走了，
	// 再取一次就是白等满整个停止预算——一个只会表现为「慢」的 bug。
	r := &lateRunnable{
		start: func(context.Context) error { return nil },
		stop:  func(context.Context) error { return nil },
	}

	start := time.Now()
	if err := Run(r, WithLogger(quietLogger()), WithStopTimeout(5*time.Second)); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("服务自己退出时应当立刻返回，实际用了 %v", elapsed)
	}
}

// ---- 启动期间收到退出信号 ----

func TestRun_SignalDuringInitSkipsServiceStart(t *testing.T) {
	// 信号接管必须早于初始化。装在初始化之后的话，连库、连 Redis、Ping 重试
	// 那几秒里 SIGTERM 走的是系统默认处置——进程当场暴毙，已经建好的资源
	// 一个都来不及注销（注册中心里那条记录、那把分布式锁只能等超时过期）
	r := &recorder{}
	started := false

	slow := pair{
		start: hook.Entry{Name: "慢组件.init", Pkg: "慢组件", Stage: hook.StageClient,
			Run: func(ctx context.Context) error {
				r.add("init:慢组件")
				syscallSelfInterrupt(t) // 正在启动时收到退出信号
				select {
				case <-ctx.Done(): // 钩子自己把 ctx 传下去了，于是当场就能放弃
					r.add("慢组件被打断")
				case <-time.After(3 * time.Second):
					t.Error("钩子的 ctx 没有被取消")
				}
				return nil
			}},
		stop: hook.Entry{Name: "慢组件.close", Pkg: "慢组件", Stage: hook.StageClient,
			Run: func(context.Context) error { r.add("close:慢组件"); return nil }},
	}
	after := comp("后面的组件", hook.StageServer, r, nil)

	srv := &lateRunnable{
		start: func(context.Context) error { started = true; return nil },
		stop:  func(context.Context) error { return nil },
	}

	comps(t, slow, after)
	if err := Run(srv, WithLogger(quietLogger())); err != nil {
		t.Fatalf("按信号退出不是故障，不该报错：%v", err)
	}
	if started {
		t.Error("初始化期间就收到了退出信号，不该再把服务起起来")
	}
	got := r.String()
	if !strings.Contains(got, "慢组件被打断") {
		t.Errorf("组件应当能从 ctx 感知到退出信号，got=%s", got)
	}
	if strings.Contains(got, "init:后面的组件") {
		t.Errorf("收到退出信号后不该再初始化剩余组件，got=%s", got)
	}
	if !strings.Contains(got, "close:慢组件") {
		t.Errorf("已经建好的组件仍要被逆序关干净，got=%s", got)
	}
}

func TestRun_InitInterruptedBySignalIsNotFailure(t *testing.T) {
	// 被取消的建连必然失败。把它当故障报上去的话，每次滚动更新撞上
	// 这个窗口都会在面板上留一条「启动失败」，而它其实是按要求退出
	r := &recorder{}
	broken := pair{start: hook.Entry{Name: "连不上的库.init", Pkg: "连不上的库", Stage: hook.StageClient,
		Run: func(ctx context.Context) error {
			syscallSelfInterrupt(t)
			<-ctx.Done()
			return ctx.Err() // 建连被取消，如实返回错误
		}}}
	before := comp("先起来的", hook.StageLog, r, nil)

	srv := &lateRunnable{
		start: func(context.Context) error { t.Error("不该启动服务"); return nil },
		stop:  func(context.Context) error { return nil },
	}

	comps(t, before, broken)
	if err := Run(srv, WithLogger(quietLogger())); err != nil {
		t.Fatalf("按信号退出不该以错误收场：%v", err)
	}
	if got := r.String(); !strings.Contains(got, "close:先起来的") {
		t.Errorf("先起来的组件仍要被关掉，got=%s", got)
	}
}

// stuckChildEnv 置位时，测试进程扮演「卡在初始化里的子进程」
const stuckChildEnv = "XONE_TEST_STUCK_CHILD"

func TestRun_SecondSignalKillsWhenStuck(t *testing.T) {
	if os.Getenv(stuckChildEnv) == "1" {
		runStuckChild()
		return
	}

	// 注册信号处理这件事本身，取消了系统原本的「收到就死」。
	// 于是框架一旦卡在某个不看 ctx 的第三方调用里（这里用 time.Sleep 模拟），
	// 信号就只是往一个没人看的 ctx 里送——进程变成只有 kill -9 收得掉，
	// K8s 得等满整个终止宽限期。第一个信号之后把默认处置还回去，
	// 第二个信号才有地方可去。
	// 用 os.Executable 而不是 os.Args[0]：定位配置文件的测试会改写 os.Args，
	// 跑在它后面时 os.Args[0] 已经是那个测试编的假程序名了
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestRun_SecondSignalKillsWhenStuck$")
	cmd.Env = append(os.Environ(), stuckChildEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	waitFor(t, stdout, "STUCK")

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // 让接管协程把默认处置还回去
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("子进程该被信号终止，got=%v", err)
		}
		st, ok := ee.Sys().(syscall.WaitStatus)
		if !ok || !st.Signaled() || st.Signal() != syscall.SIGTERM {
			t.Errorf("该以「被 SIGTERM 终止」收场（退出码 143），got=%v", ee)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("第二个信号没能终止卡住的子进程——只剩 kill -9 这一条路了")
	}
}

// runStuckChild 起一个卡在初始化里、且不看 ctx 的进程
func runStuckChild() {
	stuck := pair{start: hook.Entry{Name: "卡住的组件.init", Pkg: "卡住的组件", Stage: hook.StageClient,
		Run: func(context.Context) error {
			fmt.Println("STUCK")
			os.Stdout.Sync()
			time.Sleep(60 * time.Second) // 模拟没有超时的第三方建连
			return nil
		}}}
	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); return nil },
		stop:  func(context.Context) error { return nil },
	}
	hook.AddStart(stuck.start) // 子进程里的登记板本来就是空的，不必清
	_ = Run(srv, WithLogger(quietLogger()))
}

// waitFor 读子进程的输出直到出现 marker
func waitFor(t *testing.T, r io.Reader, marker string) {
	t.Helper()
	found := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if strings.Contains(sc.Text(), marker) {
				close(found)
				return
			}
		}
	}()
	select {
	case <-found:
	case <-time.After(10 * time.Second):
		t.Fatalf("没等到子进程输出 %q", marker)
	}
}

// ---- 停止预算 ----

func TestShutdown_StuckComponentDoesNotBlockOthers(t *testing.T) {
	// WithStopTimeout 说的是「整个退出流程的预算」，而 io.Closer.Close()
	// 没有 ctx。不看着它就等于没有上限——一个连接池关不掉，整个进程就陪着它
	// 挂到部署环境来 SIGKILL 为止
	r := &recorder{}
	stuck := pair{
		start: hook.Entry{Name: "关不掉的.init", Pkg: "关不掉的", Stage: hook.StageClient,
			Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: "关不掉的.close", Pkg: "关不掉的", Stage: hook.StageClient,
			Run: func(context.Context) error { select {} }},
	}
	after := comp("先关的", hook.StageServer, r, nil)

	srv := &lateRunnable{
		start: func(context.Context) error { return nil },
		stop:  func(context.Context) error { return nil },
	}

	start := time.Now()
	comps(t, stuck, after)
	err := Run(srv, WithLogger(quietLogger()), WithStopTimeout(300*time.Millisecond))
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("卡住的 Close 把整个退出流程拖住了，耗时=%v", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "关不掉的") {
		t.Errorf("该如实报告是哪个组件没关掉，got=%v", err)
	}
	// 卡住的那个排在后面关，它之前的仍要被关掉
	if got := r.String(); !strings.Contains(got, "close:先关的") {
		t.Errorf("卡住的组件不该拦住其余组件，got=%s", got)
	}
}

func TestShutdown_ShutdownAfterInitFailureIsBudgeted(t *testing.T) {
	// 这一支还没有 stopCtx，但已经建好的那几个照样可能关不掉
	stuck := pair{
		start: hook.Entry{Name: "关不掉的.init", Pkg: "关不掉的", Stage: hook.StageLog,
			Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: "关不掉的.close", Pkg: "关不掉的", Stage: hook.StageLog,
			Run: func(context.Context) error { select {} }},
	}
	boom := pair{start: hook.Entry{Name: "起不来的.init", Pkg: "起不来的", Stage: hook.StageClient,
		Run: func(context.Context) error { return errors.New("起不来") }}}

	srv := &lateRunnable{
		start: func(context.Context) error { t.Error("不该启动服务"); return nil },
		stop:  func(context.Context) error { return nil },
	}

	start := time.Now()
	comps(t, stuck, boom)
	err := Run(srv, WithLogger(quietLogger()), WithStopTimeout(300*time.Millisecond))
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("启动失败后的关闭没有预算，耗时=%v", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "起不来") {
		t.Errorf("初始化失败的原因该带出来，got=%v", err)
	}
}

// stopSlack 停止预算之外给调度留的余量。它必须小于预算本身：
// 「超出预算」的 bug 多半是多拿了一整份预算，余量比预算小才分得出来
const stopSlack = 400 * time.Millisecond

// stuckComp 一个关不掉的组件：停止钩子不看 ctx，永远不返回
func stuckComp(name string) pair {
	return pair{
		start: hook.Entry{Name: name + ".init", Pkg: name, Stage: hook.StageClient,
			Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: name + ".close", Pkg: name, Stage: hook.StageClient,
			Run: func(context.Context) error { select {} }},
	}
}

// flushComp 排在最后关的日志：停止钩子只要 1ms，做完记一笔
func flushComp(r *recorder) pair {
	return pair{
		start: hook.Entry{Name: "日志.init", Pkg: "日志", Stage: hook.StageLog,
			Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: "日志.close", Pkg: "日志", Stage: hook.StageLog,
			Run: func(context.Context) error { time.Sleep(time.Millisecond); r.add("flush:日志"); return nil }},
	}
}

func TestShutdown_StuckHookCannotEatLaterHooksShare(t *testing.T) {
	// 一个卡住的钩子要是能把预算吃光，排在它后面的钩子一进去就判超时，
	// 连 1ms 的活都来不及做完。最吃亏的是日志：它排在最后关，要把前面所有组件的
	// 关闭日志写出去，结果每次都被报成「没关完」。
	// 两个卡住的连着排：每个还没轮到的钩子都得各有一份，而不是大家抢剩下的那一份
	r := &recorder{}
	srv := &lateRunnable{
		start: func(context.Context) error { return nil },
		stop:  func(context.Context) error { return nil },
	}

	budget := 800 * time.Millisecond
	start := time.Now()
	comps(t, stuckComp("卡住的甲"), stuckComp("卡住的乙"), flushComp(r))
	err := Run(srv, WithLogger(quietLogger()), WithStopTimeout(budget))
	elapsed := time.Since(start)

	if got := r.String(); got != "flush:日志" {
		t.Errorf("排在卡住的钩子后面的日志钩子该跑完，got=%q", got)
	}
	if err == nil || !strings.Contains(err.Error(), "卡住的甲") || !strings.Contains(err.Error(), "卡住的乙") {
		t.Errorf("两个卡住的钩子都该被如实报告，got=%v", err)
	}
	if err != nil && strings.Contains(err.Error(), "日志.close") {
		t.Errorf("按时做完的钩子不该被报成超时，got=%v", err)
	}
	if limit := budget + stopSlack; elapsed > limit {
		t.Errorf("退出耗时超出了停止预算，elapsed=%v limit=%v", elapsed, limit)
	}
}

func TestShutdown_TotalTimeWithinStopBudget(t *testing.T) {
	// WithStopTimeout 是使用者唯一要认的数：从开始退出到 Run 返回，最坏就这么久。
	// 最坏的情形凑齐了——服务不肯退出、还有一个关不掉的连接池——也不能超出它，
	// 而且排在最后的日志仍要跑完：服务只能用前面那一段，吃不掉留给钩子的
	r := &recorder{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); <-release; return nil },
		stop:  func(context.Context) error { return nil },
	}

	const signalAt = 50 * time.Millisecond
	budget := 800 * time.Millisecond
	start := time.Now()
	comps(t, stuckComp("关不掉的"), flushComp(r))
	err := Run(signalOnStart(srv, syscall.SIGINT, signalAt), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()), WithStopTimeout(budget))
	elapsed := time.Since(start)

	if got := r.String(); got != "flush:日志" {
		t.Errorf("服务不肯退出也不该吃掉留给钩子的那一段，日志钩子该跑完，got=%q", got)
	}
	if err == nil || !strings.Contains(err.Error(), "关不掉的") {
		t.Errorf("关不掉的组件该被如实报告，got=%v", err)
	}
	// 从收到信号算起不超过一份预算（从前服务不肯退出时组件另拿一份，是两份）
	if limit := signalAt + budget + stopSlack; elapsed > limit {
		t.Errorf("退出耗时超出了停止预算，elapsed=%v limit=%v", elapsed, limit)
	}
}

// entry 造一个钩子：跑的时候记一笔，返回 err。名字里点号前面那段是它的包
func entry(r *recorder, name string, err error) hook.Entry {
	pkg, _, _ := strings.Cut(name, ".")
	return hook.Entry{Name: name, Pkg: pkg, Stage: hook.StageClient,
		Run: func(context.Context) error { r.add(name); return err }}
}

func TestRun_StopHookPairsOnlyWithItsStartHook(t *testing.T) {
	// 一起登记的就是一对。一个包先后登记了两对，第二对的启动失败时：
	// 第一对照常关——它的资源是建好了的，不关就漏；第二对不关——它根本没建起来。
	// 按包配对的话只能二选一：要么都关（第二个得处理「还没建起来」），要么都不关（第一个漏掉）
	r := &recorder{}
	boom := errors.New("第二对建不起来")
	t.Cleanup(hook.Reset)
	hook.Reset()
	hook.AddStart(entry(r, "两对.openA", nil))
	hook.AddStop(entry(r, "两对.closeA", nil))
	hook.AddStart(entry(r, "两对.openB", boom))
	hook.AddStop(entry(r, "两对.closeB", nil))
	err := Run(newServer(r), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if !errors.Is(err, boom) {
		t.Fatalf("启动钩子失败该返回它的错误，got=%v", err)
	}
	if got, want := r.String(), "两对.openA → 两对.openB → 两对.closeA"; got != want {
		t.Errorf("该只关建好了的那一对\n got=%s\nwant=%s", got, want)
	}
}

func TestRun_StopHookPairsWithLaterOfTwoStartHooks(t *testing.T) {
	// initA、initB、close 依次登记：close 配的是离它最近的 initB。
	// initB 失败，资源只建了一半，close 就不该被调到——它不必处理「建了一半」。
	// initA 建好的东西没人关，这是把两步拆成两个启动钩子的代价：要么合成一个，
	// 要么给 initA 配一个自己的停止钩子
	r := &recorder{}
	t.Cleanup(hook.Reset)
	hook.Reset()
	hook.AddStart(entry(r, "半成品.initA", nil))
	hook.AddStart(entry(r, "半成品.initB", errors.New("第二步失败")))
	hook.AddStop(entry(r, "半成品.close", nil))
	hook.AddStop(entry(r, "日志.close", nil))
	err := Run(newServer(r), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil {
		t.Fatal("启动钩子失败该返回错误")
	}
	// 日志.close 之前没有同包的启动钩子，不依赖启动，照常执行
	if got, want := r.String(), "半成品.initA → 半成品.initB → 日志.close"; got != want {
		t.Errorf("配对的启动钩子失败了就不该执行停止钩子\n got=%s\nwant=%s", got, want)
	}
}

// earlyFailure 登记一对钩子和一个不依赖启动的停止钩子，给「启动之前就失败」的用例用
func earlyFailure(t *testing.T, r *recorder) {
	t.Helper()
	t.Cleanup(hook.Reset)
	hook.Reset()
	hook.AddStart(entry(r, "库.open", nil))
	hook.AddStop(entry(r, "库.close", nil))
	hook.AddStop(entry(r, "日志.flush", nil))
}

func TestRun_RunnableInvalid_UnpairedStopHooksStillRun(t *testing.T) {
	// 文档说之前没有启动钩子的停止钩子总会执行。Runnable 被拦下时从前直接返回，
	// 日志的 flush 这类钩子一次都没跑，缓冲里的那几行就此丢掉
	r := &recorder{}
	earlyFailure(t, r)
	err := Run(&wrongStop{}, WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil || !strings.Contains(err.Error(), "Stop(context.Context) error") {
		t.Fatalf("应当照常报出 Runnable 的问题，got=%v", err)
	}
	if got, want := r.String(), "日志.flush"; got != want {
		t.Errorf("只该跑不依赖启动的那个停止钩子\n got=%s\nwant=%s", got, want)
	}
}

func TestRun_ConfigLoadFails_UnpairedStopHooksStillRun(t *testing.T) {
	r := &recorder{}
	earlyFailure(t, r)
	err := Run(newServer(r), WithConfigPath(filepath.Join(t.TempDir(), "nope.yml")), WithLogger(quietLogger()))

	if !xerror.Is(err, "xconfig") {
		t.Fatalf("应当照常报出配置的问题，got=%v", err)
	}
	if got, want := r.String(), "日志.flush"; got != want {
		t.Errorf("只该跑不依赖启动的那个停止钩子\n got=%s\nwant=%s", got, want)
	}
}

func TestRun_HookPanicWrappedOnceWithStandardOp(t *testing.T) {
	// 一个模块边界一个 xerror：panic 先在钩子这一层包一次、再在启动那一层包一次的话，
	// 文本就套成 xone start failed, err=[... xone hook failed, err=[...]]。
	// panic 出来的是 error 时要用 %w 接住，调用方才判断得了根因
	cause := errors.New("连接池炸了")
	comps(t, pair{start: hook.Entry{Name: "panicky.init", Pkg: "panicky",
		Run: func(context.Context) error { panic(cause) }}})
	err := Run(newServer(&recorder{}), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil {
		t.Fatal("panic 该变成错误")
	}
	if n := strings.Count(err.Error(), "xone "); n != 1 {
		t.Errorf("该只有一层 xone 错误，got %d 层：%v", n, err)
	}
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Op != "start" {
		t.Errorf("启动钩子的 panic 该报在 start 上，got=%v", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("panic 出来的 error 该还在链上，got=%v", err)
	}
}

func TestRun_ServicePanicReportedOnMatchingOp(t *testing.T) {
	// op 只从 CLAUDE.md 那张表里选：服务启动炸了是 start，停止炸了是 stop
	for _, tc := range []struct {
		name string
		srv  *lateRunnable
		op   string
	}{
		{"Start", &lateRunnable{
			start: func(context.Context) error { panic("Start 炸了") },
			stop:  func(context.Context) error { return nil },
		}, "start"},
		{"Stop", &lateRunnable{
			start: func(context.Context) error { return nil },
			stop:  func(context.Context) error { panic("Stop 炸了") },
		}, "stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Run(tc.srv, WithLogger(quietLogger()))
			var xe *xerror.Error
			if !errors.As(err, &xe) || xe.Op != tc.op {
				t.Errorf("该报在 %s 上，got=%v", tc.op, err)
			}
		})
	}
}

func TestRun_ErrorTellsWhoReportedIt(t *testing.T) {
	// 统一成 xerror 的全部意义就在这里：调用方拿到一个错误，
	// 既能问「最终是谁报的」，也能问「链里牵扯到谁」，
	// 而不必去匹配错误消息里的字符串前缀
	boom := xerror.Newf("xgorm", "connect", "cannot reach %s: %w", "127.0.0.1:5432", errors.New("connection refused"))
	r := &recorder{}

	comps(t, comp("XGorm", hook.StageClient, r, boom))
	err := Run(newServer(r), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil {
		t.Fatal("初始化失败该返回错误")
	}
	// 最外层是框架：是它最终把这个错误交出来的
	if got := xerror.Module(err); got != "xone" {
		t.Errorf("最外层该是 xone，got=%q", got)
	}
	// 但根因仍然问得出来
	if !xerror.Is(err, "xgorm") {
		t.Errorf("该能问出根因在 xgorm，err=%v", err)
	}
	if xerror.Is(err, "xredis") {
		t.Errorf("不该认成没参与的模块，err=%v", err)
	}
	// 原始错误链也没断
	if !errors.Is(err, boom) {
		t.Errorf("原始错误该还在链上，err=%v", err)
	}
}

// ---- 信号 ----

func TestRun_ListensForBothExitSignals(t *testing.T) {
	// K8s 发的是 SIGTERM，Ctrl-C 是 SIGINT。漏掉任何一个，
	// 那条路径上的进程就是被系统直接杀掉，没有优雅退出这回事
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			r := &recorder{}

			comps(t, comp("a", hook.StageClient, r, nil))
			if err := Run(signalOnStart(newServer(r), sig, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
				t.Fatalf("按信号退出不是故障，不该报错：%v", err)
			}
			if got := r.String(); !strings.Contains(got, "stop:server") || !strings.Contains(got, "close:a") {
				t.Errorf("收到 %v 应当走完整个优雅退出，got=%s", sig, got)
			}
		})
	}
}

// blockingHandler 一个 slog.Handler：看到指定的 msg 就停下来，
// 通知外面、等外面放行。用来把「框架正卡在某一步」变成一个确定的时刻
type blockingHandler struct {
	slog.Handler
	on      string
	entered chan struct{}
	release chan struct{}
	once    sync.Once

	mu   sync.Mutex
	msgs []string
}

func (h *blockingHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, r.Message)
	h.mu.Unlock()

	if r.Message == h.on {
		h.once.Do(func() {
			close(h.entered)
			<-h.release
		})
	}
	return h.Handler.Handle(ctx, r)
}

func (h *blockingHandler) saw(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, msg) {
			return true
		}
	}
	return false
}

func TestRun_SignalDuringConfigLoadCounts(t *testing.T) {
	// 信号接管必须早于读配置。装在读配置之后的话，那段窗口里的 SIGTERM
	// 走系统默认处置——进程当场暴毙。配置文件大、或者 Import 了好几个文件时，
	// 这段窗口并不短。
	//
	// 卡在「loading config」那条日志上再发信号，就把这段窗口变成了确定的时刻
	h := &blockingHandler{
		Handler: slog.NewTextHandler(io.Discard, nil),
		on:      "loading config",
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	r := &recorder{}
	done := make(chan error, 1)
	go func() {
		comps(t, comp("a", hook.StageClient, r, nil))
		done <- Run(newServer(r), WithConfigPath(emptyConf(t)), WithLogger(slog.New(h)))
	}()

	<-h.entered             // Run 此刻正停在读配置这一步
	syscallSelfInterrupt(t) // 信号落在这段窗口里
	close(h.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("按信号退出不是故障，不该报错：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("读配置期间的信号没被接住，Run 没有退出")
	}

	// 断言落在「信号被框架接住了」上，不落在「服务有没有起来」「组件有没有关」上：
	// 取消赶不赶得上 runStart 的那次检查是一场没意义的竞态。
	// 这条承诺说的是进程不会在这段窗口里走系统默认处置——真没接住的话，
	// 测试进程自己就死在这儿了，根本走不到下面这行
	if !h.saw("shutdown signal received") {
		t.Errorf("读配置期间的信号该由框架接住，实际日志=%v", h.msgs)
	}
}

func TestRun_CanRunAgainAfterReturn(t *testing.T) {
	// Run 返回时必须把信号注销干净。不注销的话，第二次 Run 的 handler
	// 挂在一个没人读的 channel 上，信号被前一次的残留吃掉——
	// 表现是「第二次怎么都停不下来」。同一个进程里反复 Run 的测试全靠这点
	for i := 1; i <= 2; i++ {
		r := &recorder{}

		comps(t, comp("a", hook.StageClient, r, nil))
		if err := Run(signalOnStart(newServer(r), syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
			t.Fatalf("第 %d 次 Run 失败：%v", i, err)
		}
		if !strings.Contains(r.String(), "stop:server") {
			t.Fatalf("第 %d 次没能按信号停下来，got=%s", i, r.String())
		}
	}
}

func TestRun_NextRunReadsOwnConfigAfterLoadFailure(t *testing.T) {
	// 配置跟着一次 Run 走，失败的那次也不例外：加载失败要是留在包里，
	// 同一个进程里的下一次 Run 会被它挡住，读不到自己的那一份
	bad := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(bad, []byte("Demo: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(newServer(&recorder{}), WithConfigPath(bad), WithLogger(quietLogger())); err == nil {
		t.Fatal("写坏了的配置应当让 Run 失败")
	}

	s := newServer(&recorder{})
	s.startEr = errors.New("stop right away")
	err := Run(s, WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if err == nil || !strings.Contains(err.Error(), "stop right away") {
		t.Fatalf("下一次 Run 应当读它自己的配置、照常启动，got=%v", err)
	}
}

// ---- 停止阶段的 ctx ----

func TestRun_StopCtxDoesNotInheritCancellation(t *testing.T) {
	// 收到信号之后唯一要做的事就是优雅退出，而优雅退出全靠 Stop 还能干活。
	// 沿用被取消的那个 ctx 的话，每个关闭动作一进去就被拒绝——
	// 在途请求没做完、注册中心那条记录没注销，等于没有优雅退出这回事
	var serverStopErr, hookStopErr error
	var serverHasDeadline, hookHasDeadline bool

	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); return nil },
		stop: func(ctx context.Context) error {
			serverStopErr = ctx.Err()
			_, serverHasDeadline = ctx.Deadline()
			return nil
		},
	}
	probe := pair{
		start: hook.Entry{Name: "probe.init", Pkg: "probe", Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: "probe.close", Pkg: "probe", Run: func(ctx context.Context) error {
			hookStopErr = ctx.Err()
			_, hookHasDeadline = ctx.Deadline()
			return nil
		}},
	}

	comps(t, probe)
	if err := Run(signalOnStart(srv, syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
		t.Fatalf("按信号退出不该报错：%v", err)
	}

	if serverStopErr != nil {
		t.Errorf("Stop 拿到的 ctx 不该是已取消的，got=%v", serverStopErr)
	}
	if hookStopErr != nil {
		t.Errorf("停止钩子拿到的 ctx 不该是已取消的，got=%v", hookStopErr)
	}
	// 不继承取消，但必须继承预算——否则一个关不掉的资源能把进程挂到被 SIGKILL
	if !serverHasDeadline || !hookHasDeadline {
		t.Errorf("停止阶段的 ctx 必须带停止预算，server=%v hook=%v", serverHasDeadline, hookHasDeadline)
	}
}

// ---- panic 隔离 ----

func TestRun_ServiceStartPanicIsIsolated(t *testing.T) {
	// 一个 panic 打穿进程的话，已经建好的资源一个都关不掉
	r := &recorder{}
	srv := &lateRunnable{
		start: func(context.Context) error { panic("Start 炸了") },
		stop:  func(context.Context) error { return nil },
	}

	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(srv, WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("Start 的 panic 该被转成错误，got=%v", err)
	}
	if !strings.Contains(r.String(), "close:a") {
		t.Errorf("服务炸了，组件也要被关掉，got=%s", r.String())
	}
}

func TestRun_ServiceStopPanicIsIsolated(t *testing.T) {
	r := &recorder{}
	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); return nil },
		stop:  func(context.Context) error { panic("Stop 炸了") },
	}

	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(signalOnStart(srv, syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("Stop 的 panic 该被转成错误，got=%v", err)
	}
	if !strings.Contains(r.String(), "close:a") {
		t.Errorf("Stop 炸了，其余组件仍要被关掉，got=%s", r.String())
	}
}

func TestRun_StopHookPanicDoesNotAbortOthers(t *testing.T) {
	// 退出阶段要尽量把能关的都关掉。一个钩子炸了就停手的话，
	// 排在它后面的连接池全都漏着
	r := &recorder{}
	boom := pair{
		start: hook.Entry{Name: "boom.init", Pkg: "boom", Stage: hook.StageClient,
			Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: "boom.close", Pkg: "boom", Stage: hook.StageClient,
			Run: func(context.Context) error { panic("关的时候炸了") }},
	}
	// 档位更低 = 更后关，所以它排在 boom 后面
	last := comp("最后关的", hook.StageLog, r, nil)

	comps(t, boom, last)
	err := Run(signalOnStart(newServer(r), syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("停止钩子的 panic 该被转成错误，got=%v", err)
	}
	if !strings.Contains(r.String(), "close:最后关的") {
		t.Errorf("一个钩子炸了不该拦住后面的，got=%s", r.String())
	}
}

// ---- 服务退出相关 ----

func TestRun_ServiceStopErrorIsAggregated(t *testing.T) {
	r := &recorder{}
	stopErr := errors.New("优雅退出没做完")
	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); return nil },
		stop:  func(context.Context) error { return stopErr },
	}

	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(signalOnStart(srv, syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if !errors.Is(err, stopErr) {
		t.Fatalf("Stop 的错误该被带出来，got=%v", err)
	}
	if !strings.Contains(r.String(), "close:a") {
		t.Errorf("Stop 报错不该拦住组件关闭，got=%s", r.String())
	}
}

func TestRun_ClosesOthersWhenServiceWontExit(t *testing.T) {
	// Stop 返回不等于 Start 返回。等 Start 是对的（还在处理的请求会摸到
	// 已经关掉的连接池），但不能无限等——预算耗尽就得往下走，
	// 否则一个不肯退的服务能把整个进程挂到被 SIGKILL
	r := &recorder{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); <-release; return nil },
		stop:  func(context.Context) error { return nil },
	}

	var logbuf bytes.Buffer
	start := time.Now()
	comps(t, comp("a", hook.StageClient, r, nil))
	_ = Run(signalOnStart(srv, syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)),
		WithLogger(slog.New(slog.NewTextHandler(&logbuf, nil))),
		WithStopTimeout(300*time.Millisecond))
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("不肯退的服务把整个退出流程拖住了，耗时=%v", elapsed)
	}
	if !strings.Contains(r.String(), "close:a") {
		t.Errorf("等不到服务退出也要把组件关掉，got=%s", r.String())
	}
	if !strings.Contains(logbuf.String(), "did not exit within its share of the stop budget") {
		t.Errorf("该留下一条说得清楚的告警，实际日志=\n%s", logbuf.String())
	}
}

func TestRun_StopIgnoringCtxDoesNotHangExit(t *testing.T) {
	// Stop 收了 ctx，但里面可能是一个不吃 ctx 的第三方调用。同步调的话
	// Run 永远返回不了，一个停止钩子都轮不到
	r := &recorder{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); return nil },
		stop:  func(context.Context) error { <-release; return nil },
	}

	var logbuf bytes.Buffer
	comps(t, comp("a", hook.StageClient, r, nil))
	done := make(chan error, 1)
	go func() {
		done <- Run(signalOnStart(srv, syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)),
			WithLogger(slog.New(slog.NewTextHandler(&logbuf, nil))),
			WithStopTimeout(300*time.Millisecond))
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("卡住的 Stop 把整个退出流程挂住了")
	}
	if !strings.Contains(r.String(), "close:a") {
		t.Errorf("Stop 卡住也要把组件关掉，got=%s", r.String())
	}
	if !strings.Contains(logbuf.String(), "server Stop did not return within its share of the stop budget") {
		t.Errorf("该留下一条说得清楚的告警，实际日志=\n%s", logbuf.String())
	}
}

func TestRun_DeadlineAwareStopErrorIsKept(t *testing.T) {
	// 守规矩的 Stop 恰恰是等到截止时间才返回的：xgin 等在途 handler 等到那一刻，
	// 再带着「N handler(s) still running」回来。它和框架那边的超时几乎同时发生，
	// 框架先看到超时的话，这个错误就丢了，进程以 0 退出（e2e 撞上过）。
	// 这里让它在截止时间之后 5ms 才返回，稳定地落在那一截余量里
	boom := errors.New("2 handler(s) still running")
	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); return nil },
		stop: func(ctx context.Context) error {
			<-ctx.Done()
			time.Sleep(5 * time.Millisecond)
			return boom
		},
	}
	comps(t, comp("a", hook.StageClient, &recorder{}, nil))
	err := Run(signalOnStart(srv, syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithStopTimeout(300*time.Millisecond))
	if !errors.Is(err, boom) {
		t.Errorf("Stop 带回来的错误要如实报出，got=%v", err)
	}
}

// ---- 配置 ----

func TestRun_InvalidConfigRunsNoHooks(t *testing.T) {
	// 配置是使用者唯一的操作界面。带着一份读不出来的配置往下建连接，
	// 报出来的会是一堆看不出根因的连接错误
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte("Demo:\n  Addr: [坏\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &recorder{}
	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(newServer(r), WithConfigPath(p), WithLogger(quietLogger()))

	if err == nil {
		t.Fatal("配置解析不了应当启动失败")
	}
	if got := r.String(); got != "" {
		t.Errorf("配置都读不出来，一个钩子都不该跑，got=%s", got)
	}
}

// ---- 参数与入口 ----

func TestRun_StopBudgetMustBePositive(t *testing.T) {
	// 0 不是「不限时」而是「一点都不等」：Stop 拿到一个已经过期的 context，
	// 服务当场被切断。负值同理，而且更像是算出来的而不是写死的
	for _, d := range []time.Duration{0, -time.Second} {
		r := &recorder{}
		started := false
		lr := &lateRunnable{
			start: func(context.Context) error { started = true; return nil },
			stop:  func(context.Context) error { return nil },
		}
		err := Run(lr, WithLogger(quietLogger()), WithStopTimeout(d))
		if err == nil {
			t.Errorf("停止预算 %v 应当直接失败", d)
		}
		if started {
			t.Errorf("预算非法时不该起服务，budget=%v", d)
		}
		_ = r
	}
}

func TestMustRun_ReturnsOnSuccess(t *testing.T) {
	r := &recorder{}
	srv := &lateRunnable{
		start: func(context.Context) error { r.add("start"); return nil },
		stop:  func(context.Context) error { return nil },
	}
	MustRun(srv, WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if !strings.Contains(r.String(), "start") {
		t.Errorf("成功时该正常跑完，got=%s", r.String())
	}
}

const mustRunChildEnv = "XONE_TEST_MUSTRUN_CHILD"

func TestMustRun_ExitsWithCode1OnError(t *testing.T) {
	if os.Getenv(mustRunChildEnv) == "1" {
		// 停止预算为 0 是最容易造的启动失败
		MustRun(newServer(&recorder{}), WithStopTimeout(0))
		return
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestMustRun_ExitsWithCode1OnError$")
	cmd.Env = append(os.Environ(), mustRunChildEnv+"=1")
	out, err := cmd.CombinedOutput()

	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("MustRun 出错时该以非零退出码结束，got=%v", err)
	}
	if ee.ExitCode() != 1 {
		t.Errorf("退出码该是 1，got=%d", ee.ExitCode())
	}
	if !strings.Contains(string(out), "stop budget") {
		t.Errorf("该把失败原因打到 stderr，got=%s", out)
	}
}

// ---- 框架自己的日志 ----

func TestRun_FrameworkLogsFollowGlobalLoggerSwappedByHook(t *testing.T) {
	// xlog 就是在启动钩子里调 slog.SetDefault 的。Run 开头把 logger 捕获一次的话，
	// 它之后所有框架日志都还写在旧的那个上——服务起来了，
	// 而「初始化到哪一步」的日志一行都看不到
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	var buf bytes.Buffer
	swap := pair{start: hook.Entry{Name: "swap.init", Pkg: "swap", Stage: hook.StageLog,
		Run: func(context.Context) error {
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			return nil
		}}}
	after := comp("后面的", hook.StageClient, &recorder{}, nil)

	srv := &lateRunnable{
		start: func(context.Context) error { return nil },
		stop:  func(context.Context) error { return nil },
	}
	// 不传 WithLogger：走的就是「每次重新取 slog.Default()」那条路
	comps(t, swap, after)
	if err := Run(srv, WithConfigPath(emptyConf(t))); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(buf.String(), "后面的.init") {
		t.Errorf("换掉全局 logger 之后的框架日志该写到新的那个上，实际=\n%s", buf.String())
	}
}

func TestRun_StartErrorKeptWhenStopReturnsAtDeadline(t *testing.T) {
	// 回归用例。守规矩的 Stop 看着截止时间返回，它一返回 Start 也跟着带错误返回：
	// 到了「等 Start 返回」那一步，runErr 和 serverCtx.Done() 同时就绪。
	// select 随机挑一个，挑中 Done 时 Start 的错误丢了，还白打一条「服务没按时退出」的告警。
	// 两边就绪的先后由调度决定，所以跑很多次：挑错一次的概率是一半，二十次全躲过去不可能
	for i := range 20 {
		wantErr := fmt.Errorf("serve failed #%d", i)
		stop := make(chan struct{})
		startReturned := make(chan struct{})
		r := &lateRunnable{
			start: func(context.Context) error {
				syscallSelfInterrupt(t)
				<-stop
				defer close(startReturned)
				return wantErr
			},
			stop: func(ctx context.Context) error {
				<-ctx.Done() // 用满服务那一段
				close(stop)
				<-startReturned
				time.Sleep(5 * time.Millisecond) // 让 Start 的返回值先落进 channel
				return nil
			},
		}
		var logs bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logs, nil))
		err := Run(r, WithConfigPath(emptyConf(t)), WithLogger(log), WithStopTimeout(90*time.Millisecond))
		if !errors.Is(err, wantErr) {
			t.Fatalf("第 %d 次：Start 的错误丢了，got=%v", i, err)
		}
		if strings.Contains(logs.String(), "server did not exit") {
			t.Fatalf("第 %d 次：服务已经退出了，不该告警它没退出：\n%s", i, logs.String())
		}
	}
}
