package xredis

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"go.opentelemetry.io/otel"

	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
	"github.com/jupiter-eng/xone/xmetric"

	// Using xredis brings tracing: xtrace installs the global TracerProvider and a command's Span is recorded through otel's global API.
	// To have no tracing, configure XTrace.Enable: false; to turn it off for one instance only, use that instance's own Trace: false.
	_ "github.com/jupiter-eng/xone/xtrace"
)

const (
	// pingAttempts is how many attempts the connection check makes.
	pingAttempts = 3

	// fallbackPingTimeout is the fallback timeout when no budget can be worked out from the configuration.
	fallbackPingTimeout = time.Second
)

// pingInterval is the upper bound on the first backoff, doubling after that (xutil.Retry); between 3 attempts the two
// backoff bounds are 1s and 2s. That number is written into docs/behavior.md's "The startup connection probe", which the startup budget is computed from.
//
// It is a variable rather than a constant only so tests can shorten it -- a case that cannot connect runs a full round
// of retries, which would be tens of seconds at one second each.
var pingInterval = time.Second

// New builds a Redis instance from the configuration and touches no globals.
//
// It Pings once first to confirm it can connect: a wrong address or a wrong password should surface at startup
// rather than at the first cache read in production.
//
// The ctx bounds this round of connection checking: a failure runs a full round of retries, and a stop signal should
// make it give up on the spot rather than leaving the process stuck there.
func New(ctx context.Context, cfg ClientConfig) (*redis.Client, io.Closer, error) {
	return newClient(ctx, "", cfg)
}

