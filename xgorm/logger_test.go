package xgorm

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// capture swaps the slog default logger for one writing into a buffer and returns a function that parses what was written.
func capture(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			m := map[string]any{}
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("log line is not JSON: %v, content=%q", err, line)
			}
			out = append(out, m)
		}
		return out
	}
}

func traceOnce(l logger.Interface, err error, elapsed time.Duration) {
	begin := time.Now().Add(-elapsed)
	l.Trace(context.Background(), begin, func() (string, int64) {
		return "SELECT * FROM users WHERE id = ?", 1
	}, err)
}

func TestLogger_SQLAsStructuredFields(t *testing.T) {
	// SQL carries quotes and newlines; folding it into the message text turns one log line into several and makes filtering by latency impossible.
	lines := capture(t)
	c := DefaultClientConfig()
	traceOnce(newGormLogger(c, pgDialect(), postgres.Dialector{}), nil, time.Millisecond)

	got := lines()
	if len(got) != 1 {
		t.Fatalf("expected one record, got=%v", got)
	}
	if got[0]["sql"] != "SELECT * FROM users WHERE id = ?" {
		t.Errorf("SQL must be its own field, got=%v", got[0])
	}
	if got[0]["elapsed_ms"] == nil || got[0]["rows_affected"] != float64(1) {
		t.Errorf("elapsed time and row count must be fields too, got=%v", got[0])
	}
}

func TestLogger_SlowQueryLogsWarn(t *testing.T) {
	lines := capture(t)
	c := DefaultClientConfig()
	c.SlowThreshold = 10 * time.Millisecond
	traceOnce(newGormLogger(c, pgDialect(), postgres.Dialector{}), nil, time.Second)

	got := lines()
	if len(got) != 1 || got[0]["level"] != "WARN" {
		t.Fatalf("crossing the threshold must be logged at warn, got=%v", got)
	}
	// Both are milliseconds: 1s elapsed against a 10ms threshold.
	if got[0]["threshold_ms"] != float64(10) {
		t.Errorf("the threshold in milliseconds must be included, otherwise it is unclear why this counts as slow, got=%v", got[0])
	}
	if ms, _ := got[0]["elapsed_ms"].(float64); ms < 1000 || ms > 2000 {
		t.Errorf("elapsed_ms must be milliseconds (about 1000), got=%v", got[0]["elapsed_ms"])
	}
}

func TestLogger_ErrorLogsError(t *testing.T) {
	lines := capture(t)
	// A client error is logged verbatim only when it is recognized as safe (see TestRedactedError_ClientErrors); a bad connection is one of those.
	traceOnce(newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{}), fmt.Errorf("exec: %w", driver.ErrBadConn), time.Millisecond)

	got := lines()
	if len(got) != 1 || got[0]["level"] != "ERROR" {
		t.Fatalf("a failure must be logged at error, got=%v", got)
	}
	if got[0]["error"] != driver.ErrBadConn.Error() {
		t.Errorf("the error must be included, got=%v", got[0])
	}
}

