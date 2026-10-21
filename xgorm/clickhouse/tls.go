package clickhouse

import (
	"crypto/tls"
	"fmt"
	"net/url"
	"strings"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"gorm.io/driver/clickhouse"
	"gorm.io/gorm"
)

// How the TLS block of the configuration reaches clickhouse-go.
//
// The driver's DSN can only express secure / skip_verify / tls_server_name and says nothing about
// a CA or a client certificate, so it does not go through the DSN: the DSN is parsed with the
// driver's own parser, the *tls.Config goes into Options, and clickhouse.OpenDB builds the pool
// handed to GORM. The DSN is untouched to the byte and no global is involved.

// tlsParams are the DSN parameters that speak about TLS (clickhouse-go v2.48.0 Options.fromDSN).
// With the TLS block on, not one of them may be written: two places speaking about TLS leaves no
// telling which one finally applies.
var tlsParams = []string{"secure", "skip_verify", "tls_server_name"}

// checkTLS: with the TLS block on, the DSN must not speak about TLS, and must not use http://.
//
// http:// is not "the HTTP protocol, with TLS decided separately": the driver records the scheme
// as it is and uses it to build the request address when opening an HTTP connection (v2.48.0
// conn_http.go dialHttp), so it sends plaintext HTTP even while holding a *tls.Config. Together
// with a TLS block it can therefore only be a misconfiguration.
func checkTLS(u *url.URL, q url.Values) error {
	for _, k := range tlsParams {
		if q.Has(k) {
			return fmt.Errorf("the DSN sets %s while the TLS block is enabled; configure TLS in one place only "+
				"(remove secure / skip_verify / tls_server_name from the DSN, or drop the TLS block)", k)
		}
	}
	if u.Scheme == "http" {
		return fmt.Errorf("the DSN uses http:// while the TLS block is enabled, and http:// never runs TLS; " +
			"use https:// for the HTTP protocol or clickhouse:// for the native protocol")
	}
	return nil
}

// parseDSN parses the DSN with the driver's parser. withTLS says the TLS block is on.
//
// The driver requires https:// to be written together with secure=true or it reports
// "https without TLS"; and a DSN with the TLS block on must not write secure (see checkTLS). So
// secure=true is added before parsing — only to the copy handed to the parser. The TLS settings it
// produces are then replaced wholesale by the TLS block's (see openTLS), so the added parameter
// has no effect of its own; it exists only to make the driver record the https scheme, which it
// uses to build addresses when opening an HTTP connection.
func parseDSN(dsn string, withTLS bool) (*chgo.Options, error) {
	if withTLS && strings.HasPrefix(dsn, "https://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return nil, err
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return nil, err
		}
		q.Set("secure", "true")
		u.RawQuery = q.Encode()
		dsn = u.String()
	}
	return chgo.ParseDSN(dsn)
}

// openTLS is ClickHouse's OpenTLS: parse the DSN, put cfg into the driver's Options, and build the
// pool with OpenDB.
//
// An unset ServerName is left empty and filled in by the standard library from the address of each
// connection: the native protocol goes through tls.DialWithDialer and the HTTP protocol through
// http.Transport, and both take the host part of whatever they connect to when ServerName is empty.
// Filling it in from the first host here would be wrong instead: with a multi-host DSN, connecting
// to the second host would still compare the certificate against the first host's name.
//
// The DSN is not handed to GORM's driver (the Config holds Conn alone):
// gorm.io/driver/clickhouse v0.7.0 given a DSN parses a second set of Options and keeps it, and an
// UPDATE with UpdateLocalTable goes by that copy to reach every host directly with
// clickhouse.Open (update.go), bypassing the pool — and that copy holds no TLS block, so those
// direct connections are plaintext. Without a DSN that path is never taken and an UPDATE goes out
// through the pool as usual.
//
// Version detection still moves to Ready, for the reason given under open. The pool is built right
// here (OpenDB touches no network), and any later failure in gorm.Open or xgorm's connection check
// is closed by xgorm through db.DB().
func openTLS(dsn string, cfg *tls.Config) (gorm.Dialector, error) {
	opts, err := parseDSN(dsn, true)
	if err != nil {
		return nil, errMalformedDSN // the driver's error is not passed on, for the reason given under resolve
	}
	opts.TLS = cfg.Clone()
	return clickhouse.New(clickhouse.Config{Conn: chgo.OpenDB(opts), SkipInitializeWithVersion: true}), nil
}
