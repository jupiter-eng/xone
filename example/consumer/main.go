// A message queue consumer service: no HTTP port, only workers consuming all along.
//
//	cd example/consumer && go run . --config=application.yml
//
// There is exactly one difference from ../ (the web service): the Runnable handed to xone.Run is
// not xgin but the Consumer here. The configuration, the initialization and the shutdown in reverse
// order are all identical.
package main

import (
	"context"
	"log"
	"log/slog"
	"strconv"
	"time"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/example/consumer/conf"
	"github.com/jupiter-eng/xone/xcache"

	// An anonymous import is the whole of the "wiring". Add xgorm for a database, add xredis for
	// Redis -- without changing one word on the Runnable's side.
	// Metrics come along with xcache. Tracing needs xtrace imported separately: none of xgin, xgorm,
	// xredis or xhttp, which bring tracing along, is here, and without it the logs carry no trace_id
	_ "github.com/jupiter-eng/xone/xtrace"

	// xlog comes along with xgin / xecho / xcron alone. A program with no web framework that wants
	// its logging configured by XLog imports it anonymously itself; without the import
	// slog.Default() stays the standard library's default, and writing an XLog block fails startup
	_ "github.com/jupiter-eng/xone/xlog"
)

func main() {
	// The configuration is read at any time before Start: the framework looks for the configuration
	// file and loads it on the first read, so what is read here is the final value, the same one the
	// integrations read in their start hooks
	if err := conf.Load(); err != nil {
		log.Fatal(err)
	}
	c := conf.C()

	q := newMemQueue(64)
	go feed(q) // a real project has no such line; the messages come from somebody else

	// "Where the configuration comes from" is decided here; the Consumer itself does not know the
	// conf package
	xone.MustRun(&Consumer{q: q, handle: handle,
		settings: Settings{Workers: c.Workers, Timeout: c.MessageTimeout}})
}

// handle handles one message. The business code knows the standard library's slog and the native
// clients, and none of this framework's packages; the log carries trace_id by itself (when an
// upstream forwarded one)
func handle(ctx context.Context, m Message) error {
	slog.InfoContext(ctx, "handling message", "message_id", m.ID, "topic", conf.C().Topic)

	// What comes back is the native ristretto cache. xgorm.C() (the native *gorm.DB) or xredis.C()
	// (the native *redis.Client) work exactly the same way
	xcache.Set("last_message", m.ID)

	time.Sleep(20 * time.Millisecond) // pretending to do some work
	return nil
}

// feed publishes one message every 100ms, purely so that this example has something to consume
func feed(q *memQueue) {
	for i := 1; ; i++ {
		q.publish("msg-"+strconv.Itoa(i), []byte("payload"))
		time.Sleep(100 * time.Millisecond)
	}
}
