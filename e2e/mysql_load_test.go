package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// The MySQL load tests: bare gin + MySQL (baseline's /mysql/users/:id) against the xone service in
// three configurations (everything off / the defaults / everything on) reading MySQL through the
// second xgorm instance, at concurrency 1 / 16 / 64 / 256.
// They run with XONE_E2E_LOAD=1 alone (scripts/e2e.sh --load -run MySQL).
//
// Both sides have the same connection pool (50 / 50 / 5m / 5m, with baseline set to xgorm's
// defaults) and read the same way (GORM's Where("id = ?").Take); neither side has
// interpolateParams on in the driver, so a query with parameters is a prepare and an execute, two
// round trips, on both. What is left in the difference is the framework.
//
// As in the PG group, the assertions keep to the promises and pin no number: not one request
// fails; with the metrics on the request count and the histogram are exact; with everything on
// there is one access log line per request; and the SIGTERM after the round exits with 0.
// The load generator, the service and MySQL share one 4-core machine (perhaps with other e2e cases
// running), so the numbers can only be compared with each other
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
		{"bare gin", true, mw{}},
		{"xone everything off", false, mwAllOff},
		{"xone defaults", false, mwDefault},
		{"xone everything on", false, mwAllOn},
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
			t.Logf("%s %s: %.0f server spans exported / %.0f requests handled (%.2f%%)", tg.name, ep.route, exported, handled, 100*exported/handled)
		}
		var final harness.Metrics
		if tg.m.metric {
			final = p.Metrics(t)
		}
		exit := p.Terminate(t, 30*time.Second)
		if exit.Code != 0 || exit.Signal != nil {
			t.Errorf("%s should exit with 0 on the SIGTERM after the round, got %v\n%s", tg.name, exit, p.Stderr())
		}
		if tg.m.file {
			byRoute, bad, _ := accessLogStats(t, p.Dir)
			if want := int(final.Sum("e2e_http_requests_total", "route", ep.route)); byRoute[ep.route] != want {
				t.Errorf("the documentation says one access log line per request; %s %s handled %d requests and the log file holds only %d", tg.name, ep.route, want, byRoute[ep.route])
			}
			if bad > 0 {
				t.Errorf("%d lines of the log file are not whole JSON: the concurrent writes were torn", bad)
			}
		}
	}

	t.Logf("The MySQL load results (%v warmup and %v measured per step; the load generator shares the machine with the service and MySQL, so this is a relative comparison):\n%s", loadWarmup(t), loadStep(t), cellTable(cells))
	t.Logf("MySQL against bare gin:\n%s", overheadTable(cells, "bare gin"))
}

// seedMySQLUsers creates the user table on MySQL, fills in n users and returns their ids.
// The table is the same as service/store/mysql.go and baseline create (all of them
// CREATE TABLE IF NOT EXISTS)
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
		t.Fatalf("creating the table %s: %v", table, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS ` + table) })
	// A recursive CTE generates 1..n; cte_max_recursion_depth is 1000 by default and n stays under it
	if _, err := db.ExecContext(ctx, `INSERT INTO `+table+` (name, email)
		WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM g WHERE i < ?)
		SELECT CONCAT('load-', i), CONCAT('load-', i, '@example.com') FROM g`, n); err != nil {
		t.Fatalf("filling in the data: %v", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM `+table+` ORDER BY id`)
	if err != nil {
		t.Fatalf("reading the ids: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("reading the ids: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil || len(ids) != n {
		t.Fatalf("filled in %d users and read %d back (%v)", n, len(ids), err)
	}
	return ids
}
