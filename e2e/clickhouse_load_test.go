package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// The ClickHouse load tests: bare gin + ClickHouse (baseline's /ch/events/:id) against the xone
// service in three configurations (everything off / the defaults / everything on) looking rows up
// by primary key on ClickHouse through the third xgorm instance, at concurrency 1 / 16 / 64 / 256.
// They run with XONE_E2E_LOAD=1 alone (scripts/e2e.sh --load -run ClickHouse).
//
// Both sides have the same connection pool (50 / 50 / 5m / 5m, with baseline set to xgorm's
// defaults), the same DSN (baseline adds the dial_timeout=500ms xgorm injects by default) and read
// the same way (GORM's Where("id = ?").Take). What is left in the difference is the framework.
//
// As in the PG and MySQL groups, the assertions keep to the promises and pin no number: not one
// request fails; with the metrics on the request count and the histogram are exact; with
// everything on there is one access log line per request; and the SIGTERM after the round exits
// with 0.
// The load generator, the service and ClickHouse (Docker, host networking) share one 4-core
// machine, so the numbers can only be compared with each other
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
		{"bare gin", true, mw{}},
		{"xone everything off", false, mwAllOff},
		{"xone defaults", false, mwDefault},
		{"xone everything on", false, mwAllOn},
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

	t.Logf("The ClickHouse load results (%v warmup and %v measured per step; the load generator shares the machine with the service and ClickHouse, so this is a relative comparison):\n%s", loadWarmup(t), loadStep(t), cellTable(cells))
	t.Logf("ClickHouse against bare gin:\n%s", overheadTable(cells, "bare gin"))
}

// chLoadOptions is the configuration under test for the load round, with the ch instance added; ch's Trace follows the whole tracing pipeline on and off
func chLoadOptions(m mw, table string) harness.Options {
	o := m.options(table)
	o.ClickHouse = true
	o.Overlay += fmt.Sprintf("    ch:\n      Trace: %v\n", m.trace)
	return o
}

// seedCHEvents creates the event table on ClickHouse, fills in n rows (ids 1..n) and returns those
// ids.
// The table is the same as service/store/clickhouse.go and baseline create (all of them
// CREATE TABLE IF NOT EXISTS)
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
		t.Fatalf("creating the table %s: %v", table, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS ` + table) })
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (id, name, value)
		SELECT number + 1, concat('load-', toString(number + 1)), number FROM numbers(%d)`, table, n)); err != nil {
		t.Fatalf("filling in the data: %v", err)
	}
	var got uint64
	if err := db.QueryRowContext(ctx, `SELECT count() FROM `+table).Scan(&got); err != nil || int(got) != n {
		t.Fatalf("filled in %d rows and read %d back (%v)", n, got, err)
	}
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	return ids
}
