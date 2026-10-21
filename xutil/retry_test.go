package xutil

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestRetry_NoRetryOnFirstSuccess(t *testing.T) {
	n := 0
	err := Retry(context.Background(), 3, time.Second, time.Millisecond, func(context.Context) error {
		n++
		return nil
	})
	if err != nil || n != 1 {
		t.Errorf("succeeding on the first try must return, n=%d err=%v", n, err)
	}
}

func TestRetry_RetriesAfterFailure(t *testing.T) {
	n := 0
	err := Retry(context.Background(), 3, time.Second, time.Millisecond, func(context.Context) error {
		n++
		if n < 3 {
			return errors.New("not yet")
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Errorf("it must retry until it succeeds, n=%d err=%v", n, err)
	}
}

func TestRetry_ReturnsLastErrorWhenExhausted(t *testing.T) {
	last := errors.New("not on the third try either")
	n := 0
	err := Retry(context.Background(), 3, time.Second, time.Millisecond, func(context.Context) error {
		n++
		if n == 3 {
			return last
		}
		return errors.New("no")
	})
	if !errors.Is(err, last) {
		t.Errorf("the last error must be returned, got=%v", err)
	}
	if n != 3 {
		t.Errorf("there must be 3 attempts, got=%d", n)
	}
}

func TestRetry_PerAttemptTimeout(t *testing.T) {
	// One attempt getting stuck must not eat the whole round budget.
	// With backoff replaced by 0 and a large interval, the part of the round budget reserved for backoff
	// (1s+2s) is really not spent and becomes scheduling slack for each attempt. Otherwise the budget is only
	// 3ms more than 3xtimeout, and on a busy machine the first two waking a few ms late would squeeze the third out -- a scheduling problem, not a problem with this promise.
	withJitter(t, func(time.Duration) time.Duration { return 0 })
	const timeout = 20 * time.Millisecond
	var deadlines int
	err := Retry(context.Background(), 3, timeout, time.Second, func(ctx context.Context) error {
		if d, ok := ctx.Deadline(); !ok || time.Until(d) > timeout {
			t.Errorf("each attempt must have a deadline of its own timeout, not the round budget: %v left", time.Until(d))
		}
		<-ctx.Done()
		deadlines++
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("timing out every time must return an error")
	}
	if deadlines != 3 {
		t.Errorf("each attempt must have its own deadline, got=%d of them", deadlines)
	}
}

func TestRetry_TotalBudgetCapsWholeRun(t *testing.T) {
	// The round budget exists so a stop signal arriving at startup takes effect in time:
	// an uninterruptible retry would make the process wait out a whole round before exiting.
	// With backoff replaced by an hour, this round would take four hours without a round budget and about 90ms
	// with one (5x10ms + 4x10ms). The 10s bound is far from both ends and will not misfire on a slow machine.
	withJitter(t, func(time.Duration) time.Duration { return time.Hour })
	start := time.Now()
	Retry(context.Background(), 5, 10*time.Millisecond, 10*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the whole round must finish within the round budget, took %v", elapsed)
	}
}

func TestRetry_RunsOnceWhenAttemptsBelowOne(t *testing.T) {
	n := 0
	Retry(context.Background(), 0, time.Second, time.Millisecond, func(context.Context) error { n++; return nil })
	if n != 1 {
		t.Errorf("it must run at least once, got=%d", n)
	}
}

func TestRetry_AbortsImmediatelyOnParentCancel(t *testing.T) {
	// The connection retries at startup rely on this: a stop signal must not force the process to wait out a
	// whole round. Three attempts of 5s each is ten seconds at minimum without interruption, and the signal has already arrived.
	// With backoff replaced by an hour, still waiting on backoff after cancellation would hang here for an hour; the 10s bound is far from both ends.
	withJitter(t, func(time.Duration) time.Duration { return time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	start := time.Now()

	err := Retry(ctx, 3, 5*time.Second, 5*time.Second, func(context.Context) error {
		calls++
		cancel() // the stop signal arrives during the first attempt
		return errors.New("cannot reach")
	})

	if err == nil {
		t.Fatal("the last error must be returned")
	}
	if calls != 1 {
		t.Errorf("nothing may be attempted after cancellation, got=%d attempts", calls)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("it must not still be waiting out the retry interval after cancellation, took=%v", elapsed)
	}
}

func TestRetry_ReportsCancelBetweenAttempts(t *testing.T) {
	// When the cancellation happens during backoff, reporting only the previous business error would show the
	// caller "cannot reach" while the real reason is "a stop signal arrived, no more attempts" -- which is what
	// the startup path uses to tell a failure from an orderly exit. The previous error must not be lost either: it is why it kept failing.
	withJitter(t, func(time.Duration) time.Duration { return time.Hour }) // guaranteed to still be waiting on backoff

	ctx, cancel := context.WithCancel(context.Background())
	last := errors.New("cannot reach")
	err := Retry(ctx, 3, time.Second, time.Hour, func(context.Context) error {
		time.AfterFunc(10*time.Millisecond, cancel) // cancel while waiting on backoff
		return last
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("it must say plainly that it was cancelled, got=%v", err)
	}
	if !errors.Is(err, last) {
		t.Errorf("the previous error must be kept too, got=%v", err)
	}
}

func TestRetry_ReturnsLastErrorWhenBudgetExhausted(t *testing.T) {
	// A budget running out is not a cancellation: the caller called no halt, it simply never connected. As
	// documented, the last business error is returned, with no context error mixed in to suggest a cancellation.
	// Backoff is replaced by a wait far beyond the budget, so the budget necessarily runs out between two attempts.
	withJitter(t, func(time.Duration) time.Duration { return time.Hour })

	last := errors.New("cannot reach")
	err := Retry(context.Background(), 3, 10*time.Millisecond, time.Millisecond, func(context.Context) error {
		return last
	})
	if !errors.Is(err, last) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Errorf("when the budget runs out only the last error may be reported, got=%v", err)
	}
}

func TestRetry_NilParentCtxActsAsBackground(t *testing.T) {
	n := 0
	//nolint:staticcheck // the nil compatibility behavior is verified on purpose
	if err := Retry(nil, 2, time.Second, time.Millisecond, func(context.Context) error {
		n++
		return nil
	}); err != nil {
		t.Fatalf("it must not be an error: %v", err)
	}
	if n != 1 {
		t.Errorf("success must not be retried, got=%d", n)
	}
}

func TestRetry_NoAttemptWhenParentAlreadyCanceled(t *testing.T) {
	// A stop signal arriving halfway through startup must not make every unreachable instance send one more doomed network request.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	err := Retry(ctx, 3, time.Second, time.Second, func(context.Context) error {
		calls++
		return errors.New("cannot reach")
	})

	if calls != 0 {
		t.Errorf("not even one attempt may be made, got=%d", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("it must say plainly that it was cancelled rather than report the last network error, got=%v", err)
	}
}

func TestNextBackoff_DoublesAndCaps(t *testing.T) {
	for _, c := range []struct{ in, want time.Duration }{
		{0, 0},
		{time.Second, 2 * time.Second},
		{10 * time.Second, 20 * time.Second},
		{maxBackoff / 2, maxBackoff},
		{maxBackoff, maxBackoff}, // no further growth once capped
		{2 * maxBackoff, maxBackoff},
	} {
		if got := nextBackoff(c.in); got != c.want {
			t.Errorf("nextBackoff(%v)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestJitter_BetweenZeroAndUpperBound(t *testing.T) {
	// It is "0 to d" rather than "a twitch around d": only the former really spreads out a group of replicas restarting at once.
	const d = 100 * time.Millisecond
	var sawSmall, sawLarge bool
	for range 2000 {
		got := jitter(d)
		if got < 0 || got > d {
			t.Fatalf("jitter out of range: %v is not in [0,%v]", got, d)
		}
		if got < d/4 {
			sawSmall = true
		}
		if got > d*3/4 {
			sawLarge = true
		}
	}
	if !sawSmall || !sawLarge {
		t.Error("two thousand samples must cover both ends of the range; this does not look like real jitter")
	}
	if jitter(0) != 0 || jitter(-time.Second) != 0 {
		t.Error("a non-positive backoff must not wait")
	}
}

func TestRetryBudget_SumsBackoffUpperBounds(t *testing.T) {
	// The budget is a ceiling, so it is computed from the backoff upper bounds, not from the jittered values.
	const timeout, interval = 10 * time.Millisecond, 10 * time.Millisecond

	// 3 attempts: the waiting upper bound is 10 + 20 = 30ms.
	if got, want := retryBudget(3, timeout, interval), 3*timeout+30*time.Millisecond; got != want {
		t.Errorf("budget for 3 attempts got=%v want=%v", got, want)
	}
	// A fixed interval would be only 20ms, and with backoff it is wider -- the budget has to grow with it,
	// otherwise the last attempt is cut off by its own budget.
	if retryBudget(3, timeout, interval) <= 3*timeout+2*interval {
		t.Error("with backoff the budget must be wider than with a fixed interval")
	}
	// A single attempt waits for nothing.
	if got, want := retryBudget(1, timeout, interval), timeout; got != want {
		t.Errorf("budget for a single attempt got=%v want=%v", got, want)
	}
}

func TestRetry_EveryAttemptRunsAfterBackoff(t *testing.T) {
	// Regression case. With the budget still computed from a fixed interval, backoff stretching the waiting
	// would cut the last attempt off by its own budget, which shows up as "5 configured, only 4 attempted".
	const attempts = 5
	calls := 0
	err := Retry(context.Background(), attempts, 5*time.Millisecond, time.Millisecond,
		func(context.Context) error { calls++; return errors.New("nope") })
	if err == nil {
		t.Fatal("it must fail")
	}
	if calls != attempts {
		t.Errorf("all %d attempts must run, got %d", attempts, calls)
	}
}

// recordWaits replaces the jitter with "record the backoff that was asked for and do not actually wait", and returns what was recorded.
//
// What is asserted is the backoff Retry asks for, not the wall-clock wait: the latter swings on a loaded
// machine, and a ratio assertion such as doubling would misfire sooner or later.
func recordWaits(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	withJitter(t, func(d time.Duration) time.Duration { waits = append(waits, d); return 0 })
	return &waits
}

func TestRetry_WaitDoublesBetweenAttempts(t *testing.T) {
	// This is the whole point of backoff: a fixed interval would keep hitting a recovering downstream at the
	// same rate forever. The jitter itself is watched separately by TestJitter_BetweenZeroAndUpperBound.
	waits := recordWaits(t)

	const interval = 20 * time.Millisecond
	_ = Retry(context.Background(), 4, time.Millisecond, interval,
		func(context.Context) error { return errors.New("nope") })

	want := []time.Duration{interval, 2 * interval, 4 * interval}
	if !slices.Equal(*waits, want) {
		t.Errorf("the backoffs between 4 attempts must be %v, got %v", want, *waits)
	}
}

func TestRetry_FirstWaitAlsoCapped(t *testing.T) {
	// The documentation says the wait between two attempts is at most maxBackoff. With interval configured
	// above it, the first wait used to take interval as it was, so 1h really meant waiting an hour.
	waits := recordWaits(t)

	_ = Retry(context.Background(), 3, time.Millisecond, time.Hour,
		func(context.Context) error { return errors.New("nope") })

	if len(*waits) != 2 {
		t.Fatalf("3 attempts must wait twice between them, got %v", *waits)
	}
	for _, w := range *waits {
		if w > maxBackoff {
			t.Errorf("no wait may exceed the upper bound %v, got %v", maxBackoff, *waits)
		}
	}
}

func TestRetryBudget_FirstBackoffAlsoCapped(t *testing.T) {
	if got, want := retryBudget(3, time.Millisecond, time.Hour), 3*time.Millisecond+2*maxBackoff; got != want {
		t.Errorf("the budget must be computed from the capped backoff, got=%v want=%v", got, want)
	}
}

func TestRetry_NoRetryOnPermanentError(t *testing.T) {
	// A wrong password or a missing database: retrying changes nothing and only drags startup out.
	root := errors.New("auth failed")
	calls := 0
	err := Retry(context.Background(), 5, time.Second, time.Hour, func(context.Context) error {
		calls++
		return Permanent(root)
	})

	if calls != 1 {
		t.Errorf("nothing may be attempted after a permanent error, got=%d attempts", calls)
	}
	if err != root {
		t.Errorf("the original error must be returned with the mark removed, got=%#v", err)
	}
}

func TestRetry_RecognizesWrappedPermanentError(t *testing.T) {
	root := errors.New("auth failed")
	calls := 0
	err := Retry(context.Background(), 5, time.Second, time.Hour, func(context.Context) error {
		calls++
		return fmt.Errorf("connect: %w", Permanent(root))
	})
	if calls != 1 || err != root {
		t.Errorf("there must be exactly 1 attempt returning the original error, got attempts=%d err=%#v", calls, err)
	}
}

func TestPermanent_PreservesMessageAndChain(t *testing.T) {
	root := errors.New("auth failed")
	p := Permanent(root)
	if p.Error() != root.Error() {
		t.Errorf("the mark must not change the error text, got=%q", p.Error())
	}
	if !errors.Is(p, root) {
		t.Error("the mark must not block errors.Is")
	}
	if Permanent(nil) != nil {
		t.Error("Permanent(nil) must be nil, otherwise a successful fn is taken for a failure")
	}
}

// withJitter replaces the backoff jitter with f for this test and restores it at the end.
// Duration assertions rely on it to take the waiting out of randomness and scheduling.
func withJitter(t *testing.T, f func(time.Duration) time.Duration) {
	t.Helper()
	old := jitter
	jitter = f
	t.Cleanup(func() { jitter = old })
}
