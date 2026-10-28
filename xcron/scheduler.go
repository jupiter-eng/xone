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

// job is one registered scheduled job.
type job struct {
	name  string
	sched schedule
	fn    func(context.Context) error
	o     options

	// running is the number of executions in flight. Without overlap allowed, a time point checks whether it is 0.
	// Checking before incrementing needs no CAS: every execution of a job is started by the same goroutine (its
	// scheduling loop, or start / add before that loop is up), so no two places look at it at once.
	running atomic.Int32
}

// state is how far the scheduler has got.
type state int

const (
	idle    state = iota // not up yet: Add only registers
	running              // up: Add starts scheduling on the spot
	stopped              // stopping has begun (or has finished): Add is an error
)

// scheduler holds every scheduled job. There is one per process (std), started and stopped by xcron's pair of hooks.
//
// Each job gets a goroutine and a time.Timer: work out the next time point, sleep until it, start one execution, work
// out the next. cronexpr only answers "when is the next time point"; starting, stopping, cancelling and waiting for executions in flight all happen here.
type scheduler struct {
	mu    sync.Mutex
	state state
	jobs  []*job
	cur   *lifetime // valid while state is running
}

// lifetime is one "up until stopped" of the scheduler: xone.Run can run several times in one process, one each.
type lifetime struct {
	// ctx is the parent of every execution's ctx and also the scheduling loops' stop signal: stop cancels it.
	ctx    context.Context
	cancel context.CancelFunc

	// wg counts the scheduling loops and the executions in flight. An execution is started by a scheduling loop,
	// which still holds a count of its own at that moment, so an Add always happens while the count is greater than 0 and never races the Wait in stop.
	wg sync.WaitGroup
}

var std = &scheduler{}

// add registers a job. When the scheduler is already up it starts scheduling on the spot.
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

	// RunOnStartAndWait: this is the "startup", and it waits for the first run. On failure nothing is scheduled and the name is given back, so a corrected job can be added again.
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
		lt.schedule(j, false) // the first run has already happened, so start from the next time point
	}
	return nil
}

// start brings the scheduler up: it first runs the RunOnStartAndWait jobs in parallel and only starts a scheduling
// loop per job once they have all succeeded. That first execution uses the start hook's ctx, so a stop signal cancels them.
func (s *scheduler) start(ctx context.Context) error {
	s.mu.Lock()
	lt := &lifetime{}
	lt.ctx, lt.cancel = context.WithCancel(context.WithoutCancel(ctx))
	// Add can run inside a first execution and starts its own jobs immediately. Until startup
	// finishes those jobs must follow the startup cancellation too; afterwards stop owns it.
	detachStartup := context.AfterFunc(ctx, lt.cancel)
	defer detachStartup()
	s.state, s.cur = running, lt
	jobs := slices.Clone(s.jobs)
	s.mu.Unlock()

	if err := firstRuns(ctx, jobs); err != nil {
		// The stop hook is never called (its start hook failed). A first execution may have added
		// jobs already, so reject further registration, cancel them and wait for their cleanup.
		s.mu.Lock()
		s.state = stopped
		s.mu.Unlock()
		lt.cancel()
		lt.wg.Wait()
		return err
	}

	s.mu.Lock()
	for _, j := range jobs {
		lt.schedule(j, j.o.first == firstAsync) // the first run of a RunOnStartAndWait job already happened above
	}
	s.mu.Unlock()
	slog.InfoContext(ctx, "xcron ready", "jobs", names(jobs))
	return nil
}

// firstRuns runs the first execution of the RunOnStartAndWait jobs in parallel and waits for all of them. Every failure is named.
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

// stop stops the scheduler: no new execution is started, the ctx of the executions in flight is cancelled, and it
// waits for them to return before the ctx deadline. Any that have not returned by then are named in the error.
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

	// The framework's runWithin watches the same deadline, and when both become ready at nearly the same moment it may
	// return first and drop this error, so this one log line naming them has to go out no matter what.
	busy := s.busy()
	slog.WarnContext(ctx, "xcron jobs still running when the stop budget ran out", "jobs", busy)
	return xerror.Newf("xcron", "stop", "jobs %v still running when the stop budget ran out: %w", busy, ctx.Err())
}

// busy lists the names of the jobs still running.
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

// schedule starts j's scheduling loop, running one execution first when now is true (RunOnStart). The caller holds
// scheduler.mu and state is running: stop changes state under the lock before it Waits, so the wg.Add here always precedes that Wait.
func (lt *lifetime) schedule(j *job, now bool) {
	lt.wg.Add(1)
	go lt.loop(j, now)
}

// loop is one job's scheduling loop: sleep until the next time point, start one execution, until stop.
func (lt *lifetime) loop(j *job, now bool) {
	defer lt.wg.Done()
	if now {
		lt.tick(j)
	}
	var prev time.Time
	for {
		at := next(j.sched, j.o.loc, prev)
		// The zero time means the schedule has no next time point (see the schedule interface), and time.Until
		// of it is a large negative duration: the timer would fire at once, prev would stay zero, the next round
		// would compute the zero time again, and the loop would start an execution (or write "cron job skipped")
		// as fast as the CPU allows. A spec that never fires is refused at registration, so what is left is the
		// far end of one that does: cronexpr v1.1.3 searches the years 1970..2099 and returns the zero time past
		// them, which "0 0 29 2 *" reaches in 2096, its next leap day being 2104.
		if at.IsZero() {
			slog.WarnContext(lt.ctx, "cron job has no next time point and will not run again", "job", j.name)
			return
		}
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

// tick means the time point has arrived: start one execution without waiting for it.
func (lt *lifetime) tick(j *job) {
	// When the timer and stop become ready at the same moment, select picks either one, and picking the timer must still not start anything.
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

// begin records one execution in flight; it must be called before exec, in the goroutine starting it.
func (lt *lifetime) begin(j *job) {
	j.running.Add(1)
	lt.wg.Add(1)
}

// exec runs once, with the ctx following this "up until stopped": cancelled on stop.
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
