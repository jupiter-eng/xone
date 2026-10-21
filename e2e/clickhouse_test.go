package e2e

// The ClickHouse end-to-end tests: the TestClickHouse_* family, against the e2e service's third
// xgorm instance xgorm.C("ch"), reaching the ClickHouse 24.8 in Docker through the
// xgorm/clickhouse dialect and the native protocol.
//
//	scripts/e2e.sh -run ClickHouse
//	scripts/e2e.sh --load -run ClickHouse_Load   # the load tests
//
// The ClickHouse instance is optional: it exists only once harness.Options.ClickHouse activates
// the service/application-ch.yml profile.
// When ClickHouse is unavailable (no Docker, a container that will not come up) this group skips
// case by case (harness.RequireCH) while the rest of e2e runs.
//
// The assertions follow xgorm/clickhouse/README.md, "Configuration" and "ClickHouse's timeouts and
// cancellation", the README and xgorm/clickhouse's package documentation; a failure message reads
// "the documentation says X, got Y". A number the documentation does not pin down is measured and
// not asserted, and printed with t.Logf behind a "numbers:" prefix.
//
// How the files divide up:
//
//	clickhouse_test.go             this file: the shared helpers and the constants that follow from the documented defaults
//	clickhouse_functional_test.go  writing / querying, the SQL log, spans, the pool metrics, no password leaking, the DSN rules, the version probe
//	clickhouse_auth_test.go        a wrong password: the native protocol recognizes it and does not retry, the HTTP protocol does not
//	clickhouse_fault_test.go       ClickHouse broken / stuck while running, unreachable at start, a SIGTERM during the start
//	clickhouse_shutdown_test.go    a query in flight on SIGTERM, and whether a stuck query listens to the request's ctx
//	clickhouse_load_test.go        bare gin + ClickHouse against xone + ClickHouse (XONE_E2E_LOAD=1)
//	clickhouse_tls_test.go         the TLS block: a second container with its TLS ports open, once over native and once over https

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// XGorm's defaults from xgorm/README.md and its two ClickHouse sections; service/application-ch.yml changes none of them
const (
	chDialTimeout = 500 * time.Millisecond // DialTimeout, injected into the DSN as dial_timeout
	// chAttempt: "for the other drivers it is 2 x DialTimeout" -- xgorm/clickhouse's resolve goes by the dial_timeout the driver read
	chAttempt = 2 * chDialTimeout
	// chStartBudget, "Connect retries": 3 attempts x the budget of one probe + the 1s and 2s upper bounds of the two backoffs
	chStartBudget = faultPingAttempts*chAttempt + faultPingBackoffs // 6s
	// chReadTimeout: "read_timeout is 300s when it is not written (setDefaults)"
	chReadTimeout = 300 * time.Second
)

// chEvent is one row as the ClickHouse endpoints see it
type chEvent struct {
	ID    uint64 `json:"id"`
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

// chInsert POSTs /ch/events, wants a 201 and returns the ids the service handed out
func chInsert(t *testing.T, p *harness.Process, name string, values ...int64) ([]uint64, harness.Response) {
	t.Helper()
	r := p.PostJSON(t, "/ch/events", map[string]any{"name": name, "values": values})
	if r.Status != http.StatusCreated {
		t.Fatalf("POST /ch/events should answer 201, got %v", r)
	}
	var body struct {
		IDs []uint64 `json:"ids"`
	}
	r.JSON(t, &body)
	if len(body.IDs) != len(values) {
		t.Fatalf("POST /ch/events wrote %d rows and should answer %d ids, got %v", len(values), len(values), r)
	}
	return body.IDs, r
}

// chRows reads the rows of this process's event table whose id is in ids directly from ClickHouse, ordered by id
func chRows(t *testing.T, p *harness.Process, ids ...uint64) []chEvent {
	t.Helper()
	db := harness.CH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, "SELECT id, name, value FROM "+p.Table+" WHERE has(?, id) ORDER BY id", ids)
	if err != nil {
		t.Fatalf("reading %s on ClickHouse directly: %v", p.Table, err)
	}
	defer rows.Close()
	var out []chEvent
	for rows.Next() {
		var e chEvent
		if err := rows.Scan(&e.ID, &e.Name, &e.Value); err != nil {
			t.Fatalf("reading %s on ClickHouse directly: %v", p.Table, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading %s on ClickHouse directly: %v", p.Table, err)
	}
	return out
}

// chOverlay configures the ch instance alone: body is the YAML under XGorm.Clients.ch, written two spaces in
func chOverlay(body string) string {
	return "XGorm:\n  Clients:\n    ch:\n" + indent(body, "      ")
}

// chDep GETs /dep?target=ch, with the client waiting wait at most
func chDep(p *harness.Process, timeout string, wait time.Duration) faultDepResult {
	return faultDep(p, "ch", timeout, wait)
}

// chWaitDep probes ClickHouse until it answers 200 and returns how long that took and how many
// probes it needed; it t.Fatals when it has not recovered within limit
func chWaitDep(t *testing.T, p *harness.Process, limit time.Duration) (time.Duration, int) {
	t.Helper()
	return faultWaitRecover(t, p, "ch", limit)
}

// chStart starts a process carrying a ClickHouse instance
func chStart(t *testing.T, o harness.Options) *harness.Process {
	t.Helper()
	o.ClickHouse = true
	return harness.Start(t, o)
}
