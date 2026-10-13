// 一个用到「自己写的集成」的服务。
//
//	cd example/component && go run . --config=application.yml
//
// 看点有三个：
//
//	xkv/      自己写的集成：BeforeStart 里读配置、建实例，BeforeStop 里关掉，C() 取实例
//	conf/     业务自己的配置块：一个 Load、一个 C()，在 Start 之前任何时候读都行
//	main()    装配就是匿名 import + 一行 Run，没有别的
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

	// 匿名 import 就是全部「装配」。想用数据库就加上 xgorm，
	// 想用 Redis 就加上 xredis——下面的 App 一个字都不用改
	_ "github.com/jupiter-eng/xone/example/component/xkv"
)

func main() {
	// 业务配置在这里读就行：第一次读的时候框架才去找配置文件、加载它，
	// 读到的就是最终值
	if err := conf.Load(); err != nil {
		log.Fatal(err)
	}

	// 交给框架的只有一个 Runnable。按档位建好 XKV、收到信号时取消 ctx、
	// 等 App 返回之后再把 XKV 关掉——都是框架的事
	xone.MustRun(&App{})
}

// App 本服务要干的活。它满足 xone.Runnable，所以能直接交给 Run
type App struct{}

// Start 由 xone.Run 调用，此时配置已加载、XKV 已就绪。
//
// ctx 在退出信号到达时被取消。
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

		// 拿到的就是 xkv 自己的原生类型，没有任何包装
		key := "round-" + strconv.Itoa(i)
		xkv.C().Set(key, c.Greeting)
		got, _ := xkv.C().Get(key)
		slog.InfoContext(ctx, "round done", "key", key, "value", got, "total_keys", xkv.C().Len())
	}

	slog.InfoContext(ctx, "all rounds done, exiting")
	return nil
}
