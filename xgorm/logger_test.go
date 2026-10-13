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

// capture 把 slog 默认 logger 换成写进 buffer 的，返回取解析结果的函数
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
				t.Fatalf("日志不是 JSON：%v，内容=%q", err, line)
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
	// SQL 里带引号和换行，塞进消息文本会把一行日志撑成好几行，也没法按耗时筛
	lines := capture(t)
	c := DefaultClientConfig()
	traceOnce(newGormLogger(c, pgDialect(), postgres.Dialector{}), nil, time.Millisecond)

	got := lines()
	if len(got) != 1 {
		t.Fatalf("应记一条，got=%v", got)
	}
	if got[0]["sql"] != "SELECT * FROM users WHERE id = ?" {
		t.Errorf("SQL 应是独立字段，got=%v", got[0])
	}
	if got[0]["elapsed_ms"] == nil || got[0]["rows_affected"] != float64(1) {
		t.Errorf("耗时和行数也该是字段，got=%v", got[0])
	}
}

func TestLogger_SlowQueryLogsWarn(t *testing.T) {
	lines := capture(t)
	c := DefaultClientConfig()
	c.SlowThreshold = 10 * time.Millisecond
	traceOnce(newGormLogger(c, pgDialect(), postgres.Dialector{}), nil, time.Second)

	got := lines()
	if len(got) != 1 || got[0]["level"] != "WARN" {
		t.Fatalf("超过阈值应记 warn，got=%v", got)
	}
	// 两个都是毫秒：慢了 1s、阈值 10ms
	if got[0]["threshold_ms"] != float64(10) {
		t.Errorf("该带上毫秒的阈值，否则看不出为什么算慢，got=%v", got[0])
	}
	if ms, _ := got[0]["elapsed_ms"].(float64); ms < 1000 || ms > 2000 {
		t.Errorf("elapsed_ms 该是毫秒（约 1000），got=%v", got[0]["elapsed_ms"])
	}
}

func TestLogger_ErrorLogsError(t *testing.T) {
	lines := capture(t)
	// 客户端的错只有认得出是安全的才记原文（见 TestRedactedError_ClientErrors），坏连接是其中之一
	traceOnce(newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{}), fmt.Errorf("exec: %w", driver.ErrBadConn), time.Millisecond)

	got := lines()
	if len(got) != 1 || got[0]["level"] != "ERROR" {
		t.Fatalf("出错应记 error，got=%v", got)
	}
	if got[0]["error"] != driver.ErrBadConn.Error() {
		t.Errorf("该带上错误，got=%v", got[0])
	}
}

func TestLogger_ServerErrorLogsCodeNotMessage(t *testing.T) {
	// 服务端的错误原文会把参数值带出来：实测 MySQL 8.0 的 1062 是
	// Duplicate entry 'a@b.com' for key …，PG 16 的 22P02 是
	// invalid input syntax for type integer: "notanint"。
	// SQL 本身只记占位符，参数值不能从 error 字段绕进日志
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
				t.Fatalf("出错应记 error，got=%v", got)
			}
			if fmt.Sprint(got[0]) != strings.ReplaceAll(fmt.Sprint(got[0]), secret, "") {
				t.Errorf("服务端错误原文里的参数值进了日志：%v", got[0])
			}
			if got[0]["error_code"] != c.code || !strings.Contains(fmt.Sprint(got[0]["error"]), c.code) {
				t.Errorf("该记下错误码 %s，got=%v", c.code, got[0])
			}
		})
	}
}

func TestLogger_RecordNotFoundCanBeNonError(t *testing.T) {
	// 「没查到」通常是正常的业务分支，默认还是记下来，配了才忽略
	for _, c := range []struct {
		ignore    bool
		wantLevel any
	}{
		{false, "ERROR"},
		{true, "INFO"}, // 不当错误，退回普通 SQL 日志
	} {
		lines := capture(t)
		cfg := DefaultClientConfig()
		cfg.IgnoreNotFound = c.ignore
		traceOnce(newGormLogger(cfg, pgDialect(), postgres.Dialector{}), gorm.ErrRecordNotFound, time.Millisecond)

		got := lines()
		if len(got) != 1 || got[0]["level"] != c.wantLevel {
			t.Errorf("IgnoreNotFound=%v 时应记 %v，got=%v", c.ignore, c.wantLevel, got)
		}
	}
}

func TestLogger_OmitsRowsWhenUnknown(t *testing.T) {
	// GORM 用 -1 表示「行数未知」，写成 -1 会被误读成真有 -1 行
	lines := capture(t)
	l := newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{})
	l.Trace(context.Background(), time.Now(), func() (string, int64) { return "SELECT 1", -1 }, nil)

	got := lines()
	if len(got) != 1 {
		t.Fatalf("应记一条，got=%v", got)
	}
	if _, has := got[0]["rows_affected"]; has {
		t.Errorf("行数未知时不该写这个字段，got=%v", got[0])
	}
}

func TestLogger_SilentLogsNothing(t *testing.T) {
	lines := capture(t)
	l := newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{}).LogMode(logger.Silent)
	traceOnce(l, errors.New("出错了"), time.Second)

	if got := lines(); len(got) != 0 {
		t.Errorf("Silent 时不该有任何日志，got=%v", got)
	}
}

