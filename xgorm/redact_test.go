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

// rvSecret is a parameter type pgx cannot encode: pgx writes it whole into the error with %#v
type rvSecret struct{ Token string }

// pgxEncodeError is the encoding error pgx produces itself, the same sentence an Exec reports
// after connecting (pgx v5.10.0 pgtype.go newEncodeError, wrapped once more by
// extended_query_builder.go's failed to encode args[0])
func pgxEncodeError(t *testing.T) error {
	t.Helper()
	_, err := pgtype.NewMap().Encode(pgtype.TextOID, pgtype.TextFormatCode, rvSecret{Token: secret}, nil)
	if err == nil || !strings.Contains(err.Error(), secret) {
		t.Fatalf("precondition: pgx's encoding error should carry the parameter value, got=%v", err)
	}
	return fmt.Errorf("failed to encode args[0]: %w", err)
}

func TestRedactedError_ClientErrors(t *testing.T) {
	opErr := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5432}, Err: syscall.ECONNREFUSED}
	for _, c := range []struct {
		name string
		err  error
		want string // the text that should be recorded; an empty string means it should be withheld
	}{
		{"pgx failed to encode a parameter", pgxEncodeError(t), ""},
		// database/sql's convertAssign writes the value it read back with %q
		{"Scan conversion failed", fmt.Errorf(`sql: Scan error on column index 0, name "n": converting driver.Value type []uint8 (%q) to a int: invalid syntax`, secret), ""},
		// gorm's AddError joins with "%v; %w": a safe second error must not carry the first one's text out
		{"two errors joined by gorm", fmt.Errorf("%v; %w", pgxEncodeError(t), gorm.ErrInvalidData), gorm.ErrInvalidData.Error()},
		{"a network error", fmt.Errorf("failed to connect: %w", opErr), opErr.Error()},
		{"ctx timeout", fmt.Errorf("timeout: %w", context.DeadlineExceeded), context.DeadlineExceeded.Error()},
		{"ctx cancelled", context.Canceled, context.Canceled.Error()},
		{"no record found", gorm.ErrRecordNotFound, gorm.ErrRecordNotFound.Error()},
		{"the pool is closed", sql.ErrConnDone, sql.ErrConnDone.Error()},
		{"a bad connection", driver.ErrBadConn, driver.ErrBadConn.Error()},
		{"an invalid MySQL connection", mysqldriver.ErrInvalidConn, mysqldriver.ErrInvalidConn.Error()},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, d := range []Dialect{pgDialect(), mysqlDialect(), {Name: "custom"}} {
				text, code := d.redactedError(c.err)
				if code != "" {
					t.Errorf("%s: a client-side error has no error code, got=%q", d.Name, code)
				}
				want := cmpOr(c.want, clientErrorOmitted)
				if text != want {
					t.Errorf("%s: want %q, got %q", d.Name, want, text)
				}
				if strings.Contains(text, secret) {
					t.Errorf("%s: a parameter value reached the error text: %s", d.Name, text)
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

// Both exits are checked, the log and the span: every call site has to use the withheld text
func TestLogger_ClientErrorKeepsParamValuesOut(t *testing.T) {
	lines := capture(t)
	traceOnce(newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{}), pgxEncodeError(t), 0)
	got := lines()
	if len(got) != 1 || got[0]["msg"] != "SQL failed" {
		t.Fatalf("one SQL failed line is expected, got=%v", got)
	}
	if strings.Contains(fmt.Sprint(got[0]), secret) {
		t.Errorf("a parameter value reached the log: %v", got[0])
	}
	if got[0]["error"] != clientErrorOmitted {
		t.Errorf("error should be %q, got=%v", clientErrorOmitted, got[0]["error"])
	}
}

func TestSpan_ClientErrorKeepsParamValuesOut(t *testing.T) {
	for name, err := range map[string]error{
		"pgx failed to encode a parameter": pgxEncodeError(t),
		// Recognizing ErrInvalidData as the second one means exception.message may record it alone,
		// never the whole chain
		"two errors joined by gorm": fmt.Errorf("%v; %w", pgxEncodeError(t), gorm.ErrInvalidData),
	} {
		t.Run(name, func(t *testing.T) {
			spans := recording(t)
			db := stmtDB(context.Background())
			startSpan(ConnInfo{})("create")(db)
			db.Error = err
			endSpan(pgDialect())(db)

			s := spans()[0]
			if s.Status().Code != codes.Error {
				t.Errorf("a failure should be marked as Error, got=%v", s.Status())
			}
			if blob := spanText(s); strings.Contains(blob, secret) {
				t.Errorf("a parameter value reached the span: %s", blob)
			}
		})
	}
}

// Against a real PostgreSQL: when a parameter cannot be encoded, neither the log nor the span
// holds it. It runs only with XONE_E2E=1, and the connection parameters match the environment
// variables of scripts/e2e.sh.
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
		t.Fatalf("precondition: the error returned to the caller carries pgx's text as it is, got=%v", err)
	}
	var failed []map[string]any
	for _, l := range lines() {
		if l["msg"] == "SQL failed" {
			failed = append(failed, l)
		}
	}
	if len(failed) != 1 || strings.Contains(fmt.Sprint(failed[0]), secret) || failed[0]["error"] != clientErrorOmitted {
		t.Errorf("the SQL failed line must not carry the parameter value, got=%v", failed)
	}
	var found bool
	for _, s := range spans() {
		if s.Status().Code == codes.Error {
			found = true
			if blob := spanText(s); strings.Contains(blob, secret) {
				t.Errorf("a parameter value reached the span: %s", blob)
			}
		}
	}
	if !found {
		t.Error("precondition: there should be one failing span")
	}
}

// spanText joins the span's status description, attributes and event attributes into one blob, to
// look for any value that leaked
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
