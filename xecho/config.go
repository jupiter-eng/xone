// Package xecho 按配置装好 Echo，使用者拿到的是原生的 *echo.Echo。
//
//	func main() {
//		ex := xecho.New().WithRoutes(registerRoutes)
//		xone.MustRun(ex)
//	}
//
// 内置访问日志、链路、指标、panic 恢复四个中间件，顺序由框架管（访问日志开着时
// 最外层另有一个 LogScope，给 xlog.AddKV 开请求级的字段作用域；链路关着时换成
// 只透传、不开 Span 的 Propagate）；
// 开关、监听地址、超时、TLS 都在配置文件的 XEcho 块里，业务代码里没有初始化。
//
// 访问日志的字段、指标名、Span 名和 xgin 的一样，client_ip 信谁、日志里遮什么、
// 退出时等不等 handler 也一样：这些写在两者共用的 internal/web 里。
package xecho

import (
	"fmt"
	"strings"
	"time"

	"github.com/jupiter-eng/xone/internal/web"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XEcho"

// Config 服务端配置
type Config struct {
	// Host 监听地址。默认 0.0.0.0。
	Host string `yaml:"Host"`

	// Port 监听端口。默认 8080。
	Port int `yaml:"Port"`

	// UseH2C 是否在非 TLS 下启用 HTTP/2。默认关闭。
	//
	// TLS 模式下 HTTP/2 本来就是自动的，不需要这一项。
	//
	// 只认「先验知识」：客户端一上来就发 HTTP/2 前言（gRPC、
	// curl --http2-prior-knowledge 都是）。HTTP/1.1 的 Upgrade: h2c 握手不支持，
	// 发它的客户端拿到的是一个普通的 HTTP/1.1 响应。
	UseH2C bool `yaml:"UseH2C"`

	// TLS 服务端证书和双向认证。默认不开：CertFile 和 KeyFile 都配了就是 https。
	TLS TLSConfig `yaml:"TLS"`

	// ReadHeaderTimeout 读请求头的超时。默认 10s，必须大于 0。
	//
	// 这是慢连接攻击的主要防线：不限制的话，慢客户端可以一直占着连接不放，
	// 连接数打满之后服务整体不可用。echo 自己的 e.Server 四个超时全是 0，
	// 本模块不用它，用的是按这几项建的 http.Server。
	//
	// 0 不是「用个默认值」：net/http 会退到 ReadTimeout，而那个默认也是 0，
	// 结果是不限时（实测发半个请求头的连接一直不被断开）。所以 0 和负数都拦住。
	ReadHeaderTimeout time.Duration `yaml:"ReadHeaderTimeout"`

	// ReadTimeout 读完整个请求（含 body）的超时。默认不限制。
	//
	// 默认不限制是因为它会打断大文件上传。按业务上限配一个值更好，
	// 但那个值只有业务自己知道。0 是不限制，负数启动失败。
	ReadTimeout time.Duration `yaml:"ReadTimeout"`

	// WriteTimeout 写响应的超时。默认不限制。
	//
	// 默认不限制是因为它会打断 SSE、长轮询和大文件下载。0 是不限制，负数启动失败。
	WriteTimeout time.Duration `yaml:"WriteTimeout"`

	// IdleTimeout keep-alive 连接的空闲超时。默认 60s，必须大于 0。
	//
	// 与 ReadHeaderTimeout 同理：0 退到 ReadTimeout（默认 0），等于空闲连接永不回收。
	IdleTimeout time.Duration `yaml:"IdleTimeout"`

	// TrustedProxies 信任哪些直连的对端：它们发来的 X-Forwarded-For / X-Real-IP 才用来算 client_ip
	// （c.RealIP()），它们发来的透传 Header（XTrace.ForwardHeaders / ForwardHeaderRules）和 baggage
	// 才会被收下、带给下游。「谁是自己人」只在这一处说。
	//
	// 默认是 [private]：回环、私有网段、100.64.0.0/10 和 IPv6 的 ::1、fc00::/7。
	// 负载均衡、K8s 的 Ingress 和 Pod、sidecar 都落在这些网段里，Pod IP 再随机也不用一个个写；
	// 直接暴露在公网的服务，对端是公网地址，一个都不信，X-Forwarded-For 照样伪造不了 client_ip。
	// echo 自己的默认（e.IPExtractor 为 nil）是谁发来的都信：实测公网对端 203.0.113.9
	// 发一个 X-Forwarded-For: 1.2.3.4，c.RealIP() 就是 1.2.3.4。
	//
	// client_ip 的算法和 xgin 一样（从右往左找第一个不可信的地址），同一个服务换框架不会变。
	//
	// 在默认之外再加网段时把 private 一起写上（列表整体替换默认值）；要谁都不信就写 []：
	//
	//	TrustedProxies: [private, 203.0.113.0/24]
	//	TrustedProxies: []
	//
	// 内网里也有不可信的客户端（办公网、VPN 用户能直连服务）时，别用 private，写确切的那几段。
	// IPv4 写成 10.0.0.1、10.0.0.0/8；::ffff:10.0.0.1 这种 IPv4 映射成 IPv6 的写法启动失败。
	TrustedProxies []string `yaml:"TrustedProxies"`

	// Log 是否启用访问日志中间件。默认启用。
	Log bool `yaml:"Log"`

	// LogSkipPaths 不记访问日志的路径。默认没有。
	//
	// 以 / 结尾的按前缀匹配，其余精确匹配。Metric 开着时 MetricPath 会自动加进来，不用自己写。
	LogSkipPaths []string `yaml:"LogSkipPaths"`

	// LogRequestBody 是否把请求体记进访问日志。默认不记。
	//
	// 记 body 要缓存请求体的前 256KB 并对每个字段做脱敏，代价和风险都不小。
	// 打开前先确认敏感词配全了（middleware.AddSensitiveFields，规则见 xecho/README.md「访问日志」）。
	LogRequestBody bool `yaml:"LogRequestBody"`

	// LogResponseBody 是否把响应体记进访问日志。默认不记。
	LogResponseBody bool `yaml:"LogResponseBody"`

	// LogQuery 是否把查询串记进访问日志（字段 query）。默认不记。
	//
	// 查询串里常有凭证：GET /login?token=...、签名链接、OAuth 回调的 code。
	// 打开后按字段脱敏，规则同表单 body；词表里没有的名字（比如 code）照样明文，
	// 用 middleware.AddSensitiveFields 补上。
	LogQuery bool `yaml:"LogQuery"`

	// LogRequestHeaders 是否把请求头记进访问日志（字段 request_headers）。默认不记。
	//
	// 打开后凭证类的值遮掉：Authorization、Cookie、X-Api-Key 等名单里的、名字带敏感词的；
	// 值是 URL 的（Referer）去掉查询串。名单外的业务头用 middleware.AddSensitiveHeaders 补上。
	LogRequestHeaders bool `yaml:"LogRequestHeaders"`

	// LogResponseHeaders 是否把响应头记进访问日志（字段 response_headers）。默认不记。
	//
	// 脱敏规则同请求头：Set-Cookie 等名单里的、名字带敏感词的，值都遮掉。
	LogResponseHeaders bool `yaml:"LogResponseHeaders"`

	// Trace 是否为入站请求开服务端 Span。默认启用。
	//
	// 只管 Span。关掉之后照样接上游传来的 traceparent、baggage 和透传 Header
	// （可信规则同 TrustedProxies），日志里的 trace_id 是上游的那条，
	// 经 xhttp 发出的请求也照样带给下游；响应里不再回带 X-Trace-Id。
	Trace bool `yaml:"Trace"`

	// Metric 是否启用指标中间件。默认启用。
	//
	// 启用时会自动注册 MetricPath（默认 /metrics）路由，不需要自己挂。
	Metric bool `yaml:"Metric"`

	// MetricPath 指标端点的路径。默认 /metrics，Metric 开着时必须以 / 开头。
	//
	// echo 不会拒绝别的写法，而是改写（实测 v4.16.0）：写 metrics 注册的是 /metrics，
	// 访问日志要跳过的却是 metrics，对不上；留空则注册在根路径 / 上，业务再注册首页时
	// 后注册的那个悄悄盖掉前一个——不是首页没了，就是指标没了。所以这两种都在启动时拦住。
	MetricPath string `yaml:"MetricPath"`
}

// TLSConfig 服务端 TLS，配置文件里是 TLS 块。
//
// 没有 Enable 开关：CertFile 和 KeyFile 都配了就开，都留空就是明文。
type TLSConfig struct {
	// CertFile TLS 证书路径。与 KeyFile 必须同时配或同时不配。
	CertFile string `yaml:"CertFile"`

	// KeyFile TLS 私钥路径。
	KeyFile string `yaml:"KeyFile"`

	// ClientCAFile 校验客户端证书用的 CA（PEM，可以放好几张）。默认空，不要客户端证书。
	//
	// 配了就是双向认证（tls.RequireAndVerifyClientCert）：客户端必须出示这个 CA 签的证书，
	// 不出示、或者不是它签的，握手就失败，请求到不了任何 handler——/metrics 这类
	// 框架挂的路由也一样。只能和 CertFile / KeyFile 一起配。
	ClientCAFile string `yaml:"ClientCAFile"`

	// MinVersion 接受的最低 TLS 版本："1.2" 或 "1.3"，留空同 "1.2"。默认 "1.2"。只在配了证书时生效，也只在那时校验。
	//
	// 更低的版本不收：TLS 1.0 / 1.1 早已被弃用（RFC 8996）。
	MinVersion string `yaml:"MinVersion"`
}

// DefaultConfig 全部默认值集中在这里
func DefaultConfig() Config {
	return Config{
		Host:              "0.0.0.0",
		Port:              8080,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		Log:               true,
		Trace:             true,
		Metric:            true,
		MetricPath:        "/metrics",
		TLS:               TLSConfig{MinVersion: "1.2"},
		TrustedProxies:    []string{web.TrustPrivate},
	}
}

// Validate 检查配置本身说不通的地方。
//
// xconfig.Unmarshal 解完配置文件里的 XEcho 块会调它，于是配错的配置在启动阶段
// 就失败，不必等到服务 Start；WithConfig 给的那份在装配时校验，见 XEcho.cfg。
func (c Config) Validate() error {
	// 端口、TLS、超时和代理网段的规矩是各 Web 集成共用的，写在 internal/web，报错和 xgin 一字不差
	s := c.server()
	if err := s.ValidateListen(); err != nil {
		return err
	}
	if err := s.ValidateTimeouts(); err != nil {
		return err
	}
	if err := web.ValidateProxies(c.TrustedProxies); err != nil {
		return err
	}
	// 理由见 MetricPath：echo 不拒绝，而是悄悄改写成另一个路径。
	// 关掉指标时这一项不用，写成什么都不影响
	if c.Metric && !strings.HasPrefix(c.MetricPath, "/") {
		return fmt.Errorf("MetricPath must start with /, got=%q", c.MetricPath)
	}
	return nil
}

// server 监听和优雅关闭要的那几项，交给 web.Server
func (c Config) server() web.ServerConfig {
	return web.ServerConfig{
		Host:              c.Host,
		Port:              c.Port,
		UseH2C:            c.UseH2C,
		CertFile:          c.TLS.CertFile,
		KeyFile:           c.TLS.KeyFile,
		ClientCAFile:      c.TLS.ClientCAFile,
		MinVersion:        c.TLS.MinVersion,
		ReadHeaderTimeout: c.ReadHeaderTimeout,
		ReadTimeout:       c.ReadTimeout,
		WriteTimeout:      c.WriteTimeout,
		IdleTimeout:       c.IdleTimeout,
	}
}
