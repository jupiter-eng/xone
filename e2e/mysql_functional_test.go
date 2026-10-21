package e2e

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// Create / read / update / delete go through the second instance: xgorm/README.md, XGorm, "Several
// instances" -- C() takes default and C("name") the named one.
// The data is checked against MySQL directly, and at the same time the table of that name on PG is
// checked to hold none of these rows: written into default, the endpoints would still answer
// 201 / 200 and the endpoints alone would not tell
func TestMySQL_CRUDViaSecondInstance_DataLandsInMySQL(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	p := harness.Start(t, harness.Options{})
	name, email := "my-"+harness.NewID(), "my-"+harness.NewID()+"@example.com"

	u, _ := mysqlCreate(t, p, name, email)
	if u.Name != name || u.Email != email {
		t.Errorf("POST /mysql/users should answer the values that were written, got %+v", u)
	}
	if row, ok := mysqlRow(t, p, u.ID); !ok || row != u {
		t.Fatalf("MySQL read directly should hold what was just written, %+v, got %+v (the row is there: %v)", u, row, ok)
	}
	var pgRows int
	if err := harness.DB(t).QueryRow("SELECT COUNT(*) FROM " + p.Table).Scan(&pgRows); err != nil {
		t.Fatalf("counting %s on PG directly: %v", p.Table, err)
	}
	if pgRows != 0 {
		t.Errorf("the write went to C(\"mysql\"), so %s on PG (default) should hold no row, got %d", p.Table, pgRows)
	}

	r := p.Get(t, fmt.Sprintf("/mysql/users/%d", u.ID))
	var got mysqlUser
	r.JSON(t, &got)
	if r.Status != http.StatusOK || got != u {
		t.Errorf("GET /mysql/users/%d should answer 200 with %+v, got %v", u.ID, u, r)
	}
	if r := p.Get(t, "/mysql/users/999999999"); r.Status != http.StatusNotFound {
		t.Errorf("a user that does not exist should answer 404, got %v", r)
	}

	upd := mysqlUser{ID: u.ID, Name: name + "-v2", Email: "v2-" + email}
	if r := p.Do(t, http.MethodPut, fmt.Sprintf("/mysql/users/%d", u.ID), map[string]string{"name": upd.Name, "email": upd.Email}); r.Status != http.StatusOK {
		t.Errorf("PUT /mysql/users/%d should answer 200, got %v", u.ID, r)
	}
	if row, _ := mysqlRow(t, p, u.ID); row != upd {
		t.Errorf("MySQL read directly after the update should hold %+v, got %+v", upd, row)
	}
	// Updating to the same values: MySQL reports 0 "changed rows" while the row is there, which should not be a 404
	if r := p.Do(t, http.MethodPut, fmt.Sprintf("/mysql/users/%d", u.ID), map[string]string{"name": upd.Name, "email": upd.Email}); r.Status != http.StatusOK {
		t.Errorf("the same update once more should still answer 200, got %v", r)
	}
	if r := p.Do(t, http.MethodPut, "/mysql/users/999999999", map[string]string{"name": "x"}); r.Status != http.StatusNotFound {
		t.Errorf("updating a user that does not exist should answer 404, got %v", r)
	}

	if r := p.Do(t, http.MethodDelete, fmt.Sprintf("/mysql/users/%d", u.ID), nil); r.Status != http.StatusNoContent {
		t.Errorf("DELETE /mysql/users/%d should answer 204, got %v", u.ID, r)
	}
	if _, ok := mysqlRow(t, p, u.ID); ok {
		t.Errorf("MySQL read directly after the delete should not hold id=%d", u.ID)
	}
	if r := p.Do(t, http.MethodDelete, fmt.Sprintf("/mysql/users/%d", u.ID), nil); r.Status != http.StatusNotFound {
		t.Errorf("deleting it once more should answer 404, got %v", r)
	}

	// Both instances are there: the PG group works as usual
	pu := createUser(t, p, "pg-side", "pg@example.com")
	if g, _, _ := getUser(t, p, pu.ID); g.Name != "pg-side" {
		t.Errorf("the PG endpoints should work as usual, got %+v", g)
	}
}

