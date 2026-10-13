package xmetric

import (
	"strings"
	"testing"
	"time"
)

func TestObserveDuration_AppendsSecondsSuffix(t *testing.T) {
	// Prometheus 约定指标名自带单位，面板和告警靠它推断刻度
	m := newMetrics(t, nil)
	ObserveDuration("handle_order", 300*time.Millisecond)
	ObserveDuration("already_seconds", time.Second)

	out := dump(t, m)
	if !strings.Contains(out, "handle_order_seconds_count 1") {
		t.Errorf("应自动补上 _seconds\n实际=\n%s", out)
	}
	if strings.Contains(out, "already_seconds_seconds") {
		t.Errorf("已有后缀就不该重复添加\n实际=\n%s", out)
	}
}

func TestObserveDuration_UnitIsSeconds(t *testing.T) {
	// 按毫秒传数值的话，所有样本都会落进 +Inf 桶，分位数直接失效
	m := newMetrics(t, nil)
	ObserveDuration("op", 300*time.Millisecond)

	// 0.3 秒应落在 DefBuckets 的 0.5 档
	if out := dump(t, m); !strings.Contains(out, `op_seconds_bucket{le="0.5"} 1`) {
		t.Errorf("300ms 应换算成 0.3 秒\n实际=\n%s", out)
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
		t.Errorf("应记一次观测\n实际=\n%s", out)
	}
}

func TestTimer_RepeatedCallsCountOnce(t *testing.T) {
	// defer 之外再显式调一次（提前返回时想先记一笔）会多打一次观测，
	// count 和 rate 都随之偏高
	m := newMetrics(t, nil)
	done := Timer("task")
	done()
	done()
	done()

	if out := dump(t, m); !strings.Contains(out, "task_seconds_count 1") {
		t.Errorf("重复调用只该首次生效\n实际=\n%s", out)
	}
}

func TestTrackInFlight(t *testing.T) {
	m := newMetrics(t, nil)
	done := TrackInFlight("active", T("api", "/order"))
	if out := dump(t, m); !strings.Contains(out, `active{api="/order"} 1`) {
		t.Errorf("开始时应 +1\n实际=\n%s", out)
	}

	done()
	if out := dump(t, m); !strings.Contains(out, `active{api="/order"} 0`) {
		t.Errorf("结束时应减回去\n实际=\n%s", out)
	}
}

func TestTrackInFlight_RepeatedCallsDoNotGoNegative(t *testing.T) {
	// 减穿之后计数会永久偏低，且不会有任何报错
	m := newMetrics(t, nil)
	done := TrackInFlight("active")
	done()
	done()
	done()

	if out := dump(t, m); !strings.Contains(out, "active 0") {
		t.Errorf("重复调用只该首次生效，不该减成负数\n实际=\n%s", out)
	}
}

func TestTrackInFlight_ConcurrentPairs(t *testing.T) {
	// 真实用法是每个请求一对，并发下必须准确归零
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
		t.Errorf("每个 goroutine 都成对增减，最终应归零\n实际=\n%s", out)
	}
}

// close2 往 channel 里发一个信号（不能用 close，会被多次关闭）
func close2(ch chan struct{}) { ch <- struct{}{} }
