// Package xkafka 按配置装好 franz-go，生产用原生的 *kgo.Client，消费用 Consume 登记一个处理函数。
//
//	err := xkafka.C().ProduceSync(ctx, &kgo.Record{Topic: "orders", Value: b}).FirstErr()
//
//	xkafka.Consume("orders", "billing", func(ctx context.Context, r *kgo.Record) error { … })
//
// 不提供 CWithCtx：franz-go 生产、消费的每个方法本来就收 ctx。
package xkafka

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xtls"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XKafka"

// DefaultName C() 不带参数时取的那个集群的名字，也是 Consume 默认用的集群
const DefaultName = xconfig.DefaultClientName

// Config 本模块的配置。两种写法：
//
//	XKafka:                 # 单集群
//	  Brokers: ["127.0.0.1:9092"]
//
//	XKafka:                 # 多集群
//	  Clients:
//	    default: {Brokers: ["10.0.0.1:9092", "10.0.0.2:9092"]}
//	    audit:   {Brokers: ["10.0.1.1:9092"]}
type Config struct {
	// Clients 按名字组织的集群。单集群写法会被规整成一个名为 default 的集群。
	Clients map[string]ClientConfig
}

// ClientConfig 一个 Kafka 集群的配置
type ClientConfig struct {
	// Brokers 种子 broker 的地址，host:port。必填，没有默认值。
	//
	// 只是种子：连上任意一个之后，franz-go 从元数据里拿到整个集群的 broker。
	Brokers []string `yaml:"Brokers"`

	// ClientID 发给 broker 的 client.id，broker 端的日志、配额、监控按它区分客户端。
	// 默认空：经框架建的用 XApp.Name，XApp.Name 也没配就是 franz-go 的默认值 kgo
	// （v1.21.7 实测）；直接调 New 的不读 XApp，空就是 kgo。
	ClientID string `yaml:"ClientID"`

	// DialTimeout 建一条连接（TCP，加上 TLS 握手）的超时。默认 1s；franz-go 自己的默认是 10s。
	//
	// 启动时的连通性探测每次尝试的预算是 2 × DialTimeout：建连加一个往返，见 README「行为与实测」。
	DialTimeout time.Duration `yaml:"DialTimeout"`

	// SASL 认证。默认不认证。
	SASL SASLConfig `yaml:"SASL"`

	// TLS 连 broker 时走不走 TLS。默认不走。字段和规则各模块共用，见 xtls.Config。
	TLS xtls.Config `yaml:"TLS"`

	// Producer 生产这一侧的几项设置。
	Producer ProducerConfig `yaml:"Producer"`

	// Consumer 消费这一侧的设置，对这个集群上的每个 Consume 生效。
	Consumer ConsumerConfig `yaml:"Consumer"`

	// Trace 是否给生产、消费各开一个 Span。默认开启。
	//
	// 关掉的只是 Span：链路上下文（traceparent 等）照样写进消息头、照样从消息头里接上，
	// 下游开着链路的服务看到的仍是同一条链路。
	Trace bool `yaml:"Trace"`

	// Metric 是否导出消费耗时指标（kafka_consume_duration_seconds）。默认开启。
	Metric bool `yaml:"Metric"`

	// Log 是否记逐条消息的日志：生产失败（WARN）、每次处理失败（WARN）、处理成功（DEBUG）。默认开启。
	//
	// 关掉之后，消息进了死信、被跳过、处理函数 panic、死信写不进去这几种照样记——
	// 它们意味着一条消息没按正常的路走完，不记就没有任何痕迹。
	// 只记消息的 key（可打印的 UTF-8，超过 256 字节截断），从不记 value，见 README「可观测」。
	Log bool `yaml:"Log"`
}

// SASLConfig SASL 认证
type SASLConfig struct {
	// Mechanism 认证机制：PLAIN、SCRAM-SHA-256、SCRAM-SHA-512。默认空，不认证。
	//
	// PLAIN 把密码原样发给 broker，不配 TLS 时它在网络上是明文。
	Mechanism string `yaml:"Mechanism"`

	// Username 用户名。Mechanism 非空时必填。
	Username string `yaml:"Username"`

	// Password 密码。Mechanism 非空时必填。
	//
	// 建议写成 "${KAFKA_PASSWORD}"：凭证不该进版本库，漏配时启动就失败。
	// 本模块不会把它写进任何日志和错误。
	Password string `yaml:"Password"`
}

