package xredis

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xonetest"
)

func liveCfg(f *fakeRedis) ClientConfig {
	c := DefaultClientConfig()
	c.Addr = f.addr()
	c.DialTimeout, c.ReadTimeout, c.WriteTimeout = 300*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	return c
}

// deadAddr 一个没人监听的地址
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestNew_ReturnsUsableClientWhenReachable(t *testing.T) {
	f := newFakeRedis(t)
	client, closer, err := New(context.Background(), liveCfg(f))
	if err != nil {
		t.Fatalf("应当连得上：%v", err)
	}
	defer closer.Close()

	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Errorf("拿到的应是可用的原生 client：%v", err)
	}
	if !slices.Contains(f.seen(), "ping") {
		t.Errorf("建连时应当 Ping 一次确认连得上，实际收到的命令=%v", f.seen())
	}
}

func TestNew_VerifiesConnectivityAtStartup(t *testing.T) {
	// 地址写错、密码不对这类问题该在启动时暴露，而不是线上第一次读缓存才发现
	f := newFakeRedis(t)
	f.setFailPing(true)

	c := liveCfg(f)
	_, _, err := New(context.Background(), c)
	if err == nil {
		t.Fatal("Ping 失败时应当报错")
	}
	if !strings.Contains(err.Error(), c.Addr) {
		t.Errorf("错误里应有地址，否则排查不了连的是谁：%v", err)
	}
}

func TestNew_NoPoolLeakWhenUnreachable(t *testing.T) {
	c := DefaultClientConfig()
	c.Addr = deadAddr(t)
	c.DialTimeout, c.ReadTimeout = 50*time.Millisecond, 50*time.Millisecond
	c.Trace = false

	before := testkit.Stabilize()
	for i := 0; i < 5; i++ {
		client, closer, err := New(context.Background(), c)
		if err == nil {
			reg.Close()
			t.Fatal("连不上时应当报错")
		}
		if client != nil || closer != nil {
			t.Error("失败时不该返回半成品")
		}
	}
	if after := testkit.SettleTo(before); after > before+1 {
		t.Errorf("建连失败 5 次后协程数从 %d 涨到 %d，说明 client 没被关掉", before, after)
	}
}

func TestInstall_LogsNameAndAddrButNotPassword(t *testing.T) {
	f := newFakeRedis(t)
	lines := capture(t)

	c := liveCfg(f)
	c.Password = "hunter2"
	if err := initComponent(t, Config{Clients: map[string]ClientConfig{"session": c}}); err != nil {
		t.Fatal(err)
	}

	var connected map[string]any
	for _, l := range lines() {
		blob := fmt.Sprint(l)
		if strings.Contains(blob, "hunter2") {
			t.Errorf("日志里出现了密码：%v", l)
		}
		if l["msg"] == "xredis connected" {
			connected = l
		}
	}
	// 配了好几个 Redis 时，只写地址分不出是哪一个
	if connected == nil || connected["addr"] != c.Addr || connected["name"] != "session" {
		t.Errorf("该写出实例名和地址，否则排查不了连的是谁，got=%v", lines())
	}
}

func TestNew_NoConnectOnInvalidConfig(t *testing.T) {
	c := DefaultClientConfig()
	c.Addr = ""
	if _, _, err := New(context.Background(), c); err == nil {
		t.Fatal("Addr 为空应当报错")
	}
}

func TestNew_ValidatesBeforeConnecting(t *testing.T) {
	// 对端是好的，只有配置不合法：不校验的话 go-redis 把 -1s 当成「不限时」，建连照样成功
	c := liveCfg(newFakeRedis(t))
	c.ReadTimeout = -time.Second
	_, closer, err := New(context.Background(), c)
	if closer != nil {
		closer.Close()
	}
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Op != "config" {
		t.Fatalf("直接调 New 也该先校验配置，以 op=config 报错，got=%v", err)
	}
}

