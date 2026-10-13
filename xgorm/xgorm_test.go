package xgorm

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xonetest"
)

// TestMain 调短重试间隔：连不上的用例要跑满整轮重试，按一秒算一次就是几十秒
func TestMain(m *testing.M) {
	pingInterval = 10 * time.Millisecond
	os.Exit(m.Run())
}

// deadAddr 返回一个没人监听的地址：建连必定失败，且失败得很快
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // 立刻关掉，端口就没人监听了
	return addr
}

func TestNew_NoGoroutineLeakWhenUnreachable(t *testing.T) {
	// New 失败之后不该留下活着的连接池：database/sql 的 opener 协程
	// 只在 Close 时退出，漏一个就是一个再也不会走的常驻协程。
	//
	// gorm v1.31 起会在自己的失败路径上把池子关掉，所以这条现在主要盯的是
	// gorm.Open 之后那几步——设连接池参数、Ping、挂链路——出错时的收尾。
	c := DefaultClientConfig()
	c.Driver = DriverMySQL
	c.DSN = "u:p@tcp(" + deadAddr(t) + ")/app"
	c.DialTimeout = 50 * time.Millisecond
	c.MySQL.ReadTimeout = 50 * time.Millisecond

	const rounds = 5
	before := testkit.Stabilize()
	for i := 0; i < rounds; i++ {
		db, closer, err := New(context.Background(), c)
		if err == nil {
			reg.Close()
			t.Fatal("连不上时应当报错")
		}
		if db != nil || closer != nil {
			t.Error("失败时不该返回半成品")
		}
	}

	// 必须等协程真正退出再数：连接池关闭后它的 opener 协程是异步退出的，
	// 立刻去数会把「正在退出」当成「泄漏」，也会把真泄漏淹没在噪声里
	if after := testkit.SettleTo(before); after > before+1 {
		t.Errorf("建连失败 %d 次后协程数从 %d 涨到 %d，说明连接池没被关掉", rounds, before, after)
	}
}

func TestNew_ErrorHasAddrButNoPassword(t *testing.T) {
	addr := deadAddr(t)
	c := DefaultClientConfig()
	c.Driver = DriverMySQL
	c.DSN = "u:" + secret + "@tcp(" + addr + ")/app"
	c.DialTimeout = 50 * time.Millisecond
	c.MySQL.ReadTimeout = 50 * time.Millisecond

	_, _, err := New(context.Background(), c)
	if err == nil {
		t.Fatal("连不上时应当报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误信息里出现了密码：%v", err)
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("错误信息里应有地址，否则排查不了连的是谁：%v", err)
	}
}

func TestNew_NoConnectOnBadConfig(t *testing.T) {
	c := DefaultClientConfig() // 没有 DSN
	_, _, err := New(context.Background(), c)
	if err == nil {
		t.Fatal("DSN 为空应当报错")
	}
	if !strings.Contains(err.Error(), "DSN") {
		t.Errorf("错误该说清楚是哪儿的问题，got=%v", err)
	}
}

