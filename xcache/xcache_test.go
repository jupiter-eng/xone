package xcache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xonetest"
)

func load(t *testing.T, yml string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	t.Cleanup(config.Reset)
	if err := config.Load(path); err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	return c
}

func loadErr(t *testing.T, yml string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(path, []byte(yml), 0o644)
	config.Reset()
	t.Cleanup(config.Reset)
	if err := config.Load(path); err != nil {
		return err
	}
	_, err := loadConfig()
	return err
}

// withInstances 装一套实例，测试结束后还原
func withInstances(t testing.TB, cfgs map[string]ClientConfig) {
	t.Helper()
	built := map[string]instance{}
	for name, c := range cfgs {
		cache, closer, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = closer.Close() })
		built[name] = instance{cache: cache, ttl: c.DefaultTTL, metric: c.Metric}
	}
	publish(t, built)
}

// publish 把一组现成的实例经 xclient.Build 发布出去，测试结束后清空
func publish(t testing.TB, insts map[string]instance) {
	t.Helper()
	keep := func(_ context.Context, _ string, v instance) (instance, io.Closer, error) { return v, nil, nil }
	if err := xclient.Build(context.Background(), reg, insts, keep); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = xclient.Build(context.Background(), reg, nil, keep) })
}

func TestConfig_SingleInstanceForm(t *testing.T) {
	c := load(t, "XCache:\n  MaxCost: 500\n")
	got, ok := c.Clients[DefaultName]
	if !ok {
		t.Fatalf("单实例写法应规整成名为 %s 的实例，got=%v", DefaultName, c.Clients)
	}
	if got.MaxCost != 500 {
		t.Errorf("写了的字段应生效，got=%+v", got)
	}
	if got.NumCounters != 1_000_000 || got.DefaultTTL != 5*time.Minute {
		t.Errorf("没写的字段应保持默认，got=%+v", got)
	}
}

func TestConfig_MultiInstanceForm(t *testing.T) {
	c := load(t, "XCache:\n  Clients:\n    default: {MaxCost: 100}\n    session: {MaxCost: 200, DefaultTTL: 30m}\n")
	if len(c.Clients) != 2 {
		t.Fatalf("应解出两个实例，got=%v", c.Clients)
	}
	if c.Clients["session"].DefaultTTL != 30*time.Minute {
		t.Errorf("写了的字段应生效，got=%+v", c.Clients["session"])
	}
	if c.Clients["default"].DefaultTTL != 5*time.Minute {
		t.Errorf("一个实例改了 TTL 不该影响另一个，got=%+v", c.Clients["default"])
	}
}

func TestConfig_TypoFails(t *testing.T) {
	if err := loadErr(t, "XCache:\n  MaxCoat: 1\n"); err == nil {
		t.Fatal("字段拼错应当启动失败")
	}
	if err := loadErr(t, "XCache:\n  Clients:\n    a: {MaxCoat: 1}\n"); err == nil {
		t.Fatal("实例里的字段拼错也应当启动失败")
	}
}

func TestConfig_FormsCannotBeMixed(t *testing.T) {
	if err := loadErr(t, "XCache:\n  MaxCost: 1\n  Clients:\n    a: {MaxCost: 2}\n"); err == nil {
		t.Fatal("混用两种写法应当失败")
	}
}

func TestConfig_NoConfigNoInstances(t *testing.T) {
	if c := load(t, "# 没有 XCache 这一块\n"); len(c.Clients) != 0 {
		t.Errorf("没配就不该建缓存，got=%v", c.Clients)
	}
}

func TestValidate(t *testing.T) {
	ok := DefaultClientConfig()
	if err := ok.Validate(); err != nil {
		t.Errorf("默认配置应当合法：%v", err)
	}
	for name, mutate := range map[string]func(*ClientConfig){
		"NumCounters 为 0": func(c *ClientConfig) { c.NumCounters = 0 },
		"MaxCost 为 0":     func(c *ClientConfig) { c.MaxCost = 0 },
		"BufferItems 为 0": func(c *ClientConfig) { c.BufferItems = 0 },
		// ristretto 对 ttl<0 的写入直接丢弃，配成负数就一条都存不进去
		"DefaultTTL 为负": func(c *ClientConfig) { c.DefaultTTL = -time.Second },
	} {
		c := ok
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s 应当报错", name)
		}
	}
}

