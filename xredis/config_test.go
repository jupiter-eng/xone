package xredis

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/xtls"
)

func load(t *testing.T, yml string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	t.Cleanup(config.Reset)
	if err := config.Load(path); err != nil {
		t.Fatalf("loading failed: %v", err)
	}
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loading failed: %v", err)
	}
	return c
}

func loadErr(t *testing.T, yml string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(path, []byte(yml), 0o644)
	config.Reset()
	t.Cleanup(config.Reset)
	if err := config.Load(path); err != nil {
		return err
	}
	_, err := loadConfig()
	return err
}

func TestConfig_SingleInstanceForm(t *testing.T) {
	c := load(t, "XRedis:\n  Addr: 10.0.0.1:6379\n  DB: 3\n")
	got, ok := c.Clients[DefaultName]
	if !ok {
		t.Fatalf("the single-instance form must be normalized to an instance named %s, got=%v", DefaultName, c.Clients)
	}
	if got.Addr != "10.0.0.1:6379" || got.DB != 3 {
		t.Errorf("the configuration is wrong, got=%+v", got)
	}
	if got.MinIdleConns != 5 || got.ConnMaxLifetime != 5*time.Minute || !got.Trace {
		t.Errorf("a field that is not set must keep its default, got=%+v", got)
	}
}

func TestConfig_MultiInstanceForm(t *testing.T) {
	c := load(t, `
XRedis:
  Clients:
    default:
      Addr: 10.0.0.1:6379
    cache:
      Addr: 10.0.0.2:6379
      DB: 1
      Trace: false
`)
	if len(c.Clients) != 2 {
		t.Fatalf("two instances must be decoded, got=%v", c.Clients)
	}
	if !c.Clients["default"].Trace {
		t.Error("one instance turning Trace off must not affect the other")
	}
	if cache := c.Clients["cache"]; cache.DB != 1 || cache.Trace {
		t.Errorf("a field that is set must take effect, got=%+v", cache)
	}
}

func TestConfig_TypoInCollectionElementFails(t *testing.T) {
	err := loadErr(t, "XRedis:\n  Clients:\n    default:\n      Adrr: x\n")
	if err == nil {
		t.Fatal("a misspelled field inside an instance must fail startup")
	}
	if !strings.Contains(err.Error(), "Adrr") {
		t.Errorf("the error must name the misspelled field, got=%v", err)
	}
}

func TestConfig_FormsCannotBeMixed(t *testing.T) {
	if err := loadErr(t, "XRedis:\n  Addr: x\n  Clients:\n    a:\n      Addr: y\n"); err == nil {
		t.Fatal("mixing the two forms must fail")
	}
}

func TestConfig_EmptyClientsFails(t *testing.T) {
	if err := loadErr(t, "XRedis:\n  Clients: {}\n"); err == nil {
		t.Fatal("writing Clients and leaving it empty must fail")
	}
}

func TestConfig_NoInstanceWhenUnconfigured(t *testing.T) {
	if c := load(t, "# no XRedis block\n"); len(c.Clients) != 0 {
		t.Errorf("without it configured no Redis may be connected to, got=%v", c.Clients)
	}
}

func TestConfig_PasswordFromEnvVar(t *testing.T) {
	// A credential does not belong in version control.
	t.Setenv("TEST_REDIS_PASSWORD", "hunter2")
	c := load(t, "XRedis:\n  Addr: h:6379\n  Password: \"${TEST_REDIS_PASSWORD}\"\n")
	if c.Clients[DefaultName].Password != "hunter2" {
		t.Errorf("the environment variable must be expanded, got=%+v", c.Clients[DefaultName])
	}
}

func TestConfig_MissingRequiredEnvVarFailsStartup(t *testing.T) {
	os.Unsetenv("TEST_REDIS_PASSWORD_MISSING")
	err := loadErr(t, "XRedis:\n  Password: \"${TEST_REDIS_PASSWORD_MISSING}\"\n")
	if err == nil {
		t.Fatal("a missing credential must fail startup rather than quietly become an empty string")
	}
}

func TestConfig_NegativeRetriesMeansDisabledNotUnset(t *testing.T) {
	// go-redis uses -1 for "off" and 0 for "use the default", and both have to travel down.
	c := load(t, "XRedis:\n  Addr: h:6379\n  MaxRetries: -1\n  MinRetryBackoff: -1ns\n")
	got := c.Clients[DefaultName]
	if got.MaxRetries != -1 || got.MinRetryBackoff != -1 {
		t.Errorf("a negative value must be passed to go-redis as it is, got=%+v", got)
	}
}

func TestConfig_BareMinusOneBackoffFailsStartup(t *testing.T) {
	// A duration carries a unit, and the docs say -1ns. A bare -1 does not parse as a duration and has to be reported at startup.
	if err := loadErr(t, "XRedis:\n  Addr: h:6379\n  MinRetryBackoff: -1\n"); err == nil {
		t.Fatal("a bare -1 must fail to decode")
	}
}

func TestConfig_GoRedisBackoffDefaultsMatchDocs(t *testing.T) {
	// The comments and xredis/README.md state 10ms / 1s (v9.22.0). When a go-redis upgrade changes the
	// numbers this goes red first and the documentation follows.
	rc := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer rc.Close()
	o := rc.Options()
	if o.MinRetryBackoff != 10*time.Millisecond || o.MaxRetryBackoff != time.Second {
		t.Errorf("go-redis's default backoff changed: min=%v max=%v", o.MinRetryBackoff, o.MaxRetryBackoff)
	}
}