func TestProbeTimeout(t *testing.T) {
	// 不能只用建连超时：探测是建连加一个往返，
	// 拿建连预算当整体预算，连接刚建成就会被判超时
	c := mysqlCfg("u:p@tcp(h:3306)/d")
	c.DialTimeout = time.Second
	c.MySQL.ReadTimeout = 2 * time.Second
	if got := resolvedProbeTimeout(t, c); got != 3*time.Second {
		t.Errorf("MySQL 的探测预算应为建连 + 读超时，got=%v", got)
	}

	// 配 0 是「驱动不限时」，不是「这一段不花时间」：那一段按另一段的量级给，
	// 和方言推算不出来时的 2 × DialTimeout 同一个道理。只给建连超时的话，
	// ReadTimeout: 0 时连接刚建成、登录包还没回来就被判超时
	for _, z := range []struct {
		name       string
		dial, read time.Duration
		want       time.Duration
	}{
		{"ReadTimeout 为 0", time.Second, 0, 2 * time.Second},
		{"DialTimeout 为 0", 0, 2 * time.Second, 4 * time.Second},
		{"两个都是 0 用兜底值", 0, 0, fallbackPingTimeout},
	} {
		c := mysqlCfg("u:p@tcp(h:3306)/d")
		c.DialTimeout, c.MySQL.ReadTimeout = z.dial, z.read
		if got := resolvedProbeTimeout(t, c); got != z.want {
			t.Errorf("%s：MySQL 的探测预算 want %v，got %v", z.name, z.want, got)
		}
	}

	// PG 注入的 connect_timeout 是向上取整的整秒，管的是整个建连（TCP、TLS、认证）。
	// 预算比它短的话，一次慢一点但合法的握手会在 pgx 放弃之前就被我们判超时
	c = pgCfg("host=h dbname=d")
	c.DialTimeout = 500 * time.Millisecond
	if got := resolvedProbeTimeout(t, c); got != time.Second+500*time.Millisecond {
		t.Errorf("PG 的预算要盖住注入的 connect_timeout（1s）再加一个往返，got=%v", got)
	}

	// 方言推算不出来的（没有 Resolve），按 2 × DialTimeout；DialTimeout 也是 0 就用兜底值
	c = DefaultClientConfig()
	c.DialTimeout = 300 * time.Millisecond
	if got := probeTimeout(c, ConnInfo{}); got != 600*time.Millisecond {
		t.Errorf("方言没给预算时应是 2 × DialTimeout，got=%v", got)
	}
	c.DialTimeout = 0
	if got := probeTimeout(c, ConnInfo{}); got != fallbackPingTimeout {
		t.Errorf("推算不出预算时该用兜底值，got=%v", got)
	}
	if got := probeTimeout(c, ConnInfo{ProbeTimeout: 7 * time.Second}); got != 7*time.Second {
		t.Errorf("方言给了预算就用它，got=%v", got)
	}
}

// resolvedProbeTimeout 走一遍方言的 Resolve，取 New 真正会用的那个预算
func resolvedProbeTimeout(t *testing.T, c ClientConfig) time.Duration {
	t.Helper()
	_, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	return probeTimeout(c, info)
}

func TestProbeTimeout_DSNLongerTimeoutWidensBudget(t *testing.T) {
	// 配置里的超时只是注入 DSN 的默认值。DSN 里写了更长的，驱动就会等那么久，
	// 预算还按配置算的话，一次慢但合法的建连会在驱动放弃之前被我们判超时
	for _, c := range []struct {
		name string
		cfg  ClientConfig
		min  time.Duration
	}{
		{"PG connect_timeout", pgCfg("host=h dbname=d connect_timeout=10"), 10 * time.Second},
		{"MySQL timeout", mysqlCfg("u:p@tcp(h:3306)/d?timeout=10s"), 10 * time.Second},
		{"MySQL readTimeout", mysqlCfg("u:p@tcp(h:3306)/d?readTimeout=10s"), 10 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := resolvedProbeTimeout(t, c.cfg); got < c.min {
				t.Errorf("预算要盖住 DSN 里写的 %v，got=%v", c.min, got)
			}
		})
	}
}

func TestProbe_FailsAfterRetries(t *testing.T) {
	// 重试要真的重试，也要在预算内结束——启动期卡死比连不上更难查
	pool, err := sql.Open("mysql", "u:p@tcp("+deadAddr(t)+")/app?timeout=30ms")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	c := mysqlCfg("u:p@tcp(h:1)/app")
	c.DialTimeout = 30 * time.Millisecond
	c.MySQL.ReadTimeout = 30 * time.Millisecond
	_, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	policy := probePolicy(c, info, mysqlDialect())

	start := time.Now()
	if err := xclient.Probe(context.Background(), policy, probe(pool, nil)); err == nil {
		t.Fatal("连不上时应当返回错误")
	}
	// 只盯上界。退避带抖动之后每次等待是 [0, 当前退避] 之间的随机值，
	// 可以短到接近零，所以「至少等了多久」不再是重试次数的有效代理
	// 退避的等待上界是 interval + 2*interval = 3*interval，留到 4 倍够宽
	ceiling := pingAttempts*policy.Timeout + 4*pingInterval + 2*time.Second
	if elapsed := time.Since(start); elapsed > ceiling {
		t.Errorf("重试超出了预算，用了 %v", elapsed)
	}
}

