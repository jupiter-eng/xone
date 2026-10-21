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

// Writing and querying go through the third instance: xgorm/clickhouse/README.md,
// "Configuration" -- a blank import of xgorm/clickhouse and Driver: clickhouse in the
// configuration, and "what the caller gets is still the native *gorm.DB, with the same settings
// and the same several-instances style".
// The data is checked against ClickHouse directly: every row of a batch written in one INSERT is
// there, a point lookup by primary key is right and so is the aggregate by name
func TestClickHouse_WritesAndReadsViaThirdInstance_DataLandsInClickHouse(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	p := chStart(t, harness.Options{})
	name := "ch-" + harness.NewID()

	ids, _ := chInsert(t, p, name, 10, 20, 30)
	got := chRows(t, p, ids...)
	if len(got) != 3 {
		t.Fatalf("one INSERT wrote 3 rows and ClickHouse read directly should hold 3, got %+v", got)
	}
	for i, e := range got {
		if want := (chEvent{ID: ids[i], Name: name, Value: int64(10 * (i + 1))}); e != want {
			t.Errorf("row %d should be %+v, got %+v", i+1, want, e)
		}
	}

	r := p.Get(t, fmt.Sprintf("/ch/events/%d", ids[1]))
	var e chEvent
	r.JSON(t, &e)
	if r.Status != http.StatusOK || e != got[1] {
		t.Errorf("GET /ch/events/%d should answer 200 with %+v, got %v", ids[1], got[1], r)
	}
	if r := p.Get(t, "/ch/events/1"); r.Status != http.StatusNotFound {
		t.Errorf("an id that does not exist should answer 404, got %v", r)
	}

	r = p.Get(t, "/ch/stats?name="+name)
	var s struct {
		Count uint64 `json:"count"`
		Sum   int64  `json:"sum"`
	}
	r.JSON(t, &s)
	if r.Status != http.StatusOK || s.Count != 3 || s.Sum != 60 {
		t.Errorf("the aggregate by name should be count=3 sum=60, got %v", r)
	}

	// All three instances are there: the PG and MySQL groups work as usual
	if u := createUser(t, p, "pg-side", "pg@example.com"); u.ID <= 0 {
		t.Errorf("the PG endpoints should work as usual")
	}
	mysqlCreate(t, p, "my-side", "my@example.com")
	if r := p.Get(t, "/dep?target=ch"); r.Status != http.StatusOK {
		t.Errorf("GET /dep?target=ch should answer 200, got %v", r)
	}

	// A process that did not activate the ch profile has no such instance: /ch/... answers 503 and does not panic
	plain := harness.Start(t, harness.Options{})
	if r := plain.Get(t, "/dep?target=ch"); r.Status != http.StatusServiceUnavailable || !strings.Contains(string(r.Body), "not configured") {
		t.Errorf("/dep?target=ch should answer 503 saying it is not configured when there is no ch instance, got %v", r)
	}
}

// The xgorm/clickhouse package documentation and xgorm/clickhouse/README.md, "Configuration": the
// SELECT version() the driver makes while initializing was moved into the connect probe, and "the
// version is still set on the Dialector, which the driver goes by for the column rename (< 20.4)
// and the column precision (< 21.11) an older version does not support".
// On 24.8: Dialector.Version is the server's version(), and both switches are false (a newer
// version supports all of it)
func TestClickHouse_ProbedVersionSetOnDialector_BothLegacyFlagsOffOn24_8(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	p := chStart(t, harness.Options{})
	r := p.Get(t, "/ch/version")
	var v struct {
		Dialector string `json:"dialector_version"`
		Server    string `json:"server_version"`
		NoRename  bool   `json:"dont_support_rename_column"`
		NoPrec    bool   `json:"dont_support_column_precision"`
	}
	r.JSON(t, &v)
	if r.Status != http.StatusOK || v.Server == "" {
		t.Fatalf("GET /ch/version should answer 200 with the server version, got %v", r)
	}
	if v.Dialector != v.Server {
		t.Errorf("the documentation says the version is still set on the Dialector: Dialector.Version should be the server's %q, got %q", v.Server, v.Dialector)
	}
	if !strings.HasPrefix(v.Server, "24.8.") {
		t.Logf("note: this case is written for ClickHouse 24.8 and it is connected to %s", v.Server)
	}
	if v.NoRename || v.NoPrec {
		t.Errorf("%s supports the column rename (>= 20.4) and the column precision (>= 21.11), so both switches should be false, got rename=%v precision=%v", v.Server, v.NoRename, v.NoPrec)
	}
	t.Logf("numbers: Dialector.Version=%s DontSupportRenameColumn=%v DontSupportColumnPrecision=%v", v.Dialector, v.NoRename, v.NoPrec)
}

