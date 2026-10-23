// The e2e service: a real web service using every integration, for the tests under e2e/ to start
// as a process and send requests and signals to.
//
//	E2E_PORT=8080 E2E_PG_DSN=postgres://... E2E_MYSQL_DSN='u:p@tcp(127.0.0.1:3306)/db' \
//		E2E_REDIS_ADDR=127.0.0.1:6379 ./service --config=application.yml
//
// With E2E_CH_DSN=clickhouse://u:p@127.0.0.1:9000/db and --profile=ch there is a third instance,
// the ClickHouse one, ch.
//
// It is written the way a caller would write it: a blank import is the wiring, all of the
// configuration comes from application.yml, and main holds the business configuration read, one
// span exporter hung on, and a single MustRun (with Service.Drain above 0 a winding-down layer
// wraps the service, see drain.go).
//
// The endpoints (each handler's comment has the details):
//
//	GET  /ping                  200 pong
//	POST /users                 writes PG and drops the cache
//	GET  /users/:id             reads through three levels, local cache -> Redis -> PG; ?cache=off reads PG directly
//	PUT  /users/:id             updates PG and drops the cache
//	POST /login                 password and token in the JSON, for the log redaction
//	GET  /proxy?token=xxx       calls the downstream /echo?token=xxx through xhttp, for url.full dropping the query string; &method=POST changes the method
//	POST /orders                three xflow steps, with fail_at / panic_at / rollback_fail_at injecting a fault
//	GET  /slow?ms=N             waits N milliseconds watching ctx, for the graceful exit
//	GET  /stuck?ms=N[&db=1][&mysql=1][&redis=1]  time.Sleep, deliberately not watching ctx; after the sleep it queries PG / MySQL once more and Pings Redis
//	GET  /boom                  panics
//	POST /upload                a multipart upload (the field file), answering the size and the sha256, for multipart staying out of the log
//	GET  /dep?target=redis|db|mysql|ch[&timeout=200ms]  one minimal operation against Redis / PG / MySQL / ClickHouse, answering how long it took, for the timeouts under a fault
//	POST /mysql/users and so on the create / read / update / delete on the second xgorm instance xgorm.C("mysql"), see mysql.go
//	POST /ch/events and so on   the third xgorm instance xgorm.C("ch") (ClickHouse, optional), see clickhouse.go
//
//	POST /kafka/produce         writes one record through xkafka.C() (Kafka optional), see kafka.go
//
//	GET  /api/v1/users/:id      a version prefix behind an authentication middleware: 200 with
//	                            Authorization: Bearer e2e-token, 401 without one, 403 with another, see v1.go
//	GET  /ws                    hijacks and answers 101, then ticks until the ctx is cancelled, see ws.go
//
// With Service.CronEvery above 0 there is a scheduled job e2e-tick as well, see cron.go.
// With Service.KafkaTopic non-empty it is consumed, see kafka.go; with Service.NoHTTP no xgin is
// started and it only consumes (xone.UntilSignal).
package main

import (
	"log"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/e2e/internal/spanlog"
	"github.com/jupiter-eng/xone/e2e/service/conf"
	"github.com/jupiter-eng/xone/xgin"
	"github.com/jupiter-eng/xone/xgin/middleware"
	"github.com/jupiter-eng/xone/xtrace"

	// The blank imports are the whole "wiring": every package registers itself in init, and the
	// framework starts them by stage and closes them in reverse
	_ "github.com/jupiter-eng/xone/e2e/service/store"  // the start hook creating the tables
	_ "github.com/jupiter-eng/xone/e2e/service/warmup" // a start hook that does not watch ctx, sleeping only with Service.StartStall above 0
	_ "github.com/jupiter-eng/xone/xapp"
	_ "github.com/jupiter-eng/xone/xcache"
	_ "github.com/jupiter-eng/xone/xflow"
	_ "github.com/jupiter-eng/xone/xgorm"
	_ "github.com/jupiter-eng/xone/xgorm/clickhouse" // registers Driver: clickhouse
	_ "github.com/jupiter-eng/xone/xhttp"
	_ "github.com/jupiter-eng/xone/xkafka"
	_ "github.com/jupiter-eng/xone/xredis"
)

func main() {
	// The business configuration is read in main: the framework loads the configuration file on the
	// first read, and what comes back is the final value
	if err := conf.Load(); err != nil {
		log.Fatal(err)
	}
	c := conf.C()

	if c.SpanFile != "" {
		exp, err := spanlog.NewExporter(c.SpanFile)
		if err != nil {
			log.Fatal(err)
		}
		// A synchronous exporter: a span lands on disk the moment it ends, so a test reading the file
		// has nothing to guess about how long to wait.
		// Hanging it on before Run is fine, xtrace picks it up when it initializes
		xtrace.AddSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp))
	}
	if c.SpanDiscard {
		// The way the documentation reports spans, with the exporter replaced by one that only counts, see spans.go
		xtrace.AddSpanProcessor(sdktrace.NewBatchSpanProcessor(countingExporter{}))
	}

	// xgin/README.md, "The access log": a header whose name holds no sensitive word while its content
	// is a credential goes onto the list with AddSensitiveHeaders. X-Tenant-Id holds no sensitive word
	// at all, so it being masked can only be that list entry at work -- the headers on the default
	// list all carry a sensitive word in their name, and on their own they do not tell which rule
	// masked them
	middleware.AddSensitiveHeaders("X-Tenant-Id")

	if c.CronEvery > 0 {
		addCron(c.CronEvery, c.CronHold) // see cron.go
	}
	if c.KafkaTopic != "" {
		addConsumer(c) // see kafka.go
	}
	if c.NoHTTP {
		// A process that only consumes: the work is all in the hooks, and the stop hooks run once a signal arrives
		xone.MustRun(xone.UntilSignal(), xone.WithStopTimeout(c.StopTimeout))
		return
	}

	g := xgin.New().WithRoutes(routes, probeRoutes, v1Routes, wsRoutes) // probeRoutes is in probe.go, v1Routes in v1.go, wsRoutes in ws.go
	var r xone.Runnable = g
	if c.Drain > 0 {
		r = drainer{XGin: g, d: c.Drain} // wind down once more after the service has stopped, see drain.go
	}
	xone.MustRun(r, xone.WithStopTimeout(c.StopTimeout))
}
