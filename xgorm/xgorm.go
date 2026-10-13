package xgorm

import (
	"cmp"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
	"github.com/jupiter-eng/xone/xmetric"

	// 用了 xgorm 就有链路：xtrace 装好全局的 TracerProvider，SQL 的 Span 经 otel 的全局 API 记下来。
	// 不要链路配 XTrace.Enable: false，只关某个实例的配它自己的 Trace: false
	_ "github.com/jupiter-eng/xone/xtrace"
)

const (
	// pingAttempts 建连验证的尝试次数
	pingAttempts = 3

	// fallbackPingTimeout 配置里推算不出预算时，单次探测的兜底超时
	fallbackPingTimeout = time.Second
)

// pingInterval 第一次退避的上界，之后逐次翻倍（xutil.Retry），3 次尝试之间的两次
// 退避上界是 1s、2s。这个数写在 docs/behavior.md「启动期建连探测」里，启动预算靠它推算。
//
// 是变量而不是常量，只为让测试能调短——连不上的用例要跑满整轮重试，
// 按一秒算一次就是几十秒。
var pingInterval = time.Second

// New 按配置建一个 GORM 实例，不触碰任何全局变量。
//
// ctx 限定建连验证的生命期：地址不通时这里要走满一轮探测重试，
// 收到退出信号就该当场放弃，而不是让进程卡在一个注定连不上的库上。
//
// 返回的 io.Closer 关闭底层连接池。建连失败时不会留下连接池。
func New(ctx context.Context, cfg ClientConfig) (*gorm.DB, io.Closer, error) {
	return open(ctx, "", cfg)
}

