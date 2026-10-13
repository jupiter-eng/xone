package xkafka

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xapp"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"

	// 用了 xkafka 就有链路：xtrace 装好全局的 TracerProvider 和 Propagator，
	// 生产时把链路上下文写进消息头、消费时从消息头接上，都经 otel 的全局 API
	_ "github.com/jupiter-eng/xone/xtrace"
)

// pingAttempts 启动时连通性探测的尝试次数
const pingAttempts = 3

// pingInterval 第一次退避的上界，之后逐次翻倍（xutil.Retry），3 次尝试之间的两次退避上界是 1s、2s，
// 与 xgorm / xredis 一致。是变量而不是常量，只为让测试能调短
var pingInterval = time.Second

// New 按配置建一个 Kafka 客户端，不触碰任何全局变量。返回的是原生的 *kgo.Client，没有设消费组，
// 用来生产（ProduceSync / Produce）；消费用 Consume。
//
// 会先探测一次连通性（向 broker 要一次元数据）：地址写错、密码不对这类问题应该在启动时暴露，
// 而不是等到线上第一次发消息。ctx 限定这轮探测的生命期，收到退出信号就当场放弃。
//
// 返回的 io.Closer 只是关掉客户端：franz-go 的 Close 让还在缓冲里的消息全部以
// kgo.ErrClientClosed 失败（v1.21.7 实测 10 条丢 10 条）。直接调 New 的，关之前自己先 Flush(ctx)；
// 经框架建的，停止钩子先 Flush 再 Close。
func New(ctx context.Context, cfg ClientConfig) (*kgo.Client, io.Closer, error) {
	return newClient(ctx, "", cfg)
}

// newClient 同 New，name 是配置文件里的集群名，只用来写进日志
func newClient(ctx context.Context, name string, cfg ClientConfig) (*kgo.Client, io.Closer, error) {
	opts, err := clientOpts(name, cfg)
	if err != nil {
		return nil, nil, err
	}
	client, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, nil, xerror.Newf("xkafka", "new", "create client: %w", err)
	}

	if err := xclient.Probe(ctx, probePolicy(cfg), probe(client)); err != nil {
		client.Close()
		addrs := strings.Join(cfg.Brokers, ",")
		// 原始错误用 %w 带上，调用方要靠它判断根因。它不含凭证：连不上时是拨号错误（只有地址），
		// 认证失败时是 broker 回的错误码和它的说明
		if authFailed(err) {
			return nil, nil, xerror.Newf("xkafka", "connect", "authentication to %s failed: %w", addrs, err)
		}
		return nil, nil, xerror.Newf("xkafka", "connect", "cannot reach %s: %w", addrs, err)
	}
	return client, clientCloser{client}, nil
}

// clientOpts 一个客户端（生产用的，或者某个 Consume 自己的那个）共有的设置。
// 配置先在这里校验一次：直接调 New 的没经过 xconfig
func clientOpts(name string, cfg ClientConfig) ([]kgo.Opt, error) {
	if err := cfg.Validate(); err != nil {
		return nil, xerror.Newf("xkafka", "config", "invalid config: %w", err)
	}
	tlsCfg, err := cfg.TLS.Build()
	if err != nil {
		return nil, xerror.Newf("xkafka", "config", "invalid TLS config: %w", err)
	}

	opts := []kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		// 管的是 TCP 加上 TLS 握手：franz-go v1.21.7 用 tls.Dialer 套着 net.Dialer{Timeout}，
		// 握手算在同一个超时里
		kgo.DialTimeout(cfg.DialTimeout),
		kgo.ProducerLinger(cfg.Producer.Linger),
		kgo.ProducerBatchCompression(compression(cfg.Producer.Compression)),
		// franz-go 自己的日志接到 slog，只收 WARN 及以上，见 slogLogger
		kgo.WithLogger(slogLogger{name: name}),
		kgo.WithHooks(produceHook{name: name, trace: cfg.Trace, log: cfg.Log}),
	}
	if cfg.ClientID != "" {
		opts = append(opts, kgo.ClientID(cfg.ClientID))
	}
	if tlsCfg != nil {
		opts = append(opts, kgo.DialTLSConfig(tlsCfg))
	}
	if m := saslMechanism(cfg.SASL); m != nil {
		opts = append(opts, kgo.SASL(m))
	}
	switch cfg.Producer.Acks {
	case "leader":
		// franz-go 的幂等写要求 acks=all，不关的话 NewClient 直接报错
		opts = append(opts, kgo.RequiredAcks(kgo.LeaderAck()), kgo.DisableIdempotentWrite())
	case "none":
		opts = append(opts, kgo.RequiredAcks(kgo.NoAck()), kgo.DisableIdempotentWrite())
	}
	return opts, nil
}

func compression(name string) kgo.CompressionCodec {
	switch name {
	case "gzip":
		return kgo.GzipCompression()
	case "snappy":
		return kgo.SnappyCompression()
	case "lz4":
		return kgo.Lz4Compression()
	case "zstd":
		return kgo.ZstdCompression()
	}
	return kgo.NoCompression()
}

func saslMechanism(s SASLConfig) sasl.Mechanism {
	switch s.Mechanism {
	case "PLAIN":
		return plain.Auth{User: s.Username, Pass: s.Password}.AsMechanism()
	case "SCRAM-SHA-256":
		return scram.Auth{User: s.Username, Pass: s.Password}.AsSha256Mechanism()
	case "SCRAM-SHA-512":
		return scram.Auth{User: s.Username, Pass: s.Password}.AsSha512Mechanism()
	}
	return nil
}