func TestNew_ReturnsNativeCache(t *testing.T) {
	cache, closer, err := New(DefaultClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	cache.SetWithTTL("k", 42, 1, time.Minute)
	cache.Wait() // 写入走环形缓冲异步生效，要确定性地读到就得先等
	if v, ok := cache.Get("k"); !ok || v != 42 {
		t.Errorf("应读到刚写的值，got=%v ok=%v", v, ok)
	}
}

func TestSet_UsesConfiguredDefaultTTL(t *testing.T) {
	// 包级 Set 的全部价值就在这里：不用每次都把 cost 和 TTL 写一遍
	withInstances(t, map[string]ClientConfig{DefaultName: func() ClientConfig {
		c := DefaultClientConfig()
		c.DefaultTTL = 90 * time.Second
		return c
	}()})

	Set("k", "v")
	C().Wait()

	ttl, ok := C().GetTTL("k")
	if !ok {
		t.Fatal("应能读到刚写的键")
	}
	if ttl > 90*time.Second || ttl < 80*time.Second {
		t.Errorf("应当用配置里的 TTL，got=%v", ttl)
	}
	if got := DefaultTTL(); got != 90*time.Second {
		t.Errorf("DefaultTTL 应返回配置值，got=%v", got)
	}
}

func TestGetSetDel(t *testing.T) {
	withInstances(t, map[string]ClientConfig{DefaultName: DefaultClientConfig()})

	Set("k", "v")
	C().Wait()
	if v, ok := Get[string]("k"); !ok || v != "v" {
		t.Errorf("应读到刚写的值，got=%v ok=%v", v, ok)
	}

	Del("k")
	C().Wait()
	if _, ok := Get[string]("k"); ok {
		t.Error("删掉之后不该还读得到")
	}
}

func TestGet_TypeMismatchIsMiss(t *testing.T) {
	withInstances(t, map[string]ClientConfig{DefaultName: DefaultClientConfig()})
	var b bytes.Buffer
	old := config.DebugOut
	config.DebugOut = &b
	t.Cleanup(func() { config.DebugOut = old })
	t.Setenv(config.DebugEnvKey, "1")

	type user struct{ Name string }
	Set("u", user{Name: "a"})
	C().Wait()

	if u, ok := Get[*user]("u"); ok || u != nil {
		t.Errorf("存的是 user、取的是 *user，该当作没命中，got=%v ok=%v", u, ok)
	}
	if !strings.Contains(b.String(), `"u"`) {
		t.Errorf("XONE_DEBUG 开着时该说明是类型对不上，got=%q", b.String())
	}
	if u, ok := Get[user]("u"); !ok || u.Name != "a" {
		t.Errorf("类型对得上该命中，got=%v ok=%v", u, ok)
	}
	if v, ok := Get[any]("u"); !ok || v.(user).Name != "a" {
		t.Errorf("Get[any] 什么都收，got=%v ok=%v", v, ok)
	}
}

func TestSetWithTTL(t *testing.T) {
	withInstances(t, map[string]ClientConfig{DefaultName: DefaultClientConfig()})

	SetWithTTL("k", "v", 10*time.Second)
	C().Wait()
	ttl, ok := C().GetTTL("k")
	if !ok || ttl > 10*time.Second {
		t.Errorf("应当用传进去的 TTL，got=%v ok=%v", ttl, ok)
	}
}

func TestC_PanicsWhenMissing(t *testing.T) {
	withInstances(t, nil)
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

func TestDefaultTTL_PanicsOnWrongNameInsteadOfNeverExpire(t *testing.T) {
	// 0 在 ristretto 里是「永不过期」：名字写错时静默拿到它，
	// 按这个 TTL 写进去的每一条就都不会过期了
	withInstances(t, map[string]ClientConfig{"session": DefaultClientConfig()})
	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "session") {
			t.Errorf("该和 C() 一样 panic 并列出已配置的实例名，got=%v", msg)
		}
	}()
	DefaultTTL("typo")
}

func TestSet_PanicsWithoutDefaultInstance(t *testing.T) {
	// 包级 Set 走的是另一条取实例的路径，别漏了这层保护
	withInstances(t, map[string]ClientConfig{"session": DefaultClientConfig()})
	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "session") {
			t.Errorf("应列出已配置的实例名，got=%v", msg)
		}
	}()
	Set("k", "v")
}

func TestHasNames(t *testing.T) {
	withInstances(t, map[string]ClientConfig{"session": DefaultClientConfig(), "page": DefaultClientConfig()})
	if !Has("page") || Has("nope") || Has() {
		t.Error("Has 应如实反映是否配过")
	}
	if got := Names(); len(got) != 2 || got[0] != "page" {
		t.Errorf("Names 应按名字排序，got=%v", got)
	}
}