// ---- 全局实例 ----

func withClients(t *testing.T, m map[string]*gorm.DB) {
	t.Helper()
	insts := make(map[string]instance, len(m))
	for name, db := range m {
		insts[name] = instance{db: db, metric: true}
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

func TestC_PanicsWhenMissing(t *testing.T) {
	// 返回 nil 只是把同一个 panic 推迟到调用方第一次用它的时候，
	// 那里的栈里只剩 invalid memory address，看不出根因是配置没配
	withClients(t, map[string]*gorm.DB{})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("取不到实例应当 panic")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, ConfigKey) {
			t.Errorf("一个都没配时该提示去看配置块，got=%v", msg)
		}
	}()
	C()
}

func TestC_PanicListsConfiguredInstances(t *testing.T) {
	// 名字写错和整块没配是两个不同的问题，列出实际配了哪些，两者一眼可分
	withClients(t, map[string]*gorm.DB{"main": {}, "report": {}})

	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "main") || !strings.Contains(msg, "report") {
			t.Errorf("应列出已配置的实例名，got=%v", msg)
		}
	}()
	C("typo")
}

func TestHasNames(t *testing.T) {
	withClients(t, map[string]*gorm.DB{"report": {}, "main": {}})

	if !Has("main") || Has("nope") {
		t.Error("Has 应如实反映是否配过")
	}
	if got := Names(); len(got) != 2 || got[0] != "main" || got[1] != "report" {
		t.Errorf("Names 应按名字排序，got=%v", got)
	}
	if Has() {
		t.Error("没有 default 时 Has() 应为 false")
	}
}

func TestRegister_MatchesFramework(t *testing.T) {
	// 这是本包和框架之间唯一的一根线：钩子漏登记、档位挂错，
	// 表现是「配置不生效」或者「实例比用它的东西晚就绪」，别处都测不出来
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xgorm" {
			got = &e
			break
		}
	}
	if got == nil {
		t.Fatal("没有登记启动钩子")
	}
	if got.Stage != hook.StageClient {
		t.Errorf("数据库要在业务之前就绪（StageClient），got=%v", got.Stage)
	}

	var stopped bool
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xgorm" {
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

func TestInitAll_NoopWithoutConfig(t *testing.T) {
	err := initComponent(t, DefaultConfig())
	if err != nil {
		t.Fatalf("没配 XGorm 不该报错：%v", err)
	}
	if err := reg.Close(); err != nil {
		t.Errorf("关闭不该报错：%v", err)
	}
	if len(Names()) != 0 {
		t.Errorf("不该建出实例，got=%v", Names())
	}
}

func TestInitAll_OneFailureRollsBackAll(t *testing.T) {
	// Init 返回错误时框架拿不到 closer，已经建好的必须自己收拾，否则漏连接池
	bad := DefaultClientConfig()
	bad.Driver, bad.DSN = DriverMySQL, "u:p@tcp("+deadAddr(t)+")/app"
	bad.DialTimeout, bad.MySQL.ReadTimeout = 30*time.Millisecond, 30*time.Millisecond
	cfg := Config{Clients: map[string]ClientConfig{"a": bad, "b": bad}}

	before := testkit.Stabilize()
	err := initComponent(t, cfg)
	if err == nil {
		t.Fatal("连不上时应当报错")
	}
	if !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("错误里应点名是哪个实例，got=%v", err)
	}
	if after := testkit.SettleTo(before); after > before+1 {
		t.Errorf("回滚不干净，协程数从 %d 涨到 %d", before, after)
	}
}

func TestInstallPoolMetrics_RegistersOnceWhenCalledTwice(t *testing.T) {
	// 每个开了指标的实例都会调它一次，而 collector 是进程级的一个：
	// 第二次注册 Prometheus 会报 duplicate，这套指标从此一个都导不出去
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	installPoolMetrics()
	installPoolMetrics()

	if strings.Contains(buf.String(), "failed to register") {
		t.Errorf("挂第二次应当复用已有的那个，而不是让 Prometheus 报重复注册\n实际=\n%s", buf.String())
	}
}

