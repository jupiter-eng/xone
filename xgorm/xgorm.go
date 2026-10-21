package xgorm

import (
	"cmp"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
	"github.com/jupiter-eng/xone/xmetric"

	// Using xgorm means tracing is there: xtrace installs the global TracerProvider, and SQL spans
	// are recorded through otel's global API. To go without tracing set XTrace.Enable: false, and to
	// turn it off for one instance set that instance's own Trace: false.
	_ "github.com/jupiter-eng/xone/xtrace"
)

const (
	// pingAttempts is how many times the connection check tries
	pingAttempts = 3

	// fallbackPingTimeout is the per-probe timeout when no budget can be worked out from the config
	fallbackPingTimeout = time.Second
)

// pingInterval is the ceiling of the first backoff, doubling each time (xutil.Retry), so the two
// backoff ceilings between 3 attempts are 1s and 2s. This number is written in
// docs/behavior.md under "The startup connection probe", where the startup budget is derived from it.
//
// It is a variable rather than a constant only so tests can shorten it — a test for an unreachable
// address runs a whole round of retries, which at one second a piece is tens of seconds.
var pingInterval = time.Second

// New builds one GORM instance from the configuration, touching no global variable.
//
// ctx bounds the lifetime of the connection check: against an unreachable address this runs a full
// round of probes and retries, and a stop signal should make it give up on the spot rather than
// leaving the process stuck on a database it will never reach.
//
// The io.Closer returned closes the underlying pool. A failed connection leaves no pool behind.
func New(ctx context.Context, cfg ClientConfig) (*gorm.DB, io.Closer, error) {
	return open(ctx, "", cfg)
}