func TestLogger_LogModeReturnsCopy(t *testing.T) {
	// GORM 的约定：LogMode 返回新实例，不能改共享的那个
	l := newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{})
	other := l.LogMode(logger.Silent).(*gormLogger)
	if l.level == logger.Silent {
		t.Error("不该改动原实例")
	}
	if other.level != logger.Silent {
		t.Error("副本应带上新级别")
	}
}

func TestLogger_InfoWarnError(t *testing.T) {
	lines := capture(t)
	l := newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{})
	l.Info(context.Background(), "普通消息")
	l.Warn(context.Background(), "警告 %d", 1)
	l.Error(context.Background(), "error")

	got := lines()
	if len(got) != 3 {
		t.Fatalf("应记三条，got=%v", got)
	}
	if got[1]["msg"] != "警告 1" {
		t.Errorf("有参数时才格式化，got=%v", got[1])
	}
}

func TestMessage_NotFormatStringWithoutArgs(t *testing.T) {
	// GORM 也会传不带参数的纯文本，消息里的 % 不该被当成占位符
	if got := message("100% 命中", nil); got != "100% 命中" {
		t.Errorf("没有参数时应原样返回，got=%q", got)
	}
	if got := message("命中 %d%%", []any{50}); got != "命中 50%" {
		t.Errorf("有参数时才格式化，got=%q", got)
	}
}

func TestLogger_SQLLogHasNoParamValuesAndMatchesSent(t *testing.T) {
	// GORM 只在 Logger 实现了 ParamsFilter 时才不把参数代进 SQL
	// （v1.31.2 callbacks.go:142）。没实现的话 Log: true 时每条
	// WHERE password = ? 都带着真实的值进了日志。
	//
	// 记下的也必须就是发给数据库的那条：PG 方言的 Explain 没有参数可代时
	// 把 $1 留成 $1$，日志里的语句和 Span、和 pg_stat_statements 都对不上
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
			sent := tx.Statement.SQL.String() // DryRun 留着它：这就是会发给数据库的那条

			got := lines()
			if len(got) == 0 {
				t.Fatal("前提：该记下这条 SQL")
			}
			if !strings.Contains(sent, c.placeholder) {
				t.Fatalf("前提：发出去的语句该带 %q，实际 %q", c.placeholder, sent)
			}
			for _, l := range got {
				sql, _ := l["sql"].(string)
				if strings.Contains(sql, secret) {
					t.Errorf("参数值进了日志：%v", sql)
				}
				if sql != sent {
					t.Errorf("日志里的 SQL 应就是发出去的那条：\n日志 %q\n发出 %q", sql, sent)
				}
			}
		})
	}
}

func TestLogger_ScanSQLLogHasNoParamValues(t *testing.T) {
	// Scan 执行期间 GORM 把 Logger 换成 logger.Recorder（v1.31.2 finisher_api.go:539），
	// Recorder 不问实例 Logger 的 ParamsFilter，只认进程级的 logger.RecorderParamsFilter，
	// 它的默认值把参数代进 SQL。xgorm 的 init 把它换成 withoutParams，这里验的是换上了：
	// 两条 Scan 的写法，记下的都是发出去的那条带占位符的语句
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
					t.Fatalf("前提：发出去的语句该带占位符，实际 %q", sent)
				}
				got := lines()
				if len(got) != 1 {
					t.Fatalf("Scan 应记 1 条 SQL 日志，实际 %v", got)
				}
				if sql, _ := got[0]["sql"].(string); sql != sent {
					t.Errorf("Scan 记下的 SQL 应就是发出去的那条：\n日志 %q\n发出 %q", sql, sent)
				}
			})
		}
	}
}

func TestLogger_PGPlaceholderRestoreIsExactInverseOfExplain(t *testing.T) {
	// 原文里本来就有 $1$ 这种写法（字符串字面量里）也要原样还原，
	// 不能只是「把 $N$ 都换成 $N」碰巧对上了常见的语句
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
			t.Errorf("还原后应是原文：\n原文 %q\n得到 %q", sql, got)
		}
	}
	// ? 占位符的方言不动：原文里恰好有 $1$ 也不能被改掉
	my := mysql.New(mysql.Config{DSN: "u:p@tcp(127.0.0.1:1)/app"})
	m := newGormLogger(DefaultClientConfig(), pgDialect(), my)
	const raw = "SELECT '$1$' FROM t WHERE id = ?"
	if got, _ := m.statement(func() (string, int64) { return my.Explain(raw), 0 }); got != raw {
		t.Errorf("MySQL 的语句应原样记，得到 %q", got)
	}
}

// benchTrace 开了 Log 的实例，每条 SQL 执行完 GORM 都调一次 Trace
func benchTrace(b *testing.B, level slog.Level) {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: level})))
	b.Cleanup(func() { slog.SetDefault(old) })

	l := newGormLogger(DefaultClientConfig(), pgDialect(), postgres.Dialector{})
	ctx := context.Background()
	// 照 GORM 的样子取 SQL：PG 方言的 Explain 即使没有参数也要过两遍正则
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