// xgorm/README.md, XGorm.Log: "what is recorded is the SQL with its placeholders and no parameter
// values" and "the placeholders are the ones sent to the database: ? for MySQL"; the statement in
// the log is the span's db.query.text (xgorm/trace.go: "what is recorded is the SQL with its
// placeholders, and Statement.Vars is not").
//
// Log takes per instance (it is a field of ClientConfig): with it on for mysql alone, not one SQL
// statement of default (PG) should be recorded
func TestMySQL_SQLLogHasPlaceholdersNotArgs_MatchesSpan_LogPerInstance(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	p := harness.Start(t, harness.Options{Spans: true, Overlay: sqlLog("mysql")})
	name, email := "sql-"+harness.NewID(), "sql-"+harness.NewID()+"@example.com"

	sqlOf := func(t *testing.T, tid string) []harness.Log {
		t.Helper()
		// Wait for this request's access log: it goes to the same standard output as the SQL log and is written after the handler returns,
		// so by the time it is read this request's SQL log has arrived. The server span cannot serve as the signal: a span is written to the file
		// synchronously while standard output comes through a pipe a little later, and with the machine full under load "the span arrived, the SQL log had not" happened once
		accessLog(t, p, tid)
		serverSpan(t, p, tid)
		return p.FindLogs(func(l harness.Log) bool { return l.Msg() == "SQL" && l.Str("trace_id") == tid })
	}
	check := func(t *testing.T, what, spanName, prefix string, r harness.Response, values ...string) {
		t.Helper()
		tid := traceIDOf(t, r)
		logs := sqlOf(t, tid)
		if len(logs) != 1 {
			t.Fatalf("%s should record 1 SQL log line, got %d", what, len(logs))
		}
		logged := logs[0].Str("sql")
		if !strings.HasPrefix(logged, prefix) || !strings.Contains(logged, "?") {
			t.Errorf("%s's SQL log should be %s... with ? placeholders, got %q", what, prefix, logged)
		}
		mustNotContain(t, what+"'s SQL log", logs[0].Line, values...)
		spans := spansNamed(traceSpans(t, p, tid), spanName)
		if len(spans) != 1 {
			t.Fatalf("%s's trace should hold one %s", what, spanName)
		}
		if sent := spans[0].Str("db.query.text"); sent != logged {
			t.Errorf("%s: the SQL log and the span's db.query.text should be the same statement: the log %q, the span %q", what, logged, sent)
		}
		t.Logf("numbers: the SQL %s recorded: %s", what, logged)
	}

	u, r := mysqlCreate(t, p, name, email)
	check(t, "POST /mysql/users", "gorm.create", "INSERT INTO `"+p.Table+"`", r, name, email)
	check(t, "GET /mysql/users/:id", "gorm.query", "SELECT * FROM `"+p.Table+"` WHERE id = ?",
		p.Get(t, fmt.Sprintf("/mysql/users/%d", u.ID)), fmt.Sprintf("id = %d", u.ID))
	check(t, "DELETE /mysql/users/:id", "gorm.delete", "DELETE FROM `"+p.Table+"` WHERE id = ?",
		p.Do(t, http.MethodDelete, fmt.Sprintf("/mysql/users/%d", u.ID), nil), fmt.Sprintf("id = %d", u.ID))

	// The PG instance has no Log on: not one of its statements is recorded
	pr := p.Get(t, "/users/999999999?cache=off")
	if logs := sqlOf(t, traceIDOf(t, pr)); len(logs) != 0 {
		t.Errorf("Log is on for the mysql instance alone, so default's (PG's) SQL should not be recorded, and %d lines were: %s", len(logs), logs[0].Line)
	}
}

