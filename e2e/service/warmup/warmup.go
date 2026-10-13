// Package warmup 是 e2e 服务里一个不看 ctx 的启动钩子：预热本地数据、加载模型这类
// 第三方初始化，它们收了 ctx 也不看。
//
// Service.StartStall大于 0 时它 time.Sleep 这么久，然后照样成功返回。
// 退出信号在它睡着时到达，它自己是打断不了的（docs/architecture.md「退出信号」第 2 条：打断正在跑的
// 那一个要靠它自己把 ctx 传下去）；能做的是框架在下一个钩子之前再查一次 ctx，
// 剩下的启动钩子不再跑。PG / Redis 建连那两例测不到这一条：被打断的建连看到 ctx 取消
// 就返回错误，runStart 走的是出错那条路，钩子之间那次复查根本轮不到。
//
// 不写档位，落在 StageBusiness：在它后面还有 StageServer 的 xgin 配置钩子，
// 复查没做的话，信号之后还会有钩子跑起来
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

// preload 睡 StartStall，故意不看 ctx
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
