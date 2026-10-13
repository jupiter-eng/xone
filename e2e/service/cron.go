package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"time"

	"github.com/jupiter-eng/xone/xcron"
)

// cronDrain e2e-tick 被取消之后不看 ctx 地收尾多久：测「停止钩子等在途的执行返回」
const cronDrain = 300 * time.Millisecond

// addCron 登记 e2e-tick：每次执行先记一行 e2e cron tick（带着 job、trace_id），
// 再看着 ctx 等 hold；ctx 被取消（退出）时收尾 cronDrain、记一行 e2e cron drained 再返回
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