// The span's attributes (xgorm/trace.go, the OTel database semantic conventions v1.43.0):
// db.system.name is the database, db.namespace the database name, server.address / server.port the
// address resolved from the DSN and db.operation.name the statement's first keyword, with
// db.rows_affected added when it ends;
// the parent is the server span. Every instance uses its own connection information: in one request
// the PG and the MySQL spans each report their own
func TestMySQL_SQLSpanCarriesThisInstanceConnInfo(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	p := harness.Start(t, harness.Options{Spans: true})
	name, email := "span-"+harness.NewID(), "span-"+harness.NewID()+"@example.com"

	u, r := mysqlCreate(t, p, name, email)
	upd := p.Do(t, http.MethodPut, fmt.Sprintf("/mysql/users/%d", u.ID), map[string]string{"name": name + "2"})
	del := p.Do(t, http.MethodDelete, fmt.Sprintf("/mysql/users/%d", u.ID), nil)
	miss := p.Get(t, fmt.Sprintf("/mysql/users/%d", u.ID))

	for _, c := range []struct {
		what, span, op, rows string
		r                    harness.Response
	}{
		{"POST /mysql/users", "gorm.create", "INSERT", "1", r},
		{"PUT /mysql/users/:id", "gorm.update", "UPDATE", "1", upd},
		{"DELETE /mysql/users/:id", "gorm.delete", "DELETE", "1", del},
		{"GET after the delete", "gorm.query", "SELECT", "0", miss},
	} {
		t.Run(c.what, func(t *testing.T) {
			tid := traceIDOf(t, c.r)
			srv := serverSpan(t, p, tid)
			spans := spansNamed(traceSpans(t, p, tid), c.span)
			if len(spans) != 1 {
				t.Fatalf("the trace should hold one %s, got %s", c.span, spanNames(traceSpans(t, p, tid)))
			}
			s := spans[0]
			if s.ParentSpanID != srv.SpanID || s.Kind != "client" {
				t.Errorf("%s should be a child of the server span with kind=client, got parent=%s kind=%s", c.span, s.ParentSpanID, s.Kind)
			}
			host, port, _ := net.SplitHostPort(harness.MySQLAddr())
			for k, want := range map[string]string{
				"db.system.name": "mysql", "db.namespace": "xone_e2e", "server.address": host, "server.port": port,
				"db.operation.name": c.op, "db.rows_affected": c.rows,
			} {
				if got := s.Str(k); got != want {
					t.Errorf("%s's %s should be %q, got %q", c.span, k, want, got)
				}
			}
			// "No record found" is an ordinary business branch and is not marked as an error (endSpan in trace.go)
			if s.StatusCode == "Error" {
				t.Errorf("%s should not be marked as an error, got status=%s", c.span, s.StatusCode)
			}
			mustNotContain(t, "the attributes of the span "+c.span, fmt.Sprint(s.Attributes), name, email)
		})
	}

	t.Run("PG's span in the same process reports PG's connection information", func(t *testing.T) {
		pr := p.Get(t, "/users/999999999?cache=off")
		tid := traceIDOf(t, pr)
		serverSpan(t, p, tid)
		q := spansNamed(traceSpans(t, p, tid), "gorm.query")
		host, _, _ := net.SplitHostPort(harness.PGAddr())
		if len(q) != 1 || q[0].Str("db.system.name") != "postgresql" || q[0].Str("server.address") != host {
			t.Errorf("the default instance's gorm.query should report postgresql / %s, got %v", host, spanNames(q))
		}
	})
}

// xgorm/README.md, XGorm: "Metric: true # the connection pool metrics, taking per instance: an
// instance with Metric: false does not appear in /metrics".
// The metrics carry the instance name in a name label and every instance reports its own pool:
// with MaxOpenConns: 7 configured for mysql, e2e_db_pool_max_open{name="mysql"} is 7 while default
// stays at the default 50
func TestMySQL_PoolMetricsLabeledByInstance_MetricTogglePerInstance(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pool := []string{
		"e2e_db_pool_open", "e2e_db_pool_in_use", "e2e_db_pool_idle", "e2e_db_pool_max_open",
		"e2e_db_pool_wait_total", "e2e_db_pool_wait_duration_seconds_total",
		"e2e_db_pool_closed_max_idle_total", "e2e_db_pool_closed_max_lifetime_total",
	}

	t.Run("the two instances each report their own", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: mysqlOverlay("MaxOpenConns: 7")})
		mysqlCreate(t, p, "metric", "metric@example.com")
		m := p.Metrics(t)
		for _, n := range pool {
			for _, inst := range []string{"default", "mysql"} {
				if len(m.Find(n, "name", inst)) != 1 {
					t.Errorf("/metrics should hold %s{name=%q}", n, inst)
				}
			}
		}
		if got := m.Sum("e2e_db_pool_max_open", "name", "mysql"); got != 7 {
			t.Errorf("the mysql instance has MaxOpenConns: 7, so e2e_db_pool_max_open{name=\"mysql\"} should be 7, got %v", got)
		}
		if got := m.Sum("e2e_db_pool_max_open", "name", "default"); got != 50 {
			t.Errorf("MaxOpenConns was changed for the mysql instance alone, so default should stay at the default 50, got %v", got)
		}
		if got := m.Sum("e2e_db_pool_open", "name", "mysql"); got < 1 {
			t.Errorf("after SQL has run on MySQL, e2e_db_pool_open{name=\"mysql\"} should be at least 1, got %v", got)
		}
		t.Logf("numbers: open{default}=%v open{mysql}=%v", m.Sum("e2e_db_pool_open", "name", "default"), m.Sum("e2e_db_pool_open", "name", "mysql"))
	})

	t.Run("Metric: false on mysql keeps it out", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: mysqlOverlay("Metric: false")})
		mysqlCreate(t, p, "metric", "metric@example.com")
		m := p.Metrics(t)
		for _, n := range pool {
			if len(m.Find(n, "name", "mysql")) != 0 {
				t.Errorf("the mysql instance has Metric: false, so /metrics should not hold %s{name=\"mysql\"}", n)
			}
			if len(m.Find(n, "name", "default")) != 1 {
				t.Errorf("the default instance did not turn Metric off, so /metrics should still hold %s{name=\"default\"}", n)
			}
		}
	})
}

