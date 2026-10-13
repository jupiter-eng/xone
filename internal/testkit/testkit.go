// Package testkit 仓库自己的单元测试共用的小工具：量协程数、抓一次 /metrics、
// 让 slog 闭嘴、找空闲端口、把配置文件交给 XONE_CONFIG。
//
// 只给本仓库的测试用，所以放在 internal 下；只依赖标准库和 internal/config，不给核心模块图添任何东西。
// 换一份配置并当场加载、跑钩子用公开的 xonetest，这里不重复。
// example/ 不能用它（check.sh 查 example/ 不 import internal/），那里照抄使用者的写法。
package testkit

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/config"
)

// Stabilize 等协程数不再变化（连续三次 50ms 采样相同），用来取一个基准值。最多等 10 秒
func Stabilize() int {
	last := runtime.NumGoroutine()
	stable := 0
	for i := 0; i < 200; i++ {
		time.Sleep(50 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == last {
			if stable++; stable >= 3 {
				return n
			}
			continue
		}
		last, stable = n, 0
	}
	return last
}

// SettleTo 等协程数回落到 target+1 以内，最多等 10 秒，超时返回实际值。
//
// 不能用「连续几次读数相同」当作稳定：后台协程是一批批退出的，
// 中间会有好几百毫秒纹丝不动，那时候读三次都一样，却离回落还远。
// 上一版就是这么误报的——它在半路上就宣布「稳定了，还剩 8 个」。
//
// 它只能判断「回落了没有」，判断「涨了没有」用 ClimbTo：拿它判增长的话，
// 没泄漏时当场返回会误报，有泄漏时又必然烧满整个 10 秒才肯返回。
func SettleTo(target int) int {
	for i := 0; i < 200; i++ {
		if n := runtime.NumGoroutine(); n <= target+1 {
			return n
		}
		time.Sleep(50 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}

// ClimbTo 等协程数涨到 want，最多等 d，返回最后一次读数
func ClimbTo(want int, d time.Duration) int {
	deadline := time.Now().Add(d)
	n := runtime.NumGoroutine()
	for n < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

// Scrape 对 h 发一次 GET /metrics，返回响应体
func Scrape(h http.Handler) string {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}

// QuietSlog 在这个测试里把 slog 默认 logger 换成丢弃输出的 JSON handler，结束时还原。
// 仍然走 JSON 编码，基准测到的是真实的日志开销，只是不写出去
func QuietSlog(tb testing.TB) {
	tb.Helper()
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	tb.Cleanup(func() { slog.SetDefault(old) })
}

// FreePort 找一个 127.0.0.1 上的空闲端口
func FreePort(tb testing.TB) int {
	tb.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("testkit: listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// UseConfigEnv 把 yml 写成配置文件、交给 XONE_CONFIG，但不加载：第一次有人读的时候才加载，
// 和使用者的程序一样（main 顶上、xone.Run 之前就装配的也走这条路）。测试结束时清掉。
// 要当场加载用 xonetest.UseConfigYAML
func UseConfigEnv(t *testing.T, yml string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatalf("testkit: write config: %v", err)
	}
	t.Setenv(config.EnvKey, path)
	config.Reset()
	t.Cleanup(config.Reset)
}
