package xcron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jupiter-eng/xone/xerror"
)

// job 一个登记了的定时任务
type job struct {
	name  string
	sched schedule
	fn    func(context.Context) error
	o     options

	// running 在途的执行数。不允许重叠时，到点看它是不是 0。
	// 先看再加不需要 CAS：一个任务的每一次执行都由同一个协程发起（它的调度循环，
	// 或者在调度循环起来之前的 start / add），没有两处同时在看
	running atomic.Int32
}

// state 调度器走到哪一步了
type state int

const (
	idle    state = iota // 还没起来：Add 只登记
	running              // 起来了：Add 当场开始调度
	stopped              // 开始停了（或停完了）：Add 报错
)

// scheduler 全部定时任务。一个进程一个（std），由 xcron 的那对钩子起停。
//
// 每个任务一个协程、一个 time.Timer：算出下一个时间点，睡到那一刻，发起一次执行，再算下一个。
// cronexpr 只管「下一个时间点是什么时候」，起停、取消、等在途执行都在这里
type scheduler struct {
	mu    sync.Mutex
	state state
	jobs  []*job
	cur   *lifetime // state 是 running 时有效
}

// lifetime 调度器的一次「起来到停下」：同一个进程里 xone.Run 可以跑好几次，每次一份
type lifetime struct {
	// ctx 每一次执行的 ctx 的父亲，也是调度循环的退出信号：stop 取消它
	ctx    context.Context
	cancel context.CancelFunc

	// wg 数的是调度循环和在途的执行。执行由调度循环发起，那时它自己还占着一个数，
	// 所以 Add 永远发生在计数大于 0 时，不会和 stop 里的 Wait 抢
	wg sync.WaitGroup
}

var std = &scheduler{}