// The MySQL password appears in no output at all: docs/observability.md, "The framework's own
// logs", and xgorm/dsn.go, "the approach here is to never print the DSN". With the SQL log, debug,
// the request body log and the spans all on, it runs a round of create / read / update / delete
// and breaks MySQL in the middle (where an error is likeliest to carry the connection string out),
// and then checks stdout / stderr, the span file, /metrics and the response bodies
func TestMySQL_PasswordNeverInLogsSpansMetricsOrResponses(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := harness.MySQLPassword()
	my := harness.NewProxy(t, harness.MySQLAddr())
	dsn := harness.MySQLDSN(my.Addr())
	p := harness.Start(t, harness.Options{
		MySQLAddr: my.Addr(), Spans: true,
		Overlay: sqlLog("mysql") + debugLogs + payloadLogs,
	})
	var bodies []string
	keep := func(r harness.Response) harness.Response { bodies = append(bodies, string(r.Body)); return r }

	u, r := mysqlCreate(t, p, "pw-check", "pw@example.com")
	keep(r)
	keep(p.Get(t, fmt.Sprintf("/mysql/users/%d", u.ID)))
	keep(p.Do(t, http.MethodPut, fmt.Sprintf("/mysql/users/%d", u.ID), map[string]string{"name": "pw-check-2"}))
	keep(p.Get(t, "/mysql/users/999999999"))
	keep(p.Get(t, "/dep?target=mysql"))
	keep(p.Get(t, "/stuck?ms=1&mysql=1"))

	my.Cut()
	for _, path := range []string{fmt.Sprintf("/mysql/users/%d", u.ID), "/dep?target=mysql"} {
		if r := keep(p.Get(t, path)); r.Status < 500 {
			t.Errorf("%s should fail while MySQL is cut off, got %v", path, r)
		}
	}
	if r := keep(p.PostJSON(t, "/mysql/users", map[string]string{"name": "while-cut"})); r.Status != http.StatusInternalServerError {
		t.Errorf("POST /mysql/users should answer 500 while MySQL is cut off, got %v", r)
	}
	keep(p.Get(t, "/stuck?ms=1&mysql=1"))
	my.Restore()
	mysqlWaitDep(t, p, 10*time.Second)

	if resp, err := http.Get(p.URL("/metrics")); err == nil {
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		resp.Body.Close()
		mustNotContain(t, "/metrics", b.String(), pw)
	}
	if exit := p.Terminate(t, 20*time.Second); exit.Code != 0 {
		t.Errorf("it should exit with 0 after the SIGTERM, got %v", exit)
	}

	out := p.Output()
	spans, _ := os.ReadFile(p.SpanFile)
	mustNotContain(t, "the process's stdout / stderr", out, pw, dsn)
	mustNotContain(t, "the span file", string(spans), pw)
	mustNotContain(t, "the response bodies", strings.Join(bodies, "\n"), pw)
	// And the other way around: make sure this round really took the paths that record SQL and report errors, rather than running empty
	for _, msg := range []string{"SQL", "read mysql user failed", "create mysql user failed", "xgorm connected"} {
		if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == msg })) == 0 {
			t.Errorf("this round should have produced a %q log line and there is not one: the check does not cover enough", msg)
		}
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("this round should have left a connection refused in the output and there is not one: the check does not cover enough")
	}
	t.Logf("numbers: checked %d bytes of output, %d bytes of spans and %d response bodies", len(out), len(spans), len(bodies))
}

