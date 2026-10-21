package web

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"
)

// ServerConfig is what starting an HTTP server takes: the listen address, h2c, TLS and the
// timeouts.
//
// What the fields mean, their defaults and the trade-offs behind them are written on each
// integration's public Config (xgin.Config), which is what a caller reads; an integration copies
// these few fields out of its own Config into Server.Start and calls the validation here, so the
// errors are the same everywhere.
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

// ValidateListen checks the port and the TLS fields for what does not add up.
//
// It is split from ValidateTimeouts so that an integration's Validate can put its own checks in
// between (xgin's MaxMultipartMemory sits between the two): with several fields misconfigured,
// which one is reported is the integration's decision
func (c ServerConfig) ValidateListen() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("Port must be within 1..65535, got=%d", c.Port)
	}
	// Half-configured TLS is the most dangerous misconfiguration of all: the server comes up in
	// plaintext while the configuration file looks as though a certificate was configured
	if (c.CertFile == "") != (c.KeyFile == "") {
		return fmt.Errorf("TLS.CertFile and TLS.KeyFile must both be set or both be empty")
	}
	// Likewise: mutual TLS looks turned on while it is really plaintext anybody can connect to
	if c.ClientCAFile != "" && !c.tlsEnabled() {
		return fmt.Errorf("TLS.ClientCAFile requires TLS.CertFile and TLS.KeyFile, mutual TLS runs on top of TLS")
	}
	// Checked only with TLS on: it has no effect on a plaintext server, and a value that does
	// nothing should not keep the server from starting
	if _, ok := tlsVersions[c.MinVersion]; c.tlsEnabled() && !ok {
		return fmt.Errorf("unknown TLS.MinVersion=%q, supported: 1.2 / 1.3", c.MinVersion)
	}
	return nil
}

// ValidateTimeouts checks the four timeouts.
func (c ServerConfig) ValidateTimeouts() error {
	// net/http's rules for these (as Go 1.25's documentation says, with the ReadHeaderTimeout one
	// pinned by a test):
	//   a ReadHeaderTimeout of 0 falls back to ReadTimeout, and a negative one is unlimited;
	//   an IdleTimeout of 0 falls back to ReadTimeout, and a negative one is unlimited;
	//   a ReadTimeout / WriteTimeout of 0 or less is unlimited.
	// ReadTimeout is 0 by default, so writing 0 for the first two means "no defence at all": a slow
	// client sending half a header holds a connection forever, and an idle keep-alive connection is
	// never reclaimed -- both fill the connection count up.
	// In the configuration file it looks like nothing more than a 0, which is why it is caught
	// here.
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
	// For these two a 0 is a deliberate "unlimited" (and is the default); a negative value has the
	// same effect while not looking like a way to ask for "unlimited", so it is most likely a
	// mistake
	if c.ReadTimeout < 0 || c.WriteTimeout < 0 {
		return fmt.Errorf("ReadTimeout and WriteTimeout must not be negative (use 0 for unlimited), got ReadTimeout=%v WriteTimeout=%v", c.ReadTimeout, c.WriteTimeout)
	}
	return nil
}

// tlsEnabled reports whether TLS is configured
func (c ServerConfig) tlsEnabled() bool { return c.CertFile != "" && c.KeyFile != "" }

// tlsVersions are the spellings MinVersion accepts. Left empty it takes the default of 1.2, as
// leaving the field out does
var tlsVersions = map[string]uint16{"": tls.VersionTLS12, "1.2": tls.VersionTLS12, "1.3": tls.VersionTLS13}

// serverTLS is the server's TLS setup, nil with no certificate configured. The certificate itself
// is read by Server.Start before it listens (see listen).
//
// Go 1.25's server defaults to a minimum of TLS 1.2 as well, and it is written out here all the
// same: a default changes with the Go version, while a 1.2 written in the configuration file must
// not.
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