// add 登记一个任务。调度器已经起来的话当场开始调度
func (s *scheduler) add(j *job) error {
	s.mu.Lock()
	if s.state == stopped {
		s.mu.Unlock()
		return xerror.Newf("xcron", "register", "cannot add job %q: the scheduler is shutting down", j.name)
	}
	if slices.ContainsFunc(s.jobs, func(x *job) bool { return x.name == j.name }) {
		s.mu.Unlock()
		return xerror.Newf("xcron", "config", "duplicate job name %q: give one of them xcron.WithName", j.name)
	}
	s.jobs = append(s.jobs, j)
	if s.state != running {
		s.mu.Unlock()
		return nil
	}
	lt := s.cur
	if j.o.first != firstWait {
		lt.schedule(j, j.o.first == firstAsync)
		s.mu.Unlock()
		return nil
	}

	// RunOnStartAndWait：这里就是「启动」，等第一次跑完。失败就不调度，名字也还回去，改好了可以再 Add
	lt.begin(j)
	s.mu.Unlock()
	if err := lt.exec(j); err != nil {
		s.mu.Lock()
		s.jobs = slices.DeleteFunc(s.jobs, func(x *job) bool { return x == j })
		s.mu.Unlock()
		return xerror.Newf("xcron", "execute", "first run of job %q failed: %w", j.name, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == running && s.cur == lt {
		lt.schedule(j, false) // 第一次已经跑过了，从下一个时间点开始
	}
	return nil
}

// start 起调度器：先把 RunOnStartAndWait 的任务并行跑一遍，全部成功了再给每个任务起调度循环。
// 第一次执行用的是启动钩子的 ctx：收到退出信号时它们跟着被取消
func (s *scheduler) start(ctx context.Context) error {
	s.mu.Lock()
	lt := &lifetime{}
	lt.ctx, lt.cancel = context.WithCancel(context.WithoutCancel(ctx))
	s.state, s.cur = running, lt
	jobs := slices.Clone(s.jobs)
	s.mu.Unlock()

	if err := firstRuns(ctx, jobs); err != nil {
		// 停止钩子不会被调到（它的启动钩子失败了），自己收拾：此时一个调度循环都还没起
		s.mu.Lock()
		s.state = stopped
		s.mu.Unlock()
		lt.cancel()
		return err
	}

	s.mu.Lock()
	for _, j := range jobs {
		lt.schedule(j, j.o.first == firstAsync) // RunOnStartAndWait 的第一次在上面跑过了
	}
	s.mu.Unlock()
	slog.InfoContext(ctx, "xcron ready", "jobs", names(jobs))
	return nil
}

// firstRuns 并行跑 RunOnStartAndWait 的那第一次，等全部跑完。失败的每一个都点名
func firstRuns(ctx context.Context, jobs []*job) error {
	var wg sync.WaitGroup
	errs := make([]error, len(jobs))
	for i, j := range jobs {
		if j.o.first != firstWait {
			continue
		}
		wg.Go(func() {
			if err := execute(ctx, j.name, j.o.timeout, j.fn); err != nil {
				errs[i] = fmt.Errorf("job %q: %w", j.name, err)
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return xerror.Newf("xcron", "start", "first run failed: %w", err)
	}
	return nil
}

// stop 停调度器：不再发起新的执行，取消在途执行的 ctx，在 ctx 的截止时间之前等它们返回。
// 到点还有没返回的，报错里点名是哪几个任务
func (s *scheduler) stop(ctx context.Context) error {
	s.mu.Lock()
	if s.state != running {
		s.mu.Unlock()
		return nil
	}
	s.state = stopped
	lt := s.cur
	s.mu.Unlock()

	lt.cancel()
	done := make(chan struct{})
	go func() { lt.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}

	// 框架的 runWithin 也在看同一个截止时间，两边几乎同时就绪时它可能先返回、把这里的错误丢掉，
	// 所以点名的这一条日志无论如何都要写出去
	busy := s.busy()
	slog.WarnContext(ctx, "xcron jobs still running when the stop budget ran out", "jobs", busy)
	return xerror.Newf("xcron", "stop", "jobs %v still running when the stop budget ran out: %w", busy, ctx.Err())
}

// busy 还在跑的任务名
func (s *scheduler) busy() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, j := range s.jobs {
		if j.running.Load() > 0 {
			out = append(out, j.name)
		}
	}
	return out
}

// schedule 给 j 起调度循环，now 为真时先发起一次（RunOnStart）。调用方持有 scheduler.mu
// 且 state 是 running：stop 先在锁里改 state 再 Wait，这里的 wg.Add 因此总在那次 Wait 之前
func (lt *lifetime) schedule(j *job, now bool) {
	lt.wg.Add(1)
	go lt.loop(j, now)
}

// loop 一个任务的调度循环：睡到下一个时间点，发起一次执行，直到 stop
func (lt *lifetime) loop(j *job, now bool) {
	defer lt.wg.Done()
	if now {
		lt.tick(j)
	}
	var prev time.Time
	for {
		at := next(j.sched, j.o.loc, prev)
		t := time.NewTimer(time.Until(at))
		select {
		case <-lt.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		prev = at
		lt.tick(j)
	}
}

// tick 到点了：发起一次执行，不等它
func (lt *lifetime) tick(j *job) {
	// 计时器和 stop 同时就绪时 select 随便挑一个，挑中计时器也不该再发起
	if lt.ctx.Err() != nil {
		return
	}
	if !j.o.overlap && j.running.Load() > 0 {
		slog.WarnContext(lt.ctx, "cron job skipped, previous run still running", "job", j.name)
		return
	}
	lt.begin(j)
	go lt.exec(j)
}

// begin 记下一次在途的执行，必须在 exec 之前、在发起它的那个协程里调
func (lt *lifetime) begin(j *job) {
	j.running.Add(1)
	lt.wg.Add(1)
}

// exec 跑一次，ctx 跟着这一次「起来到停下」：stop 时取消
func (lt *lifetime) exec(j *job) error {
	defer lt.wg.Done()
	defer j.running.Add(-1)
	return execute(lt.ctx, j.name, j.o.timeout, j.fn)
}

func names(jobs []*job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.name
	}
	return out
}