func TestValidate(t *testing.T) {
	ok := DefaultClientConfig()
	if err := ok.Validate(); err != nil {
		t.Errorf("the default configuration must be valid: %v", err)
	}

	bad := ok
	bad.Addr = ""
	if err := bad.Validate(); err == nil {
		t.Error("an empty Addr must be an error")
	}

	bad = ok
	bad.DB = -1
	if err := bad.Validate(); err == nil {
		t.Error("a negative DB must be an error")
	}

	for name, mutate := range map[string]func(*ClientConfig){
		"negative PoolSize":       func(c *ClientConfig) { c.PoolSize = -1 },
		"negative MinIdleConns":   func(c *ClientConfig) { c.MinIdleConns = -1 },
		"negative MaxIdleConns":   func(c *ClientConfig) { c.MaxIdleConns = -1 },
		"negative MaxActiveConns": func(c *ClientConfig) { c.MaxActiveConns = -1 },
		"negative DialTimeout":    func(c *ClientConfig) { c.DialTimeout = -time.Second },
		// go-redis takes ReadTimeout -1 as "no limit": one minus sign turns the timeout protection off
		"negative ReadTimeout":      func(c *ClientConfig) { c.ReadTimeout = -1 },
		"negative WriteTimeout":     func(c *ClientConfig) { c.WriteTimeout = -time.Second },
		"negative PoolTimeout":      func(c *ClientConfig) { c.PoolTimeout = -time.Second },
		"negative ConnMaxIdleTime":  func(c *ClientConfig) { c.ConnMaxIdleTime = -1 },
		"negative ConnMaxLifetime":  func(c *ClientConfig) { c.ConnMaxLifetime = -time.Second },
		"negative SlowThreshold":    func(c *ClientConfig) { c.SlowThreshold = -time.Millisecond },
		"MaxRetries below -1":       func(c *ClientConfig) { c.MaxRetries = -2 },
		"MinRetryBackoff of -2ns":   func(c *ClientConfig) { c.MinRetryBackoff = -2 },
		"MaxRetryBackoff of -1s":    func(c *ClientConfig) { c.MaxRetryBackoff = -time.Second },
		"CAFile set without TLS on": func(c *ClientConfig) { c.TLS.CAFile = "/etc/ca.pem" },
		"CertFile without KeyFile":  func(c *ClientConfig) { c.TLS = xtls.Config{Enable: true, CertFile: "c.pem"} },
	} {
		c := ok
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s must be an error", name)
		}
	}

	// The documented signals: -1 turns retrying off and -1ns turns the backoff off.
	sentinel := ok
	sentinel.MaxRetries, sentinel.MinRetryBackoff, sentinel.MaxRetryBackoff = -1, -1, -1
	if err := sentinel.Validate(); err != nil {
		t.Errorf("the documented -1 / -1ns must be valid: %v", err)
	}
}

func TestConfig_InvalidValuesFailAtConfigRead(t *testing.T) {
	// Caught while the config is read, with the instance named in the error; not discovered only at connect time.
	err := loadErr(t, "XRedis:\n  Clients:\n    session: {Addr: h:6379, ReadTimeout: -1s}\n")
	if err == nil {
		t.Fatal("a negative ReadTimeout must fail while the config is read")
	}
	if !strings.Contains(err.Error(), "session") || !strings.Contains(err.Error(), "ReadTimeout") {
		t.Errorf("the error must name the instance and the field, got=%v", err)
	}
}

func TestNew_NoClientSetinfoNorMaintNotifications(t *testing.T) {
	// go-redis v9.22.0 sends CLIENT SETINFO and CLIENT MAINT_NOTIFICATIONS on every new connection by default,
	// and before Redis 7.2 both answer unknown subcommand, leaving one failing Span per connection with tracing on.
	f := newFakeRedis(t)
	// go-redis only sends the maintenance-notification handshake on a connection negotiated as RESP3, so the fake server has to accept HELLO 3.
	f.setResp3(true)
	client, closer, err := New(context.Background(), liveCfg(f))
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if n := f.count("hello"); n == 0 {
		t.Fatal("precondition: a HELLO must have been sent while connecting")
	}

	o := client.Options()
	if !o.DisableIdentity {
		t.Error("DisableIdentity must be on")
	}
	if o.MaintNotificationsConfig == nil || o.MaintNotificationsConfig.Mode != maintnotifications.ModeDisabled {
		t.Errorf("maintenance notifications must be off, got=%+v", o.MaintNotificationsConfig)
	}
	if n := f.count("client"); n != 0 {
		t.Errorf("no CLIENT subcommand may be sent while connecting, received %d", n)
	}
}

func TestConfig_LogDefaultsAndOverride(t *testing.T) {
	// Log is off by default; SlowThreshold defaults to 100ms and only matters with Log on.
	c := load(t, "XRedis:\n  Addr: h:6379\n")
	if got := c.Clients[DefaultName]; got.Log || got.SlowThreshold != 100*time.Millisecond {
		t.Errorf("the defaults are Log=false and SlowThreshold=100ms, got Log=%v SlowThreshold=%v", got.Log, got.SlowThreshold)
	}
	c = load(t, "XRedis:\n  Clients:\n    a: {Addr: h:6379, Log: true, SlowThreshold: 0s}\n    b: {Addr: h:6380}\n")
	if a, b := c.Clients["a"], c.Clients["b"]; !a.Log || a.SlowThreshold != 0 || b.Log {
		t.Errorf("Log applies per instance, got a=%+v b=%+v", a, b)
	}
}
