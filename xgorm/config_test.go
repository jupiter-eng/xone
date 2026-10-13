package xgorm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/config"
)

// load 走真实的配置加载路径，把 YAML 解进本包的配置变量
func load(t *testing.T, yml string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	t.Cleanup(config.Reset)
	if err := config.Load(path); err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("加载失败：%v", err)
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
		t.Fatalf("应解出一个实例，got=%v", c.Clients)
	}
	got, ok := c.Clients[DefaultName]
	if !ok {
		t.Fatalf("单实例写法应规整成名为 %s 的实例，got=%v", DefaultName, c.Clients)
	}
	if got.Driver != DriverMySQL || got.DSN != "u:p@tcp(h:3306)/app" {
		t.Errorf("配置不对，got=%+v", got)
	}
	// 没写的字段应保持默认
	if got.MaxOpenConns != 50 || got.MaxLifetime != 5*time.Minute || !got.Trace {
		t.Errorf("没写的字段应保持默认，got=%+v", got)
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
		t.Fatalf("应解出两个实例，got=%v", c.Clients)
	}
	if c.Clients["default"].MaxOpenConns != 50 {
		t.Errorf("没写的字段应保持默认，got=%+v", c.Clients["default"])
	}
	r := c.Clients["report"]
	if r.MaxOpenConns != 5 || r.Trace {
		t.Errorf("写了的字段应生效，got=%+v", r)
	}
	// 关键：同一个 map 里，一个实例覆盖了的字段不该影响另一个
	if c.Clients["default"].Trace != true {
		t.Error("一个实例关掉 Trace 不该影响另一个")
	}
}

func TestConfig_TypoInCollectionElementFails(t *testing.T) {
	// 集合元素走的是自定义解码器，严格检查很容易在那里悄悄失效
	err := loadErr(t, "XGorm:\n  Clients:\n    default:\n      DSN: x\n      MaxOpenConn: 5\n")
	if err == nil {
		t.Fatal("实例里的字段拼错应当启动失败")
	}
	if !strings.Contains(err.Error(), "MaxOpenConn") {
		t.Errorf("错误里要点名拼错的字段，got=%v", err)
	}
}

func TestConfig_TopLevelTypoFails(t *testing.T) {
	err := loadErr(t, "XGorm:\n  DSN: x\n  MaxOpenConn: 5\n")
	if err == nil {
		t.Fatal("字段拼错应当启动失败")
	}
}

func TestConfig_FormsCannotBeMixed(t *testing.T) {
	// 混着写时「default 到底是哪个」没有不让人意外的答案，所以直接失败
	err := loadErr(t, "XGorm:\n  DSN: x\n  Clients:\n    a:\n      DSN: y\n")
	if err == nil {
		t.Fatal("混用两种写法应当失败")
	}
	if !strings.Contains(err.Error(), "cannot mix") {
		t.Errorf("错误信息该说清楚为什么，got=%v", err)
	}
}

func TestConfig_EmptyClientsFails(t *testing.T) {
	if err := loadErr(t, "XGorm:\n  Clients: {}\n"); err == nil {
		t.Fatal("写了 Clients 却是空的，应当失败")
	}
}

func TestConfig_NoConfigNoInstances(t *testing.T) {
	c := load(t, "# 整个文件里没有 XGorm 这一块\n")
	if len(c.Clients) != 0 {
		t.Errorf("没配 XGorm 就不该连任何数据库，got=%v", c.Clients)
	}
}

func TestConfig_DurationsAndEnvVars(t *testing.T) {
	t.Setenv("TEST_DB_DSN", "host=h dbname=app")
	c := load(t, "XGorm:\n  DSN: \"${TEST_DB_DSN}\"\n  DialTimeout: 2s\n  SlowThreshold: 100ms\n")
	got := c.Clients[DefaultName]
	if got.DSN != "host=h dbname=app" {
		t.Errorf("环境变量应展开，got=%+v", got)
	}
	if got.DialTimeout != 2*time.Second || got.SlowThreshold != 100*time.Millisecond {
		t.Errorf("时长应解析成 Duration，got=%+v", got)
	}
}

func TestConfig_MaxIdleConnsZeroMeansZero(t *testing.T) {
	// 预填默认值的全部意义就在这里：显式写的零值不会被「没配」的逻辑吃掉
	c := load(t, "XGorm:\n  DSN: x\n  MaxIdleConns: 0\n")
	if got := c.Clients[DefaultName].MaxIdleConns; got != 0 {
		t.Errorf("显式配 0 就该是 0，got=%d", got)
	}
}