// open is New with one more argument, the instance name: when the framework builds instances by
// name, the connection log has to say which one it is.
func open(ctx context.Context, name string, cfg ClientConfig) (*gorm.DB, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, xerror.Newf("xgorm", "config", "invalid config: %w", err)
	}

	dialect, _ := lookupDialect(cfg.Driver) // Validate already confirmed it is registered
	dsn, info, err := dialect.resolve(cfg)
	if err != nil {
		return nil, nil, xerror.New("xgorm", "config", err)
	}

	tlsCfg, err := cfg.TLS.Build()
	if err != nil {
		return nil, nil, xerror.Newf("xgorm", "config", "invalid TLS config: %w", err)
	}

	// The Logger needs to know what its placeholders look like, so it is built first
	dialector, err := dialect.dialector(dsn, tlsCfg)
	if err != nil {
		return nil, nil, xerror.New("xgorm", "config", err)
	}

	// Turn off GORM's own ping: it uses its own context, beyond the reach of our stop signal and our
	// retries. With it on, connecting to an unreachable address makes New wait inside it for the
	// DSN's full connect_timeout even when the ctx was cancelled long before (measured at 3 seconds).
	// With it off, gorm.Open only assembles and returns immediately, and every connection goes
	// through the ctx-aware probe below — cancellable and retryable.
	//
	// The built-in MySQL and ClickHouse dialects each query the version inside Initialize, which
	// connects as well; both are turned off in Open and moved into Dialect.Ready (see openMySQL).
	//
	// SkipDefaultTransaction, PrepareStmt and CreateBatchSize come from the configuration (with
	// GORM's own values as the defaults), and the rest of GORM's defaults are kept as they are,
	// measured, for the reasons in xgorm/README.md under "General": NowFunc uses local time and
	// TranslateError is false.
	gormCfg := &gorm.Config{
		DisableAutomaticPing:                     true,
		DisableForeignKeyConstraintWhenMigrating: cfg.DisableForeignKeyConstraintWhenMigrating,
		SkipDefaultTransaction:                   cfg.SkipDefaultTransaction,
		PrepareStmt:                              cfg.PrepareStmt,
		CreateBatchSize:                          cfg.CreateBatchSize,
	}
	if cfg.Log {
		gormCfg.Logger = newGormLogger(cfg, dialect, dialector)
	} else {
		// Without a Logger, GORM fills in its own default implementation, and that default writes to
		// os.Stdout with ANSI colors: slow queries and execution errors are still printed, only
		// bypassing slog — no level, no TraceID, not JSON, one line of colored text injected straight
		// into the log stream. Log=false asks for nothing printed, not for it printed elsewhere
		gormCfg.Logger = logger.Discard
	}

	db, err := gorm.Open(dialector, gormCfg)
	if err != nil {
		// Nothing to clean up here: when gorm.Open itself fails it closes the pool it built
		// (gorm v1.31.2 gorm.go Open closes it on both the failed Initialize and the failed auto ping)
		//
		// The built-in dialects never reach an authentication failure here: their Open touches no
		// network. A registered dialect that connects inside Initialize fails here on a wrong
		// password, and that still has to be reported as an authentication failure
		return nil, nil, connectError(dialect, "open %s failed: %w", info.Addr, err)
	}
	pool, err := db.DB()
	if err != nil {
		return nil, nil, xerror.Newf("xgorm", "connect", "get underlying pool: %w", err)
	}

	// From here on every step is ours, and a failure leaves nobody to clean up after us: the pool is
	// alive already, and not closing it leaks a permanent goroutine
	ok := false
	defer func() {
		if !ok {
			if cerr := pool.Close(); cerr != nil {
				slog.Warn("xgorm failed to close the pool", "error", cerr)
			}
		}
	}()
	pool.SetMaxOpenConns(cfg.MaxOpenConns)
	pool.SetMaxIdleConns(cfg.MaxIdleConns)
	pool.SetConnMaxLifetime(cfg.MaxLifetime)
	pool.SetConnMaxIdleTime(cfg.MaxIdleTime)

	policy := probePolicy(cfg, info, dialect)
	if err := xclient.Probe(ctx, policy, probe(pool, readyOf(dialect, db))); err != nil {
		return nil, nil, connectError(dialect, "cannot reach %s: %w", info.Addr, err)
	}

	if cfg.Trace {
		if err := installTracing(db, info, dialect); err != nil {
			return nil, nil, xerror.Newf("xgorm", "new", "install tracing callbacks: %w", err)
		}
	}

	logConn(name, info, cfg)

	ok = true
	return db, &poolCloser{pool: pool, info: info}, nil
}

// connectError is the error reported for a failed connection. When the dialect recognizes the
// server rejecting the credentials it reports an authentication failure; everything else follows
// format (whose arguments are the address and then the underlying error).
func connectError(d Dialect, format, addr string, err error) error {
	if d.authFailed(err) {
		return xerror.Newf("xgorm", "connect", "authentication to %s failed: %w", addr, err)
	}
	return xerror.Newf("xgorm", "connect", format, addr, err)
}

// probe is one connection check: a Ping, followed by the dialect's Ready once it passes (see
// Dialect.Ready), whose failure counts as this attempt failing and is retried the same way.
//
// It waits on the current goroutine rather than handing the work to another one: sql.DB's Close
// waits for in-flight queries, and the abandoned goroutine would not return just because the pool
// closed. Every driver honors the ctx — pgx and go-sql-driver drop the connection on cancellation,
// and ClickHouse's Ping honors the deadline — so one attempt is stuck for at most its own deadline.
func probe(pool *sql.DB, ready func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		err := pool.PingContext(ctx)
		if err == nil && ready != nil {
			err = ready(ctx)
		}
		return err
	}
}

// probePolicy is how the connection check retries. An authentication failure is not retried: the
// server already rejected these credentials outright, and two more attempts only delay the same
// error by two more backoffs (3s at most by default) while leaving two more authentication failures
// in the server's records. Whether it is recognized is the dialect's business (Dialect.AuthFailed).
func probePolicy(cfg ClientConfig, info ConnInfo, d Dialect) xclient.ProbePolicy {
	return xclient.ProbePolicy{
		Attempts:   pingAttempts,
		Timeout:    probeTimeout(cfg, info),
		Interval:   pingInterval,
		AuthFailed: d.authFailed,
	}
}

