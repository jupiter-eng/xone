package xgorm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
)

// openMySQL builds MySQL's Dialector and turns off the version query it runs inside Initialize.
//
// That SELECT VERSION() uses a hard-coded context.Background() (gorm.io/driver/mysql v1.6.0
// mysql.go:130), and it runs inside gorm.Open, before xgorm's connection check with its retries —
// which makes it the real first connection. Measured (MySQL 8.0.46, with the peer accepting the
// connection and never answering): one connection was opened and gorm.Open reported
// invalid connection after 3.0s (the handshake read waiting out ReadTimeout), with none of the
// three retries ever reached; a stop signal during startup could not reach it either, and the
// process exited 2.95s after the signal.
//
// The version itself is still needed: the driver uses it to decide whether index and column names
// can be renamed on MariaDB / MySQL 5.x, whether FOR SHARE and DROP CONSTRAINT are supported, and
// whether to use RETURNING on MariaDB 10.5+. So that query moves into probeMySQLVersion and is
// done by the connection check: governed by the ctx and retried along with it. With it off,
// gorm.Open does sql.Open and assembly alone and touches no network.
func openMySQL(dsn string) gorm.Dialector {
	return mysql.New(mysql.Config{DSN: dsn, SkipInitializeWithVersion: true})
}

// resolveMySQL handles the DSN with the driver's own parser, leaving timeouts already written in
// the DSN untouched.
func resolveMySQL(c ClientConfig) (string, ConnInfo, error) {
	cfg, err := mysqldriver.ParseDSN(c.DSN)
	if err != nil {
		// The driver's error is not passed on: it carries fragments of the DSN in its message, and
		// messages get recorded
		return "", ConnInfo{}, errMalformedDSN
	}
	if c.TLS.Enable {
		if err := checkMySQLTLS(c.DSN, cfg); err != nil {
			return "", ConnInfo{}, err
		}
	}

	if cfg.Timeout == 0 {
		cfg.Timeout = c.DialTimeout
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = c.MySQL.ReadTimeout
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = c.MySQL.WriteTimeout
	}

	// parseTime defaults to false in the driver: DATETIME / TIMESTAMP come back as []byte and cannot
	// be scanned into a time.Time — measured with go-sql-driver v1.10.1 against MySQL 8.0.46, one
	// First on a model with CreatedAt gives unsupported Scan, storing driver.Value type []uint8 into
	// type *time.Time, while writing works fine. Almost every GORM model has a time field, so when
	// the DSN does not write parseTime it is filled in as true; when it does (false included) the
	// DSN wins. The timezone follows the driver's loc (UTC by default): on write the driver converts
	// to loc before formatting and on read it interprets by loc, so the same instant survives a round
	// trip (measured, writing 03:04:05+08:00 stores 19:04:05 and reads back 19:04:05 UTC, with Equal
	// true).
	if !mysqlParamSet(c.DSN, "parseTime") {
		cfg.ParseTime = true
	}

	// The budget is read after the injection: a value written in the DSN wins.
	// timeout covers connecting (dialing) and readTimeout covers every read after it: the handshake,
	// the authentication, and the probe's round trip.
	// (go-sql-driver v1.10.1 connector.go / connection.go)
	return cfg.FormatDSN(), ConnInfo{
		Driver: "mysql", Addr: cfg.Addr, DB: cfg.DBName,
		ProbeTimeout: mysqlProbeTimeout(cfg.Timeout, cfg.ReadTimeout),
	}, nil
}

// mysqlProbeTimeout is the budget for connecting plus one round trip.
//
// When either part is 0 (no limit in the driver), it is given the magnitude of the other: 0 says
// "the driver does not govern this", not "this part costs no time", and taking the connect timeout
// as the whole budget would, with ReadTimeout: 0, declare a timeout the moment the connection is
// up and the login packet has not come back yet. With both at 0 it returns 0 and probeTimeout
// falls back to its default.
func mysqlProbeTimeout(dial, read time.Duration) time.Duration {
	return cmp.Or(dial, read) + cmp.Or(read, dial)
}

// mysqlParamSet reports whether the DSN writes the parameter key at all, whatever its value.
//
// The Config the driver parsed cannot tell "not written" from "written as the default", so the raw
// string is inspected instead, the way the driver's ParseDSN does it (go-sql-driver v1.10.1
// dsn.go): the part after the last / and after the first ? is split on &, and each item is split on
// its first = into a key and a value. A password containing ?parseTime=false does not fool it:
// the password sits before the last /.
func mysqlParamSet(dsn, key string) bool {
	i := strings.LastIndexByte(dsn, '/')
	if i < 0 {
		return false
	}
	_, params, ok := strings.Cut(dsn[i+1:], "?")
	if !ok {
		return false
	}
	for _, kv := range strings.Split(params, "&") {
		if k, _, found := strings.Cut(kv, "="); found && k == key {
			return true
		}
	}
	return false
}

// mysqlAuthFailed matches error numbers 1045 and 1044, with a *mysql.MySQLError in the error chain.
//
// Measured with go-sql-driver v1.10.1 against MySQL 8.0.46: a wrong password and a missing user are
// both 1045 (Access denied for user …); a valid account without permission on that database is 1044
// (Access denied … to database …) — and when the database does not exist, an account without global
// permissions also receives 1044 rather than 1049, since the server does not tell it whether the
// database is there. SQLSTATE is not used: 1045's is 28000 while 1044's is 42000 (the class shared
// by syntax errors and permission errors).
func mysqlAuthFailed(err error) bool {
	var myErr *mysqldriver.MySQLError
	return errors.As(err, &myErr) && (myErr.Number == mysqlAccessDenied || myErr.Number == mysqlDBAccessDenied)
}

