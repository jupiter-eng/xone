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

// gormLogger 把 GORM 的日志接到标准库 slog 上
//
// 字段是结构化的而不是拼进消息里：SQL 里带引号和换行，塞进消息文本
// 会把一行日志撑成好几行，也没法按耗时或错误筛。
type gormLogger struct {
	slowThreshold  time.Duration
	ignoreNotFound bool
	level          logger.LogLevel

	// numbered 方言的 Explain 会把 $N 改写成 $N$，记下来之前要改回去，见 statement
	numbered bool

	// dialect 认服务端错误码用，见 Dialect.ErrorCode
	dialect Dialect
}

// newGormLogger d 是这个实例的 Dialector，只用来判断它的占位符长什么样
func newGormLogger(c ClientConfig, dialect Dialect, d gorm.Dialector) *gormLogger {
	return &gormLogger{
		slowThreshold:  c.SlowThreshold,
		ignoreNotFound: c.IgnoreNotFound,
		level:          logger.Info,
		numbered:       d.Explain("$1") == "$1$",
		dialect:        dialect,
	}
}

// LogMode 返回副本，不改共享实例（GORM 的约定）
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

// Trace 每条 SQL 执行完都会被调用
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

	// 每条 SQL 都走这一支。slog 不收 info 就在这里停下：fc 要把 SQL 重新拼一遍
	// （PG 方言即使没有参数也要过两遍正则），拼完再交给 slog 丢掉全是白干
	case l.level >= logger.Info && slog.Default().Enabled(ctx, slog.LevelInfo):
		sql, rows := l.statement(fc)
		slog.InfoContext(ctx, "SQL", attrs(sql, rows, elapsed)...)
	}
}

// ParamsFilter 让 GORM 交给 Trace 的是带占位符的 SQL，不把参数值代进去。
//
// GORM 只在 Logger 实现了这个接口时才调它（v1.31.2 callbacks.go:142），
// 否则把 Statement.Vars 逐个代进 SQL 再交给我们：实测 Log: true 时
// WHERE password = ? 记下来的是 password = '<真实的值>'。参数里可能有
// 个人信息或凭证，与 trace.go 不记 Statement.Vars 是同一条原则。
func (l *gormLogger) ParamsFilter(ctx context.Context, sql string, params ...any) (string, []any) {
	return withoutParams(ctx, sql, params...)
}

// withoutParams 只留带占位符的 SQL，参数一个都不交出去。
//
// 除了 gormLogger.ParamsFilter，它还是进程级的 logger.RecorderParamsFilter（见 xgorm.go 的 init）：
// GORM 的 Scan 执行期间把实例的 Logger 换成 logger.Recorder（v1.31.2 finisher_api.go:539），
// 执行完再把 Recorder 记下的 SQL 交给原来的 Logger。Recorder 不问我们的 ParamsFilter，
// 只认这个包级变量，而它的默认值原样返回参数——于是 Raw(...).Scan(...)、
// Table(...).Select(...).Scan(...) 交过来的是参数已经代进去的 SQL，每个驱动都是。
// 实测（ClickHouse 24.8，改之前）：WHERE name = ? 记成了 WHERE name = 'sql-3fa9c1d2b7e4'
func withoutParams(_ context.Context, sql string, _ ...any) (string, []any) {
	return sql, nil
}

// explainedNumbered PG 方言的 Explain 在没有参数可代时留下的记号
var explainedNumbered = regexp.MustCompile(`\$(\d+)\$`)

// statement 取出要记的 SQL：就是发给数据库的那条，占位符原样、不含参数值。
//
// fc 交出来的 SQL 已经过了方言的 Explain，哪怕 ParamsFilter 返回的参数是空的。
// MySQL、ClickHouse 的 Explain 这时原样返回；PG 的不会：它先把每个 $N 改写成 $N$
// 等着代参数（gorm.io/driver/postgres v1.6.3 Explain → gorm v1.31.2
// logger.ExplainSQL），没有参数可代，记号就留在了日志里——实测
// INSERT ... VALUES ($1,$2) 记成了 VALUES ($1$,$2$)，和 Span 里的 db.statement、
// 和 pg_stat_statements 里的都对不上。
//
// 改回去是精确的逆运算：Explain 只在每个最长匹配的 $数字 后面补一个 $，
// 所以把每个 $数字$ 去掉末尾那个 $ 就还原了原文，原文里本来就有的 $1$ 也一样
// （Explain 把它变成 $1$$，改回来还是 $1$）。
func (l *gormLogger) statement(fc func() (string, int64)) (string, int64) {
	sql, rows := fc()
	if l.numbered {
		sql = explainedNumbered.ReplaceAllString(sql, "$$$1")
	}
	return sql, rows
}

// errorAttrs 错误的字段：服务端报的错只记错误码，不记原文。
//
// 与只记占位符 SQL、不记参数是同一条原则：服务端的错误原文会把参数值带出来
// （实测 MySQL 8.0 的 1062 是 Duplicate entry 'a@b.com' for key …），
// 不收掉的话 Log: true 时参数值换一条路照样进了日志。见 Dialect.ErrorCode
func (l *gormLogger) errorAttrs(err error) []any {
	text, code := l.dialect.redactedError(err)
	if code == "" {
		return []any{"error", text}
	}
	return []any{"error", text, "error_code", code}
}

// skipErr「没查到记录」通常是正常的业务分支，不是故障
func (l *gormLogger) skipErr(err error) bool {
	return l.ignoreNotFound && errors.Is(err, gorm.ErrRecordNotFound)
}

func attrs(sql string, rows int64, elapsed time.Duration, extra ...any) []any {
	out := make([]any, 0, 6+len(extra))
	out = append(out, "sql", sql, "elapsed_ms", ms(elapsed))
	if rows >= 0 {
		// -1 是 GORM 表示「行数未知」的约定，写成 -1 会被误读成真有 -1 行
		out = append(out, "rows_affected", rows)
	}
	return append(out, extra...)
}

// message 组装 GORM 传来的日志消息。
//
// GORM 的接口是 printf 风格的，但它也会传不带参数的纯文本，
// 那时候不能走 Sprintf——消息里的 % 会被当成占位符。
//
// 收切片而不是变参：变参会被 go vet 认成 printf 包装函数，
// 于是每个调用点都要求格式串是常量，而这里恰恰相反。
func message(msg string, args []any) string {
	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}

// ms 耗时换成毫秒，保留到微秒。字段名带单位：slog 的 JSON 把 Duration 写成纳秒整数
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
