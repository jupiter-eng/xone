// Package clickhouse adds the ClickHouse driver to xgorm.
//
// An anonymous import is all it takes, with no code to write:
//
//	import (
//		"github.com/jupiter-eng/xone/xgorm"
//		_ "github.com/jupiter-eng/xone/xgorm/clickhouse"
//	)
//
// Then write Driver: clickhouse in the configuration, and what you get is still a native *gorm.DB:
//
//	XGorm:
//	  Driver: clickhouse
//	  DSN: "${CH_DSN}"     # clickhouse://user:pass@host:9000/db
//	  TLS: {Enable: true, CAFile: /etc/ssl/ch-ca.pem}   # optional: both native and https:// use it, see tls.go
//
// Why it is its own module: measured, an application importing only xgorm has a module graph of 68
// (excluding the application itself), and adding this package takes it to 131 (the
// non-standard-library packages in go list -deps go from 165 to 208; clickhouse-go v2.48.0).
// Most of the difference is Docker (moby) and testcontainers — clickhouse-go uses them for its
// integration tests, and since go.mod cannot tell "test only" apart they sit in the main require
// block and travel to every caller. Go's MVS imposes version requirements through the module
// graph, and whoever does not use ClickHouse should not pay for it.
//
// The driver version is worth watching: the go.mod of gorm.io/driver/clickhouse v0.6.1 still
// carried 126 stale cloud.google.com/* indirect entries, giving a module graph of 733; v0.7.0
// dropped it straight to 146 (clickhouse-go was v2.30.0 then, and v2.48.0 brought it to 128; with
// xtrace, xgorm makes it 132).
package clickhouse

import (
	"cmp"
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	chproto "github.com/ClickHouse/ch-go/proto"
	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/hashicorp/go-version"
	"gorm.io/driver/clickhouse"
	"gorm.io/gorm"

	"github.com/jupiter-eng/xone/xgorm"
)

// Driver is the value the configuration's Driver has to hold
const Driver xgorm.Driver = "clickhouse"

// dialTimeoutKey is the query parameter a ClickHouse DSN uses for the connect timeout
const dialTimeoutKey = "dial_timeout"

// driverDialTimeout is what the driver uses when dial_timeout is 0 (clickhouse-go v2.48.0
// clickhouse_options.go setDefaults, which Open / OpenDB / Connector all go through). Check it
// against the driver when upgrading.
const driverDialTimeout = 30 * time.Second

// schemes are the URL schemes the driver recognizes. http / https use the HTTP protocol and the
// rest use native.
var schemes = []string{"clickhouse://", "tcp://", "http://", "https://"}

// The errors reported when a DSN cannot be parsed. The DSN is never echoed back: both url.Parse's
// and the driver's parse errors carry DSN fragments, and a DSN usually carries a password.
var (
	errNotURL = errors.New("DSN must be a URL starting with clickhouse://, tcp://, http:// or https:// " +
		"(details omitted to keep credentials out of logs)")
	errMalformedDSN = errors.New("failed to parse DSN, check the format of " + xgorm.ConfigKey +
		" (details omitted to keep credentials out of logs)")
	errMalformedQuery = errors.New("failed to parse the query part of the DSN, check the format of " + xgorm.ConfigKey +
		" (a literal % in a password must be written as %25; details omitted to keep credentials out of logs)")
)

// dialect is the one registered into xgorm. It is a variable of its own so tests can see which
// functions it wired up.
var dialect = xgorm.Dialect{
	Name:       Driver,
	Open:       open,
	OpenTLS:    openTLS,
	Resolve:    resolve,
	Ready:      probeVersion,
	AuthFailed: authFailed,
	ErrorCode:  errorCode,
}

// init only registers and initializes nothing. The actual connecting is done by xgorm in the
// framework's StageClient.
func init() { xgorm.RegisterDialect(dialect) }

// open builds the Dialector and turns off the version query it runs inside Initialize.
//
// That SELECT version() uses a hard-coded context.Background() (Initialize in
// gorm.io/driver/clickhouse v0.7.0) and runs inside gorm.Open: measured against an address that
// accepts the connection and never answers, it waited out the full dial_timeout before returning
// even with the ctx long cancelled (300ms configured, 300ms waited), and a failure was reported
// outright — none of xgorm's three connection retries was ever reached, so one hiccup kept the
// service from starting.
//
// The version itself is still needed: the driver uses it to decide whether column names can be
// renamed on older versions (< 20.4) and whether column types carry precision (< 21.11), and
// without the query a migration generates DDL an older cluster does not accept.
// So that query moves into probeVersion and is done by xgorm during the connection check: governed
// by the ctx and retried along with it.
func open(dsn string) gorm.Dialector {
	return clickhouse.New(clickhouse.Config{DSN: dsn, SkipInitializeWithVersion: true})
}

