package xgorm

import (
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// ConnInfo is the connection information that is safe to write into the log.
//
// Those few fields parsed out of the DSN that definitely hold no credentials. This package never
// prints a DSN, so there is nothing to mask — the reasoning is at the top of dsn.go.
type ConnInfo struct {
	Driver string // the driver name, mysql for instance
	Addr   string // host:port
	DB     string // the database name

	// ProbeTimeout is the budget for one probe during the connection check: connecting (handshake
	// and authentication included) plus one round trip.
	// 0 means the dialect could not work it out, and xgorm uses 2 × DialTimeout, falling back to 1s
	// when that is 0 too.
	//
	// The dialect works it out per driver from the timeouts it read out of the final DSN: when the
	// caller wrote a longer timeout in the DSN than the configuration says, the driver waits that
	// long and the budget has to widen with it, rather than declaring a slow but legal connection
	// timed out and retrying before the driver gave up on its own.
	ProbeTimeout time.Duration
}

// Dialect is one database dialect.
//
// mysql and postgres are built in. Other drivers come from modules of their own, register
// themselves in their init, and need nothing beyond an anonymous import from the caller:
//
//	import _ "github.com/jupiter-eng/xone/xgorm/clickhouse"
//
// They are separate modules because a driver is heavy. Measured, an application importing only
// xgorm has a module graph of 68 (excluding the application itself), and adding the ClickHouse
// driver takes it to 131 (the non-standard-library packages in go list -deps go from 165 to 208,
// clickhouse-go v2.48.0) — most of the difference being Docker and testcontainers, because
// clickhouse-go writes the ones its integration tests use into the main require block of its own
// go.mod, and go.mod cannot tell "test only" apart. Go's MVS is exactly what imposes those version
// requirements on the caller through the module graph, even when they use MySQL alone and import
// not one ClickHouse package.
type Dialect struct {
	// Name is the driver name, the value the configuration's Driver has to hold
	Name Driver

	// Open builds GORM's Dialector
	Open func(dsn string) gorm.Dialector

	// OpenTLS is Open, except that the connection always uses the TLS cfg gives: xgorm calls it
	// instead of Open when TLS.Enable is on in the configuration.
	// It may be left empty, and configuring a TLS block for a driver that left it empty is a
	// configuration error (its TLS is written in its own DSN).
	//
	// cfg comes from xtls.Config.Build, with ServerName empty when it was not configured; filling it
	// in from the connection address is this function's business.
	// No plaintext escape route may be left outside cfg: whoever configured TLS wants "fail when TLS
	// cannot be established".
	// A DSN that also writes TLS parameters is reported as a configuration error by Resolve and never
	// reaches here.
	// It returns a plain error, which xgorm wraps into an xerror with op config; likewise, never pass
	// on an original error carrying DSN fragments.
	OpenTLS func(dsn string, cfg *tls.Config) (gorm.Dialector, error)

	// Resolve injects the configuration's timeouts and other parameters into the DSN, and parses out
	// the connection information that is safe to record.
	//
	// Leaving it empty means the DSN is used as it is and the connection information holds the
	// driver name alone — a reasonable starting point for a driver somebody just wants running,
	// since timeouts written into the DSN work all the same.
	// Filling ProbeTimeout into the connection information makes the connection check use it as its
	// budget; without it the budget is 2 × DialTimeout.
	//
	// A plain error is enough: xgorm wraps it into one xerror at the module boundary (op config).
	// When the DSN cannot be parsed, do not pass on the parser's original error: it carries DSN
	// fragments, and error messages get recorded.
	Resolve func(c ClientConfig) (dsn string, info ConnInfo, err error)

	// Ready is called after the connection check passes, giving the driver a ctx-governed place for
	// its own initialization queries. It may be left empty.
	//
	// Some Dialectors query the server version once inside Initialize using a hard-coded
	// context.Background(): a stop signal cannot reach it, and a failure never reaches the retries,
	// because it happens inside gorm.Open, before xgorm's connection check. Such a driver turns that
	// query off in Open and moves it here: it then runs with every Ping, retries with it, and is
	// bound by the ctx.
	// It returns a plain error, which xgorm wraps into an xerror with op connect.
	Ready func(ctx context.Context, db *gorm.DB) error

	// AuthFailed reports whether this error is the server rejecting these credentials. It may be
	// left empty, which means it recognizes none.
	//
	// What it recognizes is not retried by the connection check and is reported as
	// "authentication to <addr> failed"; what it does not is retried as usual and reported as
	// "cannot reach <addr>".
	AuthFailed func(error) bool

	// ErrorCode is the error code of a server-side error (PostgreSQL's SQLSTATE, MySQL's error
	// number), and an empty string when the error did not come from the server. It may be left empty.
	//
	// The SQL log's error field and the span's status record this code alone, never the error text:
	// a server's error text carries the parameter values out with it — measured, MySQL 8.0's 1062 is
	// Duplicate entry 'a@b.com' for key 'm_err.email' and PG 16's 22P02 is
	// invalid input syntax for type integer: "notanint". The error returned to the caller is
	// unchanged. Left empty, a server-side error is not recognized and the log and the span record
	// the text as it is.
	ErrorCode func(error) string
}

// dialects holds the registered dialects.
//
// With a lock rather than a bare map: registering only inside package inits would indeed not need
// one — every init runs first, on one goroutine, and main starts reading afterwards. But
// RegisterDialect is exported and nothing stops it being called at runtime while New is reading the
// table on another goroutine, and a concurrent map read and write is the kind of error that crashes
// outright.
var (
	dialectMu sync.RWMutex
	dialects  = map[Driver]Dialect{}
)

// RegisterDialect registers a driver.
//
// Registering the same name twice panics outright: when two Dialects claim one name, whichever is
// chosen may connect a service somewhere it does not believe it is connecting. That is a problem of
// import time and should not be left until runtime.
func RegisterDialect(d Dialect) {
	if d.Name == "" {
		panic("xgorm: Dialect.Name must not be empty")
	}
	if d.Open == nil {
		panic(fmt.Sprintf("xgorm: Dialect %q has no Open", d.Name))
	}

	dialectMu.Lock()
	defer dialectMu.Unlock()
	if _, dup := dialects[d.Name]; dup {
		panic(fmt.Sprintf("xgorm: Dialect %q is already registered", d.Name))
	}
	dialects[d.Name] = d
}

// dialector picks Open or OpenTLS depending on whether TLS is configured.
// A dialect without OpenTLS configured with TLS was already rejected by ClientConfig.Validate.
func (d Dialect) dialector(dsn string, cfg *tls.Config) (gorm.Dialector, error) {
	if cfg == nil {
		return d.Open(dsn), nil
	}
	return d.OpenTLS(dsn, cfg)
}

// resolve injects the configuration's timeouts and other parameters into the DSN and parses out
// the connection information that is safe to record. How exactly is the dialect's Resolve business.
func (d Dialect) resolve(c ClientConfig) (string, ConnInfo, error) {
	if d.Resolve == nil {
		// No parsing logic provided: the DSN is used as it is, and the log can only name the driver
		return c.DSN, ConnInfo{Driver: string(c.Driver)}, nil
	}
	return d.Resolve(c)
}

// authFailed asks the dialect whether it recognizes this as an authentication failure; a dialect
// that provides none recognizes nothing.
func (d Dialect) authFailed(err error) bool {
	return err != nil && d.AuthFailed != nil && d.AuthFailed(err)
}

// errorCode is the server's error code, empty when the dialect provides none or the error did not
// come from the server.
func (d Dialect) errorCode(err error) string {
	if err == nil || d.ErrorCode == nil {
		return ""
	}
	return d.ErrorCode(err)
}

// redactedError is the error text that goes into the log and onto the span: a server-side error
// keeps its error code alone, and a client-side one is recorded verbatim only for the few kinds
// clientSafeError recognizes, with everything else becoming clientErrorOmitted.
//
// Client-side errors carry parameter values too, as measured: pgx v5.10.0 writes a parameter it
// cannot encode into the error whole with %#v (unable to encode xgorm.rvSecret{Token:"..."} into
// text format for text (OID 25)), and database/sql's failed scan writes the value it read back
// with %q (converting driver.Value type []uint8 ("...") to a int). So, as in xredis, this is an
// allowlist rather than a denylist guessing which ones are dangerous. The error returned to the
// caller is unchanged.
func (d Dialect) redactedError(err error) (text, code string) {
	if code = d.errorCode(err); code != "" {
		return fmt.Sprintf("%s error %s (message omitted, it may contain parameter values)", d.Name, code), code
	}
	if safe := clientSafeError(err); safe != nil {
		return safe.Error(), ""
	}
	return clientErrorOmitted, ""
}

// clientSafeError is the first error in the chain whose text may be recorded as it is: network
// errors (the address and the timeout, which is exactly what debugging needs), ctx cancellation and
// timeouts, and the sentinel errors of database/sql, the drivers and GORM itself, whose text is
// fixed. nil when there is none.
//
// It returns the one it recognized in the chain rather than the whole chain's text: the layers
// wrapped around it are not necessarily clean — GORM's AddError prefixes the previous error's text
// with "%v; %w" (gorm v1.31.2 gorm.go AddError), so when the later one is ErrInvalidData the whole
// chain's text still carries the parameter values of the earlier one.
func clientSafeError(err error) error {
	var ne net.Error
	if errors.As(err, &ne) {
		return ne
	}
	for _, safe := range safeErrors {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return nil
}

// safeErrors are the sentinel errors whose text is fixed and carries no value
var safeErrors = []error{
	context.Canceled, context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF,
	sql.ErrConnDone, sql.ErrTxDone, sql.ErrNoRows, driver.ErrBadConn, driver.ErrSkip,
	mysqldriver.ErrInvalidConn, mysqldriver.ErrMalformPkt, mysqldriver.ErrNoTLS, mysqldriver.ErrPktSync,
	mysqldriver.ErrPktSyncMul, mysqldriver.ErrPktTooLarge, mysqldriver.ErrBusyBuffer,
	gorm.ErrRecordNotFound, gorm.ErrInvalidTransaction, gorm.ErrNotImplemented, gorm.ErrMissingWhereClause,
	gorm.ErrUnsupportedRelation, gorm.ErrPrimaryKeyRequired, gorm.ErrModelValueRequired,
	gorm.ErrModelAccessibleFieldsRequired, gorm.ErrSubQueryRequired, gorm.ErrInvalidData, gorm.ErrUnsupportedDriver,
	gorm.ErrRegistered, gorm.ErrInvalidField, gorm.ErrEmptySlice, gorm.ErrDryRunModeUnsupported, gorm.ErrInvalidDB,
	gorm.ErrInvalidValue, gorm.ErrInvalidValueOfLength, gorm.ErrPreloadNotAllowed, gorm.ErrDuplicatedKey,
	gorm.ErrForeignKeyViolated, gorm.ErrCheckConstraintViolated,
}

// clientErrorOmitted is what the log and the span record for a client-side error outside the kinds
// clientSafeError recognizes
const clientErrorOmitted = "client error (message omitted, it may contain parameter values)"

// lookupDialect takes a registered dialect.
func lookupDialect(name Driver) (Dialect, bool) {
	dialectMu.RLock()
	defer dialectMu.RUnlock()
	d, ok := dialects[name]
	return d, ok
}

// Drivers returns the driver names currently registered, in alphabetical order.
//
// It answers "is Driver spelled right, and is the matching module imported?" — the only way to
// tell a misspelled name from a forgotten import when the driver is misconfigured.
func Drivers() []Driver {
	dialectMu.RLock()
	defer dialectMu.RUnlock()
	return slices.Sorted(maps.Keys(dialects))
}

// The two built-in ones. Filled into the map directly rather than registered in an init, so that
// "built in" and "registered from outside" are told apart at a glance in the source.
func init() {
	dialects[DriverMySQL] = Dialect{
		Name: DriverMySQL, Open: openMySQL, OpenTLS: openMySQLTLS, Resolve: resolveMySQL, Ready: probeMySQLVersion,
		AuthFailed: mysqlAuthFailed, ErrorCode: mysqlErrorCode,
	}
	dialects[DriverPostgres] = Dialect{
		Name: DriverPostgres, Open: openPostgres, OpenTLS: openPostgresTLS, Resolve: resolvePostgres,
		AuthFailed: postgresAuthFailed, ErrorCode: postgresErrorCode,
	}
}
