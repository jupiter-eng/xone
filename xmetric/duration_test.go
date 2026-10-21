package xmetric

import (
	"strings"
	"testing"
	"time"
)

func TestObserveDuration_AppendsSecondsSuffix(t *testing.T) {
	// Prometheus convention has the unit in the metric name, and dashboards and alerts infer the scale from it.
	m := newMetrics(t, nil)
	ObserveDuration("handle_order", 300*time.Millisecond)
	ObserveDuration("already_seconds", time.Second)

	out := dump(t, m)
	if !strings.Contains(out, "handle_order_seconds_count 1") {
		t.Errorf("the _seconds suffix must be added automatically\ngot=\n%s", out)
	}
	if strings.Contains(out, "already_seconds_seconds") {
		t.Errorf("a suffix already there must not be added twice\ngot=\n%s", out)
	}
}

func TestObserveDuration_UnitIsSeconds(t *testing.T) {
	// Passing a value in milliseconds would land every sample in the +Inf bucket and make the quantiles useless.
	m := newMetrics(t, nil)
	ObserveDuration("op", 300*time.Millisecond)

	// 0.3 seconds must land in DefBuckets' 0.5 bucket.
	if out := dump(t, m); !strings.Contains(out, `op_seconds_bucket{le="0.5"} 1`) {
		t.Errorf("300ms must convert to 0.3 seconds\ngot=\n%s", out)
	}
}

func TestDurationName(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"op", "op_seconds"},
		{"op_seconds", "op_seconds"},
		{"", "_seconds"},
	} {
		if got := durationName(c.in); got != c.want {
			t.Errorf("durationName(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestTimer(t *testing.T) {
	m := newMetrics(t, nil)
	done := Timer("task", T("kind", "sync"))
	time.Sleep(time.Millisecond)
	done()

	if out := dump(t, m); !strings.Contains(out, `task_seconds_count{kind="sync"} 1`) {
		t.Errorf("one observation must be recorded\ngot=\n%s", out)
	}
}

func TestTimer_RepeatedCallsCountOnce(t *testing.T) {
	// Calling it explicitly outside the defer as well (to record it on an early return) would add one observation
	// and skew count and rate upwards.
	m := newMetrics(t, nil)
	done := Timer("task")
	done()
	done()
	done()

	if out := dump(t, m); !strings.Contains(out, "task_seconds_count 1") {
		t.Errorf("calling it again must have no effect\ngot=\n%s", out)
	}
}

func TestTrackInFlight(t *testing.T) {
	m := newMetrics(t, nil)
	done := TrackInFlight("active", T("api", "/order"))
	if out := dump(t, m); !strings.Contains(out, `active{api="/order"} 1`) {
		t.Errorf("it must increment by 1 at the start\ngot=\n%s", out)
	}

	done()
	if out := dump(t, m); !strings.Contains(out, `active{api="/order"} 0`) {
		t.Errorf("it must be decremented again at the end\ngot=\n%s", out)
	}
}

func TestTrackInFlight_RepeatedCallsDoNotGoNegative(t *testing.T) {
	// Decrementing through zero would leave the count permanently low, with no error reported.
	m := newMetrics(t, nil)
	done := TrackInFlight("active")
	done()
	done()
	done()

	if out := dump(t, m); !strings.Contains(out, "active 0") {
		t.Errorf("calling it again must have no effect and must not go negative\ngot=\n%s", out)
	}
}

func TestTrackInFlight_ConcurrentPairs(t *testing.T) {
	// The real use is one pair per request, and it has to reach exactly zero under concurrency.
	m := newMetrics(t, nil)
	const n = 50
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer close2(done)
			defer TrackInFlight("inflight")()
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}

	if out := dump(t, m); !strings.Contains(out, "inflight 0") {
		t.Errorf("every goroutine increments and decrements in pairs, so it must end at zero\ngot=\n%s", out)
	}
}

// close2 sends one signal into the channel (close cannot be used, as it would be closed several times).
func close2(ch chan struct{}) { ch <- struct{}{} }