func TestCWithCtx(t *testing.T) {
	// GORM 必须这样传 ctx，不像 go-redis 每个方法都收 ctx
	withClients(t, map[string]*gorm.DB{DefaultName: {Config: &gorm.Config{}, Statement: &gorm.Statement{}}})
	db := CWithCtx(context.Background())
	if db == nil {
		t.Fatal("应返回实例")
	}
}

func TestSettle_DetectsLeakedPool(t *testing.T) {
	// 先验证这把尺子是准的：上一版读数没等协程退出、阈值又放到 +2，
	// 于是「不漏协程」那条测试怎么改都通过——一条永远不会失败的测试
	// 比没有测试更糟，它让人以为查过了。
	addr := deadAddr(t)
	before := testkit.Stabilize()

	const leaked = 5
	pools := make([]*sql.DB, 0, leaked)
	for i := 0; i < leaked; i++ {
		pool, err := sql.Open("mysql", "u:p@tcp("+addr+")/app")
		if err != nil {
			t.Fatal(err)
		}
		pool.SetMaxIdleConns(1)
		// 碰一下让 opener 协程真正起来
		pool.PingContext(context.Background())
		pools = append(pools, pool)
	}

	// 判「涨了没有」用 ClimbTo：SettleTo 的判据是「回落到 target 以内」，
	// 漏着的池子永远回落不了，它必然烧满整个 10 秒才返回。每个池子至少一个 opener 协程
	if during := testkit.ClimbTo(before+leaked, 3*time.Second); during < before+leaked {
		t.Fatalf("漏了 %d 个连接池却没看出协程增长（%d -> %d），这把尺子是坏的", leaked, before, during)
	}
	for _, p := range pools {
		p.Close()
	}
	if after := testkit.SettleTo(before); after > before+1 {
		t.Errorf("全关掉之后应当回落，got %d -> %d", before, after)
	}
}

func TestNew_CtxCanceledAbortsFirstConnect(t *testing.T) {
	// GORM 自带的那次 ping 用的是它自己的 context，我们的退出信号管不到。
	// 开着的话，连一个不可达地址时这里会先干等满 DSN 的 connect_timeout
	c := DefaultClientConfig()
	c.DSN = "postgres://u:p@10.255.255.1:5432/db" // 黑洞地址，连接会一直挂着
	c.DialTimeout = 3 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, _, err := New(ctx, c)
	if err == nil {
		t.Fatal("ctx 已取消时不该建连成功")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("该当场放弃，实际等了 %v（说明首次建连没走我们的 ctx）", elapsed)
	}
}

func TestNew_LogOffKeepsSQLErrorsOffStdout(t *testing.T) {
	// 不给 gormCfg.Logger 的话，gorm.Open 会补上 logger.Default，
	// 而那个默认实现是「带 ANSI 颜色地往 os.Stdout 写」：
	// 慢 SQL 和执行错误照样打，只是绕开了 slog——没有级别、没有 TraceID、
	// 不是 JSON，一行彩色文本直接插进日志流里。
	// Log=false 要的是不打，不是换个地方打
	var leaked bytes.Buffer
	old := logger.Default
	logger.Default = logger.New(log.New(&leaked, "", 0), logger.Config{LogLevel: logger.Info})
	t.Cleanup(func() { logger.Default = old })

	sentinel := errors.New("stub dialector was used")
	withDialect(t, Dialect{
		Name: "logprobe",
		Open: func(string) gorm.Dialector {
			return loggingDialector{err: sentinel}
		},
	})

	c := DefaultClientConfig()
	c.Driver, c.DSN, c.Log = "logprobe", "logprobe://h:1/d", false
	if _, _, err := New(context.Background(), c); !errors.Is(err, sentinel) {
		t.Fatalf("应该用注册进来的 Open，got=%v", err)
	}
	if leaked.Len() > 0 {
		t.Errorf("Log=false 时 GORM 不该往它自己的默认输出写，实际写了: %q", leaked.String())
	}
}

