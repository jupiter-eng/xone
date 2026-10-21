package xgorm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/config"
)

// load goes through the real config loading path and decodes YAML into this package's config.
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
	c := load(t, "XGorm:\n  Driver: mysql\n  DSN: u:p@tcp(h:3306)/app\n")
	if len(c.Clients) != 1 {
		t.Fatalf("one instance is expected, got=%v", c.Clients)
	}
	got, ok := c.Clients[DefaultName]
	if !ok {
		t.Fatalf("the single-instance form should normalize into an instance named %s, got=%v", DefaultName, c.Clients)
	}
	if got.Driver != DriverMySQL || got.DSN != "u:p@tcp(h:3306)/app" {
		t.Errorf("the config is wrong, got=%+v", got)
	}
	// Fields left out should keep their defaults
	if got.MaxOpenConns != 50 || got.MaxLifetime != 5*time.Minute || !got.Trace {
		t.Errorf("fields left out should keep their defaults, got=%+v", got)
	}
}

func TestConfig_MultiInstanceForm(t *testing.T) {
	c := load(t, `
XGorm:
  Clients:
    default:
      DSN: host=h1 dbname=main
    report:
      DSN: host=h2 dbname=report
      MaxOpenConns: 5
      Trace: false
`)
	if len(c.Clients) != 2 {
		t.Fatalf("two instances are expected, got=%v", c.Clients)
	}
	if c.Clients["default"].MaxOpenConns != 50 {
		t.Errorf("fields left out should keep their defaults, got=%+v", c.Clients["default"])
	}
	r := c.Clients["report"]
	if r.MaxOpenConns != 5 || r.Trace {
		t.Errorf("a field that was written should take effect, got=%+v", r)
	}
	// The point: within one map, a field one instance overrode must not affect another
	if c.Clients["default"].Trace != true {
		t.Error("one instance turning Trace off must not affect another")
	}
}

func TestConfig_TypoInCollectionElementFails(t *testing.T) {
	// Collection elements go through a custom decoder, which is where strict checking silently
	// stops working most easily
	err := loadErr(t, "XGorm:\n  Clients:\n    default:\n      DSN: x\n      MaxOpenConn: 5\n")
	if err == nil {
		t.Fatal("a misspelled field inside an instance should fail startup")
	}
	if !strings.Contains(err.Error(), "MaxOpenConn") {
		t.Errorf("the error has to name the misspelled field, got=%v", err)
	}
}

func TestConfig_TopLevelTypoFails(t *testing.T) {
	err := loadErr(t, "XGorm:\n  DSN: x\n  MaxOpenConn: 5\n")
	if err == nil {
		t.Fatal("a misspelled field should fail startup")
	}
}

func TestConfig_FormsCannotBeMixed(t *testing.T) {
	// With both written, "which one is default" has no answer that does not surprise somebody, so it
	// fails outright
	err := loadErr(t, "XGorm:\n  DSN: x\n  Clients:\n    a:\n      DSN: y\n")
	if err == nil {
		t.Fatal("mixing the two forms should fail")
	}
	if !strings.Contains(err.Error(), "cannot mix") {
		t.Errorf("the error should say why, got=%v", err)
	}
}

func TestConfig_EmptyClientsFails(t *testing.T) {
	if err := loadErr(t, "XGorm:\n  Clients: {}\n"); err == nil {
		t.Fatal("Clients written with nothing under it should fail")
	}
}

func TestConfig_NoConfigNoInstances(t *testing.T) {
	c := load(t, "# no XGorm block anywhere in this file\n")
	if len(c.Clients) != 0 {
		t.Errorf("without an XGorm block no database should be connected, got=%v", c.Clients)
	}
}

func TestConfig_DurationsAndEnvVars(t *testing.T) {
	t.Setenv("TEST_DB_DSN", "host=h dbname=app")
	c := load(t, "XGorm:\n  DSN: \"${TEST_DB_DSN}\"\n  DialTimeout: 2s\n  SlowThreshold: 100ms\n")
	got := c.Clients[DefaultName]
	if got.DSN != "host=h dbname=app" {
		t.Errorf("environment variables should expand, got=%+v", got)
	}
	if got.DialTimeout != 2*time.Second || got.SlowThreshold != 100*time.Millisecond {
		t.Errorf("a duration should parse into a Duration, got=%+v", got)
	}
}

func TestConfig_MaxIdleConnsZeroMeansZero(t *testing.T) {
	// This is the whole point of pre-filled defaults: an explicitly written zero is not swallowed by
	// the "not configured" logic
	c := load(t, "XGorm:\n  DSN: x\n  MaxIdleConns: 0\n")
	if got := c.Clients[DefaultName].MaxIdleConns; got != 0 {
		t.Errorf("an explicit 0 should be 0, got=%d", got)
	}
}

