package xredis

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"go.opentelemetry.io/otel"

	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
	"github.com/jupiter-eng/xone/xmetric"

	// 用了 xredis 就有链路：xtrace 装好全局的 TracerProvider，命令的 Span 经 otel 的全局 API 记下来。
	// 不要链路配 XTrace.Enable: false，只关某个实例的配它自己的 Trace: false
	_ "github.com/jupiter-eng/xone/xtrace"
)

const (
	// pingAttempts 建连验证的尝试次数
	pingAttempts = 3

	// fallbackPingTimeout 配置里推算不出预算时的兜底超时
	fallbackPingTimeout = time.Second
)

// pingInterval 第一次退避的上界，之后逐次翻倍（xutil.Retry），3 次尝试之间的两次
// 退避上界是 1s、2s。这个数写在 docs/behavior.md「启动期建连探测」里，启动预算靠它推算。
//
// 是变量而不是常量，只为让测试能调短——连不上的用例要跑满整轮重试，
// 按一秒算一次就是几十秒。
var pingInterval = time.Second

// New 按配置建一个 Redis 实例，不触碰任何全局变量。
//
// 会先 Ping 一次确认连得上：地址写错、密码不对这类问题应该在启动时暴露，
// 而不是等到线上第一次读缓存。
//
// ctx 限定这轮建连验证的生命期：连不上时要走满一轮重试，
// 收到退出信号就该当场放弃，而不是让进程卡在那里。
func New(ctx context.Context, cfg ClientConfig) (*redis.Client, io.Closer, error) {
	return newClient(ctx, "", cfg)
}