// loggingDialector 在 Initialize 里用 GORM 解出来的那个 Logger 写一条，
// 借此看清 Log=false 时最终生效的到底是谁
type loggingDialector struct{ err error }

func (d loggingDialector) Initialize(db *gorm.DB) error {
	db.Logger.Error(context.Background(), "probe: a query failed")
	return d.err
}
func (d loggingDialector) Name() string                    { return "logprobe" }
func (d loggingDialector) Migrator(*gorm.DB) gorm.Migrator { return nil }
func (d loggingDialector) DataTypeOf(*schema.Field) string { return "" }
func (d loggingDialector) DefaultValueOf(*schema.Field) clause.Expression {
	return clause.Expr{}
}
func (d loggingDialector) BindVarTo(clause.Writer, *gorm.Statement, any) {}
func (d loggingDialector) QuoteTo(clause.Writer, string)                 {}
func (d loggingDialector) Explain(sql string, _ ...any) string           { return sql }

func TestResolveDSN_PGParamNameInPasswordNotMistaken(t *testing.T) {
	// 曾经用正则扫 key= 判断写没写过，于是密码里出现 connect_timeout= 就能骗过它，
	// DialTimeout 这项配置悄悄失效——没有任何迹象
	cases := []struct {
		name, dsn string
		want      bool // 期望注入 connect_timeout
	}{
		{"密码里含参数名", "host=h dbname=d password='a connect_timeout=99 b'", true},
		{"普通密码", "host=h dbname=d password=plain", true},
		{"真的配过了", "host=h dbname=d connect_timeout=9", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := resolveDSN(pgCfg(c.dsn))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := pgconn.ParseConfig(got)
			if err != nil {
				t.Fatal(err)
			}
			injected := cfg.ConnectTimeout == time.Second
			if injected != c.want {
				t.Errorf("注入=%v want=%v\n入：%s\n出：%s", injected, c.want, c.dsn, got)
			}
		})
	}
}

// initComponent 走一遍框架真正会走的路径：按这份配置把实例挨个建出来。
// 多实例的建法（排序、失败回滚、panic 隔离）全在那条路上。
func initComponent(t *testing.T, c Config) error {
	t.Helper()
	t.Cleanup(func() { _ = reg.Close() })
	return install(context.Background(), c)
}

