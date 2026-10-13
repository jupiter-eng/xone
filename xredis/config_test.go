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
	c := load(t, "XRedis:\n  Addr: 10.0.0.1:6379\n  DB: 3\n")
	got, ok := c.Clients[DefaultName]
	if !ok {
		t.Fatalf("单实例写法应规整成名为 %s 的实例，got=%v", DefaultName, c.Clients)
	}
	if got.Addr != "10.0.0.1:6379" || got.DB != 3 {
		t.Errorf("配置不对，got=%+v", got)
	}
	if got.MinIdleConns != 5 || got.ConnMaxLifetime != 5*time.Minute || !got.Trace {
		t.Errorf("没写的字段应保持默认，got=%+v", got)
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
		t.Fatalf("应解出两个实例，got=%v", c.Clients)
	}
	if !c.Clients["default"].Trace {
		t.Error("一个实例关掉 Trace 不该影响另一个")
	}
	if cache := c.Clients["cache"]; cache.DB != 1 || cache.Trace {
		t.Errorf("写了的字段应生效，got=%+v", cache)
	}
}

func TestConfig_TypoInCollectionElementFails(t *testing.T) {
	err := loadErr(t, "XRedis:\n  Clients:\n    default:\n      Adrr: x\n")
	if err == nil {
		t.Fatal("实例里的字段拼错应当启动失败")
	}
	if !strings.Contains(err.Error(), "Adrr") {
		t.Errorf("错误里要点名拼错的字段，got=%v", err)
	}
}

func TestConfig_FormsCannotBeMixed(t *testing.T) {
	if err := loadErr(t, "XRedis:\n  Addr: x\n  Clients:\n    a:\n      Addr: y\n"); err == nil {
		t.Fatal("混用两种写法应当失败")
	}
}

func TestConfig_EmptyClientsFails(t *testing.T) {
	if err := loadErr(t, "XRedis:\n  Clients: {}\n"); err == nil {
		t.Fatal("写了 Clients 却是空的，应当失败")
	}
}

func TestConfig_NoInstanceWhenUnconfigured(t *testing.T) {
	if c := load(t, "# 没有 XRedis 这一块\n"); len(c.Clients) != 0 {
		t.Errorf("没配就不该连任何 Redis，got=%v", c.Clients)
	}
}

func TestConfig_PasswordFromEnvVar(t *testing.T) {
	// 凭证不该进版本库
	t.Setenv("TEST_REDIS_PASSWORD", "hunter2")
	c := load(t, "XRedis:\n  Addr: h:6379\n  Password: \"${TEST_REDIS_PASSWORD}\"\n")
	if c.Clients[DefaultName].Password != "hunter2" {
		t.Errorf("环境变量应展开，got=%+v", c.Clients[DefaultName])
	}
}

func TestConfig_MissingRequiredEnvVarFailsStartup(t *testing.T) {
	os.Unsetenv("TEST_REDIS_PASSWORD_MISSING")
	err := loadErr(t, "XRedis:\n  Password: \"${TEST_REDIS_PASSWORD_MISSING}\"\n")
	if err == nil {
		t.Fatal("凭证漏配应当启动失败，而不是静默变成空串")
	}
}

func TestConfig_NegativeRetriesMeansDisabledNotUnset(t *testing.T) {
	// go-redis 用 -1 表示「关掉」，0 表示「用默认」，两者都得传得下去
	c := load(t, "XRedis:\n  Addr: h:6379\n  MaxRetries: -1\n  MinRetryBackoff: -1ns\n")
	got := c.Clients[DefaultName]
	if got.MaxRetries != -1 || got.MinRetryBackoff != -1 {
		t.Errorf("负值应原样传给 go-redis，got=%+v", got)
	}
}

func TestConfig_BareMinusOneBackoffFailsStartup(t *testing.T) {
	// 时长得带单位，文档里写的是 -1ns。裸写 -1 解不成时长，要在启动时报出来
	if err := loadErr(t, "XRedis:\n  Addr: h:6379\n  MinRetryBackoff: -1\n"); err == nil {
		t.Fatal("裸写 -1 应当解码失败")
	}
}

func TestConfig_GoRedisBackoffDefaultsMatchDocs(t *testing.T) {
	// 注释和 xredis/README.md 写的是 10ms / 1s（v9.22.0）。升级 go-redis 之后
	// 数字变了，这里先红，文档跟着改
	rc := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer rc.Close()
	o := rc.Options()
	if o.MinRetryBackoff != 10*time.Millisecond || o.MaxRetryBackoff != time.Second {
		t.Errorf("go-redis 的默认退避变了：min=%v max=%v", o.MinRetryBackoff, o.MaxRetryBackoff)
	}
}

