// Package xredis 按配置装好 go-redis，使用者拿到的是原生的 *redis.Client。
//
//	v, err := xredis.C().Get(ctx, "key").Result()
//
// 不提供 CWithCtx：go-redis 的每个方法本来就收 ctx，再包一层没有意义。
package xredis

import (
	"fmt"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xtls"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XRedis"

// DefaultName C() 不带参数时取的那个实例的名字
const DefaultName = xconfig.DefaultClientName

// Config 本模块的配置。两种写法：
//
//	XRedis:                 # 单实例
//	  Addr: "127.0.0.1:6379"
//
//	XRedis:                 # 多实例
//	  Clients:
//	    default: {Addr: "127.0.0.1:6379"}
//	    cache:   {Addr: "127.0.0.1:6380", DB: 1}
type Config struct {
	// Clients 按名字组织的实例。单实例写法会被规整成一个名为 default 的实例。
	Clients map[string]ClientConfig
}

// ClientConfig 一个 Redis 实例的配置
type ClientConfig struct {
	// Addr 服务器地址。默认 127.0.0.1:6379。
	Addr string `yaml:"Addr"`

	// Username ACL 用户名（Redis 6.0+）。默认无。
	Username string `yaml:"Username"`

	// Password 认证密码。默认无。
	//
	// 建议写成 "${REDIS_PASSWORD}"：凭证不该进版本库，漏配时启动就失败。
	// 本模块不会把它写进任何日志。
	Password string `yaml:"Password"`

	// DB 数据库编号。默认 0。
	DB int `yaml:"DB"`

	// DialTimeout 建连超时。默认 500ms；配 0 交给 go-redis（5s）。
	//
	// 下面几个时长配 0 都不是「不限时」，是 go-redis v9.22.0 的默认值（options.go init，实测一致）。
	// 启动时建连验证的单次预算按换算之后的 DialTimeout + ReadTimeout 算。
	DialTimeout time.Duration `yaml:"DialTimeout"`

	// ReadTimeout 读超时。默认 500ms；配 0 交给 go-redis（5s）。
	ReadTimeout time.Duration `yaml:"ReadTimeout"`

	// WriteTimeout 写超时。默认 500ms；配 0 跟着换算之后的 ReadTimeout。
	WriteTimeout time.Duration `yaml:"WriteTimeout"`

	// PoolSize 连接池大小。默认 0，交给 go-redis（10 × GOMAXPROCS）。
	PoolSize int `yaml:"PoolSize"`

	// MinIdleConns 最小空闲连接数，预热连接避免冷启动抖动。默认 5。
	MinIdleConns int `yaml:"MinIdleConns"`

	// MaxIdleConns 最大空闲连接数。默认 0，即不限制。
	MaxIdleConns int `yaml:"MaxIdleConns"`

	// MaxActiveConns 最大活跃连接数。默认 0，即不限制。
	MaxActiveConns int `yaml:"MaxActiveConns"`

	// PoolTimeout 池子没有空闲连接时的等待上限。默认 1s；配 0 是换算之后的 ReadTimeout + 1s。
	PoolTimeout time.Duration `yaml:"PoolTimeout"`

	// ConnMaxIdleTime 空闲连接最长存活时间。默认 5m；配 0 交给 go-redis（30m）。
	ConnMaxIdleTime time.Duration `yaml:"ConnMaxIdleTime"`

	// ConnMaxLifetime 连接最长存活时间。默认 5m；配 0 是不按存活时间换连接（go-redis 的默认）。
	//
	// 定期换连接，服务端扩缩容后流量才会重新摊开。
	ConnMaxLifetime time.Duration `yaml:"ConnMaxLifetime"`

	// MaxRetries 最大重试次数。默认 0，交给 go-redis（3 次）；配 -1 关闭重试。
	MaxRetries int `yaml:"MaxRetries"`

	// MinRetryBackoff 最小重试退避。默认 0，交给 go-redis（10ms，实测 v9.22.0）；
	// 配 -1ns 关闭退避（得带单位，裸写 -1 解不成时长，启动报错）。
	MinRetryBackoff time.Duration `yaml:"MinRetryBackoff"`

	// MaxRetryBackoff 最大重试退避。默认 0，交给 go-redis（1s，实测 v9.22.0）；
	// 配 -1ns 关闭退避，同上。
	MaxRetryBackoff time.Duration `yaml:"MaxRetryBackoff"`

	// Trace 是否挂 OpenTelemetry 钩子。默认开启。
	//
	// 没装链路时它产出的是 noop Span，但钩子本身不是零成本：实测
	// （redisotel v9.22.0，本机回环，noop provider）每条命令多约 3µs、8 次分配、
	// 约 1.2KB。真实网络的往返动辄几百微秒，占比小得多，所以默认开着；
	// 对单条命令延迟极敏感的服务可以关掉。
	Trace bool `yaml:"Trace"`

	// Metric 是否导出连接池指标。默认开启。
	//
	// 指标在被抓取时才读 PoolStats()，不额外占协程。
	// 按实例生效：配了 Metric: false 的实例不出现在 /metrics 里。
	Metric bool `yaml:"Metric"`

	// Log 是否每条命令记一条日志（pipeline 整个记一条）。默认关闭。
	//
	// 只记命令名和第一个 key，不记值、不记其余参数；AUTH、HELLO 这类参数里可能有凭证的
	// 命令只记命令名。key 是整条记的（超过 256 字节才截断）：开着 Log 时别把令牌、手机号、
	// 邮箱拼进 key。key 不存在（redis.Nil）、WATCH 冲突（redis.TxFailedErr）不算失败，照常记 INFO。
	// 服务端报的错只记错误码（另有 error_code 字段），不记原文：实测 Redis 7.0.15
	// 的 ERR unknown command 'foo', with args beginning with: … 把参数原样带出来；
	// 超时、连不上、ctx 取消、redis: client is closed 这类客户端一侧的错误照原文记。
	// 返回给调用方的错误不变。字段和实测见 xredis/README.md「可观测」「行为与实测」。
	Log bool `yaml:"Log"`

	// SlowThreshold 超过这个耗时的命令（或 pipeline）记一条 warn 日志。默认 100ms，需 Log 开启，配 0 不记。
	SlowThreshold time.Duration `yaml:"SlowThreshold"`

	// TLS 连 Redis 时走不走 TLS。默认不走。字段和规则各模块共用，见 xtls.Config。
	//
	// 握手受 DialTimeout 管：go-redis v9.22.0 用 tls.DialWithDialer，拨号和握手共用那一个超时。
	TLS xtls.Config `yaml:"TLS"`
}

// DefaultClientConfig 单个实例的全部默认值集中在这里
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		Addr:            "127.0.0.1:6379",
		DialTimeout:     500 * time.Millisecond,
		ReadTimeout:     500 * time.Millisecond,
		WriteTimeout:    500 * time.Millisecond,
		MinIdleConns:    5,
		PoolTimeout:     time.Second,
		ConnMaxIdleTime: 5 * time.Minute,
		ConnMaxLifetime: 5 * time.Minute,
		SlowThreshold:   100 * time.Millisecond,
		Trace:           true,
		Metric:          true,
	}
}

