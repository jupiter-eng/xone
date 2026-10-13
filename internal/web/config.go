package web

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"
)

// ServerConfig 起一个 HTTP 服务要的配置：监听地址、h2c、TLS、超时。
//
// 字段的含义、默认值和取舍写在各集成公开的 Config 上（xgin.Config），使用者读的是那里；
// 集成把自己 Config 里的这几项抄过来交给 Server.Start，校验也调这里的，报错因此处处一样。
type ServerConfig struct {
	Host   string
	Port   int
	UseH2C bool

	CertFile     string
	KeyFile      string
	ClientCAFile string
	MinVersion   string

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

// ValidateListen 检查端口和 TLS 这几项说不通的地方。
//
// 和 ValidateTimeouts 分成两个，是为了集成的 Validate 能把自己的检查插在中间
// （xgin 的 MaxMultipartMemory 就在这两者之间）：配错了好几项时报的是哪一个，由集成定
func (c ServerConfig) ValidateListen() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("Port must be within 1..65535, got=%d", c.Port)
	}
	// 只配一半的 TLS 是最危险的一种配错：服务会以明文起来，
	// 而配置文件看上去是配了证书的
	if (c.CertFile == "") != (c.KeyFile == "") {
		return fmt.Errorf("TLS.CertFile and TLS.KeyFile must both be set or both be empty")
	}
	// 同理：以为开了双向认证，实际是谁都能连的明文
	if c.ClientCAFile != "" && !c.tlsEnabled() {
		return fmt.Errorf("TLS.ClientCAFile requires TLS.CertFile and TLS.KeyFile, mutual TLS runs on top of TLS")
	}
	// 只在开了 TLS 时查：明文服务上它不起作用，一个用不上的值不该让服务起不来
	if _, ok := tlsVersions[c.MinVersion]; c.tlsEnabled() && !ok {
		return fmt.Errorf("unknown TLS.MinVersion=%q, supported: 1.2 / 1.3", c.MinVersion)
	}
	return nil
}

// ValidateTimeouts 检查四个超时。
func (c ServerConfig) ValidateTimeouts() error {
	// net/http 对这几项的规矩（Go 1.25 的文档如此，ReadHeaderTimeout 那条有测试钉着）：
	//   ReadHeaderTimeout 为 0 退到 ReadTimeout，负数不限时；
	//   IdleTimeout 为 0 退到 ReadTimeout，负数不限时；
	//   ReadTimeout / WriteTimeout 为 0 或负数都是不限时。
	// ReadTimeout 默认就是 0，所以前两项写 0 等于「不设防」：慢客户端发半个头
	// 就能一直占着连接，空闲的 keep-alive 连接永不回收——都是连接数被打满。
	// 配置文件看上去只是写了个 0，所以这里拦住。
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"ReadHeaderTimeout", c.ReadHeaderTimeout},
		{"IdleTimeout", c.IdleTimeout},
	} {
		if d.val <= 0 {
			return fmt.Errorf("%s must be > 0 (0 or negative disables it), got=%v", d.name, d.val)
		}
	}
	// 这两项的 0 是有意的「不限制」（默认值就是它）；负数跟 0 效果一样，
	// 却不像是想要「不限制」的写法，多半是写错了
	if c.ReadTimeout < 0 || c.WriteTimeout < 0 {
		return fmt.Errorf("ReadTimeout and WriteTimeout must not be negative (use 0 for unlimited), got ReadTimeout=%v WriteTimeout=%v", c.ReadTimeout, c.WriteTimeout)
	}
	return nil
}

// tlsEnabled 是否配了 TLS
func (c ServerConfig) tlsEnabled() bool { return c.CertFile != "" && c.KeyFile != "" }

// tlsVersions MinVersion 收的写法。留空取默认的 1.2，和不写这一项一样
var tlsVersions = map[string]uint16{"": tls.VersionTLS12, "1.2": tls.VersionTLS12, "1.3": tls.VersionTLS13}

// serverTLS 服务端的 TLS 设置，没配证书时是 nil。证书本身由 Server.Start 在监听前读（见 listen）。
//
// Go 1.25 的服务端默认最低也是 TLS 1.2，这里照样显式写上：默认值会随 Go 版本变，
// 配置文件里写着的 1.2 不该跟着变。
func (c ServerConfig) serverTLS() (*tls.Config, error) {
	if !c.tlsEnabled() {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tlsVersions[c.MinVersion]}
	if c.ClientCAFile != "" {
		pem, err := os.ReadFile(c.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("read TLS.ClientCAFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("TLS.ClientCAFile %s contains no PEM certificate", c.ClientCAFile)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}
