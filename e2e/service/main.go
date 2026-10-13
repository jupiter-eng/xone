// e2e 服务：用齐了各集成的一个真实 Web 服务，给 e2e/ 下的测试起进程、发请求、发信号。
//
//	E2E_PORT=8080 E2E_PG_DSN=postgres://... E2E_MYSQL_DSN='u:p@tcp(127.0.0.1:3306)/db' \
//	  E2E_REDIS_ADDR=127.0.0.1:6379 go run . --config=application.yml
//
// 再加上 E2E_CH_DSN=clickhouse://u:p@127.0.0.1:9000/db 与 --profile=ch 就多一个 ClickHouse 实例 ch。
//
// 它就是使用者会写的样子：匿名 import 即装配，配置全部来自 application.yml，
// main 里只有读业务配置、挂一个 Span 出口、一行 MustRun（Service.Drain 大于 0 时服务外面套一层收尾，见 drain.go）。
//
// 接口（细节见各 handler 的注释）：
//
//	GET  /ping                  200 pong
//	POST /users                 写 PG，删缓存
//	GET  /users/:id             本地缓存 → Redis → PG 三级读；?cache=off 直接读 PG
//	PUT  /users/:id             改 PG，删缓存
//	POST /login                 JSON 里带 password、token，测日志脱敏
//	GET  /proxy?token=xxx       经 xhttp 调下游 /echo?token=xxx，测 url.full 去查询串；&method=POST 换方法
//	POST /orders                xflow 三步，fail_at / panic_at / rollback_fail_at 注入故障
//	GET  /slow?ms=N             按 ctx 等 N 毫秒，测优雅退出
//	GET  /stuck?ms=N[&db=1][&mysql=1][&redis=1]  time.Sleep，故意不看 ctx；睡完再查一次 PG / MySQL、Ping 一次 Redis
//	GET  /boom                  panic
//	POST /upload                multipart 上传（字段 file），回大小和 sha256，测 multipart 不进日志
//	GET  /dep?target=redis|db|mysql|ch[&timeout=200ms]  对 Redis / PG / MySQL / ClickHouse 做一次最小操作，回耗时，测故障下的超时
//	POST /mysql/users 等        第二个 xgorm 实例 xgorm.C("mysql") 上的增删改查，见 mysql.go
//	POST /ch/events 等          第三个 xgorm 实例 xgorm.C("ch")（ClickHouse，可选），见 clickhouse.go
//
//	POST /kafka/produce         经 xkafka.C() 写一条（Kafka 可选），见 kafka.go
//
// Service.CronEvery 大于 0 时另有一个定时任务 e2e-tick，见 cron.go。
// Service.KafkaTopic 非空时消费它，见 kafka.go；Service.NoHTTP 时不起 xgin，只消费（xone.UntilSignal）。
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

	// 匿名 import 就是全部「装配」：各包在 init 里登记自己，框架按档位起、逆序关
	_ "github.com/jupiter-eng/xone/e2e/service/store"  // 建表的启动钩子
	_ "github.com/jupiter-eng/xone/e2e/service/warmup" // 不看 ctx 的启动钩子，Service.StartStall 大于 0 时才睡
	_ "github.com/jupiter-eng/xone/xapp"
	_ "github.com/jupiter-eng/xone/xcache"
	_ "github.com/jupiter-eng/xone/xflow"
	_ "github.com/jupiter-eng/xone/xgorm"
	_ "github.com/jupiter-eng/xone/xgorm/clickhouse" // 注册 Driver: clickhouse
	_ "github.com/jupiter-eng/xone/xhttp"
	_ "github.com/jupiter-eng/xone/xkafka"
	_ "github.com/jupiter-eng/xone/xredis"
)

func main() {
	// 业务配置在 main 里读：第一次读的时候框架才加载配置文件，读到的就是最终值
	if err := conf.Load(); err != nil {
		log.Fatal(err)
	}
	c := conf.C()

	if c.SpanFile != "" {
		exp, err := spanlog.NewExporter(c.SpanFile)
		if err != nil {
			log.Fatal(err)
		}
		// 同步导出：Span 结束那一刻就落盘，测试读文件时不用猜要等多久。
		// 在 Run 之前挂也行，xtrace 初始化时会把它接上
		xtrace.AddSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp))
	}
	if c.SpanDiscard {
		// 文档里上报 Span 的写法，exporter 换成只计数的，见 spans.go
		xtrace.AddSpanProcessor(sdktrace.NewBatchSpanProcessor(countingExporter{}))
	}

	// xgin/README.md「访问日志」：名字里没有敏感词、内容却是凭证的头，
	// 用 AddSensitiveHeaders 加进名单。X-Tenant-Id 不含任何敏感词，
	// 它被遮只能是名单那一条在起作用——默认名单里的头名字都带着敏感词，单看它们分不出是哪一条遮的
	middleware.AddSensitiveHeaders("X-Tenant-Id")

	if c.CronEvery > 0 {
		addCron(c.CronEvery, c.CronHold) // 见 cron.go
	}
	if c.KafkaTopic != "" {
		addConsumer(c) // 见 kafka.go
	}
	if c.NoHTTP {
		// 只消费的进程：活全在钩子里，收到信号再跑停止钩子
		xone.MustRun(xone.UntilSignal(), xone.WithStopTimeout(c.StopTimeout))
		return
	}

	g := xgin.New().WithRoutes(routes, probeRoutes) // probeRoutes 见 probe.go
	var r xone.Runnable = g
	if c.Drain > 0 {
		r = drainer{XGin: g, d: c.Drain} // 服务停下之后再收一次尾，见 drain.go
	}
	xone.MustRun(r, xone.WithStopTimeout(c.StopTimeout))
}
