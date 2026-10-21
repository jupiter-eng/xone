// Package xgorm assembles GORM from configuration; the caller gets the native *gorm.DB.
//
//	err := xgorm.CWithCtx(ctx).First(&u, id).Error
//
// MySQL and PostgreSQL are supported, and the connection pool, the timeouts, the slow query log,
// tracing and the pool metrics are all decided by configuration, with no initialization in
// business code.
package xgorm

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xtls"
)

// ConfigKey is this module's top-level key in the config file.
const ConfigKey = "XGorm"

// DefaultName is the instance C() takes without arguments.
const DefaultName = xconfig.DefaultClientName

// Driver is the database driver.
type Driver string

const (
	DriverMySQL    Driver = "mysql"
	DriverPostgres Driver = "postgres"
)

// Config is this module's configuration. Two forms:
//
//	XGorm:                  # a single instance: write the fields directly and the name is default
//	  DSN: "${DB_DSN}"
//
//	XGorm:                  # several instances, written by name
//	  Clients:
//	    default: {DSN: "${DB_DSN}"}
//	    report:  {DSN: "${REPORT_DSN}", MaxOpenConns: 5}
type Config struct {
	// Clients holds the instances by name. The single-instance form is normalized into one instance
	// named default.
	Clients map[string]ClientConfig
}

// ClientConfig is the configuration of one database instance.
type ClientConfig struct {
	// Driver is the driver: mysql / postgres. Defaults to postgres.
	//
	// Other drivers live in modules of their own and register themselves on an anonymous import:
	//
	//	import _ "github.com/jupiter-eng/xone/xgorm/clickhouse"
	Driver Driver `yaml:"Driver"`

	// DSN is the connection string. Required.
	//
	// Writing it as "${DB_DSN}" is recommended: credentials do not belong in version control, and a
	// missing one then fails startup.
	//
	// A MySQL DSN that does not write parseTime gets parseTime=true: the driver defaults to false
	// and a DATETIME cannot be scanned into a time.Time (measured with go-sql-driver v1.10.1, one
	// First on a model with CreatedAt reports unsupported Scan). A DSN that writes it wins.
	DSN string `yaml:"DSN"`

	// DialTimeout is the connect timeout. Defaults to 500ms.
	//
	// It is injected as timeout for MySQL and as connect_timeout for PostgreSQL (rounded up to whole
	// seconds). At 0 nothing is injected and the driver's own applies (both drivers: no limit). It
	// must not be negative, and neither may any duration below.
	DialTimeout time.Duration `yaml:"DialTimeout"`

	// MySQL applies only when Driver is mysql.
	MySQL MySQLConfig `yaml:"MySQL"`

	// Postgres applies only when Driver is postgres.
	Postgres PostgresConfig `yaml:"Postgres"`

	// TLS decides whether the database connection uses TLS. Off by default (TLS parameters written
	// in the DSN still apply). The fields and rules are shared by the modules, see xtls.Config.
	//
	// With it on, TLS is decided entirely by this block: certificates are always verified and there
	// is **no fallback to plaintext**. Writing TLS parameters in the DSN as well (PostgreSQL's
	// sslmode, sslrootcert and the other ssl-prefixed ones, MySQL's tls) is a configuration error —
	// two places saying different things means whichever wins makes the other pointless.
	// How the drivers take it, and the measured behavior, are in xtls/README.md.
	TLS xtls.Config `yaml:"TLS"`

	// MaxOpenConns is the maximum number of connections. Defaults to 50.
	MaxOpenConns int `yaml:"MaxOpenConns"`

	// MaxIdleConns is the maximum number of idle connections. Defaults to 50.
	//
	// 0 means "keep not one idle connection", so every query reconnects, which is usually not what
	// you want. Larger than MaxOpenConns is fine, since database/sql caps it at MaxOpenConns itself.
	MaxIdleConns int `yaml:"MaxIdleConns"`

	// MaxLifetime is how long a connection may live. Defaults to 5m, and 0 means no limit.
	MaxLifetime time.Duration `yaml:"MaxLifetime"`

	// MaxIdleTime is how long an idle connection may live. Defaults to 5m, and 0 means no limit.
	MaxIdleTime time.Duration `yaml:"MaxIdleTime"`

	// Log routes GORM's SQL log into slog. Off by default.
	//
	// When a statement fails, the error field records the server's error code alone (with a separate
	// error_code field) and never its text: measured, MySQL 8.0's 1062 reads
	// Duplicate entry 'a@b.com' for key …, with the parameter value right in it. The error returned
	// to the caller is unchanged.
	Log bool `yaml:"Log"`

	// SlowThreshold logs a warn line for any statement slower than this. Defaults to 3s, needs Log
	// on, and 0 records none.
	SlowThreshold time.Duration `yaml:"SlowThreshold"`

	// IgnoreNotFound decides whether "no record found" is logged as an error. false by default.
	IgnoreNotFound bool `yaml:"IgnoreNotFound"`

	// DisableForeignKeyConstraintWhenMigrating keeps AutoMigrate from creating foreign key
	// constraints. false by default, which is GORM's default: foreign keys are created from the
	// associations in the models.
	//
	// It affects migration only, and queries and associations are unchanged: measured with GORM
	// v1.31.2 and PostgreSQL 16 on the two tables of Owner has many Order, the default creates
	// fk_owners_orders and an order with a nonexistent owner_id is rejected; with it on no foreign
	// key is created, that row inserts fine, and Preload("Orders") still finds it. Referential
	// integrity is your code's business from then on. Foreign keys already created are not dropped.
	DisableForeignKeyConstraintWhenMigrating bool `yaml:"DisableForeignKeyConstraintWhenMigrating"`

	// SkipDefaultTransaction stops wrapping a single Create / Update / Delete in a transaction.
	// false by default (GORM's default: it wraps).
	//
	// What it saves is the BEGIN / COMMIT round trips: measured on local loopback over 500 single
	// Creates, PostgreSQL 16 went from about 500µs to 230µs each and MySQL 8.0 from about 1.0ms to
	// 0.9ms. The cost is that writes inside hooks such as BeforeCreate no longer share the main
	// statement's fate; an explicit Transaction is unaffected.
	SkipDefaultTransaction bool `yaml:"SkipDefaultTransaction"`

	// PrepareStmt caches executed SQL as prepared statements and reuses them for the same SQL later.
	// false by default.
	//
	// Measured on local loopback over 2000 runs of the same parameterized query: MySQL 8.0 went from
	// about 180µs to 94µs each (without it the driver does prepare / execute / close, three round
	// trips per statement) and PostgreSQL 16 from about 100µs to 85µs (pgx caches statements anyway,
	// so the difference is small). The cache size and expiry use GORM's defaults (unlimited, 1h).
	// Behind a connection pooler such as PgBouncer (transaction mode especially), measure in that
	// environment before turning it on.
	PrepareStmt bool `yaml:"PrepareStmt"`

	// CreateBatchSize is how many rows one INSERT carries when Creating a slice. 0 by default: the
	// whole slice in one INSERT.
	//
	// Measured with 10 rows and a setting of 3: both PostgreSQL and MySQL sent 4 INSERTs (3+3+3+1)
	// and all 10 rows were written. Use it for a large slice to stay under the parameter limit of one
	// statement (65535 on PostgreSQL) and the packet size limit (MySQL's max_allowed_packet).
	CreateBatchSize int `yaml:"CreateBatchSize"`

	// Trace decides whether the OpenTelemetry plugin is attached. On by default.
	//
	// Without tracing installed it produces noop spans at negligible cost, which is why it is on by
	// default. The attributes follow OTel's database semantic conventions v1.43.0 (db.system.name,
	// db.namespace, db.query.text, db.operation.name, server.address / server.port).
	// db.query.text carries placeholders and no parameter values; a server-side error records the
	// error code alone (db.response.status_code).
	Trace bool `yaml:"Trace"`

	// Metric decides whether the db_pool_* connection pool metrics are exported (connection counts,
	// wait counts, wait durations and so on), labelled with the instance name. On by default.
	//
	// The metrics read sql.DB.Stats() when they are scraped and occupy no goroutine.
	// It applies per instance: an instance with Metric: false does not appear in /metrics.
	Metric bool `yaml:"Metric"`
}