func TestInitXGorm_NoSectionConnectsNothing(t *testing.T) {
	// xgorm 是可选依赖：没配不该让服务起不来，更不该去连一个默认地址
	t.Cleanup(func() { _ = closeXGorm(context.Background()) })
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")

	if err := initXGorm(context.Background()); err != nil {
		t.Fatalf("没配不该报错：%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("不该建出实例，got=%v", got)
	}
}

func TestInitXGorm_EmptySectionConnectsNothing(t *testing.T) {
	t.Cleanup(func() { _ = closeXGorm(context.Background()) })
	xonetest.UseConfigYAML(t, "XGorm:\n")

	if err := initXGorm(context.Background()); err != nil {
		t.Fatalf("空块不该报错：%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("不该建出实例，got=%v", got)
	}
}

func TestInitXGorm_BadConfigFailsStartup(t *testing.T) {
	t.Cleanup(func() { _ = closeXGorm(context.Background()) })
	xonetest.UseConfigYAML(t, "XGorm:\n  DS: \"host=127.0.0.1\"\n")

	if err := initXGorm(context.Background()); err == nil {
		t.Fatal("字段拼错应当让启动失败，否则使用者会一直以为自己配上了")
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("失败时不该留下实例，got=%v", got)
	}
}

func TestInitXGorm_ConfigAppliedToInstance(t *testing.T) {
	// 连不上是预期的——这里要的是错误里出现配置文件里写的那个实例名和地址，
	// 证明这一段配置确实走到了建连那一步，而不是在哪里被丢掉了
	t.Cleanup(func() { _ = closeXGorm(context.Background()) })
	addr := deadAddr(t)
	xonetest.UseConfigYAML(t, "XGorm:\n  Clients:\n    report:\n      Driver: mysql\n      DialTimeout: 30ms\n      DSN: \"u:p@tcp("+addr+")/app\"\n")

	err := initXGorm(context.Background())
	if err == nil {
		t.Fatal("连不上应当报错")
	}
	if !strings.Contains(err.Error(), `"report"`) {
		t.Errorf("错误里要点名是哪个实例，got=%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("失败时不该留下实例，got=%v", got)
	}
}

func TestCloseXGorm_SafeWithoutBuild(t *testing.T) {
	if err := closeXGorm(context.Background()); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

func TestPoolCloser_ClosesUnderlyingPool(t *testing.T) {
	// Closer 直接持有 *sql.DB，不绕回 gorm.DB.DB()：后者在实例已经出问题时
	// 会返回错误，于是连接池就再也关不掉了
	pool, err := sql.Open("mysql", "u:p@tcp(127.0.0.1:1)/app")
	if err != nil {
		t.Fatal(err)
	}
	c := &poolCloser{pool: pool, info: ConnInfo{Addr: "127.0.0.1:1"}}

	if err := c.Close(); err != nil {
		t.Fatalf("关闭不该报错：%v", err)
	}
	if err := pool.PingContext(context.Background()); err == nil {
		t.Error("底层连接池没被关掉")
	}
}

func TestPoolCloser_CloseErrorHasAddrButNoPassword(t *testing.T) {
	// 出错信息会进日志和告警，凭证不能跟着出去
	pool, err := sql.Open("mysql", "u:p@tcp(127.0.0.1:1)/app")
	if err != nil {
		t.Fatal(err)
	}
	c := &poolCloser{pool: pool, info: ConnInfo{Addr: "127.0.0.1:1"}}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// database/sql 的 Close 是幂等的，构造不出真实的失败；
	// 这里直接确认文案模板里带的是地址而不是 DSN
	msg := xerror.Newf("xgorm", "close", "close %s failed: %w", c.info.Addr, errors.New("boom")).Error()
	if !strings.Contains(msg, "127.0.0.1:1") {
		t.Errorf("错误里要有地址才知道是哪个库，got=%v", msg)
	}
	if strings.Contains(msg, "u:p@") {
		t.Errorf("凭证不该出现在错误里，got=%v", msg)
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

func TestNew_ErrorWrappedOnceWithAccurateOp(t *testing.T) {
	c := DefaultClientConfig()
	assertOneFrame(t, func() error { _, _, err := New(context.Background(), c); return err }(), "xgorm", "config")

	c.Driver, c.DSN = DriverMySQL, "u:p@tcp(127.0.0.1:1/app" // 括号没闭合，驱动解析不了
	assertOneFrame(t, func() error { _, _, err := New(context.Background(), c); return err }(), "xgorm", "config")
}

func TestInstall_ErrorWrappedOnceWithAccurateOp(t *testing.T) {
	// 经 Build 出来的错误要点名实例，但不能再套一层 new 把 config / connect 压下去
	bad := DefaultClientConfig()
	bad.Driver, bad.DSN = DriverMySQL, "u:p@tcp(127.0.0.1:1/app"
	err := initComponent(t, Config{Clients: map[string]ClientConfig{"a": bad}})
	assertOneFrame(t, err, "xgorm", "config")

	dead := DefaultClientConfig()
	dead.Driver, dead.DSN = DriverMySQL, "u:p@tcp("+deadAddr(t)+")/app"
	dead.DialTimeout, dead.MySQL.ReadTimeout = 30*time.Millisecond, 30*time.Millisecond
	err = initComponent(t, Config{Clients: map[string]ClientConfig{"b": dead}})
	assertOneFrame(t, err, "xgorm", "connect")
	if !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("错误里要点名实例，got %v", err)
	}
}

func TestInitXGorm_CSaysNotConfiguredRatherThanTooEarly(t *testing.T) {
	// 没配也要让注册表知道启动钩子跑过了。否则 C() 会把「没配」说成「调早了」，
	// 使用者会去查调用时机，而真正该查的是配置文件
	t.Cleanup(func() { _ = closeXGorm(context.Background()) })
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXGorm(context.Background()); err != nil {
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

func TestInstall_ConnectLogNamesInstance(t *testing.T) {
	// 多实例时 addr / db 分不出是哪一个（report 和 default 可能连的是同一个库）
	withDialect(t, Dialect{Name: "namedb", Open: func(string) gorm.Dialector { return okDialector{} }})
	c := DefaultClientConfig()
	c.Driver, c.DSN = "namedb", "namedb://h/d"

	lines := capture(t)
	if err := install(context.Background(), Config{Clients: map[string]ClientConfig{"report": c}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	var connected, ready map[string]any
	for _, l := range lines() {
		switch l["msg"] {
		case "xgorm connected":
			connected = l
		case "xgorm ready":
			ready = l
		}
	}
	if connected["name"] != "report" {
		t.Errorf("xgorm connected 应写上实例名 report，got=%v", connected)
	}
	if fmt.Sprint(ready["instances"]) != "[report]" {
		t.Errorf("xgorm ready 应列出实例名，got=%v", ready)
	}

	// 直接调 New 的没有名字，不写一个空的 name
	lines = capture(t)
	_, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	closer.Close()
	for _, l := range lines() {
		if _, ok := l["name"]; ok && l["msg"] == "xgorm connected" {
			t.Errorf("直接调 New 时不该有 name 字段，got=%v", l)
		}
	}
}

func TestNew_DialectDecidesAuthFailureRecognition(t *testing.T) {
	// 认证失败的识别挪进了各自的方言：没提供 AuthFailed 的方言，
	// 同一个 28P01 照常重试、报连不上——核心里不再有按驱动名分支的判断
	rejected := &pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: "password authentication failed"}
	calls := 0
	withDialect(t, Dialect{
		Name:  "noauthdb",
		Open:  func(string) gorm.Dialector { return okDialector{} },
		Ready: func(context.Context, *gorm.DB) error { calls++; return rejected },
	})
	c := DefaultClientConfig()
	c.Driver, c.DSN = "noauthdb", "noauthdb://h/d"
	_, _, err := New(context.Background(), c)
	if calls != pingAttempts || !strings.Contains(fmt.Sprint(err), "cannot reach ") {
		t.Errorf("方言认不出就照常重试，calls=%d err=%v", calls, err)
	}
}

func TestNew_GormOptionsReachGorm(t *testing.T) {
	// 这几项原样交给 gorm.Config：不配是 GORM 的默认，配了就是配的那个值
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprint(on), func(t *testing.T) {
			var got *gorm.Config
			withDialect(t, Dialect{
				Name: "cfgprobe",
				Open: func(string) gorm.Dialector { return configDialector{seen: &got} },
			})
			c := DefaultClientConfig()
			c.Driver, c.DSN = "cfgprobe", "cfgprobe://h:1/d"
			c.DisableForeignKeyConstraintWhenMigrating, c.SkipDefaultTransaction, c.PrepareStmt = on, on, on
			if on {
				c.CreateBatchSize = 7
			}
			New(context.Background(), c)
			if got == nil {
				t.Fatal("dialector 没被调到")
			}
			if got.DisableForeignKeyConstraintWhenMigrating != on || got.SkipDefaultTransaction != on ||
				got.PrepareStmt != on || got.CreateBatchSize != c.CreateBatchSize {
				t.Errorf("配置该原样交给 GORM：DisableForeignKeyConstraintWhenMigrating=%v SkipDefaultTransaction=%v PrepareStmt=%v CreateBatchSize=%d，want %v / %v / %v / %d",
					got.DisableForeignKeyConstraintWhenMigrating, got.SkipDefaultTransaction, got.PrepareStmt, got.CreateBatchSize,
					on, on, on, c.CreateBatchSize)
			}
		})
	}
}

// configDialector 在 Initialize 里记下 GORM 最终拿到的配置，然后让 Open 失败
type configDialector struct {
	loggingDialector
	seen **gorm.Config
}

func (d configDialector) Initialize(db *gorm.DB) error {
	cp := *db.Config
	*d.seen = &cp
	return errors.New("stop after recording config")
}
