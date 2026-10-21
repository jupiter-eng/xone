package xcron

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"time"
)

// Option is an optional setting for a scheduled job; the uses are on each With* / RunOnStart*.
type Option func(*options)

// firstRun says whether to run once as soon as the scheduler comes up.
type firstRun int

const (
	firstNone  firstRun = iota // wait for the first time point
	firstAsync                 // RunOnStart: run once in the background right away, without waiting
	firstWait                  // RunOnStartAndWait: run once right away; startup waits for it, and its failure fails startup
)

type options struct {
	name    string
	named   bool // WithName was written explicitly, so an empty string is an error rather than "not written"
	timeout time.Duration
	overlap bool
	loc     *time.Location
	first   firstRun

	// firsts counts how many RunOnStart / RunOnStartAndWait were written: writing both contradicts itself and is an error rather than last-one-wins
	firsts int
}

// WithName is the job name: the log's job field, the Span name (cron <name>) and the errors all use it, and it must be unique within one process.
//
// Without one the function name is used, with the import path prefix and a method value's -fm suffix removed (measured on Go 1.25):
//
//	an ordinary function cleanup                 -> main.cleanup
//	the method value svc.Sync (value receiver)   -> main.Service.Sync
//	the method value svc.Sync (pointer receiver) -> main.(*Service).Sync
//	a closure                                    -> main.main.func1
//
// A closure's name is numbered by the compiler in order of appearance, so moving the code changes it and neither
// logs nor alerts say which job it is -- write WithName when passing a closure.
func WithName(name string) Option {
	return func(o *options) { o.name, o.named = name, true }
}

// WithTimeout is the time limit on each execution: the ctx fn receives is cancelled when it is reached. There is no limit by default.
//
// It only cancels the ctx: an fn that does not look at the ctx runs to completion anyway, since Go has no way to
// stop a goroutine from the outside. It also covers the first run of RunOnStart / RunOnStartAndWait.
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// AllowOverlap allows the next execution to start while the previous one is still running, so the two run concurrently.
//
// It is not allowed by default: when a time point arrives while the previous run is still going, this one is skipped
// and a WARN is recorded (cron job skipped), and the next time point proceeds as usual. A job slower than its own interval therefore does not pile up.
func AllowOverlap() Option {
	return func(o *options) { o.overlap = true }
}

// WithLocation is the time zone the spec is interpreted in. It defaults to UTC.
//
//	xcron.Add("30 2 * * *", backup, xcron.WithLocation(berlin)) // 02:30 Berlin time, every day
//
// In a time zone with daylight saving, a time that does not exist on the day the clock goes forward is skipped for the
// whole day, and a time that occurs twice on the day it goes back runs twice; see the README's "Behavior (measured)". A CRON_TZ= / TZ= prefix is not accepted in the spec; the time zone is written only here.
func WithLocation(loc *time.Location) Option {
	return func(o *options) { o.loc = loc }
}

// RunOnStart runs once in the background as soon as the scheduler comes up, without waiting for it, and then follows
// the spec as usual. A failure of that run is only logged and does not affect startup.
func RunOnStart() Option {
	return func(o *options) {
		o.first = firstAsync
		o.firsts++
	}
}

// RunOnStartAndWait runs once as soon as the scheduler comes up and startup waits for it; its failure fails startup.
//
// It suits jobs whose data must be ready before the service comes up: fetching the configuration once, refreshing a
// local cache once. Several such jobs run in parallel and startup continues once all of them have finished; their ctx
// is cancelled when a stop signal arrives. For an Add after startup, Add waits for that run and returns its error.
func RunOnStartAndWait() Option {
	return func(o *options) {
		o.first = firstWait
		o.firsts++
	}
}

// buildOptions applies every Option and takes the function name when no name was written.
func buildOptions(fn any, opts []Option) options {
	o := options{loc: time.UTC}
	for _, opt := range opts {
		opt(&o)
	}
	if !o.named {
		o.name = funcName(fn)
	}
	return o
}

// validate reports whether the settings Add received hold. It returns a plain error, which Add wraps in an xerror at the boundary.
func (o options) validate() error {
	var errs []error
	if strings.TrimSpace(o.name) == "" {
		errs = append(errs, errors.New("name is empty"))
	}
	if o.timeout < 0 {
		errs = append(errs, fmt.Errorf("timeout must be >= 0, got %v", o.timeout))
	}
	if o.loc == nil {
		errs = append(errs, errors.New("location is nil"))
	}
	if o.firsts > 1 {
		errs = append(errs, errors.New("RunOnStart and RunOnStartAndWait are mutually exclusive"))
	}
	return errors.Join(errs...)
}

// funcName is the function name with the import path prefix and a method value's -fm suffix removed; see WithName.
func funcName(fn any) string {
	f := runtime.FuncForPC(reflect.ValueOf(fn).Pointer())
	if f == nil {
		return ""
	}
	name := strings.TrimSuffix(f.Name(), "-fm")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}
