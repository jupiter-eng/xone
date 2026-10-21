// Package testkit holds the small tools this repository's own unit tests share: counting
// goroutines, scraping /metrics once, keeping slog quiet, finding a free port, and handing a
// configuration file to XONE_CONFIG.
//
// It is for this repository's tests alone, which is why it sits under internal; it depends on the
// standard library and internal/config alone and adds nothing to the core's module graph.
// Swapping in a configuration and loading it on the spot, and running the hooks, are what the
// public xonetest does, and are not repeated here.
// example/ cannot use it (check.sh verifies that example/ does not import internal/), and spells
// things out there the way a caller would.
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

// Stabilize waits for the goroutine count to stop changing (three 50ms samples in a row that
// agree), to take a baseline. It waits 10 seconds at most
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

// SettleTo waits for the goroutine count to fall back to within target+1, 10 seconds at most, and
// returns the real count on a timeout.
//
// "A few readings in a row that agree" will not do as settled: the background goroutines exit in
// batches, with several hundred milliseconds of no movement in between, and three readings then
// agree while the count is still far from where it is going.
// That is exactly how the previous version misreported -- it announced "settled, 8 left" halfway
// through.
//
// It can only tell whether the count has fallen back; use ClimbTo to tell whether it has risen:
// judging growth with this one returns at once without a leak, which misreports, and with a leak
// burns the whole 10 seconds before returning.
func SettleTo(target int) int {
	for i := 0; i < 200; i++ {
		if n := runtime.NumGoroutine(); n <= target+1 {
			return n
		}
		time.Sleep(50 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}

// ClimbTo waits for the goroutine count to rise to want, d at most, and returns the last reading
func ClimbTo(want int, d time.Duration) int {
	deadline := time.Now().Add(d)
	n := runtime.NumGoroutine()
	for n < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

// Scrape sends one GET /metrics to h and returns the response body
func Scrape(h http.Handler) string {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}

// QuietSlog swaps slog's default logger for a JSON handler that discards its output for the length
// of this test and restores it at the end.
// It still goes through the JSON encoding, so a benchmark measures the real cost of logging; it is
// simply not written out
func QuietSlog(tb testing.TB) {
	tb.Helper()
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	tb.Cleanup(func() { slog.SetDefault(old) })
}

// FreePort finds a free port on 127.0.0.1
func FreePort(tb testing.TB) int {
	tb.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("testkit: listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// UseConfigEnv writes yml into a configuration file and hands it to XONE_CONFIG without loading
// it: it is loaded on the first read, as in a caller's program (and as whatever is wired up at the
// top of main, before xone.Run, goes through as well). It is cleared when the test ends.
// To load it on the spot, use xonetest.UseConfigYAML
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