func TestNew_PoolOptionsPassedThrough(t *testing.T) {
	f := newFakeRedis(t)
	c := liveCfg(f)
	c.PoolSize, c.MinIdleConns = 7, 2

	client, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	if got := client.Options().PoolSize; got != 7 {
		t.Errorf("PoolSize 应传下去，got=%d", got)
	}
	if got := client.Options().MinIdleConns; got != 2 {
		t.Errorf("MinIdleConns 应传下去，got=%d", got)
	}
}

func TestPingTimeout(t *testing.T) {
	if got := pingTimeout(&redis.Options{DialTimeout: time.Second, ReadTimeout: 2 * time.Second}); got != 3*time.Second {
		t.Errorf("Ping 预算应为建连 + 读超时，got=%v", got)
	}
	// 配置里的 0 交给 go-redis 补成默认值，预算按补完的算
	c := redis.NewClient(&redis.Options{})
	defer c.Close()
	if got := pingTimeout(c.Options()); got != 10*time.Second {
		t.Errorf("0 是 go-redis 的默认值（5s + 5s），got=%v", got)
	}
	if got := pingTimeout(&redis.Options{}); got != fallbackPingTimeout {
		t.Errorf("推算不出时该用兜底值，got=%v", got)
	}
}

// 配 0 在 go-redis v9.22.0 里是「用它的默认值」（DialTimeout 5s、ReadTimeout 5s，options.go init），
// 不是「不花时间」。预算按配置里的 0 + 0 算的话只剩 1s 兜底：一次 1.5s 才回 PONG 的建连，
// go-redis 自己还愿意等，建连验证先判了超时，3 次都失败
func TestNew_ZeroTimeoutsProbeWithGoRedisDefaults(t *testing.T) {
	f := newFakeRedis(t)
	f.setDelay("ping", 1500*time.Millisecond)
	c := liveCfg(f)
	c.DialTimeout, c.ReadTimeout, c.WriteTimeout = 0, 0, 0
	client, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatalf("go-redis 默认等 5s，1.5s 的 PONG 该连得上：%v", err)
	}
	defer closer.Close()
	if o := client.Options(); o.DialTimeout != 5*time.Second || o.ReadTimeout != 5*time.Second {
		t.Errorf("前提：0 交给 go-redis 换成默认值，got dial=%v read=%v", o.DialTimeout, o.ReadTimeout)
	}
}

func TestNew_AbortsConnectOnExitSignal(t *testing.T) {
	// 地址不通时这里要走满一轮 Ping 重试（默认 3 次 × 间隔）。
	// 启动到一半收到 SIGTERM，就该立刻放弃，而不是让进程卡在一个
	// 注定连不上的库上，把退出时间拖满整轮重试
	f := newFakeRedis(t)
	f.setFailPing(true)
	c := liveCfg(f)
	// 重试间隔调成一小时：走整轮就是挂住，当场放弃是毫秒级，上界 5s 离两头都远。
	// TestMain 里的 10ms 拿来当上界的话，机器一忙就误报
	old := pingInterval
	pingInterval = time.Hour
	t.Cleanup(func() { pingInterval = old })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟建连之前就收到了退出信号

	start := time.Now()
	_, _, err := New(ctx, c)
	if err == nil {
		t.Fatal("ctx 已取消时不该建连成功")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("应当当场放弃而不是走完整轮重试，耗时=%v", elapsed)
	}
}

func TestProbe_FailsAfterRetries(t *testing.T) {
	f := newFakeRedis(t)
	f.setFailPing(true)
	c := liveCfg(f)

	client := redis.NewClient(&redis.Options{Addr: c.Addr})
	defer client.Close()

	if err := xclient.Probe(context.Background(), probePolicy(client.Options()), probe(client)); err == nil {
		t.Fatal("Ping 一直失败时应当返回错误")
	}
	// 数服务端收到几次 ping，不看耗时：退避带抖动之后，
	// 每次等待是 [0, 当前退避] 之间的随机值，可以短到接近零。
	// 「重试了几次」本来就该数次数，耗时只是它的一个弱代理
	n := 0
	for _, cmd := range f.seen() {
		if cmd == "ping" {
			n++
		}
	}
	if n != pingAttempts {
		t.Errorf("服务端应收到 %d 次 ping，实际 %d 次", pingAttempts, n)
	}
}