func TestConfig_BoolFalseMeansFalse(t *testing.T) {
	c := load(t, "XGorm:\n  DSN: x\n  Trace: false\n  Metric: false\n")
	got := c.Clients[DefaultName]
	if got.Trace || got.Metric {
		t.Errorf("explicitly off should be off, got=%+v", got)
	}
}

func TestValidate(t *testing.T) {
	ok := DefaultClientConfig()
	ok.DSN = "host=h"
	if err := ok.Validate(); err != nil {
		t.Errorf("a valid config must not be rejected: %v", err)
	}

	for name, mutate := range map[string]func(*ClientConfig){
		"empty DSN":             func(c *ClientConfig) { c.DSN = "" },
		"unknown driver":        func(c *ClientConfig) { c.Driver = "oracle" },
		"MaxOpenConns 0":        func(c *ClientConfig) { c.MaxOpenConns = 0 },
		"MaxIdleConns negative": func(c *ClientConfig) { c.MaxIdleConns = -1 },
		// GORM only batches above 0, and a negative value silently becomes "do not batch"
		"CreateBatchSize negative": func(c *ClientConfig) { c.CreateBatchSize = -1 },
		// Every layer underneath turns a negative duration silently into "no limit": go-sql-driver's
		// FormatDSN only writes timeouts above 0, and database/sql treats a negative lifetime as 0
		"DialTimeout negative":               func(c *ClientConfig) { c.DialTimeout = -time.Second },
		"MaxLifetime negative":               func(c *ClientConfig) { c.MaxLifetime = -time.Second },
		"MaxIdleTime negative":               func(c *ClientConfig) { c.MaxIdleTime = -time.Second },
		"SlowThreshold negative":             func(c *ClientConfig) { c.SlowThreshold = -time.Second },
		"MySQL.ReadTimeout negative":         func(c *ClientConfig) { c.MySQL.ReadTimeout = -time.Second },
		"MySQL.WriteTimeout negative":        func(c *ClientConfig) { c.MySQL.WriteTimeout = -time.Second },
		"Postgres.StatementTimeout negative": func(c *ClientConfig) { c.Postgres.StatementTimeout = -time.Second },
		"Postgres.LockTimeout negative":      func(c *ClientConfig) { c.Postgres.LockTimeout = -time.Second },
		"Postgres.IdleInTxTimeout negative":  func(c *ClientConfig) { c.Postgres.IdleInTxTimeout = -time.Second },
	} {
		c := ok
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

func TestValidate_ZeroDurationIsValid(t *testing.T) {
	// Each 0 has a documented meaning: DialTimeout and friends inject nothing and the driver's own
	// applies; a lifetime is unlimited; SlowThreshold logs no slow query
	c := DefaultClientConfig()
	c.DSN = "host=h"
	c.DialTimeout, c.MaxLifetime, c.MaxIdleTime, c.SlowThreshold = 0, 0, 0, 0
	c.MySQL = MySQLConfig{}
	if err := c.Validate(); err != nil {
		t.Errorf("0 must not be rejected: %v", err)
	}
}

func TestValidate_NegativeMySQLTimeoutVanishesInDSN(t *testing.T) {
	// This is why Validate rejects negatives: go-sql-driver v1.10.1's FormatDSN only writes timeouts
	// above 0, so a negative one injected disappears from the DSN. When a driver upgrade makes this
	// no longer hold, the comment has to change with it
	c := mysqlCfg("u:p@tcp(h:3306)/d")
	c.MySQL.ReadTimeout = -time.Second
	dsn, _, err := resolveMySQL(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dsn, "readTimeout") {
		t.Errorf("the driver's behavior changed: a negative readTimeout stayed in the DSN, %s", dsn)
	}
}

func TestConfig_ValidateNamesInstance(t *testing.T) {
	c := Config{Clients: map[string]ClientConfig{"ok": DefaultClientConfig(), "bad": DefaultClientConfig()}}
	for name, cc := range c.Clients {
		cc.DSN = "host=h"
		c.Clients[name] = cc
	}
	bad := c.Clients["bad"]
	bad.MaxLifetime = -time.Second
	c.Clients["bad"] = bad
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "Clients.bad") || strings.Contains(err.Error(), "Clients.ok") {
		t.Errorf("the error has to say which instance, got=%v", err)
	}
}

func TestConfig_RejectsInvalidAtLoadWithFileAndLine(t *testing.T) {
	// Stopped while reading the configuration: not one instance has connected, and the error carries
	// the file and the line, rather than waiting until the instances are built by name and that one
	// fails
	err := loadErr(t, "XGorm:\n  Clients:\n    a:\n      DSN: x\n    b:\n      DSN: y\n      MySQL:\n        ReadTimeout: -3s\n")
	if err == nil {
		t.Fatal("a negative ReadTimeout should fail while reading the config")
	}
	for _, want := range []string{"Clients.b", "MySQL.ReadTimeout", "application.yml:6"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should contain %q, got=%v", want, err)
		}
	}
}
