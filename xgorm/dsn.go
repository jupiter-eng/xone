package xgorm

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// There is no "mask the DSN" code in this file, deliberately.
//
// Masking presupposes putting the DSN into the log and then trying to cut the password out of it,
// which is work that is never finished: the URL form, the key=value form, a password holding an @
// or a space, the same key written twice, the fallback when parsing fails — each one is a possible
// leak.
//
// The approach here is never to print the DSN at all. The log carries only structured fields
// parsed out of it that definitely hold no credentials: the driver, the address, the database
// name. No password ever reached the log, so there is nothing to mask.

// errMalformedDSN / errMalformedQuery are what is reported when the DSN fails to parse.
//
// The parser's original error is not passed on: both url.Parse and the driver's ParseDSN carry
// fragments of the DSN in their message, and messages get recorded.
var (
	errMalformedDSN = errors.New("failed to parse DSN, check the format of " + ConfigKey +
		" (details omitted to keep credentials out of logs)")
	errMalformedQuery = errors.New("failed to parse the query part of the DSN, check the format of " + ConfigKey +
		" (a literal % in a password must be written as %25; details omitted to keep credentials out of logs)")
)

// unknownDriver reports an unknown driver and lists the registered ones.
//
// Saying "unknown" alone helps little: a misspelled driver name and a forgotten import of the
// matching module are two different problems, and listing what is actually registered tells them
// apart at a glance.
func unknownDriver(name Driver) error {
	return fmt.Errorf("unknown Driver=%q, registered: %v "+
		"(drivers other than mysql / postgres live in their own module, import it to register)",
		name, Drivers())
}

// openPostgres is PostgreSQL's Dialector constructor. MySQL's is in mysql.go.
func openPostgres(dsn string) gorm.Dialector { return postgres.Open(dsn) }

// postgresAuthFailed matches SQLSTATE class 28 (invalid authorization specification).
//
// Measured with pgx v5.10.0 against PG 16: a wrong password and a missing user both report 28P01
// (the latter also as "password authentication failed" under scram authentication), with a
// *pgconn.PgError in the error chain. A missing database is 3D000, outside this class, and is
// still treated as unreachable.
func postgresAuthFailed(err error) bool {
	return strings.HasPrefix(postgresErrorCode(err), "28")
}

// postgresErrorCode is the SQLSTATE the server reported.
//
// Measured on PG 16: PgError.Error() reads "ERROR: <Message> (SQLSTATE <Code>)", and Message may
// well hold parameter values (22P02 is invalid input syntax for type integer: "notanint"), with
// Detail holding even more (23505 is Key (email)=(a@b.com) already exists.) — except that Error()
// does not include Detail.
func postgresErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// resolvePostgres fills the timeouts and runtime parameters into the DSN as defaults.
//
// Both DSN forms are supported: the postgres://... URL form and libpq's key=value form. A key the
// caller wrote explicitly is never overwritten — the configuration only provides defaults.
func resolvePostgres(c ClientConfig) (string, ConnInfo, error) {
	// An empty value does not count: an unconfigured field is not injected, and an empty entry in
	// Params does not override the fields above either
	defaults := map[string]string{}
	put := func(k, v string) {
		if v != "" {
			defaults[k] = v
		}
	}
	put("connect_timeout", seconds(c.DialTimeout))
	put("statement_timeout", millis(c.Postgres.StatementTimeout))
	put("lock_timeout", millis(c.Postgres.LockTimeout))
	put("idle_in_transaction_session_timeout", millis(c.Postgres.IdleInTxTimeout))
	// Params wins over the fields above: for the same name, what the caller wrote explicitly stands
	for k, v := range c.Postgres.Params {
		put(k, v)
	}

	dsn := c.DSN
	if isPostgresURL(dsn) {
		var err error
		if dsn, err = injectPostgresURL(dsn, defaults); err != nil {
			return "", ConnInfo{}, err
		}
	} else {
		dsn = prependPostgresKV(dsn, defaults)
	}

	// What is checked is the DSN after the parameters were filled in: an sslmode written in
	// Postgres.Params counts as being in the DSN too.
	// Before parsePostgres: when an sslrootcert written in the DSN cannot be read, what should be
	// reported is "written in both places"
	if c.TLS.Enable {
		if err := checkPostgresTLSParams(dsn); err != nil {
			return "", ConnInfo{}, err
		}
	}
	pc, err := parsePostgres(dsn)
	if err != nil {
		return "", ConnInfo{}, err
	}
	if c.TLS.Enable {
		if err := checkPostgresTCP(pc); err != nil {
			return "", ConnInfo{}, err
		}
	}
	// The connection information is parsed by pgx itself: it is what really builds a connection from
	// this DSN, so both sides necessarily read the same thing — and a password containing a fragment
	// such as host=… cannot be read as connection information and written into the log. Without a
	// port this is the 5432 pgx filled in, and with several hosts it is the first one
	info := ConnInfo{
		Driver:       "postgres",
		Addr:         net.JoinHostPort(pc.Host, strconv.Itoa(int(pc.Port))),
		DB:           pc.Database,
		ProbeTimeout: postgresProbeTimeout(pc.ConnectTimeout, c.DialTimeout),
	}
	return dsn, info, nil
}

