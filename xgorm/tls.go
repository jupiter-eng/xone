package xgorm

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// How the TLS block of the configuration reaches the two built-in drivers.
//
// Neither goes through the DSN: a DSN can only express the parameters the driver knows and cannot
// say "verify against this name" (pgx has no such parameter, and verify-full compares against the
// host alone), so it cannot carry xtls.Config's ServerName; and passing one to MySQL through the
// DSN would mean registering the *tls.Config under a process-wide name with RegisterTLSConfig,
// one per instance, to be deregistered on close. Here the *tls.Config is handed straight to the
// driver's connection configuration: the DSN is untouched to the byte and no global is involved.

// openPostgresTLS is PostgreSQL's OpenTLS.
//
// The DSN is still parsed by gorm's postgres driver (so its timezone handling stays its own), and
// the only change is replacing the TLS in pgx's connection configuration with ours before each
// connection (stdlib.OptionBeforeConnect, which hands over a shallow copy, so modifying it affects
// no other connection).
func openPostgresTLS(dsn string, cfg *tls.Config) (gorm.Dialector, error) {
	return postgres.New(postgres.Config{
		DSN: dsn,
		OptionOpenDB: []stdlib.OptionOpenDB{stdlib.OptionBeforeConnect(func(_ context.Context, cc *pgx.ConnConfig) error {
			usePostgresTLS(&cc.Config, cfg)
			return nil
		})},
	}), nil
}

// usePostgresTLS makes every host use nothing but the TLS cfg gives.
//
// pgx translates sslmode into a list of "host × TLS configuration" candidates (pgconn v5.10.0
// configTLS): the default, prefer, tries TLS without certificate verification once per host and
// then plaintext, and allow is the other way round. Here the hosts are deduplicated with one entry
// each and the TLS replaced by cfg — the plaintext entry is gone, so when the server refuses TLS
// pgx reports "server refused TLS connection" rather than quietly falling back to plaintext.
// An unset ServerName is filled in from the host name, as verify-full does.
func usePostgresTLS(c *pgconn.Config, cfg *tls.Config) {
	type target struct {
		host string
		port uint16
	}
	seen := map[target]bool{}
	var hosts []*pgconn.FallbackConfig
	for _, h := range postgresHosts(c) {
		if t := (target{h.Host, h.Port}); !seen[t] {
			seen[t] = true
			tc := cfg.Clone()
			if tc.ServerName == "" {
				tc.ServerName = h.Host
			}
			hosts = append(hosts, &pgconn.FallbackConfig{Host: h.Host, Port: h.Port, TLSConfig: tc})
		}
	}
	c.Host, c.Port, c.TLSConfig = hosts[0].Host, hosts[0].Port, hosts[0].TLSConfig
	c.Fallbacks = hosts[1:]
}

// postgresHosts are all the hosts in a connection configuration: the primary address first,
// followed by the candidates pgx generated (Fallbacks).
func postgresHosts(c *pgconn.Config) []*pgconn.FallbackConfig {
	return append([]*pgconn.FallbackConfig{{Host: c.Host, Port: c.Port}}, c.Fallbacks...)
}

// checkPostgresTLSParams: with the TLS block on, the DSN must not write TLS parameters as well.
func checkPostgresTLSParams(dsn string) error {
	key, err := postgresTLSParam(dsn)
	if err != nil {
		return err
	}
	if key != "" {
		return fmt.Errorf("the DSN sets %s while the TLS block is enabled; configure TLS in one place only "+
			"(remove the ssl* parameters from the DSN, or drop the TLS block)", key)
	}
	return nil
}

// checkPostgresTCP: with the TLS block on, every host has to go over TCP, since pgx does no TLS on
// a Unix socket (pgconn v5.10.0 ParseConfig, following libpq). pc is the connection configuration
// parsed out of this DSN.
func checkPostgresTCP(pc *pgconn.Config) error {
	for _, h := range postgresHosts(pc) {
		if network, _ := pgconn.NetworkAddress(h.Host, h.Port); network == "unix" {
			return fmt.Errorf("the TLS block is enabled but host %s is a Unix socket, TLS only runs over TCP", h.Host)
		}
	}
	return nil
}

// disallowedKey is what pgconn says when a key is not in the allowed list, with the key written
// at the end as %q.
// (pgconn v5.10.0 ParseConfigWithOptions)
var disallowedKey = regexp.MustCompile(`connection string key (".*") is not in ConnStringAllowedKeys$`)