// probeVersion queries the server's version and sets those two switches by the driver's own rules.
//
// The rules are copied from v0.7.0's Initialize and have to be checked against it when upgrading
// the driver.
func probeVersion(ctx context.Context, db *gorm.DB) error {
	d, ok := db.Dialector.(*clickhouse.Dialector)
	if !ok {
		return nil // not a Dialector we built, so there is no switch to set
	}
	var v string
	if err := db.ConnPool.QueryRowContext(ctx, "SELECT version()").Scan(&v); err != nil {
		return err
	}
	d.Version = v
	applyVersion(d.Config, v)
	return nil
}

// applyVersion sets the two options older versions do not support, by version
func applyVersion(c *clickhouse.Config, v string) {
	parsed, err := version.NewVersion(v)
	if err != nil {
		return // unparsable means treating it as a new version, as the driver does
	}
	noRename, _ := version.NewConstraint("< 20.4")
	noPrecision, _ := version.NewConstraint("< 21.11")
	if noRename.Check(parsed) {
		c.DontSupportRenameColumn = true
	}
	if noPrecision.Check(parsed) {
		c.DontSupportColumnPrecision = true
	}
}

// resolve injects the configured connect timeout into the DSN and parses out the connection
// information that is safe to record.
//
// Only the URL form is accepted (clickhouse://user:pass@host:9000/db?k=v) and everything else is
// rejected. The driver does not accept a bare host:port either (measured,
// clickhouse.ParseDSN("10.255.255.1:9000") reports
// first path segment in URL cannot contain colon), and passing it through unchanged would put the
// driver's parse error, with the whole DSN and its plaintext password, into the log. A misspelled
// scheme (clickhous://) the driver does accept, but it then skips the timeout injection here, and
// a leading space is another parse error carrying the whole DSN — all of them are stopped here.
func resolve(c xgorm.ClientConfig) (string, xgorm.ConnInfo, error) {
	if !isURL(c.DSN) {
		return "", xgorm.ConnInfo{}, errNotURL
	}

	u, err := url.Parse(c.DSN)
	if err != nil {
		// The original error is not passed on: url.Parse's error carries the whole DSN, and errors get recorded
		return "", xgorm.ConnInfo{}, errMalformedDSN
	}

	// An explicit ParseQuery rather than u.Query(): the latter swallows the error and returns only
	// the part it could parse. So a password containing a literal % (forming an invalid percent
	// escape) would have that entry silently dropped, and after writing it back the DSN holds no
	// password at all — the service reports "authentication failed" while the config file plainly
	// holds one.
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", xgorm.ConnInfo{}, errMalformedQuery
	}
	if c.TLS.Enable {
		if err := checkTLS(u, q); err != nil {
			return "", xgorm.ConnInfo{}, err
		}
	}

	// What the caller wrote explicitly in the DSN is never overwritten — the configuration only
	// provides defaults.
	// A DialTimeout of 0 injects nothing; it is written as a duration string ClickHouse accepts
	// (300ms, 5s).
	// It is appended after the original query, leaving what the caller wrote untouched to the byte
	// (as xgorm's injectPostgresURL does): parsing and re-encoding with q.Encode() would reorder the
	// parameters by key and turn a,b/c into a%2Cb%2Fc.
	if c.DialTimeout > 0 && !q.Has(dialTimeoutKey) {
		u.RawQuery = appendQuery(u.RawQuery, dialTimeoutKey, c.DialTimeout.String())
	}
	dsn := u.String()

	// Run it through the driver's own parser as well: its errors can likewise carry credentials (a
	// failed http_proxy parse echoes the whole proxy address back), and leaving it until connecting
	// time puts it into the log verbatim.
	// What is reported here is one sentence with no content; the common case is https:// without
	// secure=true (which is not needed with the TLS block on, see parseDSN).
	opts, err := parseDSN(dsn, c.TLS.Enable)
	if err != nil {
		return "", xgorm.ConnInfo{}, errMalformedDSN
	}

	// The budget is computed from the dial_timeout the driver read: a longer one written in the DSN
	// is what widens xgorm's connection check budget. The driver uses it for the TCP connection and,
	// during the handshake, as the deadline of the whole connection (v2.48.0 conn_handshake.go);
	// there is no separate setting for a round trip, so one more of the same magnitude is added.
	// With neither written, ParseDSN reads 0 and the driver fills in driverDialTimeout when
	// connecting: the budget follows the filled-in value, since taking 0 would leave xgorm with its
	// 1s fallback and let the connection check time out a slow handshake the driver was still
	// willing to wait for.
	//
	// The address is the first one the driver parsed: in the multi-host form
	// clickhouse://u:p@h1:9000,h2:9000/db the URL's Host is the whole "h1:9000,h2:9000", which the
	// driver splits on the comma and connects to in turn (in_order by default). Taking the whole
	// string as the address would make the span's server.address that whole string with no
	// server.port (SplitHostPort cannot parse it), and the connection log would not hold a
	// "host:port" either. As with PostgreSQL's multi-host form, the first one is recorded.
	// ParseDSN errors on an empty Host, so by this point Addr holds at least one entry.
	//
	// The database name likewise comes from the driver: it can be written in the path and in
	// ?database=, and the query overrides the path (v2.48.0 clickhouse_options.go fromDSN), so going
	// by the path alone would put a db in the log that is not the one actually connected to.
	return dsn, xgorm.ConnInfo{
		Driver:       string(Driver),
		Addr:         opts.Addr[0],
		DB:           opts.Auth.Database,
		ProbeTimeout: 2 * cmp.Or(opts.DialTimeout, driverDialTimeout),
	}, nil
}