// readyOf binds the dialect's Ready to this instance, and is nil when the dialect provides none
func readyOf(d Dialect, db *gorm.DB) func(context.Context) error {
	if d.Ready == nil {
		return nil
	}
	return func(ctx context.Context) error { return d.Ready(ctx, db) }
}

// probeTimeout is the timeout of one probe.
//
// It comes from the dialect (ConnInfo.ProbeTimeout), which knows which timeout the driver uses for
// connecting and which for a round trip, and what the DSN finally settles on. When the dialect
// cannot work it out, connecting plus a round trip of the same magnitude gives 2 × DialTimeout —
// the connect timeout alone will not do: a probe costs connecting plus a round trip, and taking the
// connect budget as the whole budget declares a timeout the moment the connection is up.
func probeTimeout(cfg ClientConfig, info ConnInfo) time.Duration {
	return cmp.Or(info.ProbeTimeout, 2*cfg.DialTimeout, fallbackPingTimeout)
}

// logConn writes one connection log line, with only the fields that definitely hold no credentials.
//
// name is the instance name, which exists only for instances the framework built from
// configuration; a direct call to New has no name and the field is left out
func logConn(name string, info ConnInfo, c ClientConfig) {
	attrs := make([]any, 0, 14)
	if name != "" {
		attrs = append(attrs, "name", name)
	}
	attrs = append(attrs, "driver", info.Driver, "addr", info.Addr, "db", info.DB, "tls", c.TLS.Enable,
		"max_open_conns", c.MaxOpenConns, "max_idle_conns", c.MaxIdleConns)
	slog.Info("xgorm connected", attrs...)
}

type poolCloser struct {
	pool *sql.DB
	info ConnInfo
}

func (c *poolCloser) Close() error {
	if err := c.pool.Close(); err != nil {
		return xerror.Newf("xgorm", "close", "close %s failed: %w", c.info.Addr, err)
	}
	return nil
}

// ---- The global instances ----

// C takes a GORM instance, the one named default without arguments.
//
// A failed lookup panics outright, for the reason given under xclient.Registry.Get: returning nil
// only defers the same panic to the caller's first use, where the root cause — an unconfigured
// block — is no longer visible.
//
// An optional dependency (used when configured, skipped otherwise) checks with Has first.
func C(name ...string) *gorm.DB { return reg.Get(name...).db }

// CWithCtx takes an instance and binds a ctx, which is what carries tracing and timeouts downstream.
//
// GORM requires the ctx to be passed this way (unlike go-redis, whose every method takes one),
// which is why this method exists.
func CWithCtx(ctx context.Context, name ...string) *gorm.DB {
	return C(name...).WithContext(ctx)
}

// Has reports whether the given instance is configured, for an optional dependency to check
func Has(name ...string) bool { return reg.Has(name...) }

// Names returns the configured instance names
func Names() []string { return reg.Names() }

// ---- Registration ----

// instance is one instance together with its own Metric switch.
//
// The switch travels with the instance: the pool collector is one per process and walks every
// instance when scraped, and without this bit it could not tell which instance configured
// Metric: false and would have to export all or none.
type instance struct {
	db     *gorm.DB
	metric bool
}

// reg is the named-instance registry. Taking an instance, the error when one is not found, and
// detaching cleanly on close all have to behave identically across xgorm / xredis / xcache, which
// is why they share one implementation.
var reg = xclient.NewRegistry[instance]("xgorm", ConfigKey)