// postgresProbeTimeout is the budget for one probe: the connect_timeout that ends up in effect,
// plus one DialTimeout.
//
// connect is the connect_timeout pgx read out of the final DSN: without one written it is the
// injected value — DialTimeout rounded up to whole seconds (500ms by default becomes 1s). pgx uses
// it to govern the whole connection per host: the TLS handshake after TCP, startup and
// authentication are all inside it (pgconn v5.10.0 "restricts the whole connection process";
// measured against a server that accepts TCP instantly and never answers startup,
// connect_timeout=1 waited the full 1.0s). With DialTimeout alone as the budget, a slow but legal
// handshake would be declared timed out before pgx gave up on its own. The added DialTimeout
// covers the probe's own round trip.
func postgresProbeTimeout(connect, dial time.Duration) time.Duration {
	return cmp.Or(connect, ceilSeconds(dial)) + dial
}

// parsePostgres runs it through pgx's own parser and reports an error without the DSN when it
// cannot be parsed.
//
// Without this the same error would surface only when connecting, reported by pgx with the whole
// DSN in its text: pgx masks only the two tidy spellings password=x and password='x' (v5.10.0).
// Measured, password = hunter2 (with spaces around the equals sign) and a password with escaped
// quotes both reached the error New returned with the password in it — an error that goes to the
// log and to the alerts.
//
// It has to be pgx.ParseConfig rather than pgconn.ParseConfig: gorm's postgres driver calls the
// former when connecting (gorm.io/driver/postgres v1.6.3), and on top of pgconn it also validates
// three more settings: default_query_exec_mode, statement_cache_capacity and
// description_cache_capacity. Pre-checking with pgconn alone would let those three through
// misspelled; measured, password = hunter2 with default_query_exec_mode=bogus put the password
// verbatim into the error New returned.
//
// An error such as a certificate file that cannot be read is reported as is: it holds a file path
// and no credentials, and swallowing it would leave the caller with nothing but "the DSN failed to
// parse", unable to tell which file it was.
func parsePostgres(dsn string) (*pgconn.Config, error) {
	cc, err := pgx.ParseConfig(dsn)
	if err == nil {
		return &cc.Config, nil
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return nil, fmt.Errorf("cannot read a file named in the DSN: %w", pathErr)
	}
	return nil, errMalformedDSN
}