// ProducerConfig 生产这一侧。没列出来的都是 franz-go 的默认值，数字见 README「行为与实测」
type ProducerConfig struct {
	// Acks 写成功要几个副本确认：all、leader、none。默认 all（franz-go 的默认值）。
	//
	// all 时开着幂等写（franz-go 的默认）：重试不会写出重复的消息，同一分区里不乱序。
	// leader、none 时 franz-go 要求关掉幂等写，本模块替你关：此时重试可能写出重复的消息。
	Acks string `yaml:"Acks"`

	// Linger 一个批次最多等多久再发，攒批用。默认 10ms（franz-go 的默认值）；配 0 是不等，有就发。
	Linger time.Duration `yaml:"Linger"`

	// Compression 批次的压缩算法：none、gzip、snappy、lz4、zstd。默认 snappy（franz-go 的默认值）。
	Compression string `yaml:"Compression"`
}

// ConsumerConfig 消费这一侧
type ConsumerConfig struct {
	// ResetOffset 一个消费组第一次消费某个分区（还没提交过 offset）、或者提交过的 offset 已经被清理掉时，
	// 从哪里开始：latest（只消费之后新来的）、earliest（从最早还留着的那条开始）。默认 latest。
	//
	// franz-go 的默认是 earliest（v1.21.7 实测）：新上线的消费组会把 topic 里留着的全部历史消息
	// 处理一遍。本模块默认 latest，代价是新组上线、或者 topic 刚建好的那一刻之前写进去的消息，
	// 这个组不会处理——要它们就配 earliest。
	ResetOffset string `yaml:"ResetOffset"`
}

// DefaultClientConfig 单个集群的全部默认值集中在这里
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		DialTimeout: time.Second,
		Producer: ProducerConfig{
			Acks:        "all",
			Linger:      10 * time.Millisecond,
			Compression: "snappy",
		},
		Consumer: ConsumerConfig{ResetOffset: "latest"},
		Trace:    true,
		Metric:   true,
		Log:      true,
	}
}

// DefaultConfig 默认没有任何集群——没配 XKafka 就不该连任何 Kafka
func DefaultConfig() Config { return Config{} }

// loadConfig 读配置文件里的这一块。单集群和多集群两种写法都收，
// 每个集群先铺上 DefaultClientConfig 再解，规则见 xconfig.UnmarshalClients
func loadConfig() (Config, error) {
	clients, err := xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)
	return Config{Clients: clients}, err
}

var (
	saslMechanisms = []string{"PLAIN", "SCRAM-SHA-256", "SCRAM-SHA-512"}
	acksValues     = []string{"all", "leader", "none"}
	compressions   = []string{"none", "gzip", "snappy", "lz4", "zstd"}
	resetOffsets   = []string{"latest", "earliest"}
)

// Validate 检查配置本身说不通的地方，在建连之前就失败。
//
// xconfig.UnmarshalClients 每解完一个集群调一次；直接调 New 的，New 也会调一次。
// 返回普通 error，由调它的那一层包一次 xerror。错误里不带密码。
func (c ClientConfig) Validate() error {
	var errs []error
	if len(c.Brokers) == 0 {
		errs = append(errs, errors.New("Brokers must not be empty"))
	}
	for _, b := range c.Brokers {
		if strings.TrimSpace(b) == "" {
			errs = append(errs, errors.New("Brokers must not contain an empty address"))
			break
		}
	}
	// 负的时长在 franz-go 里不是「不限时」，是一次立刻失败的拨号
	if c.DialTimeout < 0 {
		errs = append(errs, fmt.Errorf("DialTimeout must not be negative, got=%v", c.DialTimeout))
	}
	if c.Producer.Linger < 0 {
		errs = append(errs, fmt.Errorf("Producer.Linger must not be negative, got=%v", c.Producer.Linger))
	}
	errs = append(errs,
		oneOf("Producer.Acks", c.Producer.Acks, acksValues),
		oneOf("Producer.Compression", c.Producer.Compression, compressions),
		oneOf("Consumer.ResetOffset", c.Consumer.ResetOffset, resetOffsets),
		c.SASL.validate(),
		c.TLS.Validate(),
	)
	return errors.Join(errs...)
}

func (s SASLConfig) validate() error {
	if s.Mechanism == "" {
		if s.Username != "" || s.Password != "" {
			return errors.New("SASL.Username / SASL.Password are set but SASL.Mechanism is empty; set it to one of " + strings.Join(saslMechanisms, ", "))
		}
		return nil
	}
	if err := oneOf("SASL.Mechanism", s.Mechanism, saslMechanisms); err != nil {
		return err
	}
	if s.Username == "" || s.Password == "" {
		return errors.New("SASL.Username and SASL.Password are required when SASL.Mechanism is set")
	}
	return nil
}

// oneOf 值必须是列表里的一个（区分大小写：配置里写错大小写的，报错比猜更好）
func oneOf(field, v string, allowed []string) error {
	if slices.Contains(allowed, v) {
		return nil
	}
	return fmt.Errorf("%s must be one of %s, got=%q", field, strings.Join(allowed, ", "), v)
}
