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

// openLazy 造一个不连库的 *gorm.DB
//
// sql.Open 本身是懒的，但 mysql 方言在 Initialize 里会查一次 SELECT VERSION()
// 来判断服务端支持哪些特性，所以还要 SkipInitializeWithVersion 才真的不建连。
func openLazy(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		mysql.New(mysql.Config{DSN: "u:p@tcp(127.0.0.1:1)/app", SkipInitializeWithVersion: true}),
		&gorm.Config{DisableAutomaticPing: true},
	)
	if err != nil {
		t.Fatalf("建实例失败：%v", err)
	}
	t.Cleanup(func() { sqlDB, _ := db.DB(); sqlDB.Close() })
	return db
}

// recording 装一套独立的链路设施，返回取已结束 Span 的函数
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
	// 官方插件会把 ClickHouse 驱动编进来，所以回调是自己注册的；
	// 那就得自己保证一种都没漏——漏了的那种操作从此在链路里是隐形的
	db := openLazy(t)
	if err := installTracing(db, ConnInfo{Driver: "mysql"}, mysqlDialect()); err != nil {
		t.Fatalf("注册失败：%v", err)
	}

	cb := db.Callback()
	for name, p := range map[string]interface{ Get(string) func(*gorm.DB) }{
		"create": cb.Create(), "query": cb.Query(), "update": cb.Update(),
		"delete": cb.Delete(), "row": cb.Row(), "raw": cb.Raw(),
	} {
		if p.Get("xgorm:trace:before") == nil {
			t.Errorf("%s 没挂上 before 回调", name)
		}
		if p.Get("xgorm:trace:after") == nil {
			t.Errorf("%s 没挂上 after 回调", name)
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
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	s := got[0]
	if s.Name() != "gorm.query" {
		t.Errorf("Span 名不对，got=%s", s.Name())
	}
	attrs := map[string]string{}
	for _, kv := range s.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	// OTel 数据库语义约定 v1.43.0 的名字；旧名字一个都不该再出现
	for k, want := range map[string]string{
		"db.system.name": "mysql", "db.namespace": "app", "server.address": "h", "server.port": "3306",
		"db.query.text": "SELECT * FROM users WHERE id = ?", "db.operation.name": "SELECT",
	} {
		if attrs[k] != want {
			t.Errorf("%s 应是 %q，got=%v", k, want, attrs)
		}
	}
	for _, old := range []string{"db.system", "db.name", "db.statement", "db.operation"} {
		if _, ok := attrs[old]; ok {
			t.Errorf("旧属性 %s 不该再出现，got=%v", old, attrs)
		}
	}
	if attrs["db.rows_affected"] != "3" {
		t.Errorf("行数不对，got=%v", attrs)
	}
}

func TestSpan_OmitsParamValues(t *testing.T) {
	// 参数里可能有手机号、身份证、令牌，记进链路就跟着采样一路送出去了
	spans := recording(t)
	db := stmtDB(context.Background())
	startSpan(ConnInfo{})("query")(db)
	db.Statement.SQL.WriteString("SELECT * FROM users WHERE token = ?")
	db.Statement.Vars = []any{"hunter2"}
	endSpan(pgDialect())(db)

	for _, kv := range spans()[0].Attributes() {
		if strings.Contains(kv.Value.Emit(), "hunter2") {
			t.Errorf("参数值不该进链路，属性 %s=%s", kv.Key, kv.Value.Emit())
		}
	}
}

func TestSpan_MarksErrorStatus(t *testing.T) {
	spans := recording(t)
	db := stmtDB(context.Background())
	startSpan(ConnInfo{})("query")(db)
	db.Error = fmt.Errorf("exec: %w", driver.ErrBadConn) // 认得出是安全的客户端错误才记成事件
	endSpan(pgDialect())(db)

	s := spans()[0]
	if s.Status().Code != codes.Error {
		t.Errorf("出错应标成 Error，got=%v", s.Status())
	}
	if len(s.Events()) == 0 {
		t.Error("应把错误记成 Span 事件")
	}
}

func TestSpan_ServerErrorRecordsCodeNotMessage(t *testing.T) {
	// 实测 MySQL 8.0 的 1062 原文是 Duplicate entry 'a@b.com' for key …，
	// PG 16 的 22P02 原文是 invalid input syntax for type integer: "notanint"：
	// 参数值换一条路进了链路。db.query.text 特意只记占位符，这里不能再漏出去
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
				t.Errorf("出错应标成 Error 并写上错误码 %s，got=%v", c.code, s.Status())
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
				t.Errorf("服务端错误原文里的参数值进了 Span：%s", blob)
			}
			attrs := map[string]string{}
			for _, kv := range s.Attributes() {
				attrs[string(kv.Key)] = kv.Value.Emit()
			}
			if attrs["db.response.status_code"] != c.code || attrs["error.type"] != c.code {
				t.Errorf("db.response.status_code / error.type 应是 %s，got=%v", c.code, attrs)
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
			t.Errorf("operationName(%q)=%q，want %q", sql, got, want)
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
			t.Errorf("connAttrs(%+v)=%v，want %v", c.info, got, c.want)
		}
	}
}

func TestSpan_RecordNotFoundIsNotError(t *testing.T) {
	// 「没查到」是正常的业务分支，标成错误会让链路里满屏红色
	spans := recording(t)
	db := stmtDB(context.Background())
	startSpan(ConnInfo{})("query")(db)
	db.Error = gorm.ErrRecordNotFound
	endSpan(pgDialect())(db)

	if s := spans()[0]; s.Status().Code == codes.Error {
		t.Errorf("没查到记录不该标成错误，got=%v", s.Status())
	}
}

func TestSpan_StatementNilDoesNotPanic(t *testing.T) {
	startSpan(ConnInfo{})("query")(&gorm.DB{})
	endSpan(pgDialect())(&gorm.DB{})
}

func TestLogConn_OmitsCredentials(t *testing.T) {
	// 建连日志是这个模块唯一会写出连接信息的地方
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
		t.Fatalf("应记一条，got=%v", got)
	}
	blob := strings.Join([]string{got[0]["msg"].(string), info.Addr, info.DB}, " ")
	for k, v := range got[0] {
		blob += k
		if s, ok := v.(string); ok {
			blob += s
		}
	}
	if strings.Contains(blob, secret) {
		t.Errorf("建连日志里出现了密码：%v", got[0])
	}
	if got[0]["addr"] != "h:3306" || got[0]["db"] != "app" {
		t.Errorf("该写出地址和库名，否则排查不了连的是谁，got=%v", got[0])
	}
}

// benchSpan 一条查询走一遍 before / after 两个链路回调
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