func TestValidate(t *testing.T) {
	ok := DefaultClientConfig()
	if err := ok.Validate(); err != nil {
		t.Errorf("默认配置应当合法：%v", err)
	}

	bad := ok
	bad.Addr = ""
	if err := bad.Validate(); err == nil {
		t.Error("Addr 为空应当报错")
	}

	bad = ok
	bad.DB = -1
	if err := bad.Validate(); err == nil {
		t.Error("DB 为负应当报错")
	}

	for name, mutate := range map[string]func(*ClientConfig){
		"PoolSize 为负":       func(c *ClientConfig) { c.PoolSize = -1 },
		"MinIdleConns 为负":   func(c *ClientConfig) { c.MinIdleConns = -1 },
		"MaxIdleConns 为负":   func(c *ClientConfig) { c.MaxIdleConns = -1 },
		"MaxActiveConns 为负": func(c *ClientConfig) { c.MaxActiveConns = -1 },
		"DialTimeout 为负":    func(c *ClientConfig) { c.DialTimeout = -time.Second },
		// go-redis 把 ReadTimeout -1 当成「不限时」：一个减号就关掉了超时保护
		"ReadTimeout 为负":         func(c *ClientConfig) { c.ReadTimeout = -1 },
		"WriteTimeout 为负":        func(c *ClientConfig) { c.WriteTimeout = -time.Second },
		"PoolTimeout 为负":         func(c *ClientConfig) { c.PoolTimeout = -time.Second },
		"ConnMaxIdleTime 为负":     func(c *ClientConfig) { c.ConnMaxIdleTime = -1 },
		"ConnMaxLifetime 为负":     func(c *ClientConfig) { c.ConnMaxLifetime = -time.Second },
		"SlowThreshold 为负":       func(c *ClientConfig) { c.SlowThreshold = -time.Millisecond },
		"MaxRetries 小于 -1":       func(c *ClientConfig) { c.MaxRetries = -2 },
		"MinRetryBackoff 为 -2ns": func(c *ClientConfig) { c.MinRetryBackoff = -2 },
		"MaxRetryBackoff 为 -1s":  func(c *ClientConfig) { c.MaxRetryBackoff = -time.Second },
		"没开 TLS 却写了 CAFile":      func(c *ClientConfig) { c.TLS.CAFile = "/etc/ca.pem" },
		"CertFile 没配 KeyFile":    func(c *ClientConfig) { c.TLS = xtls.Config{Enable: true, CertFile: "c.pem"} },
	} {
		c := ok
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s 应当报错", name)
		}
	}

	// 写进文档的暗号：-1 关重试、-1ns 关退避
	sentinel := ok
	sentinel.MaxRetries, sentinel.MinRetryBackoff, sentinel.MaxRetryBackoff = -1, -1, -1
	if err := sentinel.Validate(); err != nil {
		t.Errorf("文档里写的 -1 / -1ns 应当合法：%v", err)
	}
}

func TestConfig_InvalidValuesFailAtConfigRead(t *testing.T) {
	// 读配置时就拦下，报错里带着是哪个实例；不等到建连才发现
	err := loadErr(t, "XRedis:\n  Clients:\n    session: {Addr: h:6379, ReadTimeout: -1s}\n")
	if err == nil {
		t.Fatal("ReadTimeout 为负应当在读配置时失败")
	}
	if !strings.Contains(err.Error(), "session") || !strings.Contains(err.Error(), "ReadTimeout") {
		t.Errorf("错误里要点名实例和字段，got=%v", err)
	}
}

func TestNew_NoClientSetinfoNorMaintNotifications(t *testing.T) {
	// go-redis v9.22.0 默认每条新连接发 CLIENT SETINFO 和 CLIENT MAINT_NOTIFICATIONS，
	// Redis 7.2 之前两条都回 unknown subcommand，链路开着时每条连接一个报错的 Span
	f := newFakeRedis(t)
	// 维护通知的握手 go-redis 只在协商成 RESP3 的连接上发，假服务端得认 HELLO 3
	f.setResp3(true)
	client, closer, err := New(context.Background(), liveCfg(f))
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if n := f.count("hello"); n == 0 {
		t.Fatal("前提：建连时该发过 HELLO")
	}

	o := client.Options()
	if !o.DisableIdentity {
		t.Error("DisableIdentity 该打开")
	}
	if o.MaintNotificationsConfig == nil || o.MaintNotificationsConfig.Mode != maintnotifications.ModeDisabled {
		t.Errorf("维护通知该关掉，got=%+v", o.MaintNotificationsConfig)
	}
	if n := f.count("client"); n != 0 {
		t.Errorf("建连时不该发 CLIENT 子命令，收到 %d 次", n)
	}
}

func TestConfig_LogDefaultsAndOverride(t *testing.T) {
	// Log 默认关着；SlowThreshold 默认 100ms，只在 Log 开着时有用
	c := load(t, "XRedis:\n  Addr: h:6379\n")
	if got := c.Clients[DefaultName]; got.Log || got.SlowThreshold != 100*time.Millisecond {
		t.Errorf("默认 Log=false、SlowThreshold=100ms，got Log=%v SlowThreshold=%v", got.Log, got.SlowThreshold)
	}
	c = load(t, "XRedis:\n  Clients:\n    a: {Addr: h:6379, Log: true, SlowThreshold: 0s}\n    b: {Addr: h:6380}\n")
	if a, b := c.Clients["a"], c.Clients["b"]; !a.Log || a.SlowThreshold != 0 || b.Log {
		t.Errorf("Log 按实例生效，got a=%+v b=%+v", a, b)
	}
}
