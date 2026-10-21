package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jupiter-eng/xone/e2e/service/store"
	"github.com/jupiter-eng/xone/xgin"
	"github.com/jupiter-eng/xone/xredis"
)

// drainer is a Runnable that winds down once more after the service has stopped: after xgin's Start
// has returned it spends d writing what it holds into PG / Redis (a write-back buffer, a consumer
// offset to commit and the like) before it returns.
// Stop is the embedded xgin's, which is what a caller writing "a service plus a wind-down" ends up
// with.
//
// What it tests is xone.Run's "wait for Start to really return before closing the rest"
// (docs/architecture.md, "The stop budget is one": the service's share is Stop plus waiting for
// Start to return). xgin alone cannot test it: xgin's Start returns as soon as Shutdown begins and
// the requests in flight are waited for by xgin.Stop rather than by Run -- take that wait out of
// Run and every other case still passes.
//
// With Service.Drain at 0 this layer is not wrapped around anything and the service is xgin itself
type drainer struct {
	*xgin.XGin
	d time.Duration
}

// Start runs xgin first and winds down once it has returned.
// The wind-down deliberately does not watch ctx: by now ctx was cancelled long ago, and winding
// down is exactly what happens after the cancellation
func (s drainer) Start(ctx context.Context) error {
	err := s.XGin.Start(ctx)
	time.Sleep(s.d)
	dbErr, redisErr := "none", "none"
	if e := store.Ping(context.WithoutCancel(ctx)); e != nil {
		dbErr = e.Error()
	}
	if e := xredis.C().Ping(context.WithoutCancel(ctx)).Err(); e != nil {
		redisErr = e.Error()
	}
	myErr := "none"
	if e := store.PingMySQL(context.WithoutCancel(ctx)); e != nil {
		myErr = e.Error()
	}
	slog.Info("drain finished", "ms", s.d.Milliseconds(), "db_error", dbErr, "mysql_error", myErr, "redis_error", redisErr)
	return err
}