// MySQLConfig is the MySQL-specific configuration.
type MySQLConfig struct {
	// ReadTimeout is the read timeout, the DSN's readTimeout. Defaults to 3s, and 0 injects nothing
	// (so the driver has no limit).
	ReadTimeout time.Duration `yaml:"ReadTimeout"`

	// WriteTimeout is the write timeout, the DSN's writeTimeout. Defaults to 5s, and 0 injects
	// nothing (so the driver has no limit).
	WriteTimeout time.Duration `yaml:"WriteTimeout"`
}

// PostgresConfig is the PostgreSQL-specific configuration.
//
// These values are sent to the server with the startup message when connecting and become
// session-level GUCs. When the DSN already writes a key of the same name explicitly, the DSN wins.
type PostgresConfig struct {
	// StatementTimeout is how long one statement may run. No limit by default.
	//
	// It is the server's clock and governs how long the server has been executing a statement it
	// received. The PG client side has no read timeout the way MySQL does: when a pooled connection
	// meets an unresponsive peer (a stuck process, a network broken midway) and the caller's ctx has
	// no deadline, the query waits until the caller gives up. Measured, setting this to 1s still
	// waited: 30s later it had not returned — the statement never reached the server, which
	// therefore had nothing to time. The only thing governing that stretch is the caller's deadline:
	// pass a ctx with one through CWithCtx, and measured with 200ms it returned in 200ms.
	StatementTimeout time.Duration `yaml:"StatementTimeout"`

	// LockTimeout is how long to wait for a lock. No limit by default.
	LockTimeout time.Duration `yaml:"LockTimeout"`

	// IdleInTxTimeout disconnects after this long idle inside a transaction. No limit by default.
	IdleInTxTimeout time.Duration `yaml:"IdleInTxTimeout"`

	// Params holds any other PG runtime parameters, appended to the DSN as they are.
	//
	// pgx's own connection parameters go here too, such as default_query_exec_mode: exec when
	// connecting through PgBouncer's transaction pooling — pgx's default cache_statement uses named
	// prepared statements, and measured against PgBouncer 1.22 in transaction pooling
	// (max_prepared_statements at 0, the default before 1.24), 20 goroutines running 1000 queries
	// produced 701 reports of prepared statement "stmtcache_…" already exists (42P05). The details
	// and the cost of each mode are in xgorm/README.md under "PostgreSQL".
	Params map[string]string `yaml:"Params"`
}