// newClient 同 New，name 是配置文件里的实例名，只用来写进命令日志（Log 开着时）；
// 直接调 New 的没有实例名，日志里就没有 name 字段
func newClient(ctx context.Context, name string, cfg ClientConfig) (*redis.Client, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, xerror.Newf("xredis", "config", "invalid config: %w", err)
	}
	tlsCfg, err := cfg.TLS.Build()
	if err != nil {
		return nil, nil, xerror.Newf("xredis", "config", "invalid TLS config: %w", err)
	}

	client := redis.NewClient(&redis.Options{
		Addr:            cfg.Addr,
		Username:        cfg.Username,
		Password:        cfg.Password,
		DB:              cfg.DB,
		DialTimeout:     cfg.DialTimeout,
		ReadTimeout:     cfg.ReadTimeout,
		WriteTimeout:    cfg.WriteTimeout,
		PoolSize:        cfg.PoolSize,
		MinIdleConns:    cfg.MinIdleConns,
		MaxIdleConns:    cfg.MaxIdleConns,
		MaxActiveConns:  cfg.MaxActiveConns,
		PoolTimeout:     cfg.PoolTimeout,
		ConnMaxIdleTime: cfg.ConnMaxIdleTime,
		ConnMaxLifetime: cfg.ConnMaxLifetime,
		MaxRetries:      cfg.MaxRetries,
		MinRetryBackoff: cfg.MinRetryBackoff,
		MaxRetryBackoff: cfg.MaxRetryBackoff,

		// nil 时是明文。握手受 DialTimeout 管：go-redis v9.22.0 用 tls.DialWithDialer，
		// 拨号和握手共用那一个超时
		TLSConfig: tlsCfg,

		// 不发 CLIENT SETINFO。
		//
		// go-redis v9.22.0 默认每建一条连接就多发一个 pipeline（CLIENT SETINFO lib-name、
		// lib-ver 两条），只为让服务端的 CLIENT LIST 里显示客户端库名和版本。
		// Redis 7.2 之前没有这个子命令：实测 Redis 7.0.15 上每条新连接都回
		// ERR unknown subcommand 'setinfo'，go-redis 吞掉这个错，但链路开着时
		// 每条连接都留下一个报错的 redis.pipeline Span——ConnMaxLifetime 每 5 分钟
		// 换一轮连接，就每 5 分钟一批。换来的信息对使用者几乎没用，关掉
		DisableIdentity: true,

		// 不发 CLIENT MAINT_NOTIFICATIONS。
		//
		// go-redis v9.22.0 默认 auto：每条新连接先发一次，服务端认就开启「维护通知」——
		// 服务端迁移、升级前推一条通知，客户端据此把读写超时临时放宽
		// （源码里的默认 RelaxedTimeout 是 10s；手头没有认这条命令的服务端，没有实测）、
		// 在后台把连接挪到新节点。go-redis 的注释说
		// 这是给 Redis Cloud 用的。在不认这条命令的服务端上，实测 Redis 7.0.15 回
		// ERR unknown subcommand 'maint_notifications'，并发建起来的头几条连接各留一个报错的
		// Span（并发 3 条时 1–2 个），之后整个 client 降级不再发。认的那种服务端上，
		// 「超时临时放宽到 10s」又和本包 ReadTimeout 的承诺对不上。关掉，行为只由配置决定
		MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},

		// 一次建连只拨一次号，重试交给 MaxRetries。
		//
		// go-redis v9.22.0 默认 DialerRetries=5、两次之间等 DialerRetryTimeout=100ms，
		// 藏在每一次「建连」里面：一次建连最多是 5×DialTimeout+4×100ms，默认配置下 2.9s，
		// 命令级的 4 次尝试再乘上去，实测 Redis 主机宕机（SYN 没有回音）时一条命令用了 11.7s，
		// 拒绝连接时也有 4×400ms 的白等。配成 1 之后一条命令的上限就是文档里那条式子
		// (MaxRetries+1)×(DialTimeout+ReadTimeout)+MaxRetries×MaxRetryBackoff（默认 7s），
		// 同样的场景实测 2.1s（MaxRetries -1 时 0.5s），拒绝连接时 70–150ms（两轮 e2e）。
		// 只拨一次，DialerRetryTimeout 就用不上了（最后一次失败之后不等），所以不设。
		// 不能写 0：0 和负数在 go-redis 里都会被换回 5
		DialerRetries: 1,

		// 让请求 ctx 的截止时间管住 socket 读写。
		//
		// go-redis 默认不这么做：不开的话，每个命令用的是 ReadTimeout /
		// WriteTimeout 这组固定值，调用方给的 deadline 只是摆设——
		// 一个 20ms 超时的请求照样会在一个慢 Redis 上等满几百毫秒，
		// 上游的超时预算和级联保护跟着一起失效。
		//
		// 它管的只是截止时间，不是取消：go-redis 把 ctx.Deadline() 设成 socket 的读写截止时间，
		// 之后不再看 ctx，ctx 被取消叫不醒一个已经阻塞在读上的命令。实测（v9.22.0）
		// ReadTimeout 1s、命令卡住 100ms 时取消 ctx，命令照样在 1s 返回；xgin 停止时断连
		// 取消了请求 ctx，卡在 Redis 上的 handler 要等到 ReadTimeout、或 xredis 的停止钩子
		// 关掉连接池才返回。xredis 在里面补不上这一截：命令的结果写在调用方的 *Cmd 上，
		// 提前返回就是和还在读的那个协程抢着写同一个对象。所以要让命令能被叫停，给 ctx 带上截止时间
		ContextTimeoutEnabled: true,
	})

	// 这之后任何一步失败都要关掉 client，否则漏一个连接池
	ok := false
	defer func() {
		if !ok {
			client.Close()
		}
	}()

	if err := xclient.Probe(ctx, probePolicy(client.Options()), probe(client)); err != nil {
		// 原始错误用 %w 带上，调用方要靠它判断根因。它不含凭证：连不上时是
		// 拨号错误（只有地址），认证失败时是服务端回的 WRONGPASS / NOAUTH 文本
		if redis.IsAuthError(err) {
			return nil, nil, xerror.Newf("xredis", "connect", "authentication to %s failed: %w", cfg.Addr, err)
		}
		return nil, nil, xerror.Newf("xredis", "connect", "cannot reach %s: %w", cfg.Addr, err)
	}

	// 链路钩子在建连验证成功之后才挂：挂在前面的话，启动时的每一次 Ping 尝试
	// 都是一个没有父 Span 的 ping（外加 redis.dial、hello），连不上时一轮重试就是一串
	// 报错的根 Span，而那是一次启动，不是业务请求
	if cfg.Trace {
		// 关掉 db.statement：redisotel v9.22.0 默认开着，把整条命令连同参数
		// 写进 Span（实测 SET session:1 <值> 原样出现），值里的会话、令牌就此
		// 进了链路后端。与 xgorm 只记占位符 SQL、不记参数是同一条原则
		//
		// TODO: redisotel 默认 WithCallerEnabled(true)，每条命令都 runtime.Callers
		// 找调用方，写进 code.function / code.filepath / code.lineno。实测关掉之后
		// 每条命令 14.0µs → 11.6µs（约 -17%，本机回环、noop provider），代价是
		// Span 里不再有这三个属性。这是对外可见的变化，暂保持默认，待定。
		//
		// 错误也一样：redisotel 把服务端的错误原文写进 exception.message 和状态描述，
		// 换成包了一层的 TracerProvider，Span 上的错误文本和命令日志用同一套规则（见 trace.go）。
		// 包的是这一刻的全局 provider：xtrace 还没装好时它是 otel 的委托，装好之后照样转过去
		// Span 名也一样：redisotel 拿整条命令的第 1 个参数当名字，spanCmdsHook 挂在它外层，交出命令让包装重新起名
		tp := redactingTracerProvider{otel.GetTracerProvider()}
		client.AddHook(spanCmdsHook{})
		if err := redisotel.InstrumentTracing(client, redisotel.WithDBStatement(false), redisotel.WithTracerProvider(tp)); err != nil {
			return nil, nil, xerror.Newf("xredis", "new", "install tracing hook: %w", err)
		}
	}

	// 日志钩子挂在链路钩子之后。go-redis 的钩子先挂的在外层，所以它在 redis Span 里面：
	// 命令 ctx 里的是这条命令的 Span，日志的 span_id 就是它，trace_id 与调用方的相同。
	// 同样在建连验证之后挂：启动时的 Ping 不是业务命令
	if cfg.Log {
		client.AddHook(logHook{name: name, slow: cfg.SlowThreshold})
	}

	ok = true
	return client, &clientCloser{client: client, addr: cfg.Addr}, nil
}