// appendQuery appends one key=value after the raw query, leaving raw as it is
func appendQuery(raw, key, value string) string {
	pair := url.QueryEscape(key) + "=" + url.QueryEscape(value)
	if raw == "" {
		return pair
	}
	return raw + "&" + pair
}

// authCodes are the error codes for a server rejecting the credentials, taken from ch-go's typed
// constants (named as in the server's src/Common/ErrorCodes.cpp): a newer server reports a wrong
// password and a missing user both as 516 AUTHENTICATION_FAILED, while 192 / 193 / 194 are the
// three an older one reported separately.
//
// During the native protocol's handshake the server answers with an Exception packet, which the
// driver returns as a *clickhouse.Exception as it is (exception() in v2.48.0 conn.go), reachable
// with errors.As on the chain. Over HTTP the driver parses a non-200 response into a
// *clickhouse.HTTPError wrapping that same *clickhouse.Exception (v2.48.0 conn_http_errors.go),
// which is recognized just as well.
// Measured (e2e, ClickHouse 24.8.14), a wrong password and a missing user are code: 516 under both
// protocols and are tried once (HTTP's status is 403); a missing database is 81 and is not here.
// v2.30.0's HTTP error was a single assembled string and was not recognized.
var authCodes = []chproto.Error{
	chproto.ErrAuthenticationFailed, // 516
	chproto.ErrUnknownUser,          // 192
	chproto.ErrWrongPassword,        // 193
	chproto.ErrRequiredPassword,     // 194
}

// authFailed reports whether the server rejected these credentials, see authCodes
func authFailed(err error) bool {
	var ex *chgo.Exception
	return errors.As(err, &ex) && slices.Contains(authCodes, chproto.Error(ex.Code))
}

// errorCode is the error code the server reported. The text (Exception.Message) may hold parameter
// values — an input it could not parse is quoted back verbatim — so the log and the span record the
// code alone.
// Over HTTP the chain holds a *clickhouse.Exception as well (see authCodes); anything unparsable (a
// 502 from a proxy, say) is left to xgorm to treat as a client-side error: network errors and ctx
// cancellation are recorded verbatim and the rest only says the text was withheld.
func errorCode(err error) string {
	var ex *chgo.Exception
	if errors.As(err, &ex) {
		return strconv.Itoa(int(ex.Code))
	}
	return ""
}

// isURL reports whether this is the URL form the driver recognizes
func isURL(dsn string) bool {
	return slices.ContainsFunc(schemes, func(p string) bool { return strings.HasPrefix(dsn, p) })
}