// ---- 全局实例 ----

func withClients(t *testing.T, m map[string]*redis.Client) {
	t.Helper()
	insts := make(map[string]instance, len(m))
	for name, c := range m {
		insts[name] = instance{client: c, metric: true}
	}
	publish(t, insts)
}

// publish 把一组现成的实例经 xclient.Build 发布出去，测试结束后清空
func publish(t *testing.T, insts map[string]instance) {
	t.Helper()
	keep := func(_ context.Context, _ string, v instance) (instance, io.Closer, error) { return v, nil, nil }
	if err := xclient.Build(context.Background(), reg, insts, keep); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = xclient.Build(context.Background(), reg, nil, keep) })
}

func TestC_PanicsWhenNotFound(t *testing.T) {
	withClients(t, map[string]*redis.Client{})
	defer func() {
		msg := fmt.Sprint(recover())
		if msg == "<nil>" {
			t.Fatal("取不到实例应当 panic")
		}
		if !strings.Contains(msg, ConfigKey) {
			t.Errorf("一个都没配时该提示去看配置块，got=%v", msg)
		}
	}()
	C()
}

func TestC_PanicMessageListsConfiguredInstances(t *testing.T) {
	withClients(t, map[string]*redis.Client{"cache": {}, "session": {}})
	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "cache") || !strings.Contains(msg, "session") {
			t.Errorf("应列出已配置的实例名，got=%v", msg)
		}
	}()
	C("typo")
}

func TestHasNames(t *testing.T) {
	withClients(t, map[string]*redis.Client{"session": {}, "cache": {}})
	if !Has("cache") || Has("nope") || Has() {
		t.Error("Has 应如实反映是否配过")
	}
	if got := Names(); len(got) != 2 || got[0] != "cache" {
		t.Errorf("Names 应按名字排序，got=%v", got)
	}
}

func TestRegister_RegistrationMatchesFramework(t *testing.T) {
	// 这是本包和框架之间唯一的一根线：钩子漏登记、档位挂错，
	// 表现是「配置不生效」或者「实例比用它的东西晚就绪」，别处都测不出来
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xredis" {
			got = &e
			break
		}
	}
	if got == nil {
		t.Fatal("没有登记启动钩子")
	}
	if got.Stage != hook.StageClient {
		t.Errorf("缓存要在业务之前就绪（StageClient），got=%v", got.Stage)
	}

	var stopped bool
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xredis" {
			stopped = true
			if e.Stage != hook.StageClient {
				// 启动和停止不在同一档的话，业务还在用的时候它就被关了
				t.Errorf("停止钩子也该在 StageClient，got=%v", e.Stage)
			}
		}
	}
	if true != stopped {
		t.Errorf("停止钩子登记情况不对，got=%v", stopped)
	}
}