// MySQL's two authentication error numbers (ER_ACCESS_DENIED_ERROR, ER_DBACCESS_DENIED_ERROR)
const (
	mysqlAccessDenied   = 1045
	mysqlDBAccessDenied = 1044
)

// mysqlErrorCode is the error number the server reported.
//
// Measured on MySQL 8.0.46: the error text carries the parameter values out with it — 1062 is
// Duplicate entry 'a@b.com' for key 'm_err.email', 1366 is
// Incorrect integer value: 'notanint' for column 'n' at row 1, and 1292 is the same story.
func mysqlErrorCode(err error) string {
	var myErr *mysqldriver.MySQLError
	if errors.As(err, &myErr) {
		return strconv.Itoa(int(myErr.Number))
	}
	return ""
}

// probeMySQLVersion queries the server's version and sets the version-dependent switches by the
// driver's own rules.
//
// It is the MySQL dialect's Ready and runs after every successful Ping, see Dialect.Ready.
func probeMySQLVersion(ctx context.Context, db *gorm.DB) error {
	d, ok := db.Dialector.(*mysql.Dialector)
	if !ok {
		return nil // not a Dialector we built, so there is no switch to set
	}
	var v string
	if err := db.ConnPool.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v); err != nil {
		return err
	}
	return applyMySQLVersion(db, d.Config, v)
}

// applyMySQLVersion sets the few options older versions do not support, by version.
//
// The rules are copied from Initialize in gorm.io/driver/mysql v1.6.0 and have to be checked
// against it when upgrading the driver.
func applyMySQLVersion(db *gorm.DB, c *mysql.Config, v string) error {
	c.ServerVersion = v
	withReturning := false
	switch {
	case strings.Contains(v, "MariaDB"):
		c.DontSupportRenameIndex = true
		c.DontSupportRenameColumn = true
		c.DontSupportForShareClause = true
		c.DontSupportNullAsDefaultValue = true
		withReturning = versionAtLeast(v, "10.5")
	case strings.HasPrefix(v, "5.6."):
		c.DontSupportRenameIndex = true
		c.DontSupportRenameColumn = true
		c.DontSupportForShareClause = true
		c.DontSupportDropConstraint = true
	case strings.HasPrefix(v, "5.7."):
		c.DontSupportRenameColumn = true
		c.DontSupportForShareClause = true
		c.DontSupportDropConstraint = true
	case strings.HasPrefix(v, "5."):
		c.DisableDatetimePrecision = true
		c.DontSupportRenameIndex = true
		c.DontSupportRenameColumn = true
		c.DontSupportForShareClause = true
		c.DontSupportDropConstraint = true
	}
	if strings.Contains(v, "TiDB") {
		c.DontSupportRenameColumnUnique = true
	}
	if withReturning && !c.DisableWithReturning {
		return enableReturning(db)
	}
	return nil
}

// enableReturning makes inserts, updates and deletes carry RETURNING.
//
// When the driver registers its callbacks inside Initialize it has already closed over "whether
// RETURNING is supported", and at that point the version has not been queried, so what it
// registered is the unsupported variant. Here those three callbacks are replaced with the
// supported configuration -- gorm:create / gorm:update / gorm:delete (gorm v1.31.2
// callbacks/create.go:38) -- with the clause list matching what the driver assembles in Initialize.
func enableReturning(db *gorm.DB) error {
	cfg := &callbacks.Config{
		CreateClauses: append(slices.Clone(mysql.CreateClauses), "RETURNING"),
		UpdateClauses: append(slices.Clone(mysql.UpdateClauses), "RETURNING"),
		DeleteClauses: append(slices.Clone(mysql.DeleteClauses), "RETURNING"),
	}
	cb := db.Callback()
	if err := cb.Create().Replace("gorm:create", callbacks.Create(cfg)); err != nil {
		return err
	}
	if err := cb.Update().Replace("gorm:update", callbacks.Update(cfg)); err != nil {
		return err
	}
	if err := cb.Delete().Replace("gorm:delete", callbacks.Delete(cfg)); err != nil {
		return err
	}
	cb.Create().Clauses = cfg.CreateClauses
	cb.Update().Clauses = cfg.UpdateClauses
	cb.Delete().Clauses = cfg.DeleteClauses
	return nil
}

// versionAtLeast reports whether v is at least floor, comparing the numeric parts segment by
// segment. Copied from the driver's checkVersion.
func versionAtLeast(v, floor string) bool {
	if v == floor {
		return true
	}
	vs, ms := strings.Split(v, "."), strings.Split(floor, ".")
	for i, s := range vs {
		if i >= len(ms) {
			return true
		}
		a, b := leadingInt(s), leadingInt(ms[i])
		if a != b {
			return a > b
		}
	}
	return false
}

var leadingDigits = regexp.MustCompile(`^\d+`)

func leadingInt(s string) int {
	n, _ := strconv.Atoi(leadingDigits.FindString(s))
	return n
}

// mysqlDriverLogger routes go-sql-driver's own log into slog.
//
// go-sql-driver v1.10.1's default logger is log.New(os.Stderr, "[mysql] ", …), writing plain text
// to stderr (`[mysql] 2026/09/24 10:00:00 packets.go:58 read tcp …: i/o timeout`), which is not
// JSON unless it goes through slog and therefore never reaches the logging platform. Its lines
// carry no level and are all supplementary information about a failure (a failed packet read, a
// bad connection being dropped, an authentication plugin falling back) that the caller received as
// an error anyway, so they are all recorded as WARN. The driver passes no ctx, so these lines
// carry no trace_id.
type mysqlDriverLogger struct{}

func (mysqlDriverLogger) Print(v ...any) {
	slog.Warn("xgorm go-sql-driver log", "detail", strings.TrimSpace(fmt.Sprint(v...)))
}