// DefaultClientConfig gathers every default of one instance in one place.
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		Driver:        DriverPostgres,
		DialTimeout:   500 * time.Millisecond,
		MySQL:         MySQLConfig{ReadTimeout: 3 * time.Second, WriteTimeout: 5 * time.Second},
		MaxOpenConns:  50,
		MaxIdleConns:  50,
		MaxLifetime:   5 * time.Minute,
		MaxIdleTime:   5 * time.Minute,
		SlowThreshold: 3 * time.Second,
		Trace:         true,
		Metric:        true,
	}
}

// DefaultConfig has no instance at all — without an XGorm block no database should be connected.
func DefaultConfig() Config { return Config{} }

// loadConfig reads this block of the config file. Both the single- and multi-instance forms are
// accepted, with DefaultClientConfig laid under each instance before decoding; the rules are in
// xconfig.UnmarshalClients.
func loadConfig() (Config, error) {
	clients, err := xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)
	return Config{Clients: clients}, err
}

// Validate checks the whole block, one instance at a time, naming the instance in the error.
//
// Reading from the config file does not need it: xconfig.UnmarshalClients calls
// ClientConfig.Validate after decoding each instance, and the error carries the file and the line.
// Code assembling a Config by hand can use this to check it first.
func (c Config) Validate() error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(c.Clients)) {
		if err := c.Clients[name].Validate(); err != nil {
			errs = append(errs, fmt.Errorf("Clients.%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Validate reports the ways one instance's configuration contradicts itself, failing before any
// connection is made.
//
// While reading the config file, xconfig.UnmarshalClients calls it after decoding each instance,
// so a wrong value fails during reading with the file and the line, before a single instance has
// connected; code calling New directly has New call it once as well.
// It returns a plain error, which the layer calling it wraps in one xerror.
func (c ClientConfig) Validate() error {
	if c.DSN == "" {
		return fmt.Errorf("DSN must not be empty")
	}
	d, ok := lookupDialect(c.Driver)
	if !ok {
		return unknownDriver(c.Driver)
	}
	if err := c.TLS.Validate(); err != nil {
		return err
	}
	if c.TLS.Enable && d.OpenTLS == nil {
		return fmt.Errorf("Driver=%q does not support the TLS block, configure TLS in its DSN instead", c.Driver)
	}
	if c.MaxOpenConns <= 0 {
		return fmt.Errorf("MaxOpenConns must be > 0, got=%d", c.MaxOpenConns)
	}
	if c.MaxIdleConns < 0 {
		return fmt.Errorf("MaxIdleConns must not be negative, got=%d", c.MaxIdleConns)
	}
	if c.CreateBatchSize < 0 {
		return fmt.Errorf("CreateBatchSize must not be negative, got=%d", c.CreateBatchSize)
	}
	// A negative duration has no meaning that holds up, and every layer underneath turns it
	// silently into "no limit": database/sql treats a negative MaxLifetime / MaxIdleTime the same as
	// 0, "do not close connections by age"; go-sql-driver v1.10.1's FormatDSN only writes a timeout /
	// readTimeout / writeTimeout greater than 0, so a negative one disappears from the DSN and a
	// query hangs forever against an unresponsive peer; PG's connect_timeout and the GUCs are
	// likewise skipped during injection. One minus sign buys a silently vanished timeout while the
	// config file looks perfectly fine. A 0 has a documented meaning in each case (inject nothing /
	// no limit) and is not covered here
	for _, f := range []struct {
		name string
		val  time.Duration
	}{
		{"DialTimeout", c.DialTimeout},
		{"MaxLifetime", c.MaxLifetime},
		{"MaxIdleTime", c.MaxIdleTime},
		{"SlowThreshold", c.SlowThreshold},
		{"MySQL.ReadTimeout", c.MySQL.ReadTimeout},
		{"MySQL.WriteTimeout", c.MySQL.WriteTimeout},
		{"Postgres.StatementTimeout", c.Postgres.StatementTimeout},
		{"Postgres.LockTimeout", c.Postgres.LockTimeout},
		{"Postgres.IdleInTxTimeout", c.Postgres.IdleInTxTimeout},
	} {
		if f.val < 0 {
			return fmt.Errorf("%s must not be negative, got=%v", f.name, f.val)
		}
	}
	return nil
}