// xgorm/README.md, XGorm: "a timeout or another parameter already written in the DSN is not
// overridden by the configuration -- the value in the configuration is a default";
// "for MySQL it is injected into the DSN as timeout" and "ReadTimeout, the read timeout, matching
// readTimeout in the DSN. 3s by default".
//
// The read timeout: with the far end silent (SetDelay(time.Hour)), a query with no deadline on a
// pooled connection should fail at readTimeout.
// The connect timeout: with the host down (Blackhole), the connections stuck in the pool are used
// up first with short deadlines, and a new connection's SYN is never answered, so it should fail at
// timeout. Each of the three sources gets a process of its own:
//
//	the defaults           readTimeout=3s (MySQL.ReadTimeout), timeout=500ms (DialTimeout)
//	the configuration (mysql alone)  MySQL.ReadTimeout: 2s, DialTimeout: 800ms
//	written in the DSN     readTimeout=1s&timeout=1500ms, with the configuration above set as well, and the DSN winning
//
// The three sets are at least 300ms (faultSlack) apart from one another: any closer and a DSN value
// overridden by the configuration would still land inside the slack
func TestMySQL_ReadAndDialTimeoutFromDSN_ElseInstanceConfig(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	cfgOverlay := mysqlOverlay("DialTimeout: 800ms\nMySQL:\n  ReadTimeout: 2s")
	for _, c := range []struct {
		name          string
		dsnQuery      string
		overlay       string
		read, connect time.Duration
	}{
		{"the defaults", "", "", mysqlReadTimeout, mysqlDialTimeout},
		{"the configuration", "", cfgOverlay, 2 * time.Second, 800 * time.Millisecond},
		{"written in the DSN", "?readTimeout=1s&timeout=1500ms", cfgOverlay, time.Second, 1500 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			t.Run("the read timeout", func(t *testing.T) {
				t.Parallel()
				my := harness.NewProxy(t, harness.MySQLAddr())
				p := harness.Start(t, harness.Options{Overlay: c.overlay,
					Env: map[string]string{"E2E_MYSQL_DSN": harness.MySQLDSN(my.Addr()) + c.dsnQuery}})
				my.SetDelay(time.Hour)
				r := mysqlDep(p, "", c.read+10*time.Second)
				if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
					t.Fatalf("with the far end silent and no deadline, the query should fail at readTimeout (%v) and answer 503, got %v", c.read, r)
				}
				if r.Server < c.read-20*time.Millisecond || r.Server > c.read+faultSlack {
					t.Errorf("readTimeout should be %v (%s) and the query should fail then, got %v: %s", c.read, c.name, r.Server, r.Error)
				}
				t.Logf("numbers: %s: with the far end silent, a query with no deadline failed after %s (readTimeout=%v): %s", c.name, faultMS(r.Server), c.read, r.Error)
			})
			t.Run("the connect timeout", func(t *testing.T) {
				t.Parallel()
				my := harness.NewProxy(t, harness.MySQLAddr())
				p := harness.Start(t, harness.Options{Overlay: c.overlay,
					Env: map[string]string{"E2E_MYSQL_DSN": harness.MySQLDSN(my.Addr()) + c.dsnQuery}})
				my.Blackhole()
				// Use up the connections stuck in the pool with short deadlines first (they are stuck on a read, which the connect timeout does not govern):
				// the driver closes such a connection when ctx ends and does not return it to the pool
				for i := 0; ; i++ {
					if i == 10 {
						t.Fatal("10 queries with a short deadline never reached a new connection")
					}
					if r := mysqlDep(p, "100ms", 10*time.Second); strings.Contains(r.Error, "dial tcp") {
						break
					}
				}
				r := mysqlDep(p, "10s", 20*time.Second)
				if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable || !strings.Contains(r.Error, "dial tcp") {
					t.Fatalf("with the host down a new connection should fail (503, dial tcp ...), got %v", r)
				}
				if !strings.Contains(r.Error, my.Addr()) {
					t.Errorf("the error should hold the address it cannot reach, %s, got %q", my.Addr(), r.Error)
				}
				if r.Server < c.connect-20*time.Millisecond || r.Server > c.connect+faultSlack {
					t.Errorf("timeout should be %v (%s) and a new connection should fail then, got %v: %s", c.connect, c.name, r.Server, r.Error)
				}
				t.Logf("numbers: %s: with the host down a new connection failed after %s (timeout=%v): %s", c.name, faultMS(r.Server), c.connect, r.Error)
			})
		})
	}
}

