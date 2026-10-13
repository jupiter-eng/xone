package xflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// recorder 记下收到的事件
type recorder struct {
	mu    sync.Mutex
	steps []*StepEvent
	flows []*FlowEvent
	panic bool
}

func (r *recorder) OnStep(_ context.Context, e *StepEvent) {
	if r.panic {
		panic("监控炸了")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, e)
}

func (r *recorder) OnFlow(_ context.Context, e *FlowEvent) {
	if r.panic {
		panic("监控炸了")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flows = append(r.flows, e)
}

func (r *recorder) stepEvents() []*StepEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*StepEvent(nil), r.steps...)
}

// withMonitor 装一个监控实现，测试结束还原
func withMonitor(t *testing.T, m Monitor) {
	t.Helper()
	old := monitor.Load()
	t.Cleanup(func() { monitor.Store(old) })
	SetMonitor(m)
}

func TestMonitor_ReceivesEveryStepAndFlowEvent(t *testing.T) {
	withConfig(t, nil)
	r := &recorder{}
	withMonitor(t, r)

	New("下单", ok("扣券"), failing("扣款", Strong)).Execute(context.Background(), &data{})

	steps := r.stepEvents()
	// 两次 Process（一成一败）+ 一次 Rollback
	if len(steps) != 3 {
		t.Fatalf("应收到 3 个步骤事件，got=%d", len(steps))
	}
	if steps[0].Rollback || steps[0].Err != nil || steps[0].Processor != "扣券" {
		t.Errorf("第一个事件不对，got=%+v", steps[0])
	}
	if steps[1].Err == nil || steps[1].Processor != "扣款" {
		t.Errorf("失败的步骤应带上错误，got=%+v", steps[1])
	}
	if !steps[2].Rollback || steps[2].Processor != "扣券" {
		t.Errorf("第三个事件应是回滚，got=%+v", steps[2])
	}

	if len(r.flows) != 1 {
		t.Fatalf("应收到一个流程事件，got=%d", len(r.flows))
	}
	if r.flows[0].Flow != "下单" || r.flows[0].Result.Success() {
		t.Errorf("流程事件不对，got=%+v", r.flows[0])
	}
	// 流程耗时要含回滚，否则「这个流程花了多久」是假的
	if r.flows[0].Duration <= 0 {
		t.Error("流程事件应带上耗时")
	}
}

func TestMonitor_PanicDoesNotInterruptFlow(t *testing.T) {
	// 监控实现出错只该丢一次观测
	withConfig(t, nil)
	withMonitor(t, &recorder{panic: true})

	d := &data{}
	res := New("下单", ok("扣券"), ok("扣款")).Execute(context.Background(), d)

	if !res.Success() {
		t.Errorf("监控 panic 不该让流程失败：%v", res)
	}
	if len(d.doneList()) != 2 {
		t.Errorf("流程应照常走完，got=%v", d.doneList())
	}
}

func TestMonitor_ZeroCallbacksWhenDisabled(t *testing.T) {
	withConfig(t, func(c *Config) { c.Monitor = false })
	r := &recorder{}
	withMonitor(t, r)

	New("下单", ok("扣券")).Execute(context.Background(), &data{})

	if len(r.stepEvents()) != 0 || len(r.flows) != 0 {
		t.Error("关掉监控后不该有任何回调")
	}
}

func TestSetMonitor_NilDisablesMonitor(t *testing.T) {
	withConfig(t, nil)
	withMonitor(t, nil)

	if activeMonitor() != nil {
		t.Error("传 nil 应当关掉监控")
	}
	// 不该 panic
	New("下单", ok("扣券")).Execute(context.Background(), &data{})
}

func TestMonitor_StepDuration(t *testing.T) {
	withConfig(t, nil)
	r := &recorder{}
	withMonitor(t, r)

	New("下单", &step{name: "慢", dep: Strong, delay: 20 * time.Millisecond}).
		Execute(context.Background(), &data{})

	steps := r.stepEvents()
	if len(steps) != 1 || steps[0].Duration < 15*time.Millisecond {
		t.Errorf("步骤耗时不对，got=%v", steps)
	}
}

func TestSlogMonitor_DoesNotPanic(t *testing.T) {
	// 默认实现，跑一遍覆盖各分支
	withConfig(t, nil)
	withMonitor(t, slogMonitor{})

	ctx := context.Background()
	New("成功", ok("a")).Execute(ctx, &data{})
	New("失败", failing("a", Strong)).Execute(ctx, &data{})
	New("回滚有失败", &step{name: "补偿失败", dep: Strong, rbErr: errors.New("x")}, failing("b", Strong)).
		Execute(ctx, &data{})
}
