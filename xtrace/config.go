// Package xtrace 按配置装好 OpenTelemetry 的全局 TracerProvider 与 Propagator。
//
// 使用者拿到的是原生的 OpenTelemetry：业务代码直接用 otel.Tracer("...") 开 Span，
// 不需要认识本包。本包只有两件事是自己的：
//
//   - AddSpanProcessor —— 挂上报用的 exporter（框架不内置任何一个）
//   - HeaderPropagator —— 按配置透传自定义 Header，如 X-Request-Id
package xtrace

import (
	"fmt"
	"math"
	"time"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XTrace"

// ForwardHeaderRule 按域名透传的 Header 规则。
// 仅当目标域名匹配 Domains 中任一模式时，才透传对应的 Headers。
type ForwardHeaderRule struct {
	// Domains 域名模式，支持精确匹配和通配前缀（*.example.com）
	Domains []string `yaml:"Domains"`

	// Headers 该规则下要透传的 Header
	Headers []string `yaml:"Headers"`
}

// Config 链路配置
type Config struct {
	// Enable 是否开启链路。默认开启。
	//
	// 关掉之后 Span 仍然能创建（是个 noop），TraceID 不再生成，
	// 但 Header 透传照常生效——那是两件事。
	Enable bool `yaml:"Enable"`

	// Console 是否把 Span 打到标准输出，仅用于本地调试。默认关闭。
	Console bool `yaml:"Console"`

	// SampleRatio 根 Span 的采样率，取值 [0, 1]。默认 1。越界或 NaN 启动失败。
	//
	// 只管「没有上游」的那些链路：上游传来了采样决定（traceparent 的 sampled 位）
	// 就照它的来，采样率是 1 也不例外——否则上游刻意丢掉的链路会在这里被捡回来。
	//
	// 写 0 是「新链路一条都不采」，但 Span 照常创建、TraceID 照常生成和透传——
	// 想让下游拿得到 TraceID、本地又不落 Span 时就这么配。
	// 要连 Span 都不产生（noop provider）请用 Enable: false，那是另一件事。
	SampleRatio float64 `yaml:"SampleRatio"`

	// ShutdownTimeout 关闭时等待 Span 导出完成的上限。默认 5s，必须大于 0。
	//
	// 必须有上限：导出端不可达时 Shutdown 会一直阻塞，
	// 没有 deadline 就是退出时挂死。
	//
	// 同样地，0 不是「不限时」而是「一点都不等」：Shutdown 拿到的是一个
	// 已经过期的 context，缓冲区里还没发出去的 Span 会被直接丢掉。
	ShutdownTimeout time.Duration `yaml:"ShutdownTimeout"`

	// ForwardHeaders 向所有域名透传的 Header。默认无。
	//
	// 只收可信对端发来的值：用 xgin 时就是 XGin.TrustedProxies 里的那些（xecho 是 XEcho.TrustedProxies），
	// 默认只信私有网段（负载均衡、K8s 的 Ingress 和 Pod），公网直连的不收。理由见 HeaderPropagator.Extract。
	ForwardHeaders []string `yaml:"ForwardHeaders"`

	// ForwardHeaderRules 按域名透传的 Header 规则。默认无。
	//
	// 用于不该外泄的内部标识：只发给自己人，不发给第三方。
	// 与 ForwardHeaders 一样，只收可信对端发来的值。
	ForwardHeaderRules []ForwardHeaderRule `yaml:"ForwardHeaderRules"`
}

// DefaultConfig 全部默认值集中在这里。
func DefaultConfig() Config {
	return Config{
		Enable:          true,
		SampleRatio:     1,
		ShutdownTimeout: 5 * time.Second,
	}
}

// Validate 检查配置本身说不通的地方，透传 Header 的规则也在内。
//
// xconfig.Unmarshal 解完配置文件里的 XTrace 块会调它，配错的值在读配置时就失败；
// 直接调 New 的，New 也会调一次。
//
// 这里返回普通 error，由调它的那一层（xconfig.Unmarshal 或 New）包一次 xerror：
// 每层都包的话，消息里会套出两层 xtrace。
func (c Config) Validate() error {
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("ShutdownTimeout must be > 0 "+
			"(0 is not unlimited, it is no wait at all, and buffered spans get dropped), got=%v", c.ShutdownTimeout)
	}
	// NaN 跟任何数比较都是 false，所以要单独判：不拦的话 TraceIDRatioBased
	// 会拿它算出一个说不清的阈值
	if math.IsNaN(c.SampleRatio) || c.SampleRatio < 0 || c.SampleRatio > 1 {
		return fmt.Errorf("SampleRatio must be within [0, 1], got=%v", c.SampleRatio)
	}
	// 通配写错、一个头两边都写，都是决定内部标识发给谁的配置，读配置时就拦下
	if c.forwardEnabled() {
		if _, err := newHeaderPropagator(c.ForwardHeaders, c.ForwardHeaderRules); err != nil {
			return err
		}
	}
	return nil
}

// forwardEnabled 是否配置了 Header 透传
func (c Config) forwardEnabled() bool {
	return len(c.ForwardHeaders) > 0 || len(c.ForwardHeaderRules) > 0
}
