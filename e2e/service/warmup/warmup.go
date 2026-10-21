// Package warmup is a start hook in the e2e service that does not watch ctx: warming up local data,
// loading a model and other third-party initialization that takes a ctx and never looks at it.
//
// With Service.StartStall above 0 it time.Sleeps that long and then returns successfully all the
// same. An exit signal arriving while it sleeps cannot interrupt it (docs/architecture.md, "Exit
// signals", point 2: interrupting the one that is running takes that one passing ctx down); what
// can be done is the framework checking ctx once more before the next hook, so that the remaining
// start hooks do not run. The PG / Redis connect cases cannot test this: a connect that is
// interrupted sees the cancelled ctx and returns an error, runStart then takes the error path, and
// the recheck between hooks never gets its turn.
//
// It writes no stage and lands in StageBusiness: after it comes xgin's StageServer configuration
// hook, so without the recheck a hook would still start running after the signal
package warmup

import (
	"context"
	"log/slog"
	"time"

	"github.com/jupiter-eng/xone/e2e/service/conf"
	"github.com/jupiter-eng/xone/xhook"
)

func init() {
	xhook.BeforeStart(preload)
}

// preload sleeps StartStall and deliberately does not watch ctx
func preload(context.Context) error {
	d := conf.C().StartStall
	if d <= 0 {
		return nil
	}
	slog.Info("warmup ignores ctx and sleeps", "ms", d.Milliseconds())
	time.Sleep(d)
	slog.Info("warmup finished", "ms", d.Milliseconds())
	return nil
}
