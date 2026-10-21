// Package xcron provides in-process scheduled jobs: Add registers them and the framework starts and stops them.
//
// A web service with a few background jobs:
//
//	func main() {
//		xcron.Add("*/5 * * * *", syncOrders)                 // every 5 minutes (UTC)
//		xcron.Add("@every 30s", refresh, xcron.RunOnStart()) // run once as soon as it comes up
//		xone.MustRun(xgin.New().WithRoutes(routes))
//	}
//
// A process running only scheduled jobs:
//
//	xone.MustRun(xone.UntilSignal())
//
// The scheduler comes up on StageServer -- the clients (xgorm, xredis, ...) and the caller's business hooks are all
// ready, so a job can call xgorm.C() directly; on shutdown it stops first, and the clients are only closed once the executions in flight have returned.
//
// Each execution has a root Span of its own (cron <name>) and carries the log field job=<name> in its ctx,
// recording one cron job finished / failed line at the end; a panic is caught and recorded at ERROR without affecting the other jobs or the process.
//
// Every replica runs them in a multi-replica deployment; running on one replica only means taking a lock, see the README's "Multiple replicas".
package xcron

import (
	"context"

	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"

	// Using xcron brings tracing: xtrace installs the global TracerProvider, and each execution's Span is recorded through otel's global API.
	_ "github.com/jupiter-eng/xone/xtrace"
)

// Add registers a scheduled job. spec is a standard 5-field cron (minute hour day month weekday), or
// @yearly / @monthly / @weekly / @daily / @hourly / @every <duration>; it is interpreted in UTC by default, see WithLocation.
//
//	err := xcron.Add("0 3 * * *", cleanup, xcron.WithTimeout(10*time.Minute))
//
// It validates on the spot and returns an *xerror.Error (module xcron): a malformed spec, a nil fn, and an empty or
// duplicate name are config; an Add after the shutdown sequence has begun is register.
//
// It may be called at any time: a job registered before xone.Run starts together with the scheduler, and one
// registered later (inside some hook, say) starts on the spot. An Add with RunOnStartAndWait after the scheduler is
// up waits for that first run and returns its error on failure (op execute), and the job is not registered either.
//
// The ctx fn receives is cancelled on shutdown (and when WithTimeout is reached). The error it returns is only logged and does not affect the next run.
func Add(spec string, fn func(ctx context.Context) error, opts ...Option) error {
	if fn == nil {
		return xerror.Newf("xcron", "config", "job with spec %q has a nil function", spec)
	}
	o := buildOptions(fn, opts)
	if err := o.validate(); err != nil {
		return xerror.Newf("xcron", "config", "job %q: %w", o.name, err)
	}
	sched, err := parse(spec)
	if err != nil {
		return xerror.Newf("xcron", "config", "job %q: bad spec %q: %w", o.name, spec, err)
	}
	return std.add(&job{name: o.name, sched: sched, fn: fn, o: o})
}

// ---- registration ----

// One pair of hooks starts and stops the whole scheduler. StageServer: it comes up only once the clients and the
// business hooks are up, and stops first -- the jobs in flight are still using the database, which is only closed once they have returned.
func init() {
	xhook.BeforeStart(startXCron, xhook.At(xhook.StageServer))
	xhook.BeforeStop(stopXCron) // the stage follows the start hook above
}

func startXCron(ctx context.Context) error { return std.start(ctx) }

func stopXCron(ctx context.Context) error { return std.stop(ctx) }