// xgorm/README.md, XGorm.Log: "what is recorded is the SQL with its placeholders and no parameter
// values" and "the placeholders are the ones sent to the database" -- ? for ClickHouse; the
// statement in the log is the span's db.query.text. Log takes per instance: with it on for ch
// alone, not one statement of PG or MySQL should be recorded.
//
// The aggregate goes through GORM's Scan: see the "Scan" subtest
func TestClickHouse_SQLLogHasPlaceholdersNotArgs_MatchesSpanDbQueryText(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	p := chStart(t, harness.Options{Spans: true, Overlay: sqlLog("ch")})
	name := "sql-" + harness.NewID()

	sqlOf := func(t *testing.T, tid string) []harness.Log {
		t.Helper()
		accessLog(t, p, tid) // the access log is written after the handler returns, so by the time it is read this request's SQL log has arrived
		serverSpan(t, p, tid)
		return p.FindLogs(func(l harness.Log) bool { return l.Msg() == "SQL" && l.Str("trace_id") == tid })
	}
	check := func(t *testing.T, what, spanName, want string, r harness.Response, values ...string) {
		t.Helper()
		tid := traceIDOf(t, r)
		logs := sqlOf(t, tid)
		if len(logs) != 1 {
			t.Fatalf("%s should record 1 SQL log line, got %d", what, len(logs))
		}
		logged := logs[0].Str("sql")
		if logged != want {
			t.Errorf("%s's SQL log should be %q with its ? placeholders, got %q", what, want, logged)
		}
		mustNotContain(t, what+"'s SQL log", logs[0].Line, values...)
		spans := spansNamed(traceSpans(t, p, tid), spanName)
		if len(spans) != 1 {
			t.Fatalf("%s's trace should hold one %s, got %s", what, spanName, spanNames(traceSpans(t, p, tid)))
		}
		if sent := spans[0].Str("db.query.text"); sent != want {
			t.Errorf("%s: the span's db.query.text should be the same statement with its placeholders, %q, got %q", what, want, sent)
		}
		t.Logf("numbers: the SQL %s recorded: %s", what, logged)
	}

	// Two rows and one set of placeholders: gorm.io/driver/clickhouse v0.7.0's Create prepares one row and Appends the rows into one batch
	// (the native protocol's batch insert), and this is the statement that goes out.
	// The values have to be long enough: mustNotContain looks at the whole log line, and a 4-digit number collides with the timestamp (measured: 4343 inside 18.434395505).
	// An 11-digit number cannot be formed inside the nanoseconds (9 digits) or inside a hexadecimal trace_id / span_id
	ids, r := chInsert(t, p, name, 73914062851, 73914062852)
	check(t, "POST /ch/events", "gorm.create", "INSERT INTO `"+p.Table+"` (`name`,`value`,`id`) VALUES (?,?,?)", r,
		name, "73914062851", "73914062852", fmt.Sprint(ids[0]))
	check(t, "GET /ch/events/:id", "gorm.query", "SELECT * FROM `"+p.Table+"` WHERE id = ? LIMIT ?",
		p.Get(t, fmt.Sprintf("/ch/events/%d", ids[0])), fmt.Sprint(ids[0]))

	t.Run("PG and MySQL have no Log on", func(t *testing.T) {
		if logs := sqlOf(t, traceIDOf(t, p.Get(t, "/users/999999999?cache=off"))); len(logs) != 0 {
			t.Errorf("Log is on for the ch instance alone, so default's (PG's) SQL should not be recorded, got %d lines", len(logs))
		}
		if logs := sqlOf(t, traceIDOf(t, p.Get(t, "/mysql/users/999999999"))); len(logs) != 0 {
			t.Errorf("Log is on for the ch instance alone, so mysql's SQL should not be recorded, got %d lines", len(logs))
		}
	})

	// GORM's Scan (v1.31.2 finisher_api.go:537) swaps the Logger for a logger.Recorder while it runs
	// and hands the SQL the Recorder caught to our Logger afterwards. The Recorder does not go through
	// xgorm's ParamsFilter: its ParamsFilter is the package-level logger.RecorderParamsFilter alone
	// (which returns the parameters unchanged by default), so what arrives is SQL that has been
	// Explained, with the parameters already substituted. This used to be a KNOWN BUG, measured before the fix:
	//
	//	SELECT count() AS n, sum(value) AS s FROM `e2e_...` WHERE name = 'sql-3fa9c1d2b7e4'
	//
	// It is not peculiar to ClickHouse: on every xgorm instance, Raw(...).Scan(...) and
	// Table(...).Select(...).Scan(...) do the same (the service's MySQL SELECT SLEEP(?) was recorded as
	// SELECT SLEEP(1.5)). xgorm's init replaces logger.RecorderParamsFilter with one that hands out no
	// parameters (xgorm/README.md, "Common").
	// The spans are unaffected: db.query.text comes from Statement.SQL and still holds the placeholders
	t.Run("Scan", func(t *testing.T) {
		r := p.Get(t, "/ch/stats?name="+name)
		tid := traceIDOf(t, r)
		logs := sqlOf(t, tid)
		if len(logs) != 1 {
			t.Fatalf("GET /ch/stats should record 1 SQL log line, got %d", len(logs))
		}
		want := "SELECT count() AS n, sum(value) AS s FROM `" + p.Table + "` WHERE name = ?"
		spans := spansNamed(traceSpans(t, p, tid), "gorm.row")
		if len(spans) != 1 || spans[0].Str("db.query.text") != want {
			t.Errorf("the span's db.query.text should be %q, got %v", want, spans)
		}
		if logged := logs[0].Str("sql"); logged != want {
			t.Errorf("did GORM's Scan go around xgorm's ParamsFilter? the SQL log should be %q with its ? placeholders, got %q", want, logged)
		}
		mustNotContain(t, "GET /ch/stats's SQL log", logs[0].Line, name)
		t.Logf("numbers: the SQL Scan recorded: %s", logs[0].Str("sql"))
	})
}

