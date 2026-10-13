// Package e2e 对 xone 做真实的 Web 服务测试：真的 PostgreSQL、MySQL、Redis、ClickHouse 与 Kafka（Docker），
// 真的进程和信号，不 mock 任何东西。
//
//	scripts/e2e.sh               # 拉起 PG / MySQL / Redis / ClickHouse / Kafka，跑全部
//	scripts/e2e.sh -run Smoke    # 参数原样交给 go test
//
// 目录：
//
//	service/    被测的服务：用齐了各集成，配置全部来自 service/application.yml
//	baseline/   裸 gin 的对照服务，压测时比出框架的开销
//	harness/    构建、起进程、发信号、读日志 / Span / 指标 / /proc、TCP 代理、下游桩、压测器
//	*_test.go   测试本身
//
// 环境变量 XONE_E2E 不为 1 时全部跳过：scripts/test.sh 遍历每个模块跑测试，
// 没有数据库的机器上这个模块必须照样全绿。ClickHouse 跑在 Docker 里，起不来时只有
// TestClickHouse_* 跳过（harness.RequireCH），其余照跑；Kafka 同理（TestKafka_*，harness.RequireKafka）。
//
// 测试照文档写的行为断言。揭示了框架的 bug 时不改断言去迁就它，
// 而是在失败的分支里调 knownBug 标记（打印 KNOWN BUG 并跳过），证据写进注释。
package e2e
