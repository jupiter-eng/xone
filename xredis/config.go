// Package xredis sets up go-redis from the configuration; the caller gets a native *redis.Client.
//
//	v, err := xredis.C().Get(ctx, "key").Result()
//
// There is no CWithCtx: every go-redis method takes a ctx anyway, and another layer would add nothing.
package xredis

import (
	"fmt"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xtls"
)

// ConfigKey is this module's top-level key in the config file.
const ConfigKey = "XRedis"

// DefaultName is the name of the instance C() returns without an argument.
const DefaultName = xconfig.DefaultClientName

// Config is this module's configuration. There are two forms:
//
//	XRedis:                 # a single instance
//	  Addr: "127.0.0.1:6379"
//
//	XRedis:                 # several instances
//	  Clients:
//	    default: {Addr: "127.0.0.1:6379"}
//	    cache:   {Addr: "127.0.0.1:6380", DB: 1}
type Config struct {
	// Clients holds the instances by name. The single-instance form is normalized to one instance named default.
	Clients map[string]ClientConfig
}

// ClientConfig is the configuration of one Redis instance.
type ClientConfig struct {
	// Addr is the server address. It defaults to 127.0.0.1:6379.
	Addr string `yaml:"Addr"`

	// Username is the ACL user name (Redis 6.0+). There is none by default.
	Username string `yaml:"Username"`

	// Password is the authentication password. There is none by default.
	//
	// Writing it as "${REDIS_PASSWORD}" is the natural choice: a credential does not belong in version control, and a missing one fails startup.
	// This module never writes it into any log.
	Password string `yaml:"Password"`

	// DB is the database number. It defaults to 0.
	DB int `yaml:"DB"`

	// DialTimeout is the connection timeout. It defaults to 500ms; 0 leaves it to go-redis (5s).
	//
	// None of the durations below means "no limit" at 0; 0 is go-redis v9.22.0's default (options.go init, measured to match).
	// The per-attempt budget of the connection check at startup is the resolved DialTimeout + ReadTimeout.
	DialTimeout time.Duration `yaml:"DialTimeout"`

	// ReadTimeout is the read timeout. It defaults to 500ms; 0 leaves it to go-redis (5s).
	ReadTimeout time.Duration `yaml:"ReadTimeout"`

	// WriteTimeout is the write timeout. It defaults to 500ms; 0 follows the resolved ReadTimeout.
	WriteTimeout time.Duration `yaml:"WriteTimeout"`

	// PoolSize is the connection pool size. It defaults to 0, left to go-redis (10 x GOMAXPROCS).
	PoolSize int `yaml:"PoolSize"`

	// MinIdleConns is the minimum number of idle connections, warming them up to avoid cold-start jitter. It defaults to 5.
	MinIdleConns int `yaml:"MinIdleConns"`

	// MaxIdleConns is the maximum number of idle connections. It defaults to 0, meaning no limit.
	MaxIdleConns int `yaml:"MaxIdleConns"`

	// MaxActiveConns is the maximum number of active connections. It defaults to 0, meaning no limit.
	MaxActiveConns int `yaml:"MaxActiveConns"`

	// PoolTimeout is the upper bound on waiting when the pool has no idle connection. It defaults to 1s; 0 is the resolved ReadTimeout + 1s.
	PoolTimeout time.Duration `yaml:"PoolTimeout"`

	// ConnMaxIdleTime is how long an idle connection lives at most. It defaults to 5m; 0 leaves it to go-redis (30m).
	ConnMaxIdleTime time.Duration `yaml:"ConnMaxIdleTime"`

	// ConnMaxLifetime is how long a connection lives at most. It defaults to 5m; 0 means connections are not replaced by age (go-redis's default).
	//
	// Replacing connections periodically is what lets traffic spread out again after the server scales.
	ConnMaxLifetime time.Duration `yaml:"ConnMaxLifetime"`

	// MaxRetries is the maximum number of retries. It defaults to 0, left to go-redis (3); -1 turns retrying off.
	MaxRetries int `yaml:"MaxRetries"`

	// MinRetryBackoff is the minimum retry backoff. It defaults to 0, left to go-redis (10ms, measured on v9.22.0);
	// -1ns turns the backoff off (the unit is required, since a bare -1 does not parse as a duration and fails startup).
	MinRetryBackoff time.Duration `yaml:"MinRetryBackoff"`

	// MaxRetryBackoff is the maximum retry backoff. It defaults to 0, left to go-redis (1s, measured on v9.22.0);
	// -1ns turns the backoff off, as above.
	MaxRetryBackoff time.Duration `yaml:"MaxRetryBackoff"`

	// Trace says whether the OpenTelemetry hook is attached. It defaults to on.
	//
	// Without tracing installed it produces noop Spans, but the hook itself is not free: measured
	// (redisotel v9.22.0, local loopback, a noop provider) at about 3µs, 8 allocations and about 1.2KB more per
	// command. A real network round trip is hundreds of microseconds, where the share is far smaller, which is why it
	// is on by default; a service extremely sensitive to single-command latency can turn it off.
	Trace bool `yaml:"Trace"`

	// Metric says whether the connection-pool metrics are exported. It defaults to on.
	//
	// The metrics read PoolStats() only when scraped and hold no extra goroutine.
	// It applies per instance: an instance configured with Metric: false does not appear in /metrics.
	Metric bool `yaml:"Metric"`

	// Log says whether one line is recorded per command (and one for a whole pipeline). It defaults to off.
	//
	// Only the command name and the first key are recorded, never the value and never the other arguments; a command
	// whose arguments may hold credentials, such as AUTH or HELLO, records the command name alone. The key is recorded
	// whole (truncated only beyond 256 bytes): with Log on, do not put a token, a phone number or an email into a key.
	// A missing key (redis.Nil) and a WATCH conflict (redis.TxFailedErr) are not failures and are recorded at INFO as usual.
	// A server-side error keeps its error code alone (in a separate error_code field) and never its text: measured on Redis 7.0.15,
	// ERR unknown command 'foo', with args beginning with: … carries the arguments out as they are; a client-side error such as
	// a timeout, a failed connection, a cancelled ctx or redis: client is closed is recorded verbatim. The error returned to the caller is unchanged. The fields are in xredis/README.md.
	Log bool `yaml:"Log"`

	// SlowThreshold records one warn line for a command (or pipeline) taking longer than this. It defaults to 100ms, requires Log, and 0 records nothing.
	SlowThreshold time.Duration `yaml:"SlowThreshold"`

	// TLS says whether to use TLS when connecting to Redis. It defaults to off. The fields and rules are shared across modules, see xtls.Config.
	//
	// The handshake is bounded by DialTimeout: go-redis v9.22.0 uses tls.DialWithDialer, so dialing and the handshake share that one timeout.
	TLS xtls.Config `yaml:"TLS"`
}