// The span's attributes (xgorm/trace.go, the OTel database semantic conventions v1.43.0;
// xgorm/README.md, "Tracing"): db.system.name=clickhouse, db.namespace the database name,
// server.address / server.port resolved from the DSN and recorded separately, db.query.text with
// its placeholders, and db.operation.name the statement's first keyword. The parent is the server
// span.
// In one request the three instances each report their own connection information
func TestClickHouse_SQLSpanCarriesThisInstanceConnInfo(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	p := chStart(t, harness.Options{Spans: true})
	name := "span-" + harness.NewID()
	ids, ins := chInsert(t, p, name, 7)
	hit := p.Get(t, fmt.Sprintf("/ch/events/%d", ids[0]))
	miss := p.Get(t, "/ch/events/1")
	agg := p.Get(t, "/ch/stats?name="+name)
	host, port, _ := net.SplitHostPort(harness.CHAddr())

	for _, c := range []struct {
		what, span, op, query string
		r                     harness.Response
	}{
		{"POST /ch/events", "gorm.create", "INSERT", "INSERT INTO `" + p.Table + "` (`name`,`value`,`id`) VALUES (?,?,?)", ins},
		{"GET /ch/events/:id", "gorm.query", "SELECT", "SELECT * FROM `" + p.Table + "` WHERE id = ? LIMIT ?", hit},
		{"GET with an id that does not exist", "gorm.query", "SELECT", "SELECT * FROM `" + p.Table + "` WHERE id = ? LIMIT ?", miss},
		{"GET /ch/stats", "gorm.row", "SELECT", "SELECT count() AS n, sum(value) AS s FROM `" + p.Table + "` WHERE name = ?", agg},
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
			for k, want := range map[string]string{
				"db.system.name": "clickhouse", "db.namespace": "xone_e2e", "server.address": host, "server.port": port,
				"db.operation.name": c.op, "db.query.text": c.query,
			} {
				if got := s.Str(k); got != want {
					t.Errorf("%s's %s should be %q, got %q", c.span, k, want, got)
				}
			}
			if s.StatusCode == "Error" {
				t.Errorf("%s should not be marked as an error (a record not found included), got status=%s %s", c.span, s.StatusCode, s.StatusDescription)
			}
			mustNotContain(t, "the attributes of the span "+c.span, fmt.Sprint(s.Attributes), name, fmt.Sprint(ids[0]))
			// The ClickHouse driver always reports 0 rows for a write (clickhouse-go v2.48.0's stdDriver.ExecContext returns driver.RowsAffected(0)),
			// so db.rows_affected means nothing on this instance and is only recorded
			t.Logf("numbers: %s's db.rows_affected=%s", c.what, s.Str("db.rows_affected"))
		})
	}

	t.Run("PG's and MySQL's spans in the same process report their own connection information", func(t *testing.T) {
		for _, c := range []struct{ path, system, addr string }{
			{"/users/999999999?cache=off", "postgresql", harness.PGAddr()},
			{"/mysql/users/999999999", "mysql", harness.MySQLAddr()},
		} {
			tid := traceIDOf(t, p.Get(t, c.path))
			serverSpan(t, p, tid)
			q := spansNamed(traceSpans(t, p, tid), "gorm.query")
			h, _, _ := net.SplitHostPort(c.addr)
			if len(q) != 1 || q[0].Str("db.system.name") != c.system || q[0].Str("server.address") != h {
				t.Errorf("%s's gorm.query should report %s / %s, got %v", c.path, c.system, h, q)
			}
		}
	})
}

