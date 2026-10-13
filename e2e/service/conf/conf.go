// Package conf 是 e2e 服务自己的配置块 Service。
//
// 和 example/component/conf 一个写法：默认值预填在结构体里，main 里读一次，
// 之后 C() 处处可用。表名、key 前缀、下游地址都由测试经 ${VAR} 注入，
// 每个测试各用各的，互不干扰。
package conf

import (
	"fmt"
	"regexp"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
)

// Config 业务配置
type Config struct {
	// Table 用户表名；订单表是它加上 _orders。会被拼进 SQL，所以只收小写的标识符
	Table string `yaml:"Table"`

	// KeyPrefix Redis 与本地缓存的 key 前缀
	KeyPrefix string `yaml:"KeyPrefix"`

	// Downstream GET /proxy 调的下游 base URL，比如 http://127.0.0.1:9000。
	// 留空时 /proxy 返回 503
	Downstream string `yaml:"Downstream"`

	// SpanFile 把每个 Span 以一行 JSON 写进这个文件，留空不写
	SpanFile string `yaml:"SpanFile"`

	// SpanDiscard 挂一个 BatchSpanProcessor，后面的 exporter 只计数不上报。
	// 压测用：量文档推荐的那条导出管线的开销，而不是 SpanFile 同步写文件的开销
	SpanDiscard bool `yaml:"SpanDiscard"`

	// UserTTL 用户在 Redis 里缓存多久
	UserTTL time.Duration `yaml:"UserTTL"`

	// StopTimeout 整个退出流程的预算，交给 xone.WithStopTimeout
	StopTimeout time.Duration `yaml:"StopTimeout"`

	// StartStall 大于 0 时，warmup 那个启动钩子不看 ctx 地睡这么久再成功返回，
	// 测「框架在每个钩子之前再查一次 ctx」。0 是不睡
	StartStall time.Duration `yaml:"StartStall"`

	// Drain 大于 0 时，服务的 Start 在 xgin 返回之后再收这么久的尾（不看 ctx），
	// 然后碰一次 PG 和 Redis，测「框架等 Start 真正返回才关其余组件」。0 是不收尾
	Drain time.Duration `yaml:"Drain"`

	// CronEvery 大于 0 时登记一个 @every CronEvery 的定时任务 e2e-tick（见 cron.go）。0 是不登记
	CronEvery time.Duration `yaml:"CronEvery"`

	// CronHold e2e-tick 每次执行看着 ctx 等多久；ctx 被取消时再不看 ctx 地收尾 300ms
	CronHold time.Duration `yaml:"CronHold"`

	// KafkaTopic 非空时用 xkafka.Consume 消费它，消费组是 KafkaGroup（见 kafka.go）。空是不消费
	KafkaTopic string `yaml:"KafkaTopic"`

	// KafkaGroup 消费组，KafkaTopic 非空时必填
	KafkaGroup string `yaml:"KafkaGroup"`

	// KafkaRetry 交给 xkafka.WithRetry；-1 是不写这个 Option（用 xkafka 的默认值）
	KafkaRetry int `yaml:"KafkaRetry"`

	// KafkaTimeout 交给 xkafka.WithTimeout；0 是不写这个 Option
	KafkaTimeout time.Duration `yaml:"KafkaTimeout"`

	// NoHTTP 不起 xgin，xone.MustRun(xone.UntilSignal())：只消费的进程
	NoHTTP bool `yaml:"NoHTTP"`
}

// DefaultConfig 默认值
func DefaultConfig() Config {
	return Config{
		Table:       "e2e_users",
		KeyPrefix:   "e2e:",
		UserTTL:     time.Minute,
		StopTimeout: 15 * time.Second,
		KafkaRetry:  -1,
	}
}

// ident 表名的写法。PG 的标识符上限是 63 字节，订单表还要再加 7 个
var ident = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,55}$`)

// Validate 解完配置之后由 xconfig.Unmarshal 调一次
func (c Config) Validate() error {
	if !ident.MatchString(c.Table) {
		return fmt.Errorf("Table %q must match %s", c.Table, ident)
	}
	if c.UserTTL <= 0 {
		return fmt.Errorf("UserTTL must be > 0, got=%v", c.UserTTL)
	}
	if c.StopTimeout <= 0 {
		return fmt.Errorf("StopTimeout must be > 0, got=%v", c.StopTimeout)
	}
	if c.KafkaTopic != "" && c.KafkaGroup == "" {
		return fmt.Errorf("KafkaGroup is required when KafkaTopic is set")
	}
	if c.StartStall < 0 || c.Drain < 0 {
		return fmt.Errorf("StartStall and Drain must be >= 0, got StartStall=%v Drain=%v", c.StartStall, c.Drain)
	}
	return nil
}

// conf 生效中的配置
var conf = DefaultConfig()

// Load 读出 Service 这一块。main 里调一次，之后 C() 处处可用
func Load() error { return xconfig.Unmarshal("Service", &conf) }

// C 返回最终配置
func C() Config { return conf }