// probePolicy 启动时的探测怎么试。认证被拒不重试：密码不对，再试几次也不对
func probePolicy(cfg ClientConfig) xclient.ProbePolicy {
	return xclient.ProbePolicy{
		Attempts:   pingAttempts,
		Timeout:    pingTimeout(cfg),
		Interval:   pingInterval,
		AuthFailed: authFailed,
	}
}

// probe 一次 Ping，ctx 到点或者被取消时当场返回，不等它。
//
// franz-go v1.21.7 的 Ping 在对端收下连接却不回话时不听 ctx：请求发出去之后等的是读超时
// （RequestTimeoutOverhead，默认 10s），实测给了 200ms 截止时间的 Ping 照样 10s 才返回，
// 期间的退出信号也一样等满 10s。所以放到协程里跑，这边看着 ctx。
// 丢下的那个协程不会漏：探测失败时 newClient 会关掉 client，连接一关，卡着的读当场返回
func probe(client *kgo.Client) func(context.Context) error {
	return func(ctx context.Context) error {
		done := make(chan error, 1)
		go func() { done <- client.Ping(ctx) }()
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// pingTimeout 单次探测的预算：建连加一个往返，按 2 × DialTimeout 算。
// DialTimeout 配 0 时 franz-go 用它自己的 10s
func pingTimeout(cfg ClientConfig) time.Duration {
	d := cfg.DialTimeout
	if d == 0 {
		d = 10 * time.Second
	}
	return 2 * d
}

// authFailed broker 明确拒绝了这组凭证（SASL 认证失败、不支持这种机制）
func authFailed(err error) bool {
	return errors.Is(err, kerr.SaslAuthenticationFailed) || errors.Is(err, kerr.UnsupportedSaslMechanism) ||
		errors.Is(err, kerr.IllegalSaslState)
}

type clientCloser struct{ client *kgo.Client }

// Close 关掉客户端。缓冲里还没发出去的消息会失败，经框架建的在这之前已经 Flush 过了
func (c clientCloser) Close() error {
	c.client.Close()
	return nil
}

// ---- 全局实例 ----

// C 取一个 Kafka 客户端（用来生产），不带参数时取名为 default 的那个。
//
// 取不到直接 panic，理由见 xclient.Registry.Get。可选依赖用 Has 先判断。
func C(name ...string) *kgo.Client { return reg.Get(name...).client }

// Has 报告指定集群是否已配置，供可选依赖判断
func Has(name ...string) bool { return reg.Has(name...) }

// Names 返回已配置的集群名
func Names() []string { return reg.Names() }

// ---- 登记 ----

// instance 一个集群的生产客户端，连同它的配置：Consume 按名字取集群时，要用同一份配置另建自己的客户端
type instance struct {
	client *kgo.Client
	name   string
	cfg    ClientConfig
}

var reg = xclient.NewRegistry[instance]("xkafka", ConfigKey)

// 两对钩子：
//   - StageClient 那一对管生产用的客户端，和 xgorm / xredis 同一档；
//   - StageServer 那一对管 Consume 登记的消费者，和 xgin / xcron 同一档：
//     客户端和业务钩子都起来了它才开始消费，停的时候它最先停——在途的消息还在用数据库，
//     也还要往死信 topic 里写，等它们处理完、提交了 offset，才轮到关客户端。
func init() {
	xhook.BeforeStart(initXKafka, xhook.At(xhook.StageClient))
	xhook.BeforeStop(closeXKafka) // 档位跟着上面那个启动钩子
	xhook.BeforeStart(startConsumers, xhook.At(xhook.StageServer))
	xhook.BeforeStop(stopConsumers)
}

// initXKafka 读配置，按名字把集群挨个建出来。
//
// 没配这一块就一个都不建，但 Build 照样走一遍：之后 C() 取不到时报的是「没配」而不是「调早了」
func initXKafka(ctx context.Context) error {
	if !xconfig.Has(ConfigKey) {
		return xclient.Build(ctx, reg, nil, build)
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if err := xclient.Build(ctx, reg, c.Clients, build); err != nil {
		return err
	}
	slog.Info("xkafka ready", "instances", reg.Names())
	return nil
}

// closeXKafka 先把每个客户端缓冲里的消息发出去（Flush），再摘掉全部实例、逆序关闭。
//
// 不 Flush 直接 Close 的话，franz-go 让缓冲里的消息全部以 kgo.ErrClientClosed 失败（见 New）：
// 退出那一刻用 Produce（异步）发出去的消息就这么丢了。Flush 受停止钩子的时限管，
// 到点还没发完的，Close 照样让它们失败，错误里带上没 Flush 完的集群名
func closeXKafka(ctx context.Context) error {
	var errs []error
	for name, inst := range reg.All() {
		if err := inst.client.Flush(ctx); err != nil {
			errs = append(errs, xerror.Newf("xkafka", "close", "flush %q before close: %w", name, err))
		}
	}
	return errors.Join(append(errs, reg.Close())...)
}

// build 建一个集群的生产客户端。ClientID 没配时用 XApp.Name：broker 端按它区分是哪个服务
func build(ctx context.Context, name string, c ClientConfig) (instance, io.Closer, error) {
	if c.ClientID == "" {
		c.ClientID = xapp.Name()
	}
	client, closer, err := newClient(ctx, name, c)
	if err != nil {
		return instance{}, nil, err
	}
	// 日志里只写地址和认证方式，用户名、密码不进日志
	slog.Info("xkafka connected", "name", name, "brokers", c.Brokers, "tls", c.TLS.Enable, "sasl", c.SASL.Mechanism)
	return instance{client: client, name: name, cfg: c}, closer, nil
}