// xgorm/README.md, "Common": the text of a ClickHouse server error holds the parameter value as
// well -- measured on 24.8, a value that fails to cast to Int64 is code: 6, message: Cannot parse
// string '<value>' as Int64 ....
// Over the native protocol the driver returns a *clickhouse.Exception whose error code the dialect
// recognizes (errorCode in xgorm/clickhouse): the error field of SQL failed and the span's status
// and attributes hold the 6 alone; the error handed back to the business is unchanged
func TestClickHouse_ArgsInServerErrorKeptOutOfSQLLogAndSpan(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	p := chStart(t, harness.Options{Spans: true, Overlay: sqlLog("ch")})
	value := "leak-" + harness.NewID()
	r := p.Do(t, http.MethodPost, "/bad-sql", map[string]string{"db": "ch", "value": value})
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
	if logs[0].Str("error_code") != "6" || logs[0].Str("error") != "clickhouse error 6 (message omitted, it may contain parameter values)" {
		t.Errorf("the SQL failed log line should record the error code 6 alone, got %s", logs[0].Line)
	}
	serverSpan(t, p, tid)
	spans := spansNamed(traceSpans(t, p, tid), "gorm.raw")
	if len(spans) != 1 {
		t.Fatalf("the trace should hold one gorm.raw, got %s", spanNames(traceSpans(t, p, tid)))
	}
	s := spans[0]
	if s.StatusCode != "Error" || s.Str("db.response.status_code") != "6" || s.Str("error.type") != "6" {
		t.Errorf("the span should be marked Error and carry the error code 6, got status=%s attrs=%v", s.StatusCode, s.Attributes)
	}
	mustNotContain(t, "Span gorm.raw", fmt.Sprint(s.StatusDescription, s.Attributes, s.Events), value)
	t.Logf("numbers: SQL failed error=%q error_code=%s", logs[0].Str("error"), logs[0].Str("error_code"))
}