func TestRegister_MatchesFramework(t *testing.T) {
	// 这是本包和框架之间唯一的一根线：钩子漏登记、档位挂错，
	// 表现是「配置不生效」或者「实例比用它的东西晚就绪」，别处都测不出来
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xcache" {
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
		if e.Pkg == "github.com/jupiter-eng/xone/xcache" {
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
	c := Config{Clients: map[string]ClientConfig{"a": DefaultClientConfig(), "b": DefaultClientConfig()}}

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
	bad := DefaultClientConfig()
	bad.MaxCost = -1
	cfg := Config{Clients: map[string]ClientConfig{"a": DefaultClientConfig(), "z": bad}}

	if err := initComponent(t, cfg); err == nil {
		t.Fatal("配置非法时应当报错")
	} else if !strings.Contains(err.Error(), `"z"`) {
		t.Errorf("错误里应点名是哪个实例，got=%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("失败时不该发布任何实例，got=%v", got)
	}
}

func TestInitAll_NoopWithoutConfig(t *testing.T) {
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

func TestNew_MaxCostIsEntryCount(t *testing.T) {
	// ristretto 默认把每条 56 字节的内部开销加进 cost，于是 cost=1 的写入
	// 实际占 57。不关掉的话 MaxCost=2000 只能存下三十几条，
	// 配置里写的数字和实际容量差着五十多倍，而且没有任何地方会提到
	c := DefaultClientConfig()
	c.MaxCost = 2000
	c.NumCounters = 20000
	cache, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	const n = 2000
	for i := range n {
		cache.SetWithTTL(strconv.Itoa(i), i, 1, time.Hour)
	}
	cache.Wait()

	live := 0
	for i := range n {
		if _, ok := cache.Get(strconv.Itoa(i)); ok {
			live++
		}
	}
	// 准入策略本来就会丢掉一部分，这里只要求同一个量级
	if live < n/2 {
		t.Errorf("MaxCost=%d、每条 cost=1，应该能装下接近 %d 条，实际只剩 %d 条", c.MaxCost, n, live)
	}
}

// initComponent 走一遍框架真正会走的路径：按这份配置把实例挨个建出来。
// 多实例的建法（排序、失败回滚、panic 隔离）全在那条路上。
func initComponent(t *testing.T, c Config) error {
	t.Helper()
	t.Cleanup(func() { _ = reg.Close() })
	return install(context.Background(), c)
}

func TestInitXCache_NoSectionBuildsNothing(t *testing.T) {
	// 本地缓存没有「默认给你开一个」的道理：没配就是不用。
	// 少了这道判断，每个进程都会白白吃下一份内存
	t.Cleanup(func() { _ = closeXCache(context.Background()) })
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")

	if err := initXCache(context.Background()); err != nil {
		t.Fatalf("没配不该报错：%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("不该建出实例，got=%v", got)
	}
}

func TestInitXCache_EmptySectionBuildsNothing(t *testing.T) {
	t.Cleanup(func() { _ = closeXCache(context.Background()) })
	xonetest.UseConfigYAML(t, "XCache:\n")

	if err := initXCache(context.Background()); err != nil {
		t.Fatalf("空块不该报错：%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("不该建出实例，got=%v", got)
	}
}

func TestInitXCache_BadConfigFailsStartup(t *testing.T) {
	// 拼错的字段被静默忽略的话，使用者会一直以为自己配上了
	t.Cleanup(func() { _ = closeXCache(context.Background()) })
	xonetest.UseConfigYAML(t, "XCache:\n  MaxCos: 500\n")

	if err := initXCache(context.Background()); err == nil {
		t.Fatal("字段拼错应当让启动失败")
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("失败时不该留下实例，got=%v", got)
	}
}

func TestInitXCache_BuildsFromConfigAndClosesCleanly(t *testing.T) {
	t.Cleanup(func() { _ = closeXCache(context.Background()) })
	xonetest.UseConfigYAML(t, "XCache:\n  Clients:\n    read:\n      MaxCost: 500\n    write:\n      MaxCost: 500\n")

	if err := initXCache(context.Background()); err != nil {
		t.Fatalf("应当建得起来：%v", err)
	}
	if got := Names(); len(got) != 2 {
		t.Fatalf("应有两个实例，got=%v", got)
	}

	if err := closeXCache(context.Background()); err != nil {
		t.Errorf("关闭不该报错：%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("关完还取得到实例，调用方会摸到一个已经关掉的缓存，got=%v", got)
	}
}

func TestCloseXCache_SafeWithoutBuild(t *testing.T) {
	// 本包压根没配时停止钩子照样会被调到，不能在这里炸
	if err := closeXCache(context.Background()); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

func TestClose_NoCrashWithConcurrentReadWrite(t *testing.T) {
	// ristretto v2.4.2 的 Close 先 close(setBuf)、最后才置 isClosed，
	// 于是和它并发的 Set / Del 会 send on closed channel；policy 的 itemsCh
	// 也是这么关的，并发的 Get 同样会炸。拿着原生 *Cache 的调用方
	// 我们拦不住，所以关闭这一步本身必须对并发读写是安全的。
	// 实测直接调 Close 时 100 轮里有 750 个协程 panic，并且 -race 报数据竞争。
	// 每轮都炸好几个，20 轮足够：变异「关闭时改调 ristretto 的 Close」连跑 5 次每次都被抓到。
	// 写协程有了 opsPerWorker 的上限之后，-race 单跑这一条约 2.4s（含编译）；
	// 变异「关闭时改调 ristretto 的 Close」照样每次都被抓到（连跑 3 次）
	const rounds, workers = 20, 8
	// 每个写协程最多写这么多次。ristretto 的 Clear 要等 setBuf 某一刻为空才退出
	// （v2.4.2 cache.go 的 drain 循环），8 个协程不停地写、而它们又只在 Close 返回之后才被叫停的话，
	// 空闲的多核机器上写得比它清得快，Close 永远不返回（实测单跑 3 次卡死 3 次，机器忙时反而能过）。
	// 写够这么多次就自己停：Close 一定等得到头，关闭期间和关完之后照样有并发的写
	const opsPerWorker = 5000
	small := DefaultClientConfig()
	small.NumCounters, small.MaxCost = 1000, 100 // 默认的 100 万个计数器每轮要分配 4MB，太慢
	var panics atomic.Int64
	for i := 0; i < rounds; i++ {
		cache, closer, err := New(small)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if recover() != nil {
						panics.Add(1)
					}
				}()
				for n := 0; n < opsPerWorker; n++ {
					select {
					case <-stop:
						return
					default:
					}
					k := strconv.Itoa(n % 64)
					cache.SetWithTTL(k, n, 1, time.Minute)
					cache.Get(k)
					if n%16 == 0 {
						cache.Del(k)
					}
				}
			}()
		}
		time.Sleep(200 * time.Microsecond)
		if err := closer.Close(); err != nil {
			t.Fatalf("关闭不该报错：%v", err)
		}
		time.Sleep(200 * time.Microsecond) // 关完之后还在写的也算
		close(stop)
		wg.Wait()
	}
	if n := panics.Load(); n > 0 {
		t.Fatalf("关闭时有并发读写，%d 个协程 panic 了", n)
	}
}

func TestClose_ReleasesEntries(t *testing.T) {
	// 关闭不走 ristretto 的 Close（理由见 New），但缓存里的东西必须放掉：
	// 否则一个停下来的实例还攥着 MaxCost 那么多条目直到进程退出
	cache, closer, err := New(DefaultClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	cache.Set("k", 1, 1)
	cache.Wait()
	if _, ok := cache.Get("k"); !ok {
		t.Fatal("前提：应能读到刚写的值")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Get("k"); ok {
		t.Error("关闭之后条目应当被清掉")
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
	bad.MaxCost = -1
	err := initComponent(t, Config{Clients: map[string]ClientConfig{"a": bad}})
	assertOneFrame(t, err, "xcache", "config")
	if !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("错误里要点名实例，got %v", err)
	}
}

func TestInitXCache_CSaysNotConfiguredRatherThanTooEarly(t *testing.T) {
	// 没配也要让注册表知道启动钩子跑过了。否则 C() 会把「没配」说成「调早了」，
	// 使用者会去查调用时机，而真正该查的是配置文件
	t.Cleanup(func() { _ = closeXCache(context.Background()) })
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXCache(context.Background()); err != nil {
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

// 便利写法每次都先经注册表取到默认实例，再交给 ristretto
func BenchmarkGet_Hit(b *testing.B) {
	withInstances(b, map[string]ClientConfig{DefaultName: DefaultClientConfig()})
	Set("k", "v")
	C().Wait()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Get[string]("k")
	}
}

func BenchmarkGet_Hit_Parallel(b *testing.B) {
	withInstances(b, map[string]ClientConfig{DefaultName: DefaultClientConfig()})
	Set("k", "v")
	C().Wait()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			Get[string]("k")
		}
	})
}

func BenchmarkSet_DefaultTTL(b *testing.B) {
	withInstances(b, map[string]ClientConfig{DefaultName: DefaultClientConfig()})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Set("k", "v")
	}
}

func TestConfig_InvalidValuesFailAtLoad(t *testing.T) {
	// 读配置时就拦下，报错里带着是哪个实例；不等到 New 才发现
	err := loadErr(t, "XCache:\n  Clients:\n    hot: {MaxCost: -1}\n")
	if err == nil {
		t.Fatal("MaxCost 为负应当在读配置时失败")
	}
	if !strings.Contains(err.Error(), "hot") || !strings.Contains(err.Error(), "MaxCost") {
		t.Errorf("错误里要点名实例和字段，got=%v", err)
	}
	if err := loadErr(t, "XCache:\n  DefaultTTL: -1s\n"); err == nil {
		t.Fatal("DefaultTTL 为负应当在读配置时失败")
	}
}

func TestNew_MetricFlagPassedToRistretto(t *testing.T) {
	// ristretto 默认不计数（Metrics 为 nil），不传下去的话 Metric: true 也什么都导不出来
	for _, on := range []bool{true, false} {
		c := DefaultClientConfig()
		c.Metric = on
		cache, closer, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		if got := cache.Metrics != nil; got != on {
			t.Errorf("Metric=%v 时 ristretto 的计数应为 %v，got=%v", on, on, got)
		}
		closer.Close()
	}
}

func TestCacheCollector_ExportsRistrettoCounters(t *testing.T) {
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	cfg := DefaultClientConfig()
	cfg.MaxCost = 1000
	cache, cc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	for i := 0; i < 3; i++ {
		cache.SetWithTTL("k"+strconv.Itoa(i), i, 1, 0)
	}
	cache.Wait()
	cache.SetWithTTL("k0", "new", 1, 0) // 更新已有的键
	cache.Wait()
	cache.Get("k0")
	cache.Get("k1")
	cache.Get("nope")
	cache.Del("k2") // 显式删除，ristretto 记在 keys_evicted 里
	cache.Wait()

	m.Registry.MustRegister(newCacheCollector("demo", nil, func() map[string]*Cache { return map[string]*Cache{"hot": cache} }))
	out := testkit.Scrape(m.Handler)
	for _, want := range []string{
		`demo_cache_hits_total{name="hot"} 2`,
		`demo_cache_misses_total{name="hot"} 1`,
		`demo_cache_keys_added_total{name="hot"} 3`,
		`demo_cache_keys_updated_total{name="hot"} 1`,
		`demo_cache_keys_evicted_total{name="hot"} 1`,
		`demo_cache_sets_dropped_total{name="hot"} 0`,
		`demo_cache_sets_rejected_total{name="hot"} 0`,
		`demo_cache_cost{name="hot"} 2`,
		`demo_cache_max_cost{name="hot"} 1000`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("导出里应有 %s\n实际=\n%s", want, out)
		}
	}
}

func TestCacheCollector_ExpiredCountsAsKeysEvicted(t *testing.T) {
	// 文档里写着 keys_evicted 包括 TTL 到期的，这里钉住：ristretto 升级后行为变了先红
	cache, cc, err := New(DefaultClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	cache.SetWithTTL("k", 1, 1, 50*time.Millisecond)
	cache.Wait()
	// ristretto 的过期清理按桶走（v2.4.2 桶宽 5s、每 2.5s 扫一轮），等到它扫过为止
	deadline := time.Now().Add(10 * time.Second)
	for cache.Metrics.KeysEvicted() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := cache.Metrics.KeysEvicted(); got != 1 {
		t.Errorf("到期被清掉的键应算进 keys_evicted，got=%d", got)
	}
}

func TestInstall_ExportsOnlyMetricEnabled_SurvivesXmetricReinstall(t *testing.T) {
	// collector 是进程级的一个，抓取时遍历全部实例：不看实例自己的开关，
	// Metric: false 就是一句空话。第二轮是同一进程里再走一遍生命周期——
	// xmetric 换了新的 Registry，collector 得跟着挂上去，否则指标全丢
	on, off := DefaultClientConfig(), DefaultClientConfig()
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
		if !strings.Contains(out, `cache_hits_total{name="on"}`) {
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

func TestInstall_LogsCarryInstanceName(t *testing.T) {
	// 配了好几个缓存时，只写参数分不出是哪一个
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	hot := DefaultClientConfig()
	hot.MaxCost = 123
	if err := initComponent(t, Config{Clients: map[string]ClientConfig{"hot": hot}}); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec["msg"] != "xcache created" {
			continue
		}
		found = true
		if rec["name"] != "hot" || rec["max_cost"] != float64(123) {
			t.Errorf("日志要写出实例名和它的参数，got=%v", rec)
		}
	}
	if !found {
		t.Errorf("每个实例建好时该有一条 xcache created\n实际=\n%s", buf.String())
	}
}