func TestInitAll_BuildsAndClosesCleanly(t *testing.T) {
	// 配置直接传进去，不用换包级变量再记得换回来：
	// start 的接收者就是那份配置
	f := newFakeRedis(t)
	c := Config{Clients: map[string]ClientConfig{"a": liveCfg(f), "b": liveCfg(f)}}

	err := initComponent(t, c)
	if err != nil {
		t.Fatalf("应当建得起来：%v", err)
	}
	if got := Names(); len(got) != 2 {
		t.Fatalf("应有两个实例，got=%v", got)
	}
	if err := reg.Close(); err != nil {
		t.Errorf("关闭不该报错：%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("关闭后应清空，否则 C() 会返回已关闭的实例，got=%v", got)
	}
}

func TestInitAll_OneFailureRollsBackAll(t *testing.T) {
	// Init 返回错误时框架拿不到 closer，已经建好的实例必须自己收拾——
	// 不收拾的话那些连接会一直挂到进程结束，而且 C() 还可能摸到它们
	f := newFakeRedis(t)

	bad := DefaultClientConfig()
	bad.Addr = deadAddr(t)
	bad.DialTimeout, bad.ReadTimeout, bad.MinIdleConns = 30*time.Millisecond, 30*time.Millisecond, 0
	cfg := Config{Clients: map[string]ClientConfig{"a": liveCfg(f), "z": bad}}

	err := initComponent(t, cfg)
	if err == nil {
		t.Fatal("有实例连不上时应当报错")
	}
	if !strings.Contains(err.Error(), `"z"`) {
		t.Errorf("错误里应点名是哪个实例，got=%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("失败时不该发布任何实例，否则 C() 会摸到半成品，got=%v", got)
	}
	// 直接看假服务端：连接都断了，才说明先建好的那个真被关了
	if n := f.waitConns(0); n != 0 {
		t.Errorf("先建好的实例应当被回滚关闭，假服务端上还开着 %d 个连接", n)
	}
}

func TestInitAll_NoOpWhenUnconfigured(t *testing.T) {
	cfg := DefaultConfig()

	err := initComponent(t, cfg)
	if err != nil {
		t.Fatalf("没配不该报错：%v", err)
	}
	reg.Close()
	if len(Names()) != 0 {
		t.Errorf("不该建出实例，got=%v", Names())
	}
}

func TestInstallPoolMetrics_RegistersOnceWhenInstalledRepeatedly(t *testing.T) {
	// 每个开了指标的实例都会调它一次，而 collector 是进程级的一个：
	// 第二次注册 Prometheus 会报 duplicate，这套指标从此一个都导不出去
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	// 别的用例留下的 go-redis 后台协程也会往默认 logger 写，buffer 得并发安全
	var buf syncBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	installPoolMetrics()
	installPoolMetrics()

	if strings.Contains(buf.String(), "failed to register") {
		t.Errorf("挂第二次应当复用已有的那个，而不是让 Prometheus 报重复注册\n实际=\n%s", buf.String())
	}
}

func TestPoolCollector(t *testing.T) {
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	stats := map[string]*redis.PoolStats{
		"cache": {TotalConns: 5, IdleConns: 3, StaleConns: 1, Hits: 100, Misses: 7, Timeouts: 2},
	}
	m.Registry.MustRegister(newPoolCollector("demo", nil, func() map[string]*redis.PoolStats { return stats }))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	out := w.Body.String()

	for _, want := range []string{
		`demo_redis_pool_connections{name="cache"} 5`,
		`demo_redis_pool_connections_idle{name="cache"} 3`,
		`demo_redis_pool_connections_stale_total{name="cache"} 1`,
		`demo_redis_pool_hits_total{name="cache"} 100`,
		`demo_redis_pool_misses_total{name="cache"} 7`,
		`demo_redis_pool_timeouts_total{name="cache"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("导出里应有 %s\n实际=\n%s", want, out)
		}
	}
}

func TestPoolStats_ReadsLiveInstance(t *testing.T) {
	f := newFakeRedis(t)
	client, closer, err := New(context.Background(), liveCfg(f))
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	withClients(t, map[string]*redis.Client{"cache": client})

	got := poolStats()
	if len(got) != 1 || got["cache"] == nil {
		t.Errorf("应读到活着的实例，got=%v", got)
	}
}

func TestNew_CommandsHonorCallerDeadline(t *testing.T) {
	// go-redis 默认不让请求的 context 管住 socket 读写：不开
	// ContextTimeoutEnabled 的话，每个命令用的是 ReadTimeout 这组固定值，
	// 调用方给的 deadline 只是摆设——一个 200ms 超时的请求照样会在一个
	// 慢 Redis 上等满 ReadTimeout，上游的超时预算和级联保护跟着一起失效
	f := newFakeRedis(t)
	f.setStall("get") // 收下 GET 但永不回复

	c := liveCfg(f)
	c.ReadTimeout = 5 * time.Second // 比 ctx 的预算大得多
	c.MaxRetries = -1               // 重试会掩盖掉这件事
	client, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = client.Get(ctx, "k").Err()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("该超时的")
	}
	// go-redis 把 ctx 的 deadline 设到 socket 上，所以报上来的是
	// os.ErrDeadlineExceeded（i/o timeout）而不是 context.DeadlineExceeded。
	// 调用方要判超时得认这个，或者干脆判 ctx.Err()
	if !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("该是超时错误，got=%v (%T)", err, err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("ctx 给了 200ms，实际等了 %v —— deadline 没管住 socket 读写", elapsed.Round(10*time.Millisecond))
	}
}

// initComponent 走一遍框架真正会走的路径：按这份配置把实例挨个建出来。
// 多实例的建法（排序、失败回滚、panic 隔离）全在那条路上。
func initComponent(t *testing.T, c Config) error {
	t.Helper()
	t.Cleanup(func() { _ = reg.Close() })
	return install(context.Background(), c)
}

func TestInitXRedis_NoConnectionsWithoutSection(t *testing.T) {
	// xredis 是可选依赖：没配不该让服务起不来，更不该去连 localhost:6379
	t.Cleanup(func() { _ = closeXRedis(context.Background()) })
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")

	if err := initXRedis(context.Background()); err != nil {
		t.Fatalf("没配不该报错：%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("不该建出实例，got=%v", got)
	}
}

func TestInitXRedis_NoConnectionsWithEmptySection(t *testing.T) {
	t.Cleanup(func() { _ = closeXRedis(context.Background()) })
	xonetest.UseConfigYAML(t, "XRedis:\n")

	if err := initXRedis(context.Background()); err != nil {
		t.Fatalf("空块不该报错：%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("不该建出实例，got=%v", got)
	}
}

func TestInitXRedis_ConfigTypoFailsStartup(t *testing.T) {
	t.Cleanup(func() { _ = closeXRedis(context.Background()) })
	xonetest.UseConfigYAML(t, "XRedis:\n  Addrs: \"127.0.0.1:6379\"\n")

	if err := initXRedis(context.Background()); err == nil {
		t.Fatal("字段拼错应当让启动失败，否则使用者会一直以为自己配上了")
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("失败时不该留下实例，got=%v", got)
	}
}

func TestInitXRedis_BuildsFromConfigAndClosesCleanly(t *testing.T) {
	t.Cleanup(func() { _ = closeXRedis(context.Background()) })
	f := newFakeRedis(t)
	xonetest.UseConfigYAML(t, "XRedis:\n  Clients:\n    cache:\n      Addr: \""+f.addr()+"\"\n    session:\n      Addr: \""+f.addr()+"\"\n")

	if err := initXRedis(context.Background()); err != nil {
		t.Fatalf("应当建得起来：%v", err)
	}
	if got := Names(); len(got) != 2 {
		t.Fatalf("应有两个实例，got=%v", got)
	}
	if C("cache").Options().Addr != f.addr() {
		t.Errorf("配置里的地址没传到实例上，got=%v", C("cache").Options().Addr)
	}

	if err := closeXRedis(context.Background()); err != nil {
		t.Errorf("关闭不该报错：%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("关完还取得到实例，调用方会摸到一个已经关掉的连接池，got=%v", got)
	}
}

func TestCloseXRedis_ClosesWithoutInit(t *testing.T) {
	if err := closeXRedis(context.Background()); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

func TestInitXRedis_EmptySectionDoesNotFailStartup(t *testing.T) {
	// 空块是「我先占个位」：把内容注释掉、或者挪到 profile 文件里之后，
	// 留下的就是这个形状。它曾经让 Run 报「XRedis 这个 key 没人读，
	// 检查拼写、或者有没有 import 对应的包」——两条都不成立
	t.Cleanup(func() { _ = closeXRedis(context.Background()) })
	xonetest.UseConfigYAML(t, "XRedis:\n")

	if err := initXRedis(context.Background()); err != nil {
		t.Fatalf("空块不该报错：%v", err)
	}
	if got := config.Unclaimed(); len(got) != 0 {
		t.Errorf("跳过了也算读过，否则框架会因为这个 key 没人认领而让启动失败，got=%v", got)
	}
}

// assertOneFrame 断言错误恰好是一层本模块的 xerror，op 是期望的那个。
// 一个模块边界一个 xerror：每层都包的话模块名重复出现，真正的 op 被压进里层
func assertOneFrame(t *testing.T, err error, module, op string) {
	t.Helper()
	var xe *xerror.Error
	if !errors.As(err, &xe) {
		t.Fatalf("应当是 xerror，got %v", err)
	}
	if xe.Module != module || xe.Op != op {
		t.Errorf("want module=%s op=%s，got module=%s op=%s（%v）", module, op, xe.Module, xe.Op, err)
	}
	var inner *xerror.Error
	if errors.As(xe.Err, &inner) {
		t.Errorf("同一个模块只该有一层 xerror，got %v", err)
	}
}

func TestInstall_ErrorWrappedOnceWithAccurateOp(t *testing.T) {
	bad := DefaultClientConfig()
	bad.Addr = ""
	err := initComponent(t, Config{Clients: map[string]ClientConfig{"a": bad}})
	assertOneFrame(t, err, "xredis", "config")

	dead := DefaultClientConfig()
	dead.Addr, dead.DialTimeout, dead.ReadTimeout = deadAddr(t), 30*time.Millisecond, 30*time.Millisecond
	err = initComponent(t, Config{Clients: map[string]ClientConfig{"b": dead}})
	assertOneFrame(t, err, "xredis", "connect")
	if !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("错误里要点名实例，got %v", err)
	}
}

func TestInstall_ExportsOnlyMetricEnabledInstances_SurvivesXmetricReinstall(t *testing.T) {
	// collector 是进程级的一个，抓取时遍历全部实例：不看实例自己的开关，
	// Metric: false 就是一句空话。第二轮是同一进程里再走一遍生命周期——
	// xmetric 换了新的 Registry，collector 得跟着挂上去，否则连接池指标全丢
	f := newFakeRedis(t)
	on, off := liveCfg(f), liveCfg(f)
	off.Metric = false

	for round := 1; round <= 2; round++ {
		m, closer, err := xmetric.New(xmetric.Config{})
		if err != nil {
			t.Fatal(err)
		}
		m.Install()
		if err := install(context.Background(), Config{Clients: map[string]ClientConfig{"on": on, "off": off}}); err != nil {
			t.Fatal(err)
		}
		out := testkit.Scrape(m.Handler)
		if !strings.Contains(out, `redis_pool_connections{name="on"}`) {
			t.Errorf("第 %d 轮：开了 Metric 的实例该导出\n实际=\n%s", round, out)
		}
		if strings.Contains(out, `name="off"`) {
			t.Errorf("第 %d 轮：Metric: false 的实例不该导出\n实际=\n%s", round, out)
		}
		if err := reg.Close(); err != nil {
			t.Fatal(err)
		}
		closer.Close()
	}
}

func TestInitXRedis_CSaysUnconfiguredNotTooEarly(t *testing.T) {
	// 没配也要让注册表知道启动钩子跑过了。否则 C() 会把「没配」说成「调早了」，
	// 使用者会去查调用时机，而真正该查的是配置文件
	t.Cleanup(func() { _ = closeXRedis(context.Background()) })
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXRedis(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "none is configured") {
			t.Errorf("没配时要说没配，got=%v", msg)
		}
	}()
	C()
}

// syncBuffer 是加了锁的 bytes.Buffer
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