// xgorm/README.md: "Metric: true # the db_pool_* connection pool metrics, taking per instance: an
// instance with Metric: false does not appear in /metrics".
// The ch instance's pool is reported under name="ch": with MaxOpenConns: 7 it is 7, while the other
// instances stay at the default 50
func TestClickHouse_PoolMetricsLabeledByInstance_MetricTogglePerInstance(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	pool := []string{
		"e2e_db_pool_open", "e2e_db_pool_in_use", "e2e_db_pool_idle", "e2e_db_pool_max_open",
		"e2e_db_pool_wait_total", "e2e_db_pool_wait_duration_seconds_total",
		"e2e_db_pool_closed_max_idle_total", "e2e_db_pool_closed_max_lifetime_total",
	}
	t.Run("the three instances each report their own", func(t *testing.T) {
		t.Parallel()
		p := chStart(t, harness.Options{Overlay: chOverlay("MaxOpenConns: 7")})
		chInsert(t, p, "metric", 1)
		m := p.Metrics(t)
		for _, n := range pool {
			for _, inst := range []string{"default", "mysql", "ch"} {
				if len(m.Find(n, "name", inst)) != 1 {
					t.Errorf("/metrics should hold %s{name=%q}", n, inst)
				}
			}
		}
		if got := m.Sum("e2e_db_pool_max_open", "name", "ch"); got != 7 {
			t.Errorf("the ch instance has MaxOpenConns: 7, so e2e_db_pool_max_open{name=\"ch\"} should be 7, got %v", got)
		}
		if got := m.Sum("e2e_db_pool_max_open", "name", "mysql"); got != 50 {
			t.Errorf("MaxOpenConns was changed for the ch instance alone, so mysql should stay at the default 50, got %v", got)
		}
		if got := m.Sum("e2e_db_pool_open", "name", "ch"); got < 1 {
			t.Errorf("after SQL has run on ClickHouse, e2e_db_pool_open{name=\"ch\"} should be at least 1, got %v", got)
		}
	})
	t.Run("Metric: false on ch keeps it out", func(t *testing.T) {
		t.Parallel()
		p := chStart(t, harness.Options{Overlay: chOverlay("Metric: false")})
		chInsert(t, p, "metric", 1)
		m := p.Metrics(t)
		for _, n := range pool {
			if len(m.Find(n, "name", "ch")) != 0 {
				t.Errorf("the ch instance has Metric: false, so /metrics should not hold %s{name=\"ch\"}", n)
			}
			if len(m.Find(n, "name", "mysql")) != 1 {
				t.Errorf("the mysql instance did not turn Metric off, so /metrics should still hold %s{name=\"mysql\"}", n)
			}
		}
	})
}

// The ClickHouse password appears in no output at all: docs/observability.md, "The framework's own
// logs", and xgorm/clickhouse/README.md, "Configuration", "none of these errors echoes the DSN
// back". With the SQL log, debug, the request body log and the spans all on, it runs a round of
// writing, point lookups, aggregates and errors and breaks ClickHouse in the middle (where an error
// is likeliest to carry the connection string out), and then checks stdout / stderr, the span file,
// /metrics and the response bodies
func TestClickHouse_PasswordNeverInLogsSpansMetricsOrResponses(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	pw := harness.CHPassword()
	chp := harness.NewProxy(t, harness.CHAddr())
	dsn := harness.CHDSN(chp.Addr())
	p := chStart(t, harness.Options{
		CHAddr: chp.Addr(), Spans: true,
		Overlay: sqlLog("ch") + debugLogs + payloadLogs,
	})
	var bodies []string
	keep := func(r harness.Response) harness.Response { bodies = append(bodies, string(r.Body)); return r }

	ids, r := chInsert(t, p, "pw-check", 1, 2)
	keep(r)
	keep(p.Get(t, fmt.Sprintf("/ch/events/%d", ids[0])))
	keep(p.Get(t, "/ch/stats?name=pw-check"))
	keep(p.Get(t, "/ch/version"))
	keep(p.Do(t, http.MethodPost, "/bad-sql", map[string]string{"db": "ch", "value": "x"}))
	keep(p.Get(t, "/stuck?ms=1&ch=1"))

	chp.Cut()
	for _, path := range []string{fmt.Sprintf("/ch/events/%d", ids[0]), "/dep?target=ch", "/ch/stats?name=x"} {
		if r := keep(p.Get(t, path)); r.Status < 500 {
			t.Errorf("%s should fail while ClickHouse is cut off, got %v", path, r)
		}
	}
	if r := keep(p.PostJSON(t, "/ch/events", map[string]any{"name": "while-cut", "values": []int{1}})); r.Status != http.StatusServiceUnavailable {
		t.Errorf("POST /ch/events should answer 503 while ClickHouse is cut off, got %v", r)
	}
	keep(p.Get(t, "/stuck?ms=1&ch=1"))
	chp.Restore()
	chWaitDep(t, p, 10*time.Second)

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
	for _, msg := range []string{"SQL", "SQL failed", "read ch event failed", "insert ch events failed", "xgorm connected"} {
		if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == msg })) == 0 {
			t.Errorf("this round should have produced a %q log line and there is not one: the check does not cover enough", msg)
		}
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("this round should have left a connection refused in the output and there is not one: the check does not cover enough")
	}
	t.Logf("numbers: checked %d bytes of output, %d bytes of spans and %d response bodies", len(out), len(spans), len(bodies))
}

