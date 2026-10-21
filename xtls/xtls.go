// Package xtls holds the one block of TLS configuration the client integrations share.
//
// xgorm, xredis, xhttp and xkafka all have a TLS block of the same shape in their configuration:
//
//	TLS:
//	  Enable: true
//	  CAFile: /etc/ssl/internal-ca.pem   # empty uses the system roots
//	  CertFile: /etc/ssl/client.pem      # set together with KeyFile when the server wants mutual authentication
//	  KeyFile: /etc/ssl/client-key.pem
//	  ServerName: db.internal            # empty takes the host part of the connection address
//
// The fields, the defaults, the validation rules and the *tls.Config they build are identical
// across those modules, so they are written once, here. Callers only write it in the config file;
// xtls.Config appears in code only when calling a module's New directly.
//
// Only the few most common items are accepted, the minimum version is fixed at TLS 1.2 and
// certificate verification is always on -- **there is no switch to skip it**; a self-signed
// certificate goes into CAFile. For finer control (cipher suites, custom verification), go around
// the configuration and build a native client; the framework does not stand in the way.
//
// This package has no third-party dependencies.
package xtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// Config is the client side of a TLS setup. Its zero value means no TLS.
type Config struct {
	// Enable turns TLS on. It defaults to false. The fields below only take effect while it is on,
	// and setting them without it fails startup -- that is most likely a forgotten Enable, and
	// connecting in plaintext would be worse than an error.
	Enable bool `yaml:"Enable"`

	// CAFile is the CA certificate used to verify the server certificate (PEM, several are
	// allowed). It defaults to empty, which uses the system roots.
	//
	// When it is set, **only** the CAs in that file are accepted and the system roots no longer
	// take part. A self-signed certificate goes here.
	CAFile string `yaml:"CAFile"`

	// CertFile is the client certificate (PEM), set together with KeyFile when the server wants
	// mutual authentication. It defaults to empty.
	CertFile string `yaml:"CertFile"`

	// KeyFile is the client private key (PEM), set together with CertFile. It defaults to empty.
	KeyFile string `yaml:"KeyFile"`

	// ServerName is the name the server certificate is verified against. It defaults to empty,
	// which takes the host part of the connection address; set it when connecting by IP to a
	// certificate that names a domain.
	ServerName string `yaml:"ServerName"`
}

// Validate checks the combinations that do not make sense. Whether the files can be read is left
// to Build.
//
// Each module's Validate calls it, so a misconfigured value fails while the config is being read.
// It returns a plain error, which the layer calling it wraps in an xerror once.
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

// Build assembles a *tls.Config from the configuration; with TLS off it returns nil, which is how
// the caller ends up in plaintext.
//
// Reading the certificate files happens here: an unreadable file, or one holding no certificate,
// is reported here rather than at the first handshake. The ServerName returned is exactly the one
// from the configuration, and empty when it was not set -- the drivers that connect with it (pgx,
// filled in by xgorm; go-sql-driver; go-redis; net/http) each supply it from the connection
// address.
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
