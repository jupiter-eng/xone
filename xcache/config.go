// Package xcache sets up a ristretto local cache from the configuration; the caller gets the native *ristretto.Cache.
//
//	xcache.Set("user:1", u)          // uses the default TTL from the configuration and cost=1
//	u, ok := xcache.Get[*User]("user:1")
//	xcache.C().SetWithTTL(k, v, 8, time.Hour)  // for full control, use the native one
//
// The package-level Get/Set are only conveniences on the default instance; no separate cache type is built --
// that would add an API to learn, to maintain, and to always find something missing in.
package xcache

import (
	"fmt"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
)

// ConfigKey is this module's top-level key in the config file.
const ConfigKey = "XCache"

// DefaultName is the name of the instance C() returns without an argument.
const DefaultName = xconfig.DefaultClientName

// Config is this module's configuration. There are two forms:
//
//	XCache:                 # a single instance
//	  MaxCost: 100000
//
//	XCache:                 # several instances
//	  Clients:
//	    default: {MaxCost: 100000}
//	    session: {MaxCost: 10000, DefaultTTL: 30m}
type Config struct {
	// Clients holds the instances by name. The single-instance form is normalized to one instance named default.
	Clients map[string]ClientConfig
}

// ClientConfig is the configuration of one cache instance.
type ClientConfig struct {
	// NumCounters is the number of counters used to track access frequency. It defaults to 1000000.
	//
	// Ten times the expected number of entries is a good value: ristretto uses it to decide which keys are worth
	// keeping, and too few makes the admission decision inaccurate and holds the hit rate down.
	NumCounters int64 `yaml:"NumCounters"`

	// MaxCost is the upper bound on the cache's total cost. It defaults to 100000.
	//
	// This package always writes with cost 1, so it is equivalent to "how many entries at most".
	// It is a real cost bound only when C().Set is called directly with a different cost.
	//
	// What is counted here is the cost the caller gives, without ristretto's 56 bytes of internal overhead per
	// entry. When recording cost in bytes, include that in the budget.
	MaxCost int64 `yaml:"MaxCost"`

	// BufferItems is the size of the internal buffer for Get. It defaults to 64, the value upstream recommends.
	BufferItems int64 `yaml:"BufferItems"`

	// DefaultTTL is the expiry used by the package-level Set. It defaults to 5m.
	//
	// 0 means it never expires; a negative value fails startup -- ristretto drops a write with ttl<0 outright.
	DefaultTTL time.Duration `yaml:"DefaultTTL"`

	// Metric says whether to export the hit rate and the other metrics. It defaults to on, as in XGorm / XRedis.
	//
	// It applies per instance: an instance configured with Metric: false does not appear in /metrics and ristretto
	// does not count for it either. The counting is ristretto's own: its Metrics is off by default, and nothing is
	// visible until it is turned on. The cost of turning it on, measured (ristretto v2.4.2, 4 cores):
	//
	//   - Memory: about 84KB more resident per instance; plus a table recording write times (for its lifetime
	//     histogram, at most 100 thousand entries), about 8-12MB more after more than 100 thousand distinct keys have been written.
	//   - CPU: no significant difference in this package's Get / Set benchmarks (p>0.2); ristretto's own 4-goroutine
	//     concurrent Get costs about 20ns more per call (110ns -> 131ns, noise +-12%).
	//
	// Turn it off per instance when there are many instances, many entries, and memory matters.
	Metric bool `yaml:"Metric"`
}

// DefaultClientConfig holds every default of a single instance in one place.
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		NumCounters: 1_000_000,
		MaxCost:     100_000,
		BufferItems: 64,
		DefaultTTL:  5 * time.Minute,
		Metric:      true,
	}
}

// DefaultConfig has no instances at all -- without XCache configured, no cache is built.
func DefaultConfig() Config { return Config{} }

// loadConfig reads this block of the config file. Both the single-instance and the multi-instance form are
// accepted, each instance is pre-filled with DefaultClientConfig before decoding, and the rules are in xconfig.UnmarshalClients.
func loadConfig() (Config, error) {
	clients, err := xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)
	return Config{Clients: clients}, err
}

// Validate checks what does not make sense in the configuration itself.
//
// xconfig.UnmarshalClients calls it once per decoded instance, so a misconfigured value fails while the config
// is read, naming the instance; a direct call to New calls it once as well.
// It returns a plain error, which the layer calling it wraps in an xerror once.
func (c ClientConfig) Validate() error {
	if c.NumCounters <= 0 {
		return fmt.Errorf("NumCounters must be > 0, got=%d", c.NumCounters)
	}
	if c.MaxCost <= 0 {
		return fmt.Errorf("MaxCost must be > 0, got=%d", c.MaxCost)
	}
	if c.BufferItems <= 0 {
		return fmt.Errorf("BufferItems must be > 0, got=%d", c.BufferItems)
	}
	// ristretto drops a write with ttl<0 outright and returns false (v2.4.2 SetWithTTL), so a negative value
	// would make the package-level Set store nothing at all, without reporting anything.
	if c.DefaultTTL < 0 {
		return fmt.Errorf("DefaultTTL must not be negative (0 means never expire), got=%s", c.DefaultTTL)
	}
	return nil
}
