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

// The GORM callbacks are registered here rather than using gorm.io/plugin/opentelemetry/tracing.
//
// That official plugin imports gorm.io/driver/clickhouse, so every binary using it compiles in the
// whole ClickHouse client: measured, a module graph of 159 and 372 compiled packages. That is too
// high a price for one span on MySQL.
//
// GORM's callback interface is simple enough that wiring it up takes only the code below, with
// otel as the one added dependency.

const tracerName = "github.com/jupiter-eng/xone/xgorm"

// tracerOpts is which version of the semantic conventions the attributes follow, reported to the
// backend along with the Tracer. Stored as a slice and expanded: building a variadic slice per SQL
// statement would be one more allocation each time.
var tracerOpts = []trace.TracerOption{trace.WithSchemaURL(semconv.SchemaURL)}

// registrar is a method value of *callback.Register.
//
// GORM's processor and callback are unexported types, so nothing outside can declare an interface
// or a variable for them — but a method value can be taken and stored, which is what keeps this a
// table.
type registrar func(name string, fn func(*gorm.DB)) error

// installTracing attaches the tracing callbacks to an instance, one before and one after each kind
// of operation.
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

// connAttrs are the attributes decided by the connection information alone, following OTel's
// database semantic conventions v1.43.0 (the same version otelhttp v0.71.0 in this repository uses).
//
// The old names (db.system, db.name, db.statement, db.operation) were renamed one after another
// after v1.26; only the new names are emitted here, with no dual writing.
func connAttrs(info ConnInfo) []attribute.KeyValue {
	out := []attribute.KeyValue{semconv.DBSystemNameKey.String(systemName(info.Driver))}
	if info.DB != "" {
		out = append(out, semconv.DBNamespace(info.DB))
	}
	host, port, err := net.SplitHostPort(info.Addr)
	if err != nil {
		// Not host:port (the dialect produced no port), so the whole thing is recorded as the address
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

// systemName maps a driver name to the db.system.name of the semantic conventions.
// Only postgres differs (the conventions call it postgresql); mysql and clickhouse match, and any
// other driver is recorded under its own name.
func systemName(driver string) string {
	if driver == string(DriverPostgres) {
		return "postgresql"
	}
	return driver
}

// startSpan opens a span and puts it back into the Statement's context.
//
// The span name and the start options are decided by op and the connection information alone, so
// building them once while registering the callbacks is enough: inside the closure they would be
// rebuilt for every SQL statement, costing 5 allocations for nothing. opts is shared by concurrent
// statements, and Start only reads the options and never modifies them.
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

// endSpan fills in the SQL and the result, then ends the span.
func endSpan(d Dialect) func(*gorm.DB) {
	return func(db *gorm.DB) {
		if db.Statement == nil {
			return
		}
		span := trace.SpanFromContext(db.Statement.Context)
		if !span.IsRecording() {
			// Without sampling, even fetching the SQL string is unnecessary
			span.End()
			return
		}
		defer span.End()

		// What is recorded is the SQL with its placeholders, never Statement.Vars — the parameters may
		// hold personal information or credentials. The row count comes from db.RowsAffected rather
		// than db.Statement.RowsAffected: the latter is promoted from the *DB embedded in Statement and
		// refers to a different instance, and there is no reason to go around it here.
		// db.rows_affected is not in the semantic conventions (which only have
		// db.response.returned_rows, the number of rows read), and this name is kept.
		sql := db.Statement.SQL.String()
		span.SetAttributes(semconv.DBQueryText(sql), attribute.Int64("db.rows_affected", db.RowsAffected))
		if op := operationName(sql); op != "" {
			span.SetAttributes(semconv.DBOperationName(op))
		}

		// "No record found" is an ordinary business branch, and marking it as an error floods the
		// trace with red
		if db.Error != nil && !errors.Is(db.Error, gorm.ErrRecordNotFound) {
			recordError(span, d, db.Error)
		}
	}
}

// recordError records the error on the span, with the same text as the log (Dialect.redactedError):
// a server-side error records the error code alone, and a client-side one records its text only for
// the few kinds recognized as safe — the text may be the parameter values themselves, and
// db.query.text deliberately records placeholders only to keep them out. RecordError writes
// err.Error() into exception.message verbatim, so it is only handed the safe error it recognized.
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

// operationName is the statement's first keyword, SELECT or INSERT for instance, in its original case.
//
// The conventions allow parsing it from the statement: the first operation name found is taken.
// GORM's callback names such as create and query do not say what the database did — a soft delete
// goes through the delete callback while an UPDATE goes out. When the first word is not purely
// alphabetic (it starts with a comment or a parenthesis) nothing is recorded, and nothing is guessed.
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