// DefaultClientConfig holds every default of a single instance in one place.
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		Addr:            "127.0.0.1:6379",
		DialTimeout:     500 * time.Millisecond,
		ReadTimeout:     500 * time.Millisecond,
		WriteTimeout:    500 * time.Millisecond,
		MinIdleConns:    5,
		PoolTimeout:     time.Second,
		ConnMaxIdleTime: 5 * time.Minute,
		ConnMaxLifetime: 5 * time.Minute,
		SlowThreshold:   100 * time.Millisecond,
		Trace:           true,
		Metric:          true,
	}
}

// DefaultConfig has no instances at all -- without XRedis configured, no Redis should be connected to.
func DefaultConfig() Config { return Config{} }

// loadConfig reads this block of the config file. Both the single-instance and the multi-instance form are accepted,
// each instance is pre-filled with DefaultClientConfig before decoding, and the rules are in xconfig.UnmarshalClients.
func loadConfig() (Config, error) {
	clients, err := xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)
	return Config{Clients: clients}, err
}

// Validate checks what does not make sense in the configuration itself and fails before anything is connected to.
//
// xconfig.UnmarshalClients calls it once per decoded instance, so a misconfigured value fails while the config is
// read, naming the instance; a direct call to New calls it once as well.
// It returns a plain error, which the layer calling it wraps in an xerror once.
func (c ClientConfig) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("Addr must not be empty")
	}
	for _, n := range []struct {
		name string
		val  int
	}{
		{"DB", c.DB},
		{"PoolSize", c.PoolSize},
		{"MinIdleConns", c.MinIdleConns},
		{"MaxIdleConns", c.MaxIdleConns},
		{"MaxActiveConns", c.MaxActiveConns},
	} {
		if n.val < 0 {
			return fmt.Errorf("%s must not be negative, got=%d", n.name, n.val)
		}
	}

	// A negative duration is a private signal of its own in go-redis: ReadTimeout -1 means "no limit" and -2 means
	// "do not even set a deadline", while ConnMaxIdleTime -1 means "do not reclaim by idleness". This package does not
	// document any of them, and one minus sign silently turning the timeout protection off is why none is accepted.
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"DialTimeout", c.DialTimeout},
		{"ReadTimeout", c.ReadTimeout},
		{"WriteTimeout", c.WriteTimeout},
		{"PoolTimeout", c.PoolTimeout},
		{"ConnMaxIdleTime", c.ConnMaxIdleTime},
		{"ConnMaxLifetime", c.ConnMaxLifetime},
		{"SlowThreshold", c.SlowThreshold},
	} {
		if d.val < 0 {
			return fmt.Errorf("%s must not be negative, got=%v", d.name, d.val)
		}
	}

	// The only three signals that are documented: MaxRetries -1 and the two backoffs at -1ns, all meaning "off".
	// go-redis recognizes exactly -1 and leaves any other negative value as it is, with behavior nobody can state.
	if c.MaxRetries < -1 {
		return fmt.Errorf("MaxRetries must be -1 (disable retries) or >= 0, got=%d", c.MaxRetries)
	}
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"MinRetryBackoff", c.MinRetryBackoff},
		{"MaxRetryBackoff", c.MaxRetryBackoff},
	} {
		if d.val < 0 && d.val != -1 {
			return fmt.Errorf("%s must be -1ns (disable backoff) or >= 0, got=%v", d.name, d.val)
		}
	}
	return c.TLS.Validate()
}
