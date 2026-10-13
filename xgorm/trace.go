package xgorm

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"unicode"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

// 这里自己注册 GORM 回调，而不是用 gorm.io/plugin/opentelemetry/tracing。
//
// 那个官方插件 import 了 gorm.io/driver/clickhouse，于是每个用它的二进制
// 都要编进整套 ClickHouse 客户端：实测模块图 159 个、编译包 372 个。
// 为了在 MySQL 上打一条 Span，这个价钱太贵了。
//
// GORM 的回调接口本身很简单，自己接上只要下面这些代码，依赖只多 otel。

const tracerName = "github.com/jupiter-eng/xone/xgorm"

// tracerOpts 属性按哪一版语义约定写，随 Tracer 一起报给后端。
// 存成切片再展开传：每条 SQL 现拼一个变参切片就是多一次分配
var tracerOpts = []trace.TracerOption{trace.WithSchemaURL(semconv.SchemaURL)}

// registrar 是 *callback.Register 的方法值。
//
// GORM 的 processor / callback 都是非导出类型，外部没法给它们声明接口或变量，
// 但方法值可以拿出来存——于是这张表还是写得成表。
type registrar func(name string, fn func(*gorm.DB)) error

// installTracing 给实例挂上链路回调，每种操作前后各一个
func installTracing(db *gorm.DB, info ConnInfo, d Dialect) error {
	cb := db.Callback()
	pairs := []struct {
		op            string
		before, after registrar
	}{
		{"create", cb.Create().Before("gorm:create").Register, cb.Create().After("gorm:create").Register},
		{"query", cb.Query().Before("gorm:query").Register, cb.Query().After("gorm:query").Register},
		{"update", cb.Update().Before("gorm:update").Register, cb.Update().After("gorm:update").Register},
		{"delete", cb.Delete().Before("gorm:delete").Register, cb.Delete().After("gorm:delete").Register},
		{"row", cb.Row().Before("gorm:row").Register, cb.Row().After("gorm:row").Register},
		{"raw", cb.Raw().Before("gorm:raw").Register, cb.Raw().After("gorm:raw").Register},
	}

	start, end := startSpan(info), endSpan(d)
	var errs []error
	for _, p := range pairs {
		errs = append(errs,
			p.before("xgorm:trace:before", start(p.op)),
			p.after("xgorm:trace:after", end),
		)
	}
	return errors.Join(errs...)
}

// connAttrs 只由连接信息决定的那几个属性，按 OTel 数据库语义约定 v1.43.0
// （与仓库里 otelhttp v0.71.0 用的是同一版）。
//
// 旧名字（db.system、db.name、db.statement、db.operation）在 v1.26 之后
// 陆续改名，这里只出新名字，不双写。
func connAttrs(info ConnInfo) []attribute.KeyValue {
	out := []attribute.KeyValue{semconv.DBSystemNameKey.String(systemName(info.Driver))}
	if info.DB != "" {
		out = append(out, semconv.DBNamespace(info.DB))
	}
	host, port, err := net.SplitHostPort(info.Addr)
	if err != nil {
		// 不是 host:port（方言没解出端口）就整个当地址记
		if info.Addr != "" {
			out = append(out, semconv.ServerAddress(info.Addr))
		}
		return out
	}
	out = append(out, semconv.ServerAddress(host))
	if n, err := strconv.Atoi(port); err == nil {
		out = append(out, semconv.ServerPort(n))
	}
	return out
}

// systemName 驱动名换成语义约定里的 db.system.name。
// 只有 postgres 不同名（约定里是 postgresql）；mysql、clickhouse 同名，其余驱动按原名记
func systemName(driver string) string {
	if driver == string(DriverPostgres) {
		return "postgresql"
	}
	return driver
}

// startSpan 开一个 Span 并把它塞回 Statement 的 context
//
// Span 名和起始选项只由 op 与连接信息决定，挂回调时拼一次就够了：
// 放进闭包里的话每条 SQL 都要重新拼一遍，白白多出 5 次分配。
// opts 由并发的各条 SQL 共用，Start 只读选项、不改它。
func startSpan(info ConnInfo) func(op string) func(*gorm.DB) {
	attrs := connAttrs(info)
	return func(op string) func(*gorm.DB) {
		name := "gorm." + op
		opts := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...)}
		return func(db *gorm.DB) {
			if db.Statement == nil {
				return
			}
			ctx, _ := otel.Tracer(tracerName, tracerOpts...).Start(db.Statement.Context, name, opts...)
			db.Statement.Context = ctx
		}
	}
}

// endSpan 补上 SQL 与结果，然后结束 Span
func endSpan(d Dialect) func(*gorm.DB) {
	return func(db *gorm.DB) {
		if db.Statement == nil {
			return
		}
		span := trace.SpanFromContext(db.Statement.Context)
		if !span.IsRecording() {
			// 没采样时连 SQL 字符串都不用取
			span.End()
			return
		}
		defer span.End()

		// 记的是带占位符的 SQL，不记 Statement.Vars——参数里可能有个人信息或凭证。
		// 行数取 db.RowsAffected 而不是 db.Statement.RowsAffected：后者是从
		// Statement 内嵌的 *DB 提升上来的，指的是另一个实例，这里没有理由绕过去。
		// db.rows_affected 不在语义约定里（约定只有 db.response.returned_rows，
		// 那是查出来的行数），沿用这个名字
		sql := db.Statement.SQL.String()
		span.SetAttributes(semconv.DBQueryText(sql), attribute.Int64("db.rows_affected", db.RowsAffected))
		if op := operationName(sql); op != "" {
			span.SetAttributes(semconv.DBOperationName(op))
		}

		// 「没查到记录」是正常的业务分支，标成错误会让链路里满屏红色
		if db.Error != nil && !errors.Is(db.Error, gorm.ErrRecordNotFound) {
			recordError(span, d, db.Error)
		}
	}
}

// recordError 把错误记进 Span，文本和日志是同一份（Dialect.redactedError）：服务端报的错只记错误码，
// 客户端的错只有认得出是安全的几类才记原文——原文里可能就是参数值，而 db.query.text
// 为了不带参数值特意只记了占位符。RecordError 会把 err.Error() 原样写进 exception.message，
// 所以只交给它认出来的那个安全的错误
func recordError(span trace.Span, d Dialect, err error) {
	text, code := d.redactedError(err)
	if code != "" {
		span.SetAttributes(semconv.DBResponseStatusCode(code), semconv.ErrorTypeKey.String(code))
	} else {
		if safe := clientSafeError(err); safe != nil {
			span.RecordError(safe)
		}
		span.SetAttributes(semconv.ErrorTypeOther)
	}
	span.SetStatus(codes.Error, text)
}

// operationName 语句的第一个关键字，如 SELECT、INSERT，原样大小写。
//
// 约定允许从语句里解析：取找到的第一个操作名。GORM 的 create / query 这类回调名
// 说的不是数据库做了什么：软删除走的是 delete 回调，发出去的却是 UPDATE。
// 第一个词不是纯字母（以注释、括号开头）就不记，不猜
func operationName(sql string) string {
	sql = strings.TrimSpace(sql)
	end := strings.IndexFunc(sql, func(r rune) bool { return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z') })
	if end < 0 {
		end = len(sql)
	}
	word := sql[:end]
	if word == "" || end < len(sql) && !unicode.IsSpace(rune(sql[end])) {
		return ""
	}
	return word
}
