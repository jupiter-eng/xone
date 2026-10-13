// Package harness 是 e2e 测试的辅助：构建并起真实的服务进程、发请求、发信号，
// 读它的日志、Span、指标和 /proc，另有可控的 TCP 代理、下游桩和压测器。
//
// 所有入口都先过 Require：环境变量 XONE_E2E 不为 1 时 t.Skip。scripts/test.sh
// 遍历每个模块跑测试，没有数据库的机器上这个模块必须照样全绿。
// 真要跑用 scripts/e2e.sh，它负责拉起 PG / MySQL / Redis 并导出下面这些连接参数：
//
//	XONE_E2E_PG_ADDR      默认 127.0.0.1:5432
//	XONE_E2E_PG_USER      默认 xone
//	XONE_E2E_PG_PASSWORD  默认 e2e-secret-pw
//	XONE_E2E_PG_DB        默认 xone_e2e
//	XONE_E2E_REDIS_ADDR   默认 127.0.0.1:6379
//	XONE_E2E_MYSQL_ADDR      默认 127.0.0.1:3306
//	XONE_E2E_MYSQL_USER      默认 xone
//	XONE_E2E_MYSQL_PASSWORD  默认 e2e-secret-pw
//	XONE_E2E_MYSQL_DB        默认 xone_e2e
//	XONE_E2E_CH_ADDR      ClickHouse native 协议，默认 127.0.0.1:9000
//	XONE_E2E_CH_HTTP_ADDR ClickHouse HTTP 协议，默认 127.0.0.1:8123
//	XONE_E2E_CH_USER      默认 xone
//	XONE_E2E_CH_PASSWORD  默认 e2e-secret-pw
//	XONE_E2E_CH_DB        默认 xone_e2e
//	XONE_E2E_CH           scripts/e2e.sh 起不来 ClickHouse 时设成 0，CH 的用例跳过（RequireCH）
//	XONE_E2E_KAFKA_ADDR   Kafka broker，默认 127.0.0.1:9092
//	XONE_E2E_KAFKA        scripts/e2e.sh 起不来 Kafka 时设成 0，Kafka 的用例跳过（RequireKafka）
//
// 每个 Start 默认用自己的端口、自己的表名和 Redis key 前缀，测试结束时删掉，
// 所以用例之间互不干扰，可以 t.Parallel。
//
// 带 t 的辅助函数失败时 t.Fatal，只能在测试的主协程里调；
// 在别的协程里发请求用不带 t 的 Request。
package harness

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Require e2e 没开时跳过这个测试。每个 e2e 测试的第一行
func Require(t testing.TB) {
	t.Helper()
	if os.Getenv("XONE_E2E") != "1" {
		t.Skip("e2e tests are off: run scripts/e2e.sh, or set XONE_E2E=1 with PostgreSQL, MySQL and Redis running")
	}
}

// RequireCH ClickHouse 用例的第一行：e2e 没开、或者 ClickHouse 不可用时跳过。
//
// ClickHouse 跑在 Docker 里，不是每台机器都有：scripts/e2e.sh 起不来它时设 XONE_E2E_CH=0，
// 这里跳过并说清为什么；没经过脚本直接跑的，拨一下 native 端口，拨不通同样跳过
func RequireCH(t testing.TB) {
	t.Helper()
	Require(t)
	if os.Getenv("XONE_E2E_CH") == "0" {
		t.Skip("ClickHouse is unavailable (scripts/e2e.sh could not start the xone-ch container): ClickHouse tests skipped")
	}
	c, err := net.DialTimeout("tcp", CHAddr(), time.Second)
	if err != nil {
		t.Skipf("ClickHouse is unavailable at %s (%v): ClickHouse tests skipped, start it with scripts/e2e.sh", CHAddr(), err)
	}
	c.Close()
}

// KnownBug 标记一个测试揭示出来的框架 bug：打印 KNOWN BUG 并跳过。
//
// 断言照文档写的行为写，失败了不为了变绿去改断言，而是在失败的那个分支里调它，
// 证据写在测试的注释里。修好之后这一行自然走不到，测试就回到正常的通过
func KnownBug(t testing.TB, msg string) {
	t.Helper()
	t.Skipf("KNOWN BUG: %s", msg)
}