func isPostgresURL(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// injectPostgresURL appends parameters to the query of a URL-form DSN, keeping the keys already there.
//
// The URL form cannot have defaults prepended the way key=value can: pgx takes the first of a
// duplicated key in a query.
//
// The added parameters go after the original query, leaving what the caller wrote untouched to the
// byte. It used to parse and rewrite the whole thing with q.Encode(), which turned
// TimeZone=Europe/Berlin into Europe%2FBerlin: pgx decodes that, but gorm's postgres driver reads
// the raw string with its gormTimeZone regex without decoding, so every connection set the
// timezone to Europe%2FBerlin, which the server rejects, and all of them failed.
func injectPostgresURL(dsn string, injects map[string]string) (string, error) {
	if len(injects) == 0 {
		return dsn, nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		// The original error is likewise not passed on: url.Parse's error carries the whole DSN
		return "", errMalformedDSN
	}
	// An explicit ParseQuery rather than u.Query(): the latter swallows the error and returns only
	// the part it could parse. So a password containing a literal % (forming an invalid percent
	// escape) would have that entry silently dropped, and after writing it back the DSN holds no
	// password at all — the service reports "authentication failed" while the config file plainly
	// holds one.
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		// The original error is likewise not passed on: it carries the offending fragment, which is
		// most likely the credential itself
		return "", errMalformedQuery
	}

	pairs := []string{}
	if u.RawQuery != "" {
		pairs = append(pairs, u.RawQuery)
	}
	for _, k := range slices.Sorted(maps.Keys(injects)) {
		if !q.Has(k) {
			pairs = append(pairs, queryEscape(k)+"="+queryEscape(injects[k]))
		}
	}
	u.RawQuery = strings.Join(pairs, "&")
	return u.String(), nil
}

// queryEscape escapes by the query's rules while leaving / as it is.
//
// A / is legal in a query to begin with (RFC 3986) and it makes no difference whether pgx decodes
// it; turning it into %2F would make a TimeZone: Europe/Berlin in Params run into the same problem
// as above — gorm reads the raw string.
func queryEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "%2F", "/")
}

// gormTimeZone is the regex gorm's postgres driver uses to find the timezone in a DSN, copied
// verbatim (timeZoneMatcher in postgres.go, gorm.io/driver/postgres v1.6.3). It does not go
// through pgx's parser and takes the first match in the whole string.
var gormTimeZone = regexp.MustCompile("(time_zone|TimeZone|timezone)=(.*?)($|&| )")

// prependPostgresKV puts the defaults in front of a key=value form DSN.
//
// pgx takes the later of a key written twice (pgconn v5.10.0 parseKeywordValueSettings; measured,
// connect_timeout=1 … connect_timeout=9 reads as 9s), so what the caller wrote naturally counts.
// There is no need to understand this DSN first to know which keys they wrote: that would mean
// copying pgx's quoting and escaping rules, and one mistake in that copy is a silent failure such
// as "a connect_timeout= inside a password fooled the detection" or "connect_timeout = 10 went
// unrecognized and was overridden by the default" — both of which really happened.
//
// The timezone is the one exception: gorm takes the first match with the regex above, so a
// prepended one would win. So when the DSN already holds a timezone it recognizes, no timezone is
// prepended.
func prependPostgresKV(dsn string, defaults map[string]string) string {
	skipTimeZone := gormTimeZone.MatchString(dsn)
	pairs := []string{}
	for _, k := range slices.Sorted(maps.Keys(defaults)) {
		if skipTimeZone && gormTimeZone.MatchString(k+"=") {
			continue
		}
		pairs = append(pairs, k+"="+quoteKV(defaults[k]))
	}
	if len(pairs) == 0 {
		return dsn
	}
	return strings.Join(pairs, " ") + " " + dsn
}

// quoteKV quotes and escapes by libpq's rules when the value holds a space, a single quote or a
// backslash.
func quoteKV(v string) string {
	if v == "" {
		return "''"
	}
	if !strings.ContainsAny(v, ` '\`) {
		return v
	}
	e := strings.ReplaceAll(v, `\`, `\\`)
	e = strings.ReplaceAll(e, `'`, `\'`)
	return "'" + e + "'"
}

// seconds rounds up to whole seconds, since libpq's connect_timeout only accepts whole seconds
// with a minimum of 1.
func seconds(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return strconv.FormatInt(int64(ceilSeconds(d)/time.Second), 10)
}

// ceilSeconds rounds up to whole seconds, and is 0 for anything <= 0. The same value as the
// connect_timeout injected into the DSN.
func ceilSeconds(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(math.Ceil(d.Seconds())) * time.Second
}

// millis converts to whole milliseconds, which is what PG's timeout GUCs use.
// It rounds up for the reason seconds does: 0 means "no limit" for these GUCs, and rounding 500µs
// down to 0 would make the timeout silently disappear.
func millis(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return strconv.FormatInt(int64((d+time.Millisecond-1)/time.Millisecond), 10)
}
