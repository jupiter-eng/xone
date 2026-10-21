package xredis

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/testkit"
)

// capture swaps the slog default logger for one writing into a buffer and returns a function that parses what was written.
func capture(t *testing.T) func() []map[string]any {
	t.Helper()
	return captureAt(t, slog.LevelDebug)
}

// captureAt is capture with the handler accepting level and above only.
func captureAt(t *testing.T, level slog.Level) func() []map[string]any {
	t.Helper()
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(old) })

	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			m := map[string]any{}
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("the log line is not JSON: %v, content=%q", err, line)
			}
			out = append(out, m)
		}
		return out
	}
}

func TestSettle_DetectsLeakedGoroutines(t *testing.T) {
	// First prove the measuring stick is accurate -- a test that can never fail is worse than no test,
	// because it makes people believe the thing was checked.
	const leak = 5
	before := testkit.Stabilize()

	stop := make(chan struct{})
	for i := 0; i < leak; i++ {
		go func() { <-stop }()
	}

	// ClimbTo is used to decide "did it rise", not SettleTo.
	// SettleTo waits for a fall back to within target, which has two problems when used to detect growth:
	// under shuffled ordering, the previous case's goroutines are still leaving and it declares "already fallen back" and misreports;
	// and when it does not misreport, it necessarily burns the full 10-second budget before returning.
	if during := testkit.ClimbTo(before+leak, 3*time.Second); during < before+leak {
		t.Fatalf("%d leaked goroutines produced no visible growth (%d -> %d), so this measuring stick is broken", leak, before, during)
	}

	close(stop)
	if after := testkit.SettleTo(before); after > before+1 {
		t.Errorf("it must fall back once the goroutines have exited, got %d -> %d", before, after)
	}
}

// TestMain shortens the retry interval and routes the slog default logger into a discard.
// The cases that cannot connect fill a screen with "failed to dial" by design, which is expected noise.
//
// What is discarded is the slog end, not go-redis's logger through redis.SetLogger:
// that would overwrite the one wired up in init, and "go-redis's logs reach slog" could never be tested again.
func TestMain(m *testing.M) {
	pingInterval = 10 * time.Millisecond
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}
