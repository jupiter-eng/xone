package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// MySQL 的压测：裸 gin + MySQL（baseline 的 /mysql/users/:id）对照 xone 服务的三种配置
// （全关 / 默认 / 全开）走第二个 xgorm 实例读 MySQL，并发 1 / 16 / 64 / 256 各一档。
// 只在 XONE_E2E_LOAD=1 时跑（scripts/e2e.sh --load -run MySQL）。
//
// 两边的连接池一样（50 / 50 / 5m / 5m，baseline 照 xgorm 的默认值配），读法一样
// （GORM 的 Where("id = ?").Take）；驱动两边都没开 interpolateParams，带参数的查询都是
// 预处理 + 执行两个来回，差出来的只有框架。
//
// 断言和 PG 那组压测一样只守承诺、不卡数字：一个请求都不失败；指标开着时请求计数和直方图
// 一个不多一个不少；全开时每个请求一条访问日志；压完 SIGTERM 以 0 退出。
// 压测器、服务、MySQL 挤在同一台 4 核机器上（可能还有别的 e2e 在跑），数字只能互相比
func TestMySQL_Load_BareGinVsXOneWithMySQLByConcurrency(t *testing.T) {
	requireLoad(t)
	table := "e2e_load_my_" + harness.NewID()
	ids := seedMySQLUsers(t, table, 1000)
	ep := endpoint{name: "/mysql/users/:id", route: "/mysql/users/:id", path: func(i int, _ bool) string {
		return fmt.Sprintf("/mysql/users/%d", ids[(i*7919)%len(ids)])
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
			p = harness.StartBaseline(t, harness.Options{Table: table, DiscardStdout: true})
		} else {
			p = harness.Start(t, tg.m.options(table))
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

	t.Logf("MySQL 压测结果（每档预热 %v、正式 %v；压测器与服务、MySQL 同机，只作相对比较）：\n%s", loadWarmup(t), loadStep(t), cellTable(cells))
	t.Logf("MySQL 相对裸 gin：\n%s", overheadTable(cells, "裸 gin"))
}

// seedMySQLUsers 在 MySQL 上建用户表、灌 n 个用户，返回它们的 id。
// 表结构和 service/store/mysql.go、baseline 建的一样（都是 CREATE TABLE IF NOT EXISTS）
func seedMySQLUsers(t *testing.T, table string, n int) []int64 {
	t.Helper()
	db := harness.MySQL(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+table+` (
		id         BIGINT AUTO_INCREMENT PRIMARY KEY,
		name       VARCHAR(255) NOT NULL,
		email      VARCHAR(255) NOT NULL DEFAULT '',
		created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
	)`); err != nil {
		t.Fatalf("建表 %s：%v", table, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS ` + table) })
	// 递归 CTE 生成 1..n；cte_max_recursion_depth 默认 1000，n 不超过它
	if _, err := db.ExecContext(ctx, `INSERT INTO `+table+` (name, email)
		WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM g WHERE i < ?)
		SELECT CONCAT('load-', i), CONCAT('load-', i, '@example.com') FROM g`, n); err != nil {
		t.Fatalf("灌数据：%v", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM `+table+` ORDER BY id`)
	if err != nil {
		t.Fatalf("读 id：%v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("读 id：%v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil || len(ids) != n {
		t.Fatalf("灌了 %d 个用户，读回 %d 个（%v）", n, len(ids), err)
	}
	return ids
}
