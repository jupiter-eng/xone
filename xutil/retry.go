package xutil

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// maxBackoff is the longest wait between two attempts.
//
// It is a constant rather than a sixth parameter: almost no caller needs to change it, and one more parameter
// is a cost every caller pays in understanding. 30s is longer than any reasonable wait for a connection and
// shorter than any reasonable startup budget, so it only comes into play when doubling has run away.
const maxBackoff = 30 * time.Second

// Retry calls fn repeatedly until it succeeds, with a separate time limit on each attempt.
//
// A timeout of 0 or less means no limit on one attempt, as it does for net/http's own timeouts; the whole round is
// then bounded by parent alone, and a fn that never returns is never given up on. Every caller in this repository
// configures a positive one and fails startup without it.
//
// The wait between two attempts starts at interval and doubles each time, capped at maxBackoff, with jitter:
// the actual wait is a random value in [0, the current backoff].
//
// Doubling keeps a recovering downstream from being hit at the same rate forever; jitter keeps a group of
// replicas restarting at once from hitting it all in the same instant -- without jitter, backoff only moves
// the thundering herd from one moment to another.
//
// A whole round has one budget (attempts x timeout plus the sum of the backoff upper bounds), and once it is
// reached nothing is retried. The budget uses the backoff upper bounds rather than the jittered values:
// a budget is a ceiling, so it has to hold in the worst case. With no timeout there is no round budget either,
// for the reason given where it is built.
//
// Cancelling parent aborts the whole round immediately, with no further attempts and no further waiting.
// The connection retries at startup rely on this: a stop signal must not make the process wait out a whole
// round. Passing nil is the same as context.Background().
//
// When fn returns Permanent(err) nothing is retried and err itself is returned (with the Permanent layer removed):
// a wrong password or a missing database gives the same result however often it is retried, only later.
//
// It returns the last error; when nothing ever succeeded and the budget ran out first, that is the error from the
// attempt before it ran out. When parent is cancelled between two attempts, the error returned satisfies both
// errors.Is(err, parent.Err()) and errors.Is(err, the last error): the cancellation is why this round stopped, and the earlier failures are the background.
func Retry(parent context.Context, attempts int, timeout, interval time.Duration, fn func(context.Context) error) error {
	if attempts < 1 {
		attempts = 1
	}
	if parent == nil {
		parent = context.Background()
	}
	// Already cancelled on entry means not even one attempt. Without this line, a stop signal arriving halfway
	// through startup would still make every unreachable instance send one more doomed network request.
	if err := parent.Err(); err != nil {
		return err
	}

	// The round budget exists only when an attempt has a limit of its own. Built from the backoffs
	// alone it would be a couple of seconds, and every attempt would be handed a ctx that had already
	// expired before fn saw it -- measured, three attempts and three dead ctxs, with each one failing
	// the instant it started for any fn that honours its ctx. "No limit" has to mean no limit on
	// either, and what bounds the round is then parent alone.
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, retryBudget(attempts, timeout, interval))
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	defer cancel()

	var last error
	backoff := min(interval, maxBackoff) // the first one is capped too: with interval configured above the cap, the documented "at most maxBackoff" still holds
	for i := range attempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				// Two reasons lead here and they must be reported apart: the caller calling a halt has to be reported
				// as cancellation -- reporting only the previous "cannot reach" would make the startup path take an
				// orderly exit for a failure; a round budget that merely ran out is no cancellation, and the last error is what is reported
				if err := parent.Err(); err != nil {
					return fmt.Errorf("%w, last attempt failed: %w", err, last)
				}
				return last
			case <-time.After(jitter(backoff)):
			}
			backoff = nextBackoff(backoff)
		}
		attemptCtx, attemptCancel := attemptContext(ctx, timeout)
		last = fn(attemptCtx)
		attemptCancel()
		if last == nil {
			return nil
		}
		if p := (*permanentError)(nil); errors.As(last, &p) {
			return p.err
		}
	}
	return last
}

// attemptContext is one attempt's ctx: its own time limit, or the round's alone when timeout says there is none.
func attemptContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

// Permanent marks err as "retrying will not help": Retry returns err immediately and attempts nothing further.
// The mark is only for Retry to see, and Retry returns the err with the mark removed; a nil err gives nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err}
}

// permanentError is Permanent's mark. Error and Unwrap both pass through, so the mark changes neither the
// error text nor errors.Is / errors.As.
type permanentError struct{ err error }

func (p *permanentError) Error() string { return p.err.Error() }
func (p *permanentError) Unwrap() error { return p.err }

// retryBudget is the ceiling for a whole round: the timeout of each attempt plus the backoff upper bounds.
func retryBudget(attempts int, timeout, interval time.Duration) time.Duration {
	budget := timeout * time.Duration(attempts)
	backoff := min(interval, maxBackoff) // same as Retry: the first one is capped too
	for i := 1; i < attempts; i++ {
		budget += backoff
		backoff = nextBackoff(backoff)
	}
	return budget
}

// nextBackoff doubles, capped at maxBackoff.
func nextBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d >= maxBackoff/2 {
		return maxBackoff
	}
	return d * 2
}

// jitter takes a random value in [0, d].
//
// It is "0 to d" rather than "a little either side of d": only the former really spreads out a group of
// replicas restarting at once, while the latter just makes them twitch around the same moment.
//
// It is a variable rather than a function only so tests can replace it with the identity map and measure
// "the backoff really does double" exactly -- with randomness in the mix, a wait can only be asserted statistically.
var jitter = func(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}