// open 就是 New，多一个实例名：框架按名字建实例时，建连日志里要写上是哪一个
func open(ctx context.Context, name string, cfg ClientConfig) (*gorm.DB, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, xerror.Newf("xgorm", "config", "invalid config: %w", err)
	}

	dialect, _ := lookupDialect(cfg.Driver) // Validate 已经确认它注册过
	dsn, info, err := dialect.resolve(cfg)
	if err != nil {
		return nil, nil, xerror.New("xgorm", "config", err)
	}

	tlsCfg, err := cfg.TLS.Build()
	if err != nil {
		return nil, nil, xerror.Newf("xgorm", "config", "invalid TLS config: %w", err)
	}

	// Logger 要看它的占位符长什么样，所以先造出来
	dialector, err := dialect.dialector(dsn, tlsCfg)
	if err != nil {
		return nil, nil, xerror.New("xgorm", "config", err)
	}

	// 关掉 GORM 自带的那次 ping：它用的是自己的 context，我们的退出信号
	// 和重试都管不到它。开着的话，连一个不可达的地址时 New 会先在里面
	// 干等满 DSN 的 connect_timeout，哪怕 ctx 早就被取消了（实测 3 秒）。
	// 关掉之后 gorm.Open 只做装配、立刻返回，全部建连都走下面那次
	// ctx-aware 的探测 —— 取消得了、也重试得了。
	//
	// 内置的 MySQL 和 ClickHouse 方言在 Initialize 里各有一次查版本，也会建连，
	// 两者都在 Open 里关掉、挪进了 Dialect.Ready（见 openMySQL）。
	//
	// SkipDefaultTransaction、PrepareStmt、CreateBatchSize 交给配置（默认值和 GORM 一样），
	// 其余几项 GORM 的默认值原样保留，量过，理由见 xgorm/README.md「通用」：
	// NowFunc 用本地时间、TranslateError=false。
	gormCfg := &gorm.Config{
		DisableAutomaticPing:                     true,
		DisableForeignKeyConstraintWhenMigrating: cfg.DisableForeignKeyConstraintWhenMigrating,
		SkipDefaultTransaction:                   cfg.SkipDefaultTransaction,
		PrepareStmt:                              cfg.PrepareStmt,
		CreateBatchSize:                          cfg.CreateBatchSize,
	}
	if cfg.Log {
		gormCfg.Logger = newGormLogger(cfg, dialect, dialector)
	} else {
		// 不给 Logger 的话 GORM 会补上自己的默认实现，而那个默认实现
		// 是「带 ANSI 颜色地往 os.Stdout 写」：慢 SQL 和执行错误照样打，
		// 只是绕开了 slog——没有级别、没有 TraceID、不是 JSON，
		// 一行彩色文本直接插进日志流里。Log=false 要的是不打，不是换个地方打
		gormCfg.Logger = logger.Discard
	}

	db, err := gorm.Open(dialector, gormCfg)
	if err != nil {
		// 不用替它收拾：gorm.Open 自己失败时会把它建的池子关掉
		// （gorm v1.31.2 gorm.go 的 Open，Initialize 失败与自动 ping 失败两条路都关）
		//
		// 内置方言走不到这里的认证失败：它们的 Open 不碰网络。注册进来的方言要是
		// 在 Initialize 里建连，密码错就出在这里，照样要说清是认证失败
		return nil, nil, connectError(dialect, "open %s failed: %w", info.Addr, err)
	}
	pool, err := db.DB()
	if err != nil {
		return nil, nil, xerror.Newf("xgorm", "connect", "get underlying pool: %w", err)
	}

	// 从这里往后的每一步都是我们自己的，失败了没人替我们收拾：
	// 连接池已经活着，不关就漏一个常驻协程
	ok := false
	defer func() {
		if !ok {
			if cerr := pool.Close(); cerr != nil {
				slog.Warn("xgorm failed to close the pool", "error", cerr)
			}
		}
	}()
	pool.SetMaxOpenConns(cfg.MaxOpenConns)
	pool.SetMaxIdleConns(cfg.MaxIdleConns)
	pool.SetConnMaxLifetime(cfg.MaxLifetime)
	pool.SetConnMaxIdleTime(cfg.MaxIdleTime)

	policy := probePolicy(cfg, info, dialect)
	if err := xclient.Probe(ctx, policy, probe(pool, readyOf(dialect, db))); err != nil {
		return nil, nil, connectError(dialect, "cannot reach %s: %w", info.Addr, err)
	}

	if cfg.Trace {
		if err := installTracing(db, info, dialect); err != nil {
			return nil, nil, xerror.Newf("xgorm", "new", "install tracing callbacks: %w", err)
		}
	}

	logConn(name, info, cfg)

	ok = true
	return db, &poolCloser{pool: pool, info: info}, nil
}

// connectError 建连失败时报的错。方言认得出是服务端拒绝了凭证的，报认证失败；
// 其余按 format 报（format 里依次是地址和底层错误）
func connectError(d Dialect, format, addr string, err error) error {
	if d.authFailed(err) {
		return xerror.Newf("xgorm", "connect", "authentication to %s failed: %w", addr, err)
	}
	return xerror.Newf("xgorm", "connect", format, addr, err)
}

// probe 一次建连验证：Ping，通过之后接着执行方言的 Ready（见 Dialect.Ready），
// 后者失败同样算这一次没通过、同样重试。
//
// 直接在当前协程里等，不丢给别的协程：sql.DB 的 Close 会等在途的查询，
// 丢下的那个协程并不会因为连接池关了就返回。驱动都认 ctx——
// pgx 和 go-sql-driver 取消时当场断开这条连接，ClickHouse 的 Ping 认截止时间
// ——所以一次尝试最多卡到它自己的截止时间。
func probe(pool *sql.DB, ready func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		err := pool.PingContext(ctx)
		if err == nil && ready != nil {
			err = ready(ctx)
		}
		return err
	}
}

// probePolicy 建连验证怎么试。认证失败不重试：服务端已经明确拒绝了这组凭证，
// 再试两次只是把同一个错误多等两轮退避（默认最多 3s）才报出来，
// 还会在服务端多留两条认证失败的记录。认不认得出由方言决定（Dialect.AuthFailed）
func probePolicy(cfg ClientConfig, info ConnInfo, d Dialect) xclient.ProbePolicy {
	return xclient.ProbePolicy{
		Attempts:   pingAttempts,
		Timeout:    probeTimeout(cfg, info),
		Interval:   pingInterval,
		AuthFailed: d.authFailed,
	}
}

