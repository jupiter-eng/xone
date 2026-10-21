// A service using an integration of its own.
//
//	cd example/component && go run . --config=application.yml
//
// There are three things to look at:
//
//	xkv/      an integration of our own: BeforeStart reads the configuration and builds the
//	          instance, BeforeStop closes it, and C() takes it
//	conf/     the business's own configuration block: one Load and one C(), read at any time before
//	          Start
//	main()    the wiring is an anonymous import plus one line of Run, and nothing else
package main

import (
	"context"
	"log"
	"log/slog"
	"strconv"
	"time"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/example/component/conf"
	"github.com/jupiter-eng/xone/example/component/xkv"

	// An anonymous import is the whole of the "wiring". Add xgorm for a database, add xredis for
	// Redis -- without changing one word of the App below
	_ "github.com/jupiter-eng/xone/example/component/xkv"
)

func main() {
	// The business configuration is read right here: the framework looks for the configuration file
	// and loads it on the first read, so what is read is the final value
	if err := conf.Load(); err != nil {
		log.Fatal(err)
	}

	// All the framework is handed is one Runnable. Building XKV by stage, cancelling the ctx when the
	// signal arrives, and closing XKV only after App has returned are all its business
	xone.MustRun(&App{})
}

// App is the work this service does. It satisfies xone.Runnable and can be handed to Run directly
type App struct{}

// Start is called by xone.Run, by which time the configuration is loaded and XKV is ready.
//
// The ctx is cancelled when a shutdown signal arrives.
func (a *App) Start(ctx context.Context) error {
	c := conf.C()
	slog.InfoContext(ctx, "app starting",
		"greeting", c.Greeting, "rounds", c.Rounds, "kv_path", xkv.Conf().Path)

	for i := 1; i <= c.Rounds; i++ {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "shutdown signal received, stopping early", "done_rounds", i-1)
			return nil
		case <-time.After(300 * time.Millisecond):
		}

		// What comes back is xkv's own native type, with no wrapper around it
		key := "round-" + strconv.Itoa(i)
		xkv.C().Set(key, c.Greeting)
		got, _ := xkv.C().Get(key)
		slog.InfoContext(ctx, "round done", "key", key, "value", got, "total_keys", xkv.C().Len())
	}

	slog.InfoContext(ctx, "all rounds done, exiting")
	return nil
}
