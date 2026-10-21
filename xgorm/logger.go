package xgorm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// gormLogger routes GORM's logging into the standard library's slog.
//
// The fields are structured rather than formatted into the message: SQL carries quotes and
// newlines, and putting it into the message text turns one log line into several and leaves no way
// to filter by duration or by error.
type gormLogger struct {
	slowThreshold  time.Duration
	ignoreNotFound bool
	level          logger.LogLevel

	// numbered: the Explain of that dialect rewrites $N into $N$, which has to be undone before
	// recording, see statement
	numbered bool

	// dialect is used to recognize server error codes, see Dialect.ErrorCode
	dialect Dialect
}

// newGormLogger takes d, this instance's Dialector, only to tell what its placeholders look like.
func newGormLogger(c ClientConfig, dialect Dialect, d gorm.Dialector) *gormLogger {
	return &gormLogger{
		slowThreshold:  c.SlowThreshold,
		ignoreNotFound: c.IgnoreNotFound,
		level:          logger.Info,
		numbered:       d.Explain("$1") == "$1$",
		dialect:        dialect,
	}
}

// LogMode returns a copy and does not modify the shared instance (GORM's contract).
func (l *gormLogger) LogMode(level logger.LogLevel) logger.Interface {
	cp := *l
	cp.level = level
	return &cp
}

func (l *gormLogger) Info(ctx context.Context, msg string, args ...any) {
	if l.level >= logger.Info {
		slog.InfoContext(ctx, message(msg, args))
	}
}

func (l *gormLogger) Warn(ctx context.Context, msg string, args ...any) {
	if l.level >= logger.Warn {
		slog.WarnContext(ctx, message(msg, args))
	}
}

func (l *gormLogger) Error(ctx context.Context, msg string, args ...any) {
	if l.level >= logger.Error {
		slog.ErrorContext(ctx, message(msg, args))
	}
}

// Trace is called after every SQL statement.
func (l *gormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.level <= logger.Silent {
		return
	}
	elapsed := time.Since(begin)

	switch {
	case err != nil && l.level >= logger.Error && !l.skipErr(err):
		sql, rows := l.statement(fc)
		slog.ErrorContext(ctx, "SQL failed", attrs(sql, rows, elapsed, l.errorAttrs(err)...)...)

	case l.slowThreshold > 0 && elapsed > l.slowThreshold && l.level >= logger.Warn:
		sql, rows := l.statement(fc)
		slog.WarnContext(ctx, "slow SQL", attrs(sql, rows, elapsed, "threshold_ms", ms(l.slowThreshold))...)

		// Every SQL statement comes through here. Stop as soon as slog refuses info: fc rebuilds the
		// SQL (the PG dialect runs two regexes over it even without parameters), and building it only
		// for slog to drop it is wasted work.
	case l.level >= logger.Info && slog.Default().Enabled(ctx, slog.LevelInfo):
		sql, rows := l.statement(fc)
		slog.InfoContext(ctx, "SQL", attrs(sql, rows, elapsed)...)
	}
}

// ParamsFilter makes GORM hand Trace the SQL with its placeholders rather than interpolating the
// parameter values.
//
// GORM only calls it when the Logger implements this interface (v1.31.2 callbacks.go:142);
// otherwise it interpolates Statement.Vars into the SQL one by one before handing it over:
// measured with Log: true, WHERE password = ? was recorded as password = '<the real value>'.
// The parameters may hold personal information or credentials, which is the same principle that
// keeps trace.go from recording Statement.Vars.
func (l *gormLogger) ParamsFilter(ctx context.Context, sql string, params ...any) (string, []any) {
	return withoutParams(ctx, sql, params...)
}