// probePolicy 建连验证怎么试。认证失败（WRONGPASS / NOAUTH）不重试：密码不对，
// 再试几次也不对，只是让启动多等两次退避（默认最多 1s + 2s）才报出来
func probePolicy(opt *redis.Options) xclient.ProbePolicy {
	return xclient.ProbePolicy{
		Attempts:   pingAttempts,
		Timeout:    pingTimeout(opt),
		Interval:   pingInterval,
		AuthFailed: redis.IsAuthError,
	}
}

// probe 一次 Ping。ctx 被取消时当场返回，不等这次读撞上 ReadTimeout。
//
// 理由见 New 里 ContextTimeoutEnabled 的注释：go-redis 只认 ctx 的截止时间，
// 取消叫不醒一个阻塞在读上的命令。实测（v9.22.0）对端收下连接不回话时，启动期间的
// 退出信号要等这次读超时才生效：ReadTimeout 500ms 时信号后约 450ms，配 3s 时约 2.95s。
// 所以 Ping 放到协程里跑，这边同时看着 ctx，取消了就不等它。
// xclient.Probe 不替 fn 做这件事（见它的注释），这一截只能在这里补。
//
// 丢下的那个协程不会漏：建连失败时 New 会关掉 client，连接池一关，卡着的读当场返回。
// 截止时间到了照样等它——那一刻 go-redis 自己也返回了，它的 i/o timeout 比 ctx 的错误更说明问题
func probe(client *redis.Client) func(context.Context) error {
	return func(ctx context.Context) error {
		done := make(chan error, 1)
		go func() { done <- client.Ping(ctx).Err() }()
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return <-done
		}
	}
}

// pingTimeout 单次 Ping 的超时：建连加一个往返。
//
// 读的是 go-redis 补完默认值之后的 Options（client.Options()），不是配置：配置里的 0
// 在 go-redis v9.22.0 里是「用它的默认值」——DialTimeout 5s、ReadTimeout 5s（options.go init），
// 按 0 + 0 算的话预算只剩兜底的 1s，go-redis 自己还愿意等的一次慢建连会被判超时。
// Validate 拦下了负的时长，补完之后两者都是正数；兜底值只防 go-redis 哪天改了默认值
func pingTimeout(opt *redis.Options) time.Duration {
	return cmp.Or(opt.DialTimeout+opt.ReadTimeout, fallbackPingTimeout)
}

type clientCloser struct {
	client *redis.Client
	addr   string
}

func (c *clientCloser) Close() error {
	if err := c.client.Close(); err != nil {
		return xerror.Newf("xredis", "close", "close %s failed: %w", c.addr, err)
	}
	return nil
}

// ---- 全局实例 ----

// C 取一个 Redis 实例，不带参数时取名为 default 的那个。
//
// 取不到直接 panic，理由见 xclient.Registry.Get。
// 不提供 CWithCtx：go-redis 的每个方法本来就收 ctx，再包一层没有意义。
//
// 可选依赖（配了就用、没配就跳过）用 Has 先判断。
func C(name ...string) *redis.Client { return reg.Get(name...).client }

// Has 报告指定实例是否已配置，供可选依赖判断
func Has(name ...string) bool { return reg.Has(name...) }

// Names 返回已配置的实例名
func Names() []string { return reg.Names() }

// ---- 登记 ----