// The DSN rules of xgorm/clickhouse/README.md, "Configuration" (resolve in xgorm/clickhouse):
//
//   - it has to be a URL under one of the four schemes clickhouse:// tcp:// http:// https://, and
//     the start fails otherwise -- a bare host:port, a misspelled scheme and a leading space
//     included;
//   - at the start the DSN also goes through the driver's own parser (https://, for instance, has
//     to carry secure=true);
//   - none of these errors echoes the DSN back: the raw errors from the driver and from url.Parse
//     carry the whole DSN, the plaintext password with it.
//
// Each way of getting it wrong gets a process of its own: a non-zero exit, never having started
// listening, an error that says which rule it is, and no password and no DSN in the output;
// and not one connection should go out (the proxy can count)
func TestClickHouse_DSNTypoFailsStartup_NamesTheItem_NoDSNOrPasswordEcho(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	secret := "dsn-secret-" + harness.NewID()
	chp := harness.NewProxy(t, harness.CHAddr())
	addr := chp.Addr()
	notURL := "DSN must be a URL starting with clickhouse://, tcp://, http:// or https://"
	malformed := "failed to parse DSN, check the format of XGorm"
	for _, c := range []struct {
		name, dsn, want string
	}{
		{"a bare host:port", "xone:" + secret + "@" + addr + "/xone_e2e", notURL},
		{"a misspelled scheme", "clickhous://xone:" + secret + "@" + addr + "/xone_e2e", notURL},
		{"a leading space", " " + harness.CHDSNWith("clickhouse", addr, secret), notURL},
		{"https without secure", harness.CHDSNWith("https", addr, secret), malformed},
		{"http with secure", harness.CHDSNWith("http", addr, secret) + "?secure=true", malformed},
		{"a literal percent sign in the query string", harness.CHDSNWith("clickhouse", addr, "x") + "?password=" + secret + "%zz", "a literal % in a password must be written as %25"},
		{"a malformed http_proxy", harness.CHDSNWith("http", addr, secret) + "?http_proxy=" + "http%3A%2F%2Fu%3A" + secret + "%40%5B%3A%3A1", malformed},
		{"a malformed dial_timeout", harness.CHDSNWith("clickhouse", addr, secret) + "?dial_timeout=" + secret, malformed},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			exit, p := faultStartFails(t, harness.Options{ClickHouse: true, Env: map[string]string{"E2E_CH_DSN": c.dsn}})
			stderr := p.Stderr()
			faultMustContain(t, "stderr", stderr, "xgorm config failed", `instance "ch"`, c.want)
			mustNotContain(t, "the process's stdout / stderr", p.Output(), secret, strings.TrimSpace(c.dsn))
			if exit.Uptime > faultStartSlack {
				t.Errorf("a malformed DSN is a configuration error and should not try to connect, so it should exit within %v, got %v", faultStartSlack, exit.Uptime)
			}
			t.Logf("numbers: %s: it exited with %d after %v; the error: %s", c.name, exit.Code, exit.Uptime.Round(time.Millisecond), lastLine(stderr))
		})
	}
	t.Cleanup(func() {
		if n := chp.Accepted(); n != 0 {
			t.Errorf("a malformed DSN should send no connection at all, and the proxy saw %d", n)
		}
	})
}