// withoutParams keeps the SQL with its placeholders and hands out no parameter at all.
//
// Beyond gormLogger.ParamsFilter it is also the process-wide logger.RecorderParamsFilter (see the
// init in xgorm.go): during GORM's Scan the instance's Logger is swapped for logger.Recorder
// (v1.31.2 finisher_api.go:539), and afterwards the SQL the Recorder captured is handed to the
// original Logger. The Recorder never asks our ParamsFilter and honors only that package-level
// variable, whose default returns the parameters as they are — so Raw(...).Scan(...) and
// Table(...).Select(...).Scan(...) used to hand over SQL with the parameters already interpolated,
// on every driver. Measured (ClickHouse 24.8, before the fix): WHERE name = ? was recorded as
// WHERE name = 'sql-3fa9c1d2b7e4'.
func withoutParams(_ context.Context, sql string, _ ...any) (string, []any) {
	return sql, nil
}

// explainedNumbered is the marker the PG dialect's Explain leaves behind when there is no
// parameter to interpolate.
var explainedNumbered = regexp.MustCompile(`\$(\d+)\$`)

// statement extracts the SQL to record: exactly the one sent to the database, with its
// placeholders intact and no parameter values.
//
// What fc hands over has already been through the dialect's Explain, even when the parameters
// ParamsFilter returned are empty. MySQL's and ClickHouse's Explain return it unchanged at that
// point; PG's does not: it first rewrites every $N into $N$ in preparation for interpolation
// (gorm.io/driver/postgres v1.6.3 Explain → gorm v1.31.2 logger.ExplainSQL), and with no
// parameters to interpolate the marker stays in the log — measured, INSERT ... VALUES ($1,$2) was
// recorded as VALUES ($1$,$2$), matching neither db.statement on the span nor pg_stat_statements.
//
// Undoing it is an exact inverse: Explain only appends one $ after each longest-matching $number,
// so dropping the trailing $ of every $number$ restores the original — including a $1$ that was
// there to begin with (Explain turns it into $1$$, and undoing gives $1$ back).
func (l *gormLogger) statement(fc func() (string, int64)) (string, int64) {
	sql, rows := fc()
	if l.numbered {
		sql = explainedNumbered.ReplaceAllString(sql, "$$$1")
	}
	return sql, rows
}

// errorAttrs are the error's fields: a server-side error records its code alone, never its text.
//
// This is the same principle as recording placeholder SQL without parameters: a server's error
// text carries the parameter values out with it (measured, MySQL 8.0's 1062 reads
// Duplicate entry 'a@b.com' for key …), and without stripping it the parameter values would reach
// the log by another route whenever Log: true. See Dialect.ErrorCode.
func (l *gormLogger) errorAttrs(err error) []any {
	text, code := l.dialect.redactedError(err)
	if code == "" {
		return []any{"error", text}
	}
	return []any{"error", text, "error_code", code}
}

// skipErr: "no record found" is usually an ordinary business branch rather than a failure.
func (l *gormLogger) skipErr(err error) bool {
	return l.ignoreNotFound && errors.Is(err, gorm.ErrRecordNotFound)
}

func attrs(sql string, rows int64, elapsed time.Duration, extra ...any) []any {
	out := make([]any, 0, 6+len(extra))
	out = append(out, "sql", sql, "elapsed_ms", ms(elapsed))
	if rows >= 0 {
		// -1 is GORM's convention for "the row count is unknown", and recording -1 would read as a
		// genuine -1 rows
		out = append(out, "rows_affected", rows)
	}
	return append(out, extra...)
}

// message assembles the log message GORM passed in.
//
// GORM's interface is printf-style, but it also passes plain text with no arguments, and Sprintf
// must not be used then — a % in the message would be taken for a placeholder.
//
// It takes a slice rather than variadic arguments: variadic ones make go vet treat this as a
// printf wrapper, which then demands a constant format string at every call site, and here the
// opposite is true.
func message(msg string, args []any) string {
	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}

// ms converts a duration to milliseconds, down to the microsecond. The field name carries the
// unit: slog's JSON writes a Duration as an integer of nanoseconds.
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
