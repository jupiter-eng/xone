// Package xhttp 按配置装好出站 HTTP 客户端，使用者拿到的是原生的 *resty.Client。
//
//	resp, err := xhttp.R(ctx).SetResult(&out).Get(url)
//
// 链路、指标、连接池、重试策略都由配置决定，业务代码里没有初始化。
package xhttp

import (
	"fmt"
	"time"

	"github.com/jupiter-eng/xone/xtls"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XHttp"

// Config 出站 HTTP 客户端配置
//
// 与 xgorm / xredis 不同，这里没有多实例：HTTP 客户端不连任何外部资源，
// 不配也能用，配一份公共的连接池和超时就够了。需要第二套参数的场景
// 直接用 New 自己建一个。
type Config struct {
	// Timeout 单次尝试的超时。默认 60s。
	//
	// 配 0 是「永不超时」，不是「用个默认值」：对端不响应时请求会一直挂着。
	//
	// 注意它管的是一次尝试，不是一次逻辑请求：开了 RetryCount 之后，
	// 最坏情况是 (RetryCount+1) × Timeout 再加上几次退避等待
	// （实测 Timeout=300ms、RetryCount=3 时整整 1.24s）。
	// 要给整个逻辑请求封顶，用调用方的 ctx——重试的退避和每次尝试都听它的：
	//
	//	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	//	defer cancel()
	//	resp, err := xhttp.C().R().SetContext(ctx).Get(url)
	Timeout time.Duration `yaml:"Timeout"`

	// DialTimeout 建立 TCP 连接的超时。默认 30s。
	DialTimeout time.Duration `yaml:"DialTimeout"`

	// DialKeepAlive TCP keep-alive 探测间隔。默认 30s。
	//
	// 这是唯一一个负值有意义的时长：标准库的 net.Dialer 用负值表示
	// 「不发探测」，所以它不在 Validate 的非负检查里。
	DialKeepAlive time.Duration `yaml:"DialKeepAlive"`

	// MaxIdleConns 全局最大空闲连接数。默认 100。
	MaxIdleConns int `yaml:"MaxIdleConns"`

	// MaxIdleConnsPerHost 每个 host 的最大空闲连接数。默认 10。
	//
	// 标准库的默认值是 2，对只调几个下游的服务来说太小，
	// 会让连接反复重建，尾延迟里全是握手。
	MaxIdleConnsPerHost int `yaml:"MaxIdleConnsPerHost"`

	// MaxConnsPerHost 每个 host 的连接数上限（含正在用的）。默认 0，不限制，与标准库一致。
	//
	// 不限制的意思是并发多少就开多少：实测对一个每次 300ms 才回的下游并发 200 个请求，
	// 它收到 200 条新连接；紧接着再来 200 个，只有 MaxIdleConnsPerHost 那 10 条复用得上，
	// 另开 190 条。下游一慢，连接数跟着并发一起涨，可能先耗尽对端的连接数或本机的端口。
	// 配成正数之后超出的请求排队等连接，等的时间算在 Timeout 和调用方的 ctx 里。
	MaxConnsPerHost int `yaml:"MaxConnsPerHost"`

	// IdleConnTimeout 空闲连接多久后关闭。默认 90s，与标准库一致。
	//
	// 下游（或中间的负载均衡）先于它关掉空闲连接时，恰好在那一刻复用这条连接的请求
	// 会失败：实测服务端空闲超时 200ms、请求间隔在 200ms 上下时，300 个 POST 失败 27 个
	// （connection reset by peer / use of closed network connection）；GET 由标准库自动
	// 在新连接上重发，200 个一个都没失败。所以它要小于下游的 keep-alive 超时。
	IdleConnTimeout time.Duration `yaml:"IdleConnTimeout"`

	// RetryCount 重试次数。默认 0，即不重试。
	RetryCount int `yaml:"RetryCount"`

	// RetryWaitTime 重试的起始等待时间。默认 100ms。
	RetryWaitTime time.Duration `yaml:"RetryWaitTime"`

	// RetryMaxWaitTime 重试等待时间的上限。默认 2s。
	RetryMaxWaitTime time.Duration `yaml:"RetryMaxWaitTime"`

	// RetryOnlyIdempotent 是否只重试幂等方法。默认开启。
	//
	// 传输层超时分不出「请求没到服务端」和「服务端处理完了但响应丢了」，
	// 重发一个 POST 就可能变成重复下单、重复扣款。
	// 确认接口幂等（比如带幂等键）之后再关掉它。
	//
	// 自己在 client 上 AddRetryCondition 挂的条件也放不回没拿到响应的 POST（xhttp 另在 RetryAfter 上否决）；
	// 放得回的只有拿到了响应之后按状态码重试的条件（比如 5xx）——那时否决只能把响应换成一个编出来的错误，
	// 所以这种条件里的方法要自己判断。自己 SetRetryAfter 会换掉这道关。
	// 不论开关，body 是 io.Reader 的请求都不重试：第一次尝试就把它读完了，重发的是空 body。
	RetryOnlyIdempotent bool `yaml:"RetryOnlyIdempotent"`

	// Trace 是否为出站请求开 Span。默认开启。
	//
	// 只管 Span。关掉之后 traceparent、baggage 和 XTrace.ForwardHeaders 里的
	// 透传 Header 照样带给下游（链路标识来自上游，或者本进程别处开的 Span）：
	// 这一跳不记 Span，不该让整条链路和透传在这里断掉。
	Trace bool `yaml:"Trace"`

	// Metric 是否导出出站请求的指标（按方法、目标、状态码分）。默认开启。
	Metric bool `yaml:"Metric"`

	// Log 是否每个逻辑请求记一条日志（所有重试结束之后记一次）。默认关闭。
	//
	// 记方法、host、路径、状态码、耗时、尝试次数，失败时加上错误；查询串、片段、userinfo、
	// Header、body 一律不记，错误原文里 URL 的查询串也去掉。resty 返回了错误（传输层错误、
	// SetResult 解不开、重定向被拒）和 5xx 记 warn。
	// 开着时 resty 自己在重试路径上的那几行（每次尝试一行 WARN、用完一行 ERROR）不再打，
	// 一个请求只有这一行；自己 SetLogger 的不受影响。SetDebug(true) 的请求转储不在此列，
	// 查询串和 Header 原样进日志。字段和实测见 xhttp/README.md「可观测」「行为与实测」。
	Log bool `yaml:"Log"`

	// SlowThreshold 整次逻辑请求（含重试和退避）超过这个耗时就记一条 warn 日志。默认 1s，需 Log 开启，配 0 不记。
	SlowThreshold time.Duration `yaml:"SlowThreshold"`

	// TLS 出站 https 请求的 TLS 设置：自签的 CA、双向认证的客户端证书、比对的名字。
	// 默认不配，用标准库的默认（系统根证书、不带客户端证书）。字段和规则各模块共用，见 xtls.Config。
	//
	// 只管 https:// 的请求，不会把 http:// 升级成 TLS。它对这个客户端发出的**每一个**
	// https 请求都生效：配了 CAFile 就只认这个 CA，公网上的 https 下游从此校验不过；
	// ServerName 更是拿同一个名字去比对每一个下游。所以这里配的是「只调一类内部下游」
	// 的那个客户端，要同时调公网的，另用 New 建一个。
	TLS xtls.Config `yaml:"TLS"`
}

// DefaultConfig 全部默认值集中在这里
func DefaultConfig() Config {
	return Config{
		Timeout:             60 * time.Second,
		DialTimeout:         30 * time.Second,
		DialKeepAlive:       30 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		RetryWaitTime:       100 * time.Millisecond,
		RetryMaxWaitTime:    2 * time.Second,
		RetryOnlyIdempotent: true,
		SlowThreshold:       time.Second,
		Trace:               true,
		Metric:              true,
	}
}

// Validate 检查配置本身说不通的地方。
//
// xconfig.Unmarshal 解完配置文件里的 XHttp 块会调它，配错的值在读配置时就失败、
// 带着是哪个文件哪一块；直接调 New 的，New 也会调一次。
// 返回普通 error，由调它的那一层包一次 xerror。
func (c Config) Validate() error {
	if c.RetryCount < 0 {
		return fmt.Errorf("RetryCount must not be negative, got=%d", c.RetryCount)
	}
	if c.MaxIdleConns < 0 || c.MaxIdleConnsPerHost < 0 || c.MaxConnsPerHost < 0 {
		return fmt.Errorf("connection counts must not be negative, MaxIdleConns=%d MaxIdleConnsPerHost=%d MaxConnsPerHost=%d",
			c.MaxIdleConns, c.MaxIdleConnsPerHost, c.MaxConnsPerHost)
	}
	// 负的时长没有一个说得通的含义，而且底下每个字段的反应都不一样：
	// DialTimeout 写成负数的话，拨号的 deadline 一开始就是过去的时间点，
	// 每一次请求当场 i/o timeout；Timeout 写成负数反而被标准库当成
	// 「不限时」（它只在 > 0 时才设 deadline），于是超时保护整个消失。
	// 两种都是一个减号换来的静默故障，配置文件看上去毫无问题
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"Timeout", c.Timeout},
		{"DialTimeout", c.DialTimeout},
		{"IdleConnTimeout", c.IdleConnTimeout},
		{"RetryWaitTime", c.RetryWaitTime},
		{"RetryMaxWaitTime", c.RetryMaxWaitTime},
		{"SlowThreshold", c.SlowThreshold},
	} {
		if d.val < 0 {
			return fmt.Errorf("%s must not be negative, got=%v", d.name, d.val)
		}
	}
	return c.TLS.Validate()
}