// newClient is New with name being the instance name from the config file, used only in the command log (with Log on);
// a direct call to New has no instance name, so the log carries no name field.
func newClient(ctx context.Context, name string, cfg ClientConfig) (*redis.Client, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, xerror.Newf("xredis", "config", "invalid config: %w", err)
	}
	tlsCfg, err := cfg.TLS.Build()
	if err != nil {
		return nil, nil, xerror.Newf("xredis", "config", "invalid TLS config: %w", err)
	}

	client := redis.NewClient(&redis.Options{
		Addr:            cfg.Addr,
		Username:        cfg.Username,
		Password:        cfg.Password,
		DB:              cfg.DB,
		DialTimeout:     cfg.DialTimeout,
		ReadTimeout:     cfg.ReadTimeout,
		WriteTimeout:    cfg.WriteTimeout,
		PoolSize:        cfg.PoolSize,
		MinIdleConns:    cfg.MinIdleConns,
		MaxIdleConns:    cfg.MaxIdleConns,
		MaxActiveConns:  cfg.MaxActiveConns,
		PoolTimeout:     cfg.PoolTimeout,
		ConnMaxIdleTime: cfg.ConnMaxIdleTime,
		ConnMaxLifetime: cfg.ConnMaxLifetime,
		MaxRetries:      cfg.MaxRetries,
		MinRetryBackoff: cfg.MinRetryBackoff,
		MaxRetryBackoff: cfg.MaxRetryBackoff,

		// nil means plaintext. The handshake is bounded by DialTimeout: go-redis v9.22.0 uses tls.DialWithDialer,
		// so dialing and the handshake share that one timeout.
		TLSConfig: tlsCfg,

		// Do not send CLIENT SETINFO.
		//
		// go-redis v9.22.0 sends an extra pipeline on every connection by default (CLIENT SETINFO lib-name and
		// lib-ver), only so the server's CLIENT LIST shows the client library name and version.
		// Before Redis 7.2 that subcommand does not exist: measured on Redis 7.0.15, every new connection answered
		// ERR unknown subcommand 'setinfo'. go-redis swallows that error, but with tracing on every connection
		// leaves a failing redis.pipeline Span -- and ConnMaxLifetime replaces the connections every 5 minutes,
		// so that is a batch every 5 minutes. What it buys the caller is next to nothing, so it is turned off.
		DisableIdentity: true,

		// Do not send CLIENT MAINT_NOTIFICATIONS.
		//
		// go-redis v9.22.0 defaults to auto: every new connection sends it once, and a server that accepts it turns
		// "maintenance notifications" on -- the server pushes a notice before a migration or an upgrade and the client
		// temporarily relaxes its read and write timeouts (the default RelaxedTimeout in the source is 10s; no server
		// accepting this command was at hand, so it is not measured) and moves connections to the new node in the
		// background. go-redis's comment says this is for Redis Cloud. On a server that does not accept it, measured on
		// Redis 7.0.15 it answered ERR unknown subcommand 'maint_notifications' and the first few connections brought up
		// concurrently each left a failing Span (1-2 with 3 concurrent), after which the whole client stops sending it.
		// On a server that does accept it, "timeouts relaxed to 10s" contradicts this package's ReadTimeout promise. Turned off, the behavior is decided by the configuration alone.
		MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},

		// One connection attempt dials once, and retrying is left to MaxRetries.
		//
		// go-redis v9.22.0 defaults to DialerRetries=5 with DialerRetryTimeout=100ms between them, hidden inside every
		// "connect": one connect is at most 5xDialTimeout+4x100ms, which is 2.9s with the defaults, multiplied again by
		// the 4 command-level attempts -- measured, one command took 11.7s with the Redis host down (SYN unanswered),
		// and a refused connection still wasted 4x400ms. Set to 1, a command's upper bound is the formula in the docs,
		// (MaxRetries+1)x(DialTimeout+ReadTimeout)+MaxRetriesxMaxRetryBackoff (7s with the defaults),
		// and the same scenarios measured 2.1s (0.5s with MaxRetries -1) and 70-150ms for a refused connection (two e2e rounds).
		// Dialing once makes DialerRetryTimeout unreachable (nothing waits after the last failure), so it is not set.
		// It cannot be 0: go-redis turns both 0 and a negative value back into 5.
		DialerRetries: 1,

		// Let the request ctx's deadline govern the socket reads and writes.
		//
		// go-redis does not do this by default: without it, every command uses the fixed ReadTimeout /
		// WriteTimeout pair and the caller's deadline is decoration --
		// a request with a 20ms timeout would still wait hundreds of milliseconds on a slow Redis,
		// and the upstream's timeout budget and cascade protection fail with it.
		//
		// It governs the deadline only, not cancellation: go-redis sets ctx.Deadline() as the socket read and write
		// deadline and then stops looking at the ctx, so cancelling it cannot wake a command already blocked on a read.
		// Measured (v9.22.0) with ReadTimeout 1s and the command stuck 100ms in, cancelling the ctx still returned at 1s;
		// when xgin disconnects on shutdown and cancels the request ctx, a handler stuck on Redis returns only at
		// ReadTimeout, or when xredis's stop hook closes the pool. xredis cannot fill that gap from inside: a command's
		// result is written onto the caller's *Cmd, and returning early would race the goroutine still reading into it. So, to make a command stoppable, give the ctx a deadline.
		ContextTimeoutEnabled: true,
	})

	// From here on, any failing step has to close the client or a connection pool leaks.
	ok := false
	defer func() {
		if !ok {
			client.Close()
		}
	}()

	if err := xclient.Probe(ctx, probePolicy(client.Options()), probe(client)); err != nil {
		// The original error is carried with %w, which the caller needs to judge the root cause. It holds no credential:
		// a failed connection gives a dial error (only the address), and a failed authentication gives the server's WRONGPASS / NOAUTH text.
		if redis.IsAuthError(err) {
			return nil, nil, xerror.Newf("xredis", "connect", "authentication to %s failed: %w", cfg.Addr, err)
		}
		return nil, nil, xerror.Newf("xredis", "connect", "cannot reach %s: %w", cfg.Addr, err)
	}

	// The tracing hook is attached only after the connection check succeeded: attached before it, every Ping attempt at
	// startup would be a parentless ping Span (plus redis.dial and hello), and a failure would make one round of retries
	// a string of failing root Spans -- for a startup, not a business request.
	if cfg.Trace {
		// Turn db.statement off: redisotel v9.22.0 has it on by default and writes the whole command with its arguments
		// into the Span (measured: SET session:1 <value> appeared as it is), so the session or token in the value
		// reaches the tracing backend. The same principle that has xgorm record SQL with placeholders and no parameters.
		//
		// TODO: redisotel defaults to WithCallerEnabled(true) and runs runtime.Callers on every command to find the
		// caller, writing code.function / code.filepath / code.lineno. Measured, turning it off takes a command from
		// 14.0µs to 11.6µs (about -17%, local loopback with a noop provider), at the cost of those three attributes
		// disappearing from the Span. That is a caller-visible change, so the default is kept for now; undecided.
		//
		// The same for errors: redisotel writes the server's error text into exception.message and the status description,
		// so a wrapping TracerProvider takes over and the error text on a Span follows the same rules as the command log (see trace.go).
		// What is wrapped is the global provider at this moment: before xtrace is installed it is otel's delegate, and it forwards there once it is.
		// The same for the Span name: redisotel takes the command's first argument as the name, and spanCmdsHook hangs outside it and hands over the commands for the wrapper to rename.
		tp := redactingTracerProvider{otel.GetTracerProvider()}
		client.AddHook(spanCmdsHook{})
		if err := redisotel.InstrumentTracing(client, redisotel.WithDBStatement(false), redisotel.WithTracerProvider(tp)); err != nil {
			return nil, nil, xerror.Newf("xredis", "new", "install tracing hook: %w", err)
		}
	}

	// The log hook is attached after the tracing hook. go-redis puts the one added first on the outside, so it runs
	// inside the redis Span: the command ctx holds this command's Span, which is the log's span_id, and the trace_id matches the caller's.
	// It is likewise attached after the connection check: a Ping at startup is not a business command.
	if cfg.Log {
		client.AddHook(logHook{name: name, slow: cfg.SlowThreshold})
	}

	ok = true
	return client, &clientCloser{client: client, addr: cfg.Addr}, nil
}