// readyOf 把方言的 Ready 绑到这个实例上，方言没提供就是 nil
func readyOf(d Dialect, db *gorm.DB) func(context.Context) error {
	if d.Ready == nil {
		return nil
	}
	return func(ctx context.Context) error { return d.Ready(ctx, db) }
}

// probeTimeout 单次探测的超时。
//
// 由方言推算（ConnInfo.ProbeTimeout）：它知道驱动拿哪个超时管建连、哪个管往返，
// 也知道 DSN 里最终生效的是多少。方言推算不出来时按建连加一个同量级的往返
// 给 2 × DialTimeout——不能只给建连超时：探测的耗时是建连加一个往返，
// 拿建连预算当整体预算，连接刚建成就会被判超时。
func probeTimeout(cfg ClientConfig, info ConnInfo) time.Duration {
	return cmp.Or(info.ProbeTimeout, 2*cfg.DialTimeout, fallbackPingTimeout)
}

// logConn 记一条建连日志，只写确定不含凭证的字段。
//
// name 是实例名，框架按配置建的才有；直接调 New 的没有名字，这个字段就不写
func logConn(name string, info ConnInfo, c ClientConfig) {
	attrs := make([]any, 0, 14)
	if name != "" {
		attrs = append(attrs, "name", name)
	}
	attrs = append(attrs, "driver", info.Driver, "addr", info.Addr, "db", info.DB, "tls", c.TLS.Enable,
		"max_open_conns", c.MaxOpenConns, "max_idle_conns", c.MaxIdleConns)
	slog.Info("xgorm connected", attrs...)
}

type poolCloser struct {
	pool *sql.DB
	info ConnInfo
}

func (c *poolCloser) Close() error {
	if err := c.pool.Close(); err != nil {
		return xerror.Newf("xgorm", "close", "close %s failed: %w", c.info.Addr, err)
	}
	return nil
}

// ---- 全局实例 ----

// C 取一个 GORM 实例，不带参数时取名为 default 的那个。
//
// 取不到直接 panic，理由见 xclient.Registry.Get：返回 nil 只会把同一个 panic
// 推迟到调用方第一次用它的时候，而那里看不出根因是配置没配。
//
// 可选依赖（配了就用、没配就跳过）用 Has 先判断。
func C(name ...string) *gorm.DB { return reg.Get(name...).db }

// CWithCtx 取实例并绑定 ctx，链路和超时才能传到下游。
//
// GORM 必须这样传 ctx（不像 go-redis 每个方法都收 ctx），所以这个方法是必要的。
func CWithCtx(ctx context.Context, name ...string) *gorm.DB {
	return C(name...).WithContext(ctx)
}

// Has 报告指定实例是否已配置，供可选依赖判断
func Has(name ...string) bool { return reg.Has(name...) }

// Names 返回已配置的实例名
func Names() []string { return reg.Names() }

// ---- 登记 ----

// instance 一个实例连同它自己的 Metric 开关。
//
// 开关跟着实例走：连接池 collector 是进程级的一个，抓取时遍历全部实例，
// 不带着这一位的话它分不出哪个实例配了 Metric: false，只能全导或全不导。
type instance struct {
	db     *gorm.DB
	metric bool
}

// reg 具名实例注册表。取实例、找不到时的报错、关闭时摘干净，
// 这些语义在 xgorm / xredis / xcache 之间必须一致，所以共用一份实现。
var reg = xclient.NewRegistry[instance]("xgorm", ConfigKey)