// Main 给 TestMain 用：跑完全部测试后删掉构建出来的二进制。
//
//	func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
func Main(m *testing.M) int {
	code := m.Run()
	removeBinaries()
	return code
}

// ModuleDir e2e 模块的根目录（e2e/ 的绝对路径）
func ModuleDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(file))
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// PGAddr PostgreSQL 的 host:port，不经代理
func PGAddr() string { return env("XONE_E2E_PG_ADDR", "127.0.0.1:5432") }

// RedisAddr Redis 的 host:port，不经代理
func RedisAddr() string { return env("XONE_E2E_REDIS_ADDR", "127.0.0.1:6379") }

// PGDSN 连到 addr 的 DSN。addr 传 PGAddr() 直连，传 Proxy.Addr() 经代理
func PGDSN(addr string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(env("XONE_E2E_PG_USER", "xone"), env("XONE_E2E_PG_PASSWORD", "e2e-secret-pw")),
		Host:     addr,
		Path:     "/" + env("XONE_E2E_PG_DB", "xone_e2e"),
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// MySQLAddr MySQL 的 host:port，不经代理
func MySQLAddr() string { return env("XONE_E2E_MYSQL_ADDR", "127.0.0.1:3306") }

// MySQLPassword harness 连 MySQL 用的密码
func MySQLPassword() string { return env("XONE_E2E_MYSQL_PASSWORD", "e2e-secret-pw") }

// MySQLDSN 连到 addr 的 go-sql-driver DSN。addr 传 MySQLAddr() 直连，传 Proxy.Addr() 经代理。
//
// 不带任何超时参数：timeout / readTimeout / writeTimeout 由 xgorm 按配置注入，
// 测试要验的正是注入的那一份；要测「DSN 里写了的不被覆盖」时自己在后面拼
func MySQLDSN(addr string) string {
	return env("XONE_E2E_MYSQL_USER", "xone") + ":" + MySQLPassword() + "@tcp(" + addr + ")/" + env("XONE_E2E_MYSQL_DB", "xone_e2e")
}

// CHAddr ClickHouse native 协议的 host:port，不经代理
func CHAddr() string { return env("XONE_E2E_CH_ADDR", "127.0.0.1:9000") }

// CHHTTPAddr ClickHouse HTTP 协议的 host:port，不经代理
func CHHTTPAddr() string { return env("XONE_E2E_CH_HTTP_ADDR", "127.0.0.1:8123") }

// CHPassword harness 连 ClickHouse 用的密码
func CHPassword() string { return env("XONE_E2E_CH_PASSWORD", "e2e-secret-pw") }

// CHDSN 连到 addr 的 native 协议 DSN。addr 传 CHAddr() 直连，传 Proxy.Addr() 经代理。
//
// 不带任何超时参数：dial_timeout 由 xgorm 按 DialTimeout 注入，测试要验的正是注入的那一份
func CHDSN(addr string) string { return CHDSNWith("clickhouse", addr, CHPassword()) }

// CHDSNWith 指定 scheme（clickhouse / tcp / http / https）和密码的 DSN
func CHDSNWith(scheme, addr, password string) string {
	u := url.URL{
		Scheme: scheme,
		User:   url.UserPassword(env("XONE_E2E_CH_USER", "xone"), password),
		Host:   addr,
		Path:   "/" + env("XONE_E2E_CH_DB", "xone_e2e"),
	}
	return u.String()
}

// NewID 一个随机的小写标识符，比如 3fa9c1d2b7e4。表名、key 前缀用它拼
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var ports struct {
	mu   sync.Mutex
	used map[int]bool
}

// FreePort 找一个本机空闲端口。同一个测试进程里不会发出两次同一个端口
func FreePort(t testing.TB) int {
	t.Helper()
	ports.mu.Lock()
	defer ports.mu.Unlock()
	if ports.used == nil {
		ports.used = map[int]bool{}
	}
	for range 100 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("find a free port: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		if !ports.used[port] {
			ports.used[port] = true
			return port
		}
	}
	t.Fatal("find a free port: every port the kernel offered was already handed out")
	return 0
}