// xgorm/clickhouse/README.md, "Configuration": "DialTimeout: 500ms # injected into the DSN as
// dial_timeout, and one already written in the DSN is not overridden".
// With the host down (Blackhole) a new connection's SYN is never answered and the dial fails at
// dial_timeout -- each of the three sources gets a process of its own:
//
//	the defaults         dial_timeout=500ms (DialTimeout's default, injected)
//	the configuration (ch alone)  DialTimeout: 1s
//	written in the DSN   dial_timeout=1500ms, with DialTimeout: 1s set in the configuration as well, and the DSN winning
//
// The connections in the pool are used up with short deadlines first: they are stuck on a read,
// which dial_timeout does not govern
func TestClickHouse_DialTimeoutFromDSN_ElseInstanceDialTimeout(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	for _, c := range []struct {
		name     string
		dsnQuery string
		overlay  string
		connect  time.Duration
	}{
		{"the defaults", "", "", chDialTimeout},
		{"the configuration", "", chOverlay("DialTimeout: 1s"), time.Second},
		{"written in the DSN", "?dial_timeout=1500ms", chOverlay("DialTimeout: 1s"), 1500 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			chp := harness.NewProxy(t, harness.CHAddr())
			p := chStart(t, harness.Options{Overlay: c.overlay, Env: map[string]string{"E2E_CH_DSN": harness.CHDSN(chp.Addr()) + c.dsnQuery}})
			chp.Blackhole()
			for i := 0; ; i++ {
				if i == 10 {
					t.Fatal("10 queries with a short deadline never reached a new connection")
				}
				if r := chDep(p, "100ms", 10*time.Second); strings.Contains(r.Error, "dial tcp") {
					break
				}
			}
			r := chDep(p, "", 20*time.Second)
			if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable || !strings.Contains(r.Error, "dial tcp "+chp.Addr()) {
				t.Fatalf("with the host down a new connection should fail (503, dial tcp %s ...), got %v", chp.Addr(), r)
			}
			if r.Server < c.connect-20*time.Millisecond || r.Server > c.connect+faultSlack {
				t.Errorf("dial_timeout should be %v (%s) and a new connection should fail then, got %v: %s", c.connect, c.name, r.Server, r.Error)
			}
			t.Logf("numbers: %s: with the host down a new connection failed after %s (dial_timeout=%v): %s", c.name, faultMS(r.Server), c.connect, r.Error)
		})
	}
}

// A multi-host DSN (clickhouse://u:p@h1:9000,h2:9000/db): the driver splits it on the comma into
// several addresses and tries them in turn (connection_open_strategy is in_order by default),
// moving to the next one when the first cannot be reached. xgorm/README.md, "Tracing":
// server.address / server.port are "the host and the port resolved from the DSN, recorded
// separately"; addr in the connect log is "host:port". As with PostgreSQL's multiple hosts, the
// first one is recorded.
//
// This used to be a bug: resolve took the URL's whole Host ("h1:9000,h2:9000") for the address,
// net.SplitHostPort could not take it apart, server.address in the span was the whole string with
// no server.port, and addr in the connect log and in the errors was the whole string too
func TestClickHouse_MultiHostDSN_FirstThenNext_SpanAndLogRecordFirstHost(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	first, second := harness.NewProxy(t, harness.CHAddr()), harness.NewProxy(t, harness.CHAddr())
	dsn := strings.Replace(harness.CHDSN(first.Addr()), first.Addr(), first.Addr()+","+second.Addr(), 1)
	p := chStart(t, harness.Options{Spans: true, Env: map[string]string{"E2E_CH_DSN": dsn}})

	ls := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgorm connected" && l.Str("name") == "ch" })
	if len(ls) != 1 || ls[0].Str("addr") != first.Addr() {
		t.Errorf("addr in the connect log should be the first host, %s, got %v", first.Addr(), ls)
	}
	ids, r := chInsert(t, p, "multi", 1)
	tid := traceIDOf(t, r)
	serverSpan(t, p, tid)
	host, port, _ := net.SplitHostPort(first.Addr())
	if s := spansNamed(traceSpans(t, p, tid), "gorm.create"); len(s) != 1 || s[0].Str("server.address") != host || s[0].Str("server.port") != port {
		t.Errorf("the span's server.address / server.port should be the first host's %s / %s, got %v", host, port, s)
	}
	if first.Accepted() == 0 || second.Accepted() != 0 {
		t.Errorf("in_order: it should connect to the first host alone, and the first saw %d connections and the second %d", first.Accepted(), second.Accepted())
	}

	first.Cut()
	if r := p.Get(t, fmt.Sprintf("/ch/events/%d", ids[0])); r.Status != http.StatusOK {
		t.Errorf("with the first host gone the driver should move to the second and the query should still answer 200, got %v", r)
	}
	if second.Accepted() == 0 {
		t.Errorf("it should connect to the second host once the first is gone, and the second saw no connection at all")
	}
	t.Logf("numbers: with the first host gone, the second saw %d connections", second.Accepted())
}
