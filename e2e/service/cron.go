package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"time"

	"github.com/jupiter-eng/xone/xcron"
)

// cronDrain is how long e2e-tick winds down without watching ctx after it was cancelled: it tests
// "a stop hook waits for the run in flight to return"
const cronDrain = 300 * time.Millisecond

// addCron registers e2e-tick: every run records an e2e cron tick line (carrying job and trace_id)
// first and then waits hold while watching ctx; when ctx is cancelled (the exit) it winds down for
// cronDrain, records an e2e cron drained line and returns
func addCron(every, hold time.Duration) {
	err := xcron.Add(fmt.Sprintf("@every %v", every), func(ctx context.Context) error {
		slog.InfoContext(ctx, "e2e cron tick")
		select {
		case <-time.After(hold):
			return nil
		case <-ctx.Done():
		}
		time.Sleep(cronDrain)
		slog.InfoContext(ctx, "e2e cron drained")
		return ctx.Err()
	}, xcron.WithName("e2e-tick"))
	if err != nil {
		log.Fatal(err)
	}
}
