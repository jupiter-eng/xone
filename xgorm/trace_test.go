package xgorm

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// openLazy builds a *gorm.DB that does not connect to a database.
//
// sql.Open is lazy on its own, but the mysql dialect runs one SELECT VERSION() in Initialize to
// find out which features the server supports, so SkipInitializeWithVersion is also needed to really stay unconnected.
func openLazy(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		mysql.New(mysql.Config{DSN: "u:p@tcp(127.0.0.1:1)/app", SkipInitializeWithVersion: true}),
		&gorm.Config{DisableAutomaticPing: true},
	)
	if err != nil {
		t.Fatalf("creating the instance failed: %v", err)
	}
	t.Cleanup(func() { sqlDB, _ := db.DB(); sqlDB.Close() })
	return db
}

// recording installs a separate tracing setup and returns a function that collects the finished Spans.
func recording(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)),
	)
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old); tp.Shutdown(context.Background()) })

	return func() []sdktrace.ReadOnlySpan {
		spans := make([]sdktrace.ReadOnlySpan, 0, len(exp.GetSpans()))
		for _, s := range exp.GetSpans().Snapshots() {
			spans = append(spans, s)
		}
		return spans
	}
}

func stmtDB(ctx context.Context) *gorm.DB {
	return &gorm.DB{Statement: &gorm.Statement{Context: ctx}}
}

func TestInstallTracing_HooksAllSixOperations(t *testing.T) {
	// The official plugin would compile in the ClickHouse driver, so the callbacks are registered here instead;
	// that means making sure none is missing -- any operation that is missed becomes invisible in traces from then on.
	db := openLazy(t)
	if err := installTracing(db, ConnInfo{Driver: "mysql"}, mysqlDialect()); err != nil {
		t.Fatalf("registration failed: %v", err)
	}

	cb := db.Callback()
	for name, p := range map[string]interface{ Get(string) func(*gorm.DB) }{
		"create": cb.Create(), "query": cb.Query(), "update": cb.Update(),
		"delete": cb.Delete(), "row": cb.Row(), "raw": cb.Raw(),
	} {
		if p.Get("xgorm:trace:before") == nil {
			t.Errorf("%s has no before callback attached", name)
		}
		if p.Get("xgorm:trace:after") == nil {
			t.Errorf("%s has no after callback attached", name)
		}
	}
}

func TestSpan_IncludesConnInfoAndSQL(t *testing.T) {
	spans := recording(t)
	info := ConnInfo{Driver: "mysql", Addr: "h:3306", DB: "app"}

	db := stmtDB(context.Background())
	startSpan(info)("query")(db)
	db.Statement.SQL.WriteString("SELECT * FROM users WHERE id = ?")
	db.RowsAffected = 3
	endSpan(pgDialect())(db)

	got := spans()
	if len(got) != 1 {
		t.Fatalf("expected one Span, got=%d", len(got))
	}
	s := got[0]
	if s.Name() != "gorm.query" {
		t.Errorf("wrong Span name, got=%s", s.Name())
	}
	attrs := map[string]string{}
	for _, kv := range s.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	// Names from the OTel database semantic conventions v1.43.0; none of the old names may appear any more.
	for k, want := range map[string]string{
		"db.system.name": "mysql", "db.namespace": "app", "server.address": "h", "server.port": "3306",
		"db.query.text": "SELECT * FROM users WHERE id = ?", "db.operation.name": "SELECT",
	} {
		if attrs[k] != want {
			t.Errorf("%s must be %q, got=%v", k, want, attrs)
		}
	}
	for _, old := range []string{"db.system", "db.name", "db.statement", "db.operation"} {
		if _, ok := attrs[old]; ok {
			t.Errorf("the old attribute %s must no longer appear, got=%v", old, attrs)
		}
	}
	if attrs["db.rows_affected"] != "3" {
		t.Errorf("wrong row count, got=%v", attrs)
	}
}

func TestSpan_OmitsParamValues(t *testing.T) {
	// Parameters may hold phone numbers, national IDs or tokens; recording them in a trace ships them out along with the sampling.
	spans := recording(t)
	db := stmtDB(context.Background())
	startSpan(ConnInfo{})("query")(db)
	db.Statement.SQL.WriteString("SELECT * FROM users WHERE token = ?")
	db.Statement.Vars = []any{"hunter2"}
	endSpan(pgDialect())(db)

	for _, kv := range spans()[0].Attributes() {
		if strings.Contains(kv.Value.Emit(), "hunter2") {
			t.Errorf("parameter values must not reach the trace, attribute %s=%s", kv.Key, kv.Value.Emit())
		}
	}
}

func TestSpan_MarksErrorStatus(t *testing.T) {
	spans := recording(t)
	db := stmtDB(context.Background())
	startSpan(ConnInfo{})("query")(db)
	db.Error = fmt.Errorf("exec: %w", driver.ErrBadConn) // only a client error recognized as safe is recorded as an event
	endSpan(pgDialect())(db)

	s := spans()[0]
	if s.Status().Code != codes.Error {
		t.Errorf("a failure must be marked Error, got=%v", s.Status())
	}
	if len(s.Events()) == 0 {
		t.Error("the error must be recorded as a Span event")
	}
}

