package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// ClickHouse 的压测：裸 gin + ClickHouse（baseline 的 /ch/events/:id）对照 xone 服务的三种配置
// （全关 / 默认 / 全开）走第三个 xgorm 实例按主键点查 ClickHouse，并发 1 / 16 / 64 / 256 各一档。
// 只在 XONE_E2E_LOAD=1 时跑（scripts/e2e.sh --load -run ClickHouse）。
//
// 两边的连接池一样（50 / 50 / 5m / 5m，baseline 照 xgorm 的默认值配），DSN 一样（baseline 补上 xgorm
// 默认注入的 dial_timeout=500ms），读法一样（GORM 的 Where("id = ?").Take），差出来的只有框架。
//
// 断言和 PG / MySQL 那两组一样只守承诺、不卡数字：一个请求都不失败；指标开着时请求计数和直方图
// 一个不多一个不少；全开时每个请求一条访问日志；压完 SIGTERM 以 0 退出。
// 压测器、服务、ClickHouse（Docker，host 网络）挤在同一台 4 核机器上，数字只能互相比
func TestClickHouse_Load_BareGinVsXOneWithClickHouseByConcurrency(t *testing.T) {
	requireLoad(t)
	harness.RequireCH(t)
	table := "e2e_load_ch_" + harness.NewID()
	ids := seedCHEvents(t, table, 1000)
	ep := endpoint{name: "/ch/events/:id", route: "/ch/events/:id", path: func(i int, _ bool) string {
		return fmt.Sprintf("/ch/events/%d", ids[(i*7919)%len(ids)])
	}}
	concs := []int{1, 16, 64, 256}
	targets := []struct {
		name     string
		baseline bool
		m        mw
	}{
		{"裸 gin", true, mw{}},
		{"xone 全关", false, mwAllOff},
		{"xone 默认", false, mwDefault},
		{"xone 全开", false, mwAllOn},
	}

	var cells []cell
	for _, tg := range targets {
		var p *harness.Process
		if tg.baseline {
			p = harness.StartBaseline(t, harness.Options{Table: table, DiscardStdout: true, ClickHouse: true})
		} else {
			p = harness.Start(t, chLoadOptions(tg.m, table))
		}
		for _, conc := range concs {
			var before harness.Metrics
			if tg.m.metric {
				before = p.Metrics(t)
			}
			c := measure(t, p, tg.name, ep, tg.baseline, conc)
			if tg.m.metric {
				checkRequestMetric(t, c, ep.route, before, p.Metrics(t))
			}
			cells = append(cells, c)
		}
		if tg.m.export {
			exported, handled := waitSpansFlushed(t, p, ep.route)
			t.Logf("%s %s：导出服务端 Span %.0f 个 / 处理请求 %.0f 个（%.2f%%）", tg.name, ep.route, exported, handled, 100*exported/handled)
		}
		var final harness.Metrics
		if tg.m.metric {
			final = p.Metrics(t)
		}
		exit := p.Terminate(t, 30*time.Second)
		if exit.Code != 0 || exit.Signal != nil {
			t.Errorf("%s 压完之后 SIGTERM 应以 0 退出，实际 %v\n%s", tg.name, exit, p.Stderr())
		}
		if tg.m.file {
			byRoute, bad, _ := accessLogStats(t, p.Dir)
			if want := int(final.Sum("e2e_http_requests_total", "route", ep.route)); byRoute[ep.route] != want {
				t.Errorf("文档说每个请求一条访问日志；%s %s 处理了 %d 个请求，日志文件里只有 %d 条", tg.name, ep.route, want, byRoute[ep.route])
			}
			if bad > 0 {
				t.Errorf("日志文件里有 %d 行不是完整的 JSON：并发写入串行了", bad)
			}
		}
	}

	t.Logf("ClickHouse 压测结果（每档预热 %v、正式 %v；压测器与服务、ClickHouse 同机，只作相对比较）：\n%s", loadWarmup(t), loadStep(t), cellTable(cells))
	t.Logf("ClickHouse 相对裸 gin：\n%s", overheadTable(cells, "裸 gin"))
}

// chLoadOptions 压测的被测配置，加上 ch 实例；ch 的 Trace 跟着整套链路一起开关
func chLoadOptions(m mw, table string) harness.Options {
	o := m.options(table)
	o.ClickHouse = true
	o.Overlay += fmt.Sprintf("    ch:\n      Trace: %v\n", m.trace)
	return o
}

// seedCHEvents 在 ClickHouse 上建事件表、灌 n 行（id 1..n），返回这些 id。
// 表结构和 service/store/clickhouse.go、baseline 建的一样（都是 CREATE TABLE IF NOT EXISTS）
func seedCHEvents(t *testing.T, table string, n int) []uint64 {
	t.Helper()
	db := harness.CH(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+table+` (
		id         UInt64,
		name       String,
		value      Int64,
		created_at DateTime64(3) DEFAULT now64(3)
	) ENGINE = MergeTree ORDER BY id`); err != nil {
		t.Fatalf("建表 %s：%v", table, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS ` + table) })
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (id, name, value)
		SELECT number + 1, concat('load-', toString(number + 1)), number FROM numbers(%d)`, table, n)); err != nil {
		t.Fatalf("灌数据：%v", err)
	}
	var got uint64
	if err := db.QueryRowContext(ctx, `SELECT count() FROM `+table).Scan(&got); err != nil || int(got) != n {
		t.Fatalf("灌了 %d 行，读回 %d 行（%v）", n, got, err)
	}
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	return ids
}