// postgresTLSParam is the first TLS parameter written in the DSN (a key starting with ssl), or an
// empty string when there is none.
//
// The DSN is read with pgx's own parser rather than copying its quoting and escaping rules here
// (the reasoning is under prependPostgresKV): ConnStringAllowedKeys makes pgx report the name of
// any key outside the list, and it does so before reading any file. Starting from an empty list,
// every reported key that does not start with ssl is added and parsing is retried, until it either
// parses (no TLS parameter written) or reports one starting with ssl. The key itself carries no
// credentials and may go into the error; the rest of the error (the whole DSN) is not passed on.
//
// Environment variables (PGSSLMODE and friends) and TLS settings in a service file do not count:
// they are not in the DSN, and with the TLS block on they are all overridden by it (see
// usePostgresTLS).
func postgresTLSParam(dsn string) (string, error) {
	allowed := []string{}
	for {
		_, err := pgconn.ParseConfigWithOptions(dsn, pgconn.ParseConfigOptions{ConnStringAllowedKeys: allowed})
		if err == nil {
			return "", nil
		}
		key, ok := disallowed(err)
		if !ok || slices.Contains(allowed, key) {
			// parsePostgres already parsed this DSN successfully, so reaching here can only mean pgx
			// changed the wording of that message
			return "", errors.New("cannot tell whether the DSN sets TLS parameters (details omitted to keep credentials out of logs)")
		}
		if strings.HasPrefix(key, "ssl") {
			return key, nil
		}
		allowed = append(allowed, key)
	}
}

// disallowed extracts the rejected key out of pgconn's error.
func disallowed(err error) (string, bool) {
	var pce *pgconn.ParseConfigError
	if !errors.As(err, &pce) {
		return "", false
	}
	m := disallowedKey.FindStringSubmatch(err.Error())
	if m == nil {
		return "", false
	}
	key, uerr := strconv.Unquote(m[1])
	return key, uerr == nil
}

// checkMySQLTLS: with the TLS block on, the DSN must not write tls= or
// allowFallbackToPlaintext= either, and must not use a Unix socket.
//
// allowFallbackToPlaintext=true means "fall back to plaintext when the server's handshake packet
// does not carry CLIENT_SSL" (go-sql-driver v1.10.1 packets.go readHandshakePacket). That bit
// arrives in plaintext, so a man in the middle clearing it makes the client hand over the login
// packet and every SQL statement after it in the clear — even with the TLS block on. Writing false
// is rejected too: like tls=, this parameter belongs in exactly one place.
func checkMySQLTLS(dsn string, cfg *mysqldriver.Config) error {
	if mysqlParamSet(dsn, "tls") {
		return fmt.Errorf("the DSN sets tls while the TLS block is enabled; configure TLS in one place only " +
			"(remove tls= from the DSN, or drop the TLS block)")
	}
	if mysqlParamSet(dsn, "allowFallbackToPlaintext") {
		return fmt.Errorf("the DSN sets allowFallbackToPlaintext while the TLS block is enabled; " +
			"the TLS block never falls back to plaintext (remove allowFallbackToPlaintext= from the DSN)")
	}
	if cfg.Net != "tcp" && cfg.Net != "tcp6" && cfg.Net != "tcp4" {
		return fmt.Errorf("the TLS block is enabled but the DSN connects over %s, TLS only runs over TCP", cfg.Net)
	}
	return nil
}

// openMySQLTLS is MySQL's OpenTLS: parse the DSN, put cfg into the driver's connection
// configuration, and build the pool through a connector.
//
// An unset ServerName is filled in by the driver from the host part of Addr (go-sql-driver
// v1.10.1 Config.normalize, which NewConnector calls). Version detection still moves to Ready, for
// the reason given under openMySQL.
//
// The pool is built right here: when gorm.Open's Initialize fails it closes it for us, and any
// failure after that is closed by open (the same one db.DB() returns, as on the Open path).
func openMySQLTLS(dsn string, cfg *tls.Config) (gorm.Dialector, error) {
	dc, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		return nil, errMalformedDSN // the driver's error is not passed on, for the reason given under resolveMySQL
	}
	dc.TLS = cfg.Clone()
	// A backstop: checkMySQLTLS already rejected this parameter in the DSN, and pinning it here
	// again means that when the server (or a man in the middle) withholds CLIENT_SSL the result is
	// ErrNoTLS rather than a fallback to plaintext
	dc.AllowFallbackToPlaintext = false
	connector, err := mysqldriver.NewConnector(dc)
	if err != nil {
		return nil, errMalformedDSN
	}
	return mysql.New(mysql.Config{
		Conn:                      sql.OpenDB(connector),
		DSNConfig:                 dc,
		SkipInitializeWithVersion: true,
	}), nil
}
