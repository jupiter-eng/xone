// Package xtls 是各客户端集成共用的那一块 TLS 配置。
//
// xgorm、xredis、xhttp、xkafka 的配置里都有一个同样形状的 TLS 块：
//
//	TLS:
//	  Enable: true
//	  CAFile: /etc/ssl/internal-ca.pem   # 不填用系统根证书
//	  CertFile: /etc/ssl/client.pem      # 服务端要求双向认证时和 KeyFile 成对填
//	  KeyFile: /etc/ssl/client-key.pem
//	  ServerName: db.internal            # 不填取连接地址的主机部分
//
// 字段、默认值、校验规则、装出来的 *tls.Config 在这几个模块里一模一样，
// 所以只在这里写一次。使用者只在配置文件里写它；直接调某个模块的 New 时
// 才会在代码里写到 xtls.Config。
//
// 只收最常用的几项，最低版本固定 TLS 1.2，证书校验一直开着，
// **没有跳过校验的开关**——自签证书把 CA 填进 CAFile。要更细的控制（加密套件、
// 自定义校验）就绕开配置、自己造原生 client，框架不挡路。
//
// 本包零第三方依赖。
package xtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// Config 客户端这一侧的 TLS 设置。零值就是不走 TLS
type Config struct {
	// Enable 是否走 TLS。默认 false。下面几项只在开着时生效，没开却写了它们，启动失败——
	// 那多半是忘了开，照明文连过去比报错更糟。
	Enable bool `yaml:"Enable"`

	// CAFile 校验服务端证书用的 CA 证书（PEM，可以放好几张）。默认空，用系统的根证书。
	//
	// 填了就**只**认这个文件里的 CA，系统根证书不再参与校验。自签的证书填这里。
	CAFile string `yaml:"CAFile"`

	// CertFile 客户端证书（PEM），服务端要求双向认证时和 KeyFile 成对填。默认空。
	CertFile string `yaml:"CertFile"`

	// KeyFile 客户端私钥（PEM），和 CertFile 成对填。默认空。
	KeyFile string `yaml:"KeyFile"`

	// ServerName 校验服务端证书时比对的名字。默认空，取连接地址的主机部分；
	// 按 IP 连、而证书上写的是域名时填它。
	ServerName string `yaml:"ServerName"`
}

// Validate 检查几项之间说不通的地方。文件读不读得出来留给 Build。
//
// 各模块的 Validate 会调它，于是配错的值在读配置时就失败。
// 返回普通 error，由调它的那一层包一次 xerror。
func (c Config) Validate() error {
	if !c.Enable {
		if c != (Config{}) {
			return errors.New("TLS fields are set but TLS.Enable is false; set TLS.Enable: true or remove them")
		}
		return nil
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return errors.New("TLS.CertFile and TLS.KeyFile must be set together")
	}
	return nil
}

// Build 按配置装出 *tls.Config；没开 TLS 时返回 nil，调用方由此走明文。
//
// 读证书文件就在这里：读不出来、里面没有证书，都是这里报错，而不是等到第一次握手。
// 返回的 ServerName 就是配置里的那个，没填时是空的——拿它去连的驱动
// （pgx 由 xgorm 代填、go-sql-driver、go-redis、net/http）各自按连接地址补上。
func (c Config) Build() (*tls.Config, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if !c.Enable {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read TLS.CAFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("TLS.CAFile %s contains no PEM certificate", c.CAFile)
		}
		cfg.RootCAs = pool
	}
	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS.CertFile / TLS.KeyFile: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}
