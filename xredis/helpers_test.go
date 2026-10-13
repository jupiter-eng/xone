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

// capture 把 slog 默认 logger 换成写进 buffer 的，返回取解析结果的函数
func capture(t *testing.T) func() []map[string]any {
	t.Helper()
	return captureAt(t, slog.LevelDebug)
}

// captureAt 同 capture，handler 只收 level 及以上
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
				t.Fatalf("日志不是 JSON：%v，内容=%q", err, line)
			}
			out = append(out, m)
		}
		return out
	}
}

func TestSettle_DetectsLeakedGoroutines(t *testing.T) {
	// 先验证这把尺子是准的——一条永远不会失败的测试比没有测试更糟，
	// 它让人以为查过了
	const leak = 5
	before := testkit.Stabilize()

	stop := make(chan struct{})
	for i := 0; i < leak; i++ {
		go func() { <-stop }()
	}

	// 用 ClimbTo 而不是 SettleTo 来判断「涨了没有」。
	// SettleTo 的判据是「回落到 target 以内」，拿它判断增长有两个毛病：
	// 打乱顺序跑时，上一个用例的协程正在退场，它会当场判定「已回落」而误报；
	// 没误报的时候又必然烧满整个 10 秒预算才肯返回。
	if during := testkit.ClimbTo(before+leak, 3*time.Second); during < before+leak {
		t.Fatalf("漏了 %d 个协程却没看出增长（%d -> %d），这把尺子是坏的", leak, before, during)
	}

	close(stop)
	if after := testkit.SettleTo(before); after > before+1 {
		t.Errorf("协程退出后应当回落，got %d -> %d", before, after)
	}
}

// TestMain 调短重试间隔，并把 slog 默认 logger 接到丢弃里。
// 连不上的用例本来就会刷一屏「failed to dial」，那是预期内的噪声。
//
// 丢的是 slog 这一头，不是调 redis.SetLogger 换掉 go-redis 的 logger：
// 那样会盖掉 init 里接好的那个，「go-redis 的日志进 slog」就再也测不到了
func TestMain(m *testing.M) {
	pingInterval = 10 * time.Millisecond
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}