func init() {
	xhook.BeforeStart(initXGorm, xhook.At(xhook.StageClient))
	xhook.BeforeStop(closeXGorm) // 档位跟着上面那个启动钩子

	// go-sql-driver 的日志是进程级的一个，只能在这里接，理由同 xredis：New 不碰全局，
	// 启动钩子里接又会盖掉使用者在 main 里自己调的 mysql.SetLogger。
	// 它不读配置、不做 IO，写的是一个包级变量，和登记钩子是同一类事。
	// 驱动在解析 DSN 时把当时的 logger 抄进连接配置，之后再调 SetLogger 只对新解析的 DSN 生效
	// ——xgorm 在启动钩子里才解析，所以 main 里、xone 启动之前调的那次照样生效
	_ = mysqldriver.SetLogger(mysqlDriverLogger{}) // 只在参数为 nil 时报错

	// GORM 的 Scan 借用的 Recorder 只认这个进程级的变量，默认把参数值代进 SQL，
	// 绕开了实例 Logger 的 ParamsFilter，见 withoutParams。道理同上：只能在这里接，
	// 使用者要换（比如只在本地调试时看参数）就在 main 里重新赋值，main 在所有 init 之后才跑。
	// 进程里使用者自己 gorm.Open 的实例也吃这一条：Scan 的 SQL 日志一样不带参数值
	logger.RecorderParamsFilter = withoutParams
}

// initXGorm 读配置，按名字把实例挨个建出来。
//
// 没配这一块就一个都不建：xgorm 是可选依赖，没配不该让服务起不来。
// 但 Build 照样走一遍，注册表由此知道启动钩子跑过了——之后 C() 取不到时
// 报的是「没配」，而不是「调早了」。
func initXGorm(ctx context.Context) error {
	if !xconfig.Has(ConfigKey) {
		return xclient.Build(ctx, reg, nil, build)
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	return install(ctx, c)
}

// install 按配置把实例挨个建出来
func install(ctx context.Context, c Config) error {
	if err := xclient.Build(ctx, reg, c.Clients, build); err != nil {
		return err
	}
	installPoolMetrics() // 一个实例都没开 Metric 时它什么都不导出，不必特判
	slog.Info("xgorm ready", "instances", reg.Names())
	return nil
}

// closeXGorm 摘掉全部实例并逆序关闭。
//
// 启动钩子失败时它不会被调到——停止钩子只在和它配对的启动钩子成功之后才执行，
// 所以这里不必处理「还没建起来」。
func closeXGorm(context.Context) error { return reg.Close() }

// build 建一个实例。包一层 New 而不是直接把 New 交出去，
// 是为了把这个实例的 Metric 开关一起带进注册表，并让建连日志写上实例名
func build(ctx context.Context, name string, c ClientConfig) (instance, io.Closer, error) {
	db, closer, err := open(ctx, name, c)
	if err != nil {
		return instance{}, nil, err
	}
	return instance{db: db, metric: c.Metric}, closer, nil
}

// installPoolMetrics 把连接池 collector 挂到当前的 Registry 上。
//
// 每次 install 都挂，不用 sync.Once：xmetric 重新装一次（同一进程里走第二轮
// 生命周期）就是一个新的 Registry，Once 挡住的正是这第二次，连接池指标
// 从此一个都导不出去。同一个 Registry 上重复挂由 xmetric.Register 认出来、
// 复用已有的那个，不会报重复注册。
//
// xmetric 在 StageTelemetry 就绪，早于这里；collector 抓取时才遍历实例。
//
// 不让启动失败：指标导不出去是可观测性问题，不该拦住服务起来。
func installPoolMetrics() {
	if _, err := xmetric.RegisterAs(newPoolCollector(xmetric.Namespace(), xmetric.ConstLabels(), poolStats)); err != nil {
		slog.Error("xgorm failed to register pool metrics", "error", err)
	}
}

// poolStats 读开了 Metric 的各实例连接池的实时状态
func poolStats() map[string]sql.DBStats {
	out := map[string]sql.DBStats{}
	for name, inst := range reg.All() {
		if !inst.metric {
			continue
		}
		pool, err := inst.db.DB()
		if err != nil || pool == nil {
			continue
		}
		out[name] = pool.Stats()
	}
	return out
}