// probePolicy is how the connection check retries. A failed authentication (WRONGPASS / NOAUTH) is not retried: a wrong
// password stays wrong, and retrying would only make startup wait out two more backoffs (at most 1s + 2s by default) before reporting it.
func probePolicy(opt *redis.Options) xclient.ProbePolicy {
	return xclient.ProbePolicy{
		Attempts:   pingAttempts,
		Timeout:    pingTimeout(opt),
		Interval:   pingInterval,
		AuthFailed: redis.IsAuthError,
	}
}

// probe makes one Ping. It returns on the spot when the ctx is cancelled rather than waiting for that read to hit ReadTimeout.
//
// The reasoning is in New's comment on ContextTimeoutEnabled: go-redis honors only the ctx's deadline, and
// cancelling cannot wake a command blocked on a read. Measured (v9.22.0) against a peer accepting the connection and
// never answering, a stop signal during startup takes effect only after that read times out: about 450ms after the signal with ReadTimeout 500ms, and about 2.95s with 3s.
// So the Ping runs in a goroutine while this side watches the ctx and does not wait for it once cancelled.
// xclient.Probe does not do this for fn (see its comment), so this stretch can only be filled in here.
//
// The abandoned goroutine does not leak: on a failed connection New closes the client, and closing the pool makes the blocked read return at once.
// A reached deadline still waits for it -- go-redis returns at that moment too, and its i/o timeout says more than the ctx's error.
func probe(client *redis.Client) func(context.Context) error {
	return func(ctx context.Context) error {
		done := make(chan error, 1)
		go func() { done <- client.Ping(ctx).Err() }()
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return <-done
		}
	}
}

// pingTimeout is one Ping's timeout: a connection plus one round trip.
//
// It reads the Options after go-redis filled in its defaults (client.Options()), not the configuration: a 0 in the
// configuration means "use its default" in go-redis v9.22.0 -- DialTimeout 5s and ReadTimeout 5s (options.go init) --
// and computing 0 + 0 would leave only the 1s fallback, timing out a slow connection go-redis itself was still willing to wait for.
// Validate rejects negative durations, so both are positive after the defaults are filled in; the fallback only guards against go-redis changing its defaults one day.
func pingTimeout(opt *redis.Options) time.Duration {
	return cmp.Or(opt.DialTimeout+opt.ReadTimeout, fallbackPingTimeout)
}

type clientCloser struct {
	client *redis.Client
	addr   string
}

func (c *clientCloser) Close() error {
	if err := c.client.Close(); err != nil {
		return xerror.Newf("xredis", "close", "close %s failed: %w", c.addr, err)
	}
	return nil
}

// ---- global instances ----

// C fetches a Redis instance; without an argument, the one named default.
//
// If there is none it panics, for the reason given in xclient.Registry.Get.
// There is no CWithCtx: every go-redis method takes a ctx anyway, and another layer would add nothing.
//
// An optional dependency (used when configured, skipped when not) checks with Has first.
func C(name ...string) *redis.Client { return reg.Get(name...).client }

// Has reports whether the given instance is configured, for checking an optional dependency.
func Has(name ...string) bool { return reg.Has(name...) }

// Names returns the configured instance names.
func Names() []string { return reg.Names() }

// ---- registration ----

// instance is one instance together with its own Metric switch, for the same reason as in xgorm:
// the collector is a single process-wide object, and without this bit there would be no way to tell which instance was configured with Metric: false.
type instance struct {
	client *redis.Client
	metric bool
}

// reg is the registry of named instances. Fetching an instance, the error when there is none, and clearing out
// on close must mean the same across xgorm / xredis / xcache, so they share one implementation.
var reg = xclient.NewRegistry[instance]("xredis", ConfigKey)