func init() {
	xhook.BeforeStart(initXGorm, xhook.At(xhook.StageClient))
	xhook.BeforeStop(closeXGorm) // the stage follows the start hook above

	// go-sql-driver's logger is one per process and can only be wired up here, for the reason xredis
	// gives: New touches no globals, and wiring it in a start hook would overwrite a
	// mysql.SetLogger the caller made in main themselves.
	// It reads no configuration, does no IO and writes a package-level variable, which makes it the
	// same kind of thing as registering a hook.
	// The driver copies the logger of the moment into its connection configuration while parsing a
	// DSN, so a later SetLogger only affects DSNs parsed afterwards — and since xgorm parses inside
	// a start hook, the call made in main before xone starts still takes effect.
	_ = mysqldriver.SetLogger(mysqlDriverLogger{}) // it only errors when the argument is nil

	// The Recorder GORM's Scan borrows honors this process-wide variable alone and interpolates the
	// parameter values into the SQL by default, bypassing the instance Logger's ParamsFilter, see
	// withoutParams. Same reasoning as above: it can only be wired up here, and a caller who wants to
	// change it (to see parameters while debugging locally, say) reassigns it in main, which runs
	// after every init. Instances the caller opened with gorm.Open themselves are covered too: their
	// Scan SQL logs carry no parameter values either.
	logger.RecorderParamsFilter = withoutParams
}

// initXGorm reads the configuration and builds the instances one by one, by name.
//
// Without the block nothing is built at all: xgorm is an optional dependency and its absence
// should not keep a service from starting. Build still runs, which is how the registry knows the
// start hooks ran — so a later failed C() reports "not configured" rather than "called too early".
func initXGorm(ctx context.Context) error {
	if !xconfig.Has(ConfigKey) {
		return xclient.Build(ctx, reg, nil, build)
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	return install(ctx, c)
}

// install builds the instances one by one from the configuration
func install(ctx context.Context, c Config) error {
	if err := xclient.Build(ctx, reg, c.Clients, build); err != nil {
		return err
	}
	installPoolMetrics() // with no instance enabling Metric it exports nothing, so there is no special case
	slog.Info("xgorm ready", "instances", reg.Names())
	return nil
}

// closeXGorm detaches every instance and closes them in reverse order.
//
// It is never reached when a start hook fails — a stop hook only runs after the start hook it
// pairs with succeeded — so it never has to handle "it was never built".
func closeXGorm(context.Context) error { return reg.Close() }

// build builds one instance. It wraps New rather than handing New over directly, so that this
// instance's Metric switch travels into the registry and the connection log carries the instance name
func build(ctx context.Context, name string, c ClientConfig) (instance, io.Closer, error) {
	db, closer, err := open(ctx, name, c)
	if err != nil {
		return instance{}, nil, err
	}
	return instance{db: db, metric: c.Metric}, closer, nil
}

// installPoolMetrics attaches the pool collector to the current Registry.
//
// It attaches on every install, with no sync.Once: a reinstalled xmetric (a second lifecycle round
// in one process) is a new Registry, and a Once would block exactly that second attachment, after
// which not one pool metric is ever exported. Attaching twice to the same Registry is recognized by
// xmetric.Register, which reuses the existing one rather than reporting a duplicate registration.
//
// xmetric is ready at StageTelemetry, before this; the collector walks the instances when scraped.
//
// It does not fail startup: metrics that cannot be exported are an observability problem and should
// not keep the service from coming up.
func installPoolMetrics() {
	if _, err := xmetric.RegisterAs(newPoolCollector(xmetric.Namespace(), xmetric.ConstLabels(), poolStats)); err != nil {
		slog.Error("xgorm failed to register pool metrics", "error", err)
	}
}

// poolStats reads the live state of the pools of every instance with Metric on
func poolStats() map[string]sql.DBStats {
	out := map[string]sql.DBStats{}
	for name, inst := range reg.All() {
		if !inst.metric {
			continue
		}
		pool, err := inst.db.DB()
		if err != nil || pool == nil {
			continue
		}
		out[name] = pool.Stats()
	}
	return out
}
