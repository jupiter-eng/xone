// 一个消息队列消费者服务：没有 HTTP 端口，只有一直在消费的 worker。
//
//	cd example/consumer && go run . --config=application.yml
//
// 与 ../（Web 服务）的区别只有一处：交给 xone.Run 的 Runnable 不是 xgin，
// 是这里的 Consumer。配置、初始化、逆序关闭全都一模一样。
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

	// 匿名 import 就是全部「装配」。想用数据库就加上 xgorm，
	// 想用 Redis 就加上 xredis——Runnable 那边一个字都不用改。
	// 指标跟着 xcache 一起来。链路要单独 import xtrace：这里没有 xgin、xgorm、xredis、xhttp
	// 这些自带链路的集成，少了它日志就带不上 trace_id
	_ "github.com/jupiter-eng/xone/xtrace"

	// xlog 只跟着 xgin / xecho / xcron 来。没有 Web 框架的程序要按 XLog 配日志，自己匿名 import 它；
	// 不 import 的话 slog.Default() 保持标准库默认，写了 XLog 块会启动失败
	_ "github.com/jupiter-eng/xone/xlog"
)

func main() {
	// 配置在 Start 之前任何时候读都行：第一次读的时候框架才去找配置文件、加载它，
	// 这里读到的就是最终值，和各集成在启动钩子里读到的是同一份
	if err := conf.Load(); err != nil {
		log.Fatal(err)
	}
	c := conf.C()

	q := newMemQueue(64)
	go feed(q) // 真实项目里没有这一行，消息是别人发进来的

	// 「哪来的配置」由这里决定，Consumer 自己不认识 conf 包
	xone.MustRun(&Consumer{q: q, handle: handle,
		settings: Settings{Workers: c.Workers, Timeout: c.MessageTimeout}})
}

// handle 处理一条消息。业务代码只认识标准库 slog 和原生 client，
// 不认识本框架的包；日志会自动带上 trace_id（上游透传了的话）
func handle(ctx context.Context, m Message) error {
	slog.InfoContext(ctx, "handling message", "message_id", m.ID, "topic", conf.C().Topic)

	// 拿到的是原生的 ristretto 缓存。换成 xgorm.C()（原生 *gorm.DB）
	// 或者 xredis.C()（原生 *redis.Client）是一样的
	xcache.Set("last_message", m.ID)

	time.Sleep(20 * time.Millisecond) // 假装在干活
	return nil
}

// feed 每 100ms 发一条消息进来，纯粹为了让这个例子有东西可消费
func feed(q *memQueue) {
	for i := 1; ; i++ {
		q.publish("msg-"+strconv.Itoa(i), []byte("payload"))
		time.Sleep(100 * time.Millisecond)
	}
}