// With a deadline from the caller, a query on MySQL returns at that moment (xgorm.CWithCtx: "take
// an instance and bind ctx to it, which is what carries the trace and the timeout downstream").
// A pooled connection (stuck on a read) and a new one (stuck on the SYN with the host down) both
// have to honor it
func TestMySQL_HungOrDown_QueryReturnsAtCallerDeadline(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const deadline = 200 * time.Millisecond
	for _, m := range faultSilentModes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			my := harness.NewProxy(t, harness.MySQLAddr())
			p := harness.Start(t, harness.Options{MySQLAddr: my.Addr()})
			m.inject(my)
			var took []time.Duration
			for i := range 6 {
				r := mysqlDep(p, deadline.String(), 10*time.Second)
				if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
					t.Fatalf("attempt %d: the caller gave %v and it should answer 503 at that moment, got %v", i+1, deadline, r)
				}
				if r.Server > deadline+faultSlack || r.Server < deadline-10*time.Millisecond {
					t.Errorf("attempt %d: the caller gave %v and the query should return at that moment, got %v: %s", i+1, deadline, r.Server, r.Error)
				}
				took = append(took, r.Server)
			}
			t.Logf("numbers: MySQL %s with the caller giving %v: the 6 attempts took %v in turn; %s", m.name, deadline, took, faultSummary(took))
		})
	}
}

// xgorm/README.md, "Common": the text of a server error holds the parameter value itself
// (measured: MySQL 8.0.46's 1366 is Incorrect integer value: '<value>' for column 'id', and PG 16's
// 22P02 is invalid input syntax for type bigint: "<value>").
// The error field of the SQL failed log line and the span's status, attributes and events hold the
// error code alone; the error handed back to the business is unchanged
func TestXGorm_ArgsInServerErrorKeptOutOfSQLLogAndSpan(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	p := harness.Start(t, harness.Options{Spans: true, Overlay: sqlLog("default", "mysql")})

	// And along the way: the connect log says which instance it is (the two instances connect to databases of the same name, which addr / db alone cannot tell apart)
	for _, name := range []string{"default", "mysql"} {
		if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgorm connected" && l.Str("name") == name })); n != 1 {
			t.Errorf("there should be one xgorm connected with name=%s, got %d", name, n)
		}
	}

	for _, c := range []struct {
		db, span, code string
	}{
		{"mysql", "gorm.raw", "1366"},
		{"pg", "gorm.raw", "22P02"},
	} {
		t.Run(c.db, func(t *testing.T) {
			value := "leak-" + harness.NewID()
			r := p.Do(t, http.MethodPost, "/bad-sql", map[string]string{"db": c.db, "value": value})
			if r.Status != http.StatusInternalServerError {
				t.Fatalf("this SQL should fail, got %v", r)
			}
			if m := r.Map(t); m["error_has_value"] != true {
				t.Errorf("the error handed back to the business has to be unchanged (its text holds the value), got %v", m)
			}
			tid := traceIDOf(t, r)
			accessLog(t, p, tid)
			logs := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "SQL failed" && l.Str("trace_id") == tid })
			if len(logs) != 1 {
				t.Fatalf("there should be 1 SQL failed line, got %d", len(logs))
			}
			mustNotContain(t, "the SQL failed log line", logs[0].Line, value)
			if logs[0].Str("error_code") != c.code || !strings.Contains(logs[0].Str("error"), c.code) {
				t.Errorf("the SQL failed log line should record the error code %s, got %s", c.code, logs[0].Line)
			}

			serverSpan(t, p, tid)
			spans := spansNamed(traceSpans(t, p, tid), c.span)
			if len(spans) != 1 {
				t.Fatalf("the trace should hold one %s, got %s", c.span, spanNames(traceSpans(t, p, tid)))
			}
			s := spans[0]
			if s.StatusCode != "Error" || s.Str("db.response.status_code") != c.code || s.Str("error.type") != c.code {
				t.Errorf("the span should be marked Error and carry the error code %s, got status=%s attrs=%v", c.code, s.StatusCode, s.Attributes)
			}
			mustNotContain(t, "Span "+c.span, fmt.Sprint(s.StatusDescription, s.Attributes, s.Events), value)
			t.Logf("numbers: %s's SQL failed error=%q", c.db, logs[0].Str("error"))
		})
	}
}