func TestSpan_ServerErrorRecordsCodeNotMessage(t *testing.T) {
	// Measured: MySQL 8.0 reports 1062 as Duplicate entry 'a@b.com' for key ..., and
	// PG 16 reports 22P02 as invalid input syntax for type integer: "notanint":
	// parameter values take another route into the trace. db.query.text deliberately records only placeholders, so they must not leak here either.
	for _, c := range []struct {
		name string
		d    Dialect
		err  error
		code string
	}{
		{"MySQL", mysqlDialect(), fmt.Errorf("wrapped: %w", &mysqldriver.MySQLError{Number: 1062, Message: "Duplicate entry '" + secret + "' for key 'u.email'"}), "1062"},
		{"PG", pgDialect(), &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type integer: "` + secret + `"`, Detail: "Key (email)=(" + secret + ")"}, "22P02"},
	} {
		t.Run(c.name, func(t *testing.T) {
			spans := recording(t)
			db := stmtDB(context.Background())
			startSpan(ConnInfo{})("create")(db)
			db.Error = c.err
			endSpan(c.d)(db)

			s := spans()[0]
			if s.Status().Code != codes.Error || !strings.Contains(s.Status().Description, c.code) {
				t.Errorf("a failure must be marked Error and carry the error code %s, got=%v", c.code, s.Status())
			}
			blob := s.Status().Description
			for _, kv := range s.Attributes() {
				blob += " " + string(kv.Key) + "=" + kv.Value.Emit()
			}
			for _, e := range s.Events() {
				for _, kv := range e.Attributes {
					blob += " " + string(kv.Key) + "=" + kv.Value.Emit()
				}
			}
			if strings.Contains(blob, secret) {
				t.Errorf("a parameter value from the server error text reached the Span: %s", blob)
			}
			attrs := map[string]string{}
			for _, kv := range s.Attributes() {
				attrs[string(kv.Key)] = kv.Value.Emit()
			}
			if attrs["db.response.status_code"] != c.code || attrs["error.type"] != c.code {
				t.Errorf("db.response.status_code / error.type must be %s, got=%v", c.code, attrs)
			}
		})
	}
}

func TestOperationName_FirstKeyword(t *testing.T) {
	for sql, want := range map[string]string{
		"SELECT * FROM t":           "SELECT",
		"  insert into t values(1)": "insert",
		"UPDATE\n t SET a=1":        "UPDATE",
		"DELETE":                    "DELETE",
		"/* hint */ SELECT 1":       "",
		"(SELECT 1)":                "",
		"SELECT(1)":                 "",
		"":                          "",
	} {
		if got := operationName(sql); got != want {
			t.Errorf("operationName(%q)=%q, want %q", sql, got, want)
		}
	}
}

func TestConnAttrs_SplitsAddrIntoHostAndPort(t *testing.T) {
	for _, c := range []struct {
		info ConnInfo
		want map[string]string
	}{
		{ConnInfo{Driver: "postgres", Addr: "[::1]:5432", DB: "d"},
			map[string]string{"db.system.name": "postgresql", "server.address": "::1", "server.port": "5432", "db.namespace": "d"}},
		{ConnInfo{Driver: "clickhouse", Addr: "h"},
			map[string]string{"db.system.name": "clickhouse", "server.address": "h"}},
		{ConnInfo{Driver: "stub"}, map[string]string{"db.system.name": "stub"}},
	} {
		got := map[string]string{}
		for _, kv := range connAttrs(c.info) {
			got[string(kv.Key)] = kv.Value.Emit()
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("connAttrs(%+v)=%v, want %v", c.info, got, c.want)
		}
	}
}

func TestSpan_RecordNotFoundIsNotError(t *testing.T) {
	// "not found" is a normal business branch; marking it as an error would fill the trace view with red.
	spans := recording(t)
	db := stmtDB(context.Background())
	startSpan(ConnInfo{})("query")(db)
	db.Error = gorm.ErrRecordNotFound
	endSpan(pgDialect())(db)

	if s := spans()[0]; s.Status().Code == codes.Error {
		t.Errorf("a missing record must not be marked as an error, got=%v", s.Status())
	}
}

func TestSpan_StatementNilDoesNotPanic(t *testing.T) {
	startSpan(ConnInfo{})("query")(&gorm.DB{})
	endSpan(pgDialect())(&gorm.DB{})
}

func TestLogConn_OmitsCredentials(t *testing.T) {
	// The connect log is the only place in this module that writes out connection information.
	lines := capture(t)
	c := DefaultClientConfig()
	c.Driver, c.DSN = DriverMySQL, "u:"+secret+"@tcp(h:3306)/app"
	_, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	logConn("", info, c)

	got := lines()
	if len(got) != 1 {
		t.Fatalf("expected one record, got=%v", got)
	}
	blob := strings.Join([]string{got[0]["msg"].(string), info.Addr, info.DB}, " ")
	for k, v := range got[0] {
		blob += k
		if s, ok := v.(string); ok {
			blob += s
		}
	}
	if strings.Contains(blob, secret) {
		t.Errorf("the password appeared in the connect log: %v", got[0])
	}
	if got[0]["addr"] != "h:3306" || got[0]["db"] != "app" {
		t.Errorf("address and database name must be written out, otherwise there is no way to tell what was connected to, got=%v", got[0])
	}
}

// benchSpan runs one query through both tracing callbacks, before and after.
func benchSpan(b *testing.B, tp trace.TracerProvider) {
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	b.Cleanup(func() { otel.SetTracerProvider(old) })

	before := startSpan(ConnInfo{Driver: "mysql", Addr: "h:3306", DB: "app"})("query")
	after := endSpan(mysqlDialect())
	db := stmtDB(context.Background())
	db.Statement.SQL.WriteString("SELECT * FROM users WHERE id = ?")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Statement.Context = context.Background()
		before(db)
		after(db)
	}
}

func BenchmarkSpan_OneQuery_NoTracing(b *testing.B) { benchSpan(b, noop.NewTracerProvider()) }

func BenchmarkSpan_OneQuery_NotSampled(b *testing.B) {
	benchSpan(b, sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample())))
}

func BenchmarkSpan_OneQuery_AlwaysSampled(b *testing.B) {
	benchSpan(b, sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample())))
}
