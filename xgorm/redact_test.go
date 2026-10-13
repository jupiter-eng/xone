package xgorm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// rvSecret 一个 pgx 编码不了的参数类型：pgx 报错时用 %#v 把它整个写进错误
type rvSecret struct{ Token string }

// pgxEncodeError pgx 自己造出来的那条编码错误，和建连之后 Exec 报的是同一句
// （pgx v5.10.0 pgtype.go newEncodeError，外面再包一层 extended_query_builder.go 的 failed to encode args[0]）
func pgxEncodeError(t *testing.T) error {
	t.Helper()
	_, err := pgtype.NewMap().Encode(pgtype.TextOID, pgtype.TextFormatCode, rvSecret{Token: secret}, nil)
	if err == nil || !strings.Contains(err.Error(), secret) {
		t.Fatalf("前提：pgx 的编码错误里该带着参数值，got=%v", err)
	}
	return fmt.Errorf("failed to encode args[0]: %w", err)
}

func TestRedactedError_ClientErrors(t *testing.T) {
	opErr := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5432}, Err: syscall.ECONNREFUSED}
	for _, c := range []struct {
		name string
		err  error
		want string // 该记下的原文；空串表示该收掉
	}{
		{"pgx 编码参数失败", pgxEncodeError(t), ""},
		// database/sql 的 convertAssign 用 %q 写出读回来的值
		{"Scan 转换失败", fmt.Errorf(`sql: Scan error on column index 0, name "n": converting driver.Value type []uint8 (%q) to a int: invalid syntax`, secret), ""},
		// gorm 的 AddError 用 "%v; %w" 拼：后一个是安全的也不能把前一个的原文带出去
		{"gorm 拼起来的两个错误", fmt.Errorf("%v; %w", pgxEncodeError(t), gorm.ErrInvalidData), gorm.ErrInvalidData.Error()},
		{"网络错误", fmt.Errorf("failed to connect: %w", opErr), opErr.Error()},
		{"ctx 超时", fmt.Errorf("timeout: %w", context.DeadlineExceeded), context.DeadlineExceeded.Error()},
		{"ctx 取消", context.Canceled, context.Canceled.Error()},
		{"没查到记录", gorm.ErrRecordNotFound, gorm.ErrRecordNotFound.Error()},
		{"连接池已关", sql.ErrConnDone, sql.ErrConnDone.Error()},
		{"坏连接", driver.ErrBadConn, driver.ErrBadConn.Error()},
		{"MySQL 连接失效", mysqldriver.ErrInvalidConn, mysqldriver.ErrInvalidConn.Error()},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, d := range []Dialect{pgDialect(), mysqlDialect(), {Name: "custom"}} {
				text, code := d.redactedError(c.err)
				if code != "" {
					t.Errorf("%s：客户端的错没有错误码，got=%q", d.Name, code)
				}
				want := cmpOr(c.want, clientErrorOmitted)
				if text != want {
					t.Errorf("%s：want %q，got %q", d.Name, want, text)
				}
				if strings.Contains(text, secret) {
					t.Errorf("%s：参数值进了错误文本：%s", d.Name, text)
				}
			}
		})
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// 日志和 Span 两个出口各验一次：调用点上都得用收过的文本
func TestLogger_ClientErrorKeepsParamValuesOut(t *testing.T) {
	lines := capture(t)
	traceOnce(newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{}), pgxEncodeError(t), 0)
	got := lines()
	if len(got) != 1 || got[0]["msg"] != "SQL failed" {
		t.Fatalf("该记一条 SQL failed，got=%v", got)
	}
	if strings.Contains(fmt.Sprint(got[0]), secret) {
		t.Errorf("参数值进了日志：%v", got[0])
	}
	if got[0]["error"] != clientErrorOmitted {
		t.Errorf("error 该是 %q，got=%v", clientErrorOmitted, got[0]["error"])
	}
}

func TestSpan_ClientErrorKeepsParamValuesOut(t *testing.T) {
	for name, err := range map[string]error{
		"pgx 编码参数失败": pgxEncodeError(t),
		// 认得出后一个是 ErrInvalidData，exception.message 也只能记它，不能记整条链
		"gorm 拼起来的两个错误": fmt.Errorf("%v; %w", pgxEncodeError(t), gorm.ErrInvalidData),
	} {
		t.Run(name, func(t *testing.T) {
			spans := recording(t)
			db := stmtDB(context.Background())
			startSpan(ConnInfo{})("create")(db)
			db.Error = err
			endSpan(pgDialect())(db)

			s := spans()[0]
			if s.Status().Code != codes.Error {
				t.Errorf("出错应标成 Error，got=%v", s.Status())
			}
			if blob := spanText(s); strings.Contains(blob, secret) {
				t.Errorf("参数值进了 Span：%s", blob)
			}
		})
	}
}

// 真的 PostgreSQL：参数值编码不了时，日志和 Span 里都没有它。
// 只在 XONE_E2E=1 时跑，连接参数和 scripts/e2e.sh 的同名环境变量一致
func TestNew_PG_ClientEncodeErrorKeepsParamValuesOut(t *testing.T) {
	if os.Getenv("XONE_E2E") != "1" {
		t.Skip("needs a real PostgreSQL; set XONE_E2E=1 (see scripts/e2e.sh)")
	}
	env := func(k, def string) string { return cmpOr(os.Getenv(k), def) }
	cfg := pgCfg(fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		env("XONE_E2E_PG_USER", "xone"), env("XONE_E2E_PG_PASSWORD", "e2e-secret-pw"),
		env("XONE_E2E_PG_ADDR", "127.0.0.1:5432"), env("XONE_E2E_PG_DB", "xone_e2e")))
	cfg.Log, cfg.Trace, cfg.Metric = true, true, false
	spans := recording(t)
	db, closer, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	lines := capture(t)
	err = db.Exec("SELECT ?::text", rvSecret{Token: secret}).Error
	if err == nil || !strings.Contains(err.Error(), secret) {
		t.Fatalf("前提：返回给调用方的错误原样带着 pgx 的原文，got=%v", err)
	}
	var failed []map[string]any
	for _, l := range lines() {
		if l["msg"] == "SQL failed" {
			failed = append(failed, l)
		}
	}
	if len(failed) != 1 || strings.Contains(fmt.Sprint(failed[0]), secret) || failed[0]["error"] != clientErrorOmitted {
		t.Errorf("SQL failed 日志不该带参数值，got=%v", failed)
	}
	var found bool
	for _, s := range spans() {
		if s.Status().Code == codes.Error {
			found = true
			if blob := spanText(s); strings.Contains(blob, secret) {
				t.Errorf("参数值进了 Span：%s", blob)
			}
		}
	}
	if !found {
		t.Error("前提：该有一个出错的 Span")
	}
}

// spanText Span 的状态描述、属性和事件属性拼成一段，查有没有漏出去的值
func spanText(s sdktrace.ReadOnlySpan) string {
	blob := s.Status().Description
	for _, kv := range s.Attributes() {
		blob += " " + string(kv.Key) + "=" + kv.Value.Emit()
	}
	for _, e := range s.Events() {
		for _, kv := range e.Attributes {
			blob += " " + string(kv.Key) + "=" + kv.Value.Emit()
		}
	}
	return blob
}
