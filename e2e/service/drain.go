package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jupiter-eng/xone/e2e/service/store"
	"github.com/jupiter-eng/xone/xgin"
	"github.com/jupiter-eng/xone/xredis"
)

// drainer 服务停下之后还要收一次尾的 Runnable：xgin 的 Start 返回之后，
// 再花 d 把手上的活写进 PG / Redis（写回缓冲、提交消费位点这类），然后才返回。
// Stop 是嵌进来的 xgin 的，使用者写「服务 + 收尾」时就是这个样子。
//
// 测的是 xone.Run 的「等 Start 真正返回再关其余组件」（docs/architecture.md「停止预算是一份」：
// 服务那一段是 Stop + 等 Start 返回）。光用 xgin 测不到这一条：xgin 的 Start 在
// Shutdown 一开始就返回了，在途请求是 xgin.Stop 自己等的，不是 Run 等的——
// 把 Run 里那次等待删掉，别的用例照样全过。
//
// Service.Drain为 0 时不套这一层，服务就是 xgin 本身
type drainer struct {
	*xgin.XGin
	d time.Duration
}

// Start 先跑 xgin，它返回之后收尾。
// 收尾故意不看 ctx：此刻 ctx 早就取消了，收尾本来就是取消之后才做的事
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