func TestConfig_BoolFalseMeansFalse(t *testing.T) {
	c := load(t, "XGorm:\n  DSN: x\n  Trace: false\n  Metric: false\n")
	got := c.Clients[DefaultName]
	if got.Trace || got.Metric {
		t.Errorf("显式关掉就该是关的，got=%+v", got)
	}
}

func TestValidate(t *testing.T) {
	ok := DefaultClientConfig()
	ok.DSN = "host=h"
	if err := ok.Validate(); err != nil {
		t.Errorf("合法配置不该报错：%v", err)
	}

	for name, mutate := range map[string]func(*ClientConfig){
		"DSN 为空":           func(c *ClientConfig) { c.DSN = "" },
		"驱动不认识":            func(c *ClientConfig) { c.Driver = "oracle" },
		"MaxOpenConns 为 0": func(c *ClientConfig) { c.MaxOpenConns = 0 },
		"MaxIdleConns 为负":  func(c *ClientConfig) { c.MaxIdleConns = -1 },
		// GORM 只在 > 0 时分批，负数静默变成「不分批」
		"CreateBatchSize 为负": func(c *ClientConfig) { c.CreateBatchSize = -1 },
		// 负的时长底下每一处都静默变成「不限」：go-sql-driver 的 FormatDSN 只写 > 0 的超时，
		// database/sql 把负的存活时间当成 0
		"DialTimeout 为负":               func(c *ClientConfig) { c.DialTimeout = -time.Second },
		"MaxLifetime 为负":               func(c *ClientConfig) { c.MaxLifetime = -time.Second },
		"MaxIdleTime 为负":               func(c *ClientConfig) { c.MaxIdleTime = -time.Second },
		"SlowThreshold 为负":             func(c *ClientConfig) { c.SlowThreshold = -time.Second },
		"MySQL.ReadTimeout 为负":         func(c *ClientConfig) { c.MySQL.ReadTimeout = -time.Second },
		"MySQL.WriteTimeout 为负":        func(c *ClientConfig) { c.MySQL.WriteTimeout = -time.Second },
		"Postgres.StatementTimeout 为负": func(c *ClientConfig) { c.Postgres.StatementTimeout = -time.Second },
		"Postgres.LockTimeout 为负":      func(c *ClientConfig) { c.Postgres.LockTimeout = -time.Second },
		"Postgres.IdleInTxTimeout 为负":  func(c *ClientConfig) { c.Postgres.IdleInTxTimeout = -time.Second },
	} {
		c := ok
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s 应当报错", name)
		}
	}
}

func TestValidate_ZeroDurationIsValid(t *testing.T) {
	// 0 各有写明的含义：DialTimeout 等不注入、用驱动自己的；存活时间不限；SlowThreshold 不记慢日志
	c := DefaultClientConfig()
	c.DSN = "host=h"
	c.DialTimeout, c.MaxLifetime, c.MaxIdleTime, c.SlowThreshold = 0, 0, 0, 0
	c.MySQL = MySQLConfig{}
	if err := c.Validate(); err != nil {
		t.Errorf("0 不该被拒：%v", err)
	}
}

func TestValidate_NegativeMySQLTimeoutVanishesInDSN(t *testing.T) {
	// 这是 Validate 拦负数的理由：go-sql-driver v1.10.1 的 FormatDSN 只写 > 0 的超时，
	// 负数注进去，DSN 里就没有这个超时了。驱动升级后这里不再成立的话，注释要跟着改
	c := mysqlCfg("u:p@tcp(h:3306)/d")
	c.MySQL.ReadTimeout = -time.Second
	dsn, _, err := resolveMySQL(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dsn, "readTimeout") {
		t.Errorf("驱动的行为变了：负的 readTimeout 留在了 DSN 里，%s", dsn)
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
		t.Errorf("错误要说清是哪个实例，got=%v", err)
	}
}

func TestConfig_RejectsInvalidAtLoadWithFileAndLine(t *testing.T) {
	// 拦在读配置这一步：一个实例都还没连，报错里有文件和行号，
	// 而不是等到按名字挨个建连、建到它才失败
	err := loadErr(t, "XGorm:\n  Clients:\n    a:\n      DSN: x\n    b:\n      DSN: y\n      MySQL:\n        ReadTimeout: -3s\n")
	if err == nil {
		t.Fatal("负的 ReadTimeout 读配置时就该失败")
	}
	for _, want := range []string{"Clients.b", "MySQL.ReadTimeout", "application.yml:6"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里应有 %q，got=%v", want, err)
		}
	}
}