// instance 一个实例连同它自己的 Metric 开关，理由同 xgorm：
// collector 是进程级的一个，不带着这一位就分不出哪个实例配了 Metric: false。
type instance struct {
	client *redis.Client
	metric bool
}

// reg 具名实例注册表。取实例、找不到时的报错、关闭时摘干净，
// 这些语义在 xgorm / xredis / xcache 之间必须一致，所以共用一份实现。
var reg = xclient.NewRegistry[instance]("xredis", ConfigKey)

func init() {
	xhook.BeforeStart(initXRedis, xhook.At(xhook.StageClient))
	xhook.BeforeStop(closeXRedis) // 档位跟着上面那个启动钩子

	// go-redis 的日志是进程级的一个，只能在这里接：New 不碰全局，启动钩子里接又会
	// 盖掉使用者在 main 里自己调的 redis.SetLogger。在 init 里接，main 里再调的那次照样生效。
	// 它不读配置、不做 IO，写的是一个包级变量，和登记钩子是同一类事
	redis.SetLogger(slogLogger{})
}

// slogLogger 把 go-redis 自己的日志接到 slog 上。
//
// go-redis v9.22.0 的默认 logger 是标准库 log，往 stderr 写纯文本
// （`redis: 2026/09/24 10:00:00 pool.go:762: redis: connection pool: failed to dial after 1 attempts: ...`），
// 不进 slog 就不是 JSON、不带 trace_id，进不了日志平台。它的日志没有级别，
// 写出来的几乎都是出错时的补充（建连失败、连接关不掉、通知处理失败），
// 调用方同时也拿到了错误，所以一律记成 WARN。ctx 原样传下去，用命令 ctx 记的那些
// （比如过期时间被截到 1ms）由此带上 trace_id；建连失败那句不带——v9.22.0 在它自己的协程里、
// 用 context.Background() 拨号，实测 Redis 拒绝连接时 45 条 failed to dial 一条都没有 trace_id
type slogLogger struct{}

func (slogLogger) Printf(ctx context.Context, format string, v ...any) {
	slog.WarnContext(ctx, "xredis go-redis log", "detail", fmt.Sprintf(format, v...))
}

// initXRedis 读配置，按名字把实例挨个建出来。
//
// 没配这一块就一个都不建：xredis 是可选依赖，没配不该让服务起不来。
// 但 Build 照样走一遍，注册表由此知道启动钩子跑过了——之后 C() 取不到时
// 报的是「没配」，而不是「调早了」。
func initXRedis(ctx context.Context) error {
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
	slog.Info("xredis ready", "instances", reg.Names())
	return nil
}

// closeXRedis 摘掉全部实例并逆序关闭。
//
// 启动钩子失败时它不会被调到——停止钩子只在和它配对的启动钩子成功之后才执行，
// 所以这里不必处理「还没建起来」。
func closeXRedis(context.Context) error { return reg.Close() }

// build 建一个实例。包一层而不是直接把 New 交出去，
// 是为了把这个实例的 Metric 开关一起带进注册表，并在日志里写上实例名（命令日志也带）——
// 配了好几个 Redis 时，只写地址分不出是哪一个（同一个地址不同库号的更是如此）
func build(ctx context.Context, name string, c ClientConfig) (instance, io.Closer, error) {
	client, closer, err := newClient(ctx, name, c)
	if err != nil {
		return instance{}, nil, err
	}
	// 日志里只写地址和库号，密码不进日志——所以也就不需要脱敏
	slog.Info("xredis connected", "name", name, "addr", c.Addr, "db", c.DB,
		"tls", c.TLS.Enable, "min_idle_conns", c.MinIdleConns)
	return instance{client: client, metric: c.Metric}, closer, nil
}

// installPoolMetrics 把连接池 collector 挂到当前的 Registry 上。
//
// 每次 install 都挂、不用 sync.Once，理由同 xgorm：xmetric 重装之后是新的
// Registry，Once 会把这第二次挡掉；同一个 Registry 上重复挂由 xmetric.Register
// 复用已有的那个。
//
// 不让启动失败：指标导不出去是可观测性问题，不该拦住服务起来。
func installPoolMetrics() {
	if _, err := xmetric.RegisterAs(newPoolCollector(xmetric.Namespace(), xmetric.ConstLabels(), poolStats)); err != nil {
		slog.Error("xredis failed to register pool metrics", "error", err)
	}
}

// poolStats 读开了 Metric 的各实例连接池的实时状态
func poolStats() map[string]*redis.PoolStats {
	out := map[string]*redis.PoolStats{}
	for name, inst := range reg.All() {
		if inst.metric {
			out[name] = inst.client.PoolStats()
		}
	}
	return out
}