func init() {
	xhook.BeforeStart(initXRedis, xhook.At(xhook.StageClient))
	xhook.BeforeStop(closeXRedis) // the stage follows the start hook above

	// go-redis's logger is a single process-wide one and can only be wired up here: New touches no globals, and doing
	// it in a start hook would overwrite a redis.SetLogger the caller made in main. Wired up in init, a later call in main still wins.
	// It reads no configuration, does no IO and writes one package variable, which is the same kind of thing as registering a hook.
	redis.SetLogger(slogLogger{})
}

// slogLogger routes go-redis's own logs into slog.
//
// go-redis v9.22.0's default logger is the standard library log writing plain text to stderr
// (`redis: 2026/09/24 10:00:00 pool.go:762: redis: connection pool: failed to dial after 1 attempts: ...`),
// which without slog is neither JSON nor carries a trace_id and never reaches the logging platform. Its logs have no
// level and are almost all extra detail on an error (a failed connection, a connection that cannot be closed, a failed
// notification), while the caller already has the error too, so they are all recorded at WARN. The ctx is passed
// through, so the ones logged with a command ctx (an expiry truncated to 1ms, say) carry a trace_id; the failed-connection
// line does not -- v9.22.0 dials in a goroutine of its own with context.Background(), and measured with Redis refusing connections, not one of 45 failed to dial lines had a trace_id.
type slogLogger struct{}

func (slogLogger) Printf(ctx context.Context, format string, v ...any) {
	slog.WarnContext(ctx, "xredis go-redis log", "detail", fmt.Sprintf(format, v...))
}

// initXRedis reads the configuration and builds the instances one by one by name.
//
// Without this block configured it builds none: xredis is an optional dependency, and not configuring it must not keep
// the service from starting. Build still runs, so the registry learns that the start hook ran -- after which a C()
// that finds nothing reports "not configured" rather than "called too early".
func initXRedis(ctx context.Context) error {
	if !xconfig.Has(ConfigKey) {
		return xclient.Build(ctx, reg, nil, build)
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	return install(ctx, c)
}

// install builds the instances one by one from the configuration.
func install(ctx context.Context, c Config) error {
	if err := xclient.Build(ctx, reg, c.Clients, build); err != nil {
		return err
	}
	installPoolMetrics() // with no instance enabling Metric it exports nothing, so no special case is needed
	slog.Info("xredis ready", "instances", reg.Names())
	return nil
}

// closeXRedis removes every instance and closes them in reverse.
//
// It is never called when the start hook failed -- a stop hook only runs after the start hook it pairs with
// succeeded, so it does not have to handle "it was never built".
func closeXRedis(context.Context) error { return reg.Close() }

// build builds one instance. It wraps New rather than handing New over directly, so that this instance's Metric
// switch travels into the registry and the instance name reaches the log (the command log carries it too) --
// with several Redis instances configured, the address alone does not say which one it is (least of all for the same address with different database numbers).
func build(ctx context.Context, name string, c ClientConfig) (instance, io.Closer, error) {
	client, closer, err := newClient(ctx, name, c)
	if err != nil {
		return instance{}, nil, err
	}
	// Only the address and the database number go into the log, never the password -- so there is nothing to redact either.
	slog.Info("xredis connected", "name", name, "addr", c.Addr, "db", c.DB,
		"tls", c.TLS.Enable, "min_idle_conns", c.MinIdleConns)
	return instance{client: client, metric: c.Metric}, closer, nil
}

// installPoolMetrics attaches the pool collector to the current Registry.
//
// It attaches on every install rather than using a sync.Once, for the same reason as in xgorm: after xmetric is
// reinstalled the Registry is a new one and a Once would block this second attachment, while attaching twice to the
// same Registry is handled by xmetric.Register reusing the existing one.
//
// It does not fail startup: metrics not being exported is an observability problem and must not keep the service from coming up.
func installPoolMetrics() {
	if _, err := xmetric.RegisterAs(newPoolCollector(xmetric.Namespace(), xmetric.ConstLabels(), poolStats)); err != nil {
		slog.Error("xredis failed to register pool metrics", "error", err)
	}
}

// poolStats reads the live pool state of each instance with Metric enabled.
func poolStats() map[string]*redis.PoolStats {
	out := map[string]*redis.PoolStats{}
	for name, inst := range reg.All() {
		if inst.metric {
			out[name] = inst.client.PoolStats()
		}
	}
	return out
}