// DefaultConfig 默认没有任何实例——没配 XRedis 就不该连任何 Redis
func DefaultConfig() Config { return Config{} }

// loadConfig 读配置文件里的这一块。单实例和多实例两种写法都收，
// 每个实例先铺上 DefaultClientConfig 再解，规则见 xconfig.UnmarshalClients
func loadConfig() (Config, error) {
	clients, err := xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)
	return Config{Clients: clients}, err
}

// Validate 检查配置本身说不通的地方，在建连之前就失败。
//
// xconfig.UnmarshalClients 每解完一个实例调一次，配错的值在读配置时就失败、
// 带着是哪个实例；直接调 New 的，New 也会调一次。
// 返回普通 error，由调它的那一层包一次 xerror。
func (c ClientConfig) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("Addr must not be empty")
	}
	for _, n := range []struct {
		name string
		val  int
	}{
		{"DB", c.DB},
		{"PoolSize", c.PoolSize},
		{"MinIdleConns", c.MinIdleConns},
		{"MaxIdleConns", c.MaxIdleConns},
		{"MaxActiveConns", c.MaxActiveConns},
	} {
		if n.val < 0 {
			return fmt.Errorf("%s must not be negative, got=%d", n.name, n.val)
		}
	}

	// 负的时长在 go-redis 里各有各的暗号：ReadTimeout -1 是「不限时」、-2 是
	// 「连 deadline 都不设」，ConnMaxIdleTime -1 是「不按空闲回收」。本包没把它们写进
	// 文档，一个减号换来的是静默关掉超时保护，所以一律不收
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"DialTimeout", c.DialTimeout},
		{"ReadTimeout", c.ReadTimeout},
		{"WriteTimeout", c.WriteTimeout},
		{"PoolTimeout", c.PoolTimeout},
		{"ConnMaxIdleTime", c.ConnMaxIdleTime},
		{"ConnMaxLifetime", c.ConnMaxLifetime},
		{"SlowThreshold", c.SlowThreshold},
	} {
		if d.val < 0 {
			return fmt.Errorf("%s must not be negative, got=%v", d.name, d.val)
		}
	}

	// 写进文档的暗号只有这三个：MaxRetries -1、两个退避 -1ns，都是「关掉」。
	// go-redis 只认恰好 -1，别的负数原样留下，行为没人说得清
	if c.MaxRetries < -1 {
		return fmt.Errorf("MaxRetries must be -1 (disable retries) or >= 0, got=%d", c.MaxRetries)
	}
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"MinRetryBackoff", c.MinRetryBackoff},
		{"MaxRetryBackoff", c.MaxRetryBackoff},
	} {
		if d.val < 0 && d.val != -1 {
			return fmt.Errorf("%s must be -1ns (disable backoff) or >= 0, got=%v", d.name, d.val)
		}
	}
	return c.TLS.Validate()
}