func TestLogger_ServerErrorLogsCodeNotMessage(t *testing.T) {
	// The server's error text carries parameter values out with it: measured, MySQL 8.0 reports 1062 as
	// Duplicate entry 'a@b.com' for key ..., and PG 16 reports 22P02 as
	// invalid input syntax for type integer: "notanint".
	// The SQL itself only records placeholders, so parameter values must not slip into the log through the error field.
	for _, c := range []struct {
		name string
		d    Dialect
		err  error
		code string
	}{
		{"MySQL", mysqlDialect(), &mysqldriver.MySQLError{Number: 1062, Message: "Duplicate entry '" + secret + "' for key 'u.email'"}, "1062"},
		{"PG", pgDialect(), fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type integer: "` + secret + `"`}), "22P02"},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := capture(t)
			traceOnce(newGormLogger(DefaultClientConfig(), c.d, postgres.Dialector{}), c.err, time.Millisecond)
			got := lines()
			if len(got) != 1 || got[0]["level"] != "ERROR" {
				t.Fatalf("a failure must be logged at error, got=%v", got)
			}
			if fmt.Sprint(got[0]) != strings.ReplaceAll(fmt.Sprint(got[0]), secret, "") {
				t.Errorf("a parameter value from the server error text reached the log: %v", got[0])
			}
			if got[0]["error_code"] != c.code || !strings.Contains(fmt.Sprint(got[0]["error"]), c.code) {
				t.Errorf("the error code %s must be recorded, got=%v", c.code, got[0])
			}
		})
	}
}

func TestLogger_RecordNotFoundCanBeNonError(t *testing.T) {
	// "not found" is usually a normal business branch; it is still recorded by default and ignored only when configured.
	for _, c := range []struct {
		ignore    bool
		wantLevel any
	}{
		{false, "ERROR"},
		{true, "INFO"}, // not treated as an error, falls back to a plain SQL log
	} {
		lines := capture(t)
		cfg := DefaultClientConfig()
		cfg.IgnoreNotFound = c.ignore
		traceOnce(newGormLogger(cfg, pgDialect(), postgres.Dialector{}), gorm.ErrRecordNotFound, time.Millisecond)

		got := lines()
		if len(got) != 1 || got[0]["level"] != c.wantLevel {
			t.Errorf("with IgnoreNotFound=%v the level must be %v, got=%v", c.ignore, c.wantLevel, got)
		}
	}
}

func TestLogger_OmitsRowsWhenUnknown(t *testing.T) {
	// GORM uses -1 for "row count unknown"; writing -1 would read as if there really were -1 rows.
	lines := capture(t)
	l := newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{})
	l.Trace(context.Background(), time.Now(), func() (string, int64) { return "SELECT 1", -1 }, nil)

	got := lines()
	if len(got) != 1 {
		t.Fatalf("expected one record, got=%v", got)
	}
	if _, has := got[0]["rows_affected"]; has {
		t.Errorf("the field must be omitted when the row count is unknown, got=%v", got[0])
	}
}

func TestLogger_SilentLogsNothing(t *testing.T) {
	lines := capture(t)
	l := newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{}).LogMode(logger.Silent)
	traceOnce(l, errors.New("boom"), time.Second)

	if got := lines(); len(got) != 0 {
		t.Errorf("Silent must produce no logs at all, got=%v", got)
	}
}

func TestLogger_LogModeReturnsCopy(t *testing.T) {
	// GORM's contract: LogMode returns a new instance and must not mutate the shared one.
	l := newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{})
	other := l.LogMode(logger.Silent).(*gormLogger)
	if l.level == logger.Silent {
		t.Error("the original instance must not be modified")
	}
	if other.level != logger.Silent {
		t.Error("the copy must carry the new level")
	}
}

func TestLogger_InfoWarnError(t *testing.T) {
	lines := capture(t)
	l := newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{})
	l.Info(context.Background(), "plain message")
	l.Warn(context.Background(), "warning %d", 1)
	l.Error(context.Background(), "error")

	got := lines()
	if len(got) != 3 {
		t.Fatalf("expected three records, got=%v", got)
	}
	if got[1]["msg"] != "warning 1" {
		t.Errorf("formatting only happens when there are arguments, got=%v", got[1])
	}
}

func TestMessage_NotFormatStringWithoutArgs(t *testing.T) {
	// GORM also passes plain text without arguments; a % in the message must not be treated as a verb.
	if got := message("100% hit", nil); got != "100% hit" {
		t.Errorf("without arguments it must be returned as is, got=%q", got)
	}
	if got := message("hit %d%%", []any{50}); got != "hit 50%" {
		t.Errorf("formatting only happens when there are arguments, got=%q", got)
	}
}

func TestLogger_SQLLogHasNoParamValuesAndMatchesSent(t *testing.T) {
	// GORM leaves parameters out of the SQL only when the Logger implements ParamsFilter
	// (v1.31.2 callbacks.go:142). Without it, with Log: true every
	// WHERE password = ? reaches the log carrying the real value.
	//
	// What is recorded must also be exactly what was sent to the database: when the PG dialect's Explain has no
	// parameters to substitute it leaves $1 as $1$, and the logged statement then matches neither the Span nor pg_stat_statements.
	const secret = "hunter2-in-a-where"
	for _, c := range []struct {
		name        string
		dialector   gorm.Dialector
		placeholder string
	}{
		{"mysql", mysql.New(mysql.Config{DSN: "u:p@tcp(127.0.0.1:1)/app", SkipInitializeWithVersion: true}), "password = ?"},
		{"postgres", postgres.New(postgres.Config{DSN: "postgres://u:p@127.0.0.1:1/app"}), "password = $1 AND name = $2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := capture(t)
			db, err := gorm.Open(c.dialector,
				&gorm.Config{DisableAutomaticPing: true, DryRun: true, Logger: newGormLogger(DefaultClientConfig(), pgDialect(), c.dialector)},
			)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { sqlDB, _ := db.DB(); sqlDB.Close() })

			var rows []struct{ ID int }
			tx := db.Table("users").Where("password = ?", secret)
			if c.name == "postgres" {
				tx = tx.Where("name = ?", secret)
			}
			tx = tx.Find(&rows)
			sent := tx.Statement.SQL.String() // DryRun keeps it: this is the statement that would go to the database

			got := lines()
			if len(got) == 0 {
				t.Fatal("precondition: this SQL must be recorded")
			}
			if !strings.Contains(sent, c.placeholder) {
				t.Fatalf("precondition: the sent statement must carry %q, got %q", c.placeholder, sent)
			}
			for _, l := range got {
				sql, _ := l["sql"].(string)
				if strings.Contains(sql, secret) {
					t.Errorf("a parameter value reached the log: %v", sql)
				}
				if sql != sent {
					t.Errorf("the logged SQL must be exactly what was sent:\nlogged %q\nsent   %q", sql, sent)
				}
			}
		})
	}
}

func TestLogger_ScanSQLLogHasNoParamValues(t *testing.T) {
	// During Scan, GORM swaps the Logger for logger.Recorder (v1.31.2 finisher_api.go:539), and
	// Recorder does not consult the instance Logger's ParamsFilter, only the process-wide logger.RecorderParamsFilter,
	// whose default substitutes parameters into the SQL. xgorm's init replaces it with withoutParams; what is checked here is that it took:
	// both ways of writing a Scan record the statement that was sent, with placeholders.
	const secret = "hunter2-in-a-scan"
	for _, c := range []struct {
		name      string
		dialector gorm.Dialector
	}{
		{"mysql", mysql.New(mysql.Config{DSN: "u:p@tcp(127.0.0.1:1)/app", SkipInitializeWithVersion: true})},
		{"postgres", postgres.New(postgres.Config{DSN: "postgres://u:p@127.0.0.1:1/app"})},
	} {
		db, err := gorm.Open(c.dialector,
			&gorm.Config{DisableAutomaticPing: true, DryRun: true, Logger: newGormLogger(DefaultClientConfig(), pgDialect(), c.dialector)},
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sqlDB, _ := db.DB(); sqlDB.Close() })

		for _, scan := range []struct {
			name string
			run  func(dest any) *gorm.DB
		}{
			{"Raw", func(dest any) *gorm.DB {
				return db.Raw("SELECT count(*) AS n FROM users WHERE password = ?", secret).Scan(dest)
			}},
			{"Select", func(dest any) *gorm.DB {
				return db.Table("users").Select("count(*) AS n").Where("password = ?", secret).Scan(dest)
			}},
		} {
			t.Run(c.name+"_"+scan.name, func(t *testing.T) {
				lines := capture(t)
				var dest struct{ N int }
				tx := scan.run(&dest)
				sent := tx.Statement.SQL.String()
				if sent == "" || strings.Contains(sent, secret) {
					t.Fatalf("precondition: the sent statement must carry placeholders, got %q", sent)
				}
				got := lines()
				if len(got) != 1 {
					t.Fatalf("Scan must produce 1 SQL log record, got %v", got)
				}
				if sql, _ := got[0]["sql"].(string); sql != sent {
					t.Errorf("the SQL recorded for Scan must be exactly what was sent:\nlogged %q\nsent   %q", sql, sent)
				}
			})
		}
	}
}

func TestLogger_PGPlaceholderRestoreIsExactInverseOfExplain(t *testing.T) {
	// A $1$ that was in the original statement (inside a string literal) must be restored as is;
	// it must not be a "replace every $N$ with $N" that happens to match the common statements.
	l := newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{})
	for _, sql := range []string{
		"SELECT * FROM users WHERE id = $1 LIMIT $2",
		"INSERT INTO t (a,b,c) VALUES ($1,$2,$10)",
		"SELECT '$1$' , $1",
		"SELECT '$1$$2' || $3",
		"SELECT $$ dollar $$, $tag$ body $tag$",
		"SELECT price FROM t WHERE note = '$5 off'",
	} {
		got, _ := l.statement(func() (string, int64) { return postgres.Dialector{}.Explain(sql), 0 })
		if got != sql {
			t.Errorf("the restored statement must equal the original:\noriginal %q\ngot      %q", sql, got)
		}
	}
	// Dialects with ? placeholders are left alone: a $1$ that happens to appear in the original must not be touched.
	my := mysql.New(mysql.Config{DSN: "u:p@tcp(127.0.0.1:1)/app"})
	m := newGormLogger(DefaultClientConfig(), pgDialect(), my)
	const raw = "SELECT '$1$' FROM t WHERE id = ?"
	if got, _ := m.statement(func() (string, int64) { return my.Explain(raw), 0 }); got != raw {
		t.Errorf("a MySQL statement must be recorded as is, got %q", got)
	}
}

// benchTrace runs an instance with Log on; GORM calls Trace once after every SQL statement.
func benchTrace(b *testing.B, level slog.Level) {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: level})))
	b.Cleanup(func() { slog.SetDefault(old) })

	l := newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{})
	ctx := context.Background()
	// Take the SQL the way GORM does: the PG dialect's Explain runs two regexp passes even with no parameters.
	fc := func() (string, int64) {
		return postgres.Dialector{}.Explain("SELECT * FROM users WHERE id = $1 LIMIT $2"), 1
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Trace(ctx, time.Now().Add(-time.Millisecond), fc, nil)
	}
}

func BenchmarkTrace_OneSQL(b *testing.B) { benchTrace(b, slog.LevelInfo) }

func BenchmarkTrace_SlogLevelAboveInfoDropsSQL(b *testing.B) { benchTrace(b, slog.LevelWarn) }
