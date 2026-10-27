package xlog

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jupiter-eng/xone/xapp"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
)

// New builds a logger from the configuration.
//
// It is a pure constructor: it touches no globals, reads no config file and does not need the framework running. Tests call it directly.
//
// The io.Closer returned is for cleanup (closing the log file) and is never nil even without file output,
// so the caller does not have to check.
//
// It carries only cfg.Fields; the default fields such as service and hostname are added when the framework assembles it (initXLog).
func New(cfg Config) (*slog.Logger, io.Closer, error) { return build(cfg, nil) }

// build is New with every log line also carrying the base fields (overridden by any cfg.Fields of the same name).
func build(cfg Config, base []slog.Attr) (*slog.Logger, io.Closer, error) {
	// Every configuration item is validated before any file is touched. The other way round, a malformed Format would
	// leave the log file already created with its fd open while New returned an error -- the caller would have no
	// Closer to close, and that fd and its symbolic link would simply stay there.
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, nil, err
	}
	newHandler, err := handlerFor(cfg.Format)
	if err != nil {
		return nil, nil, err
	}
	loc, err := parseLocation(cfg.Timezone)
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.File.validate(); err != nil {
		return nil, nil, xerror.New("xlog", "config", err)
	}

	writers := make([]io.Writer, 0, 2)
	closers := make([]io.Closer, 0, 1)

	if cfg.Console {
		writers = append(writers, os.Stdout)
	}
	if cfg.File.Enable {
		w, err := newFileWriter(cfg.File)
		if err != nil {
			return nil, nil, err
		}
		writers = append(writers, w)
		closers = append(closers, w)
	}

	// With no output at all nothing is written (the same as io.Discard) rather than an error being reported:
	// "I simply do not want logs" is a reasonable choice and must not keep the service from starting.
	out := &fanout{writers: writers, clock: time.Now}

	opts := &slog.HandlerOptions{Level: level, AddSource: cfg.AddSource, ReplaceAttr: inLocation(loc)}
	return withStatic(slog.New(newCtxHandler(newHandler(out, opts))), base, cfg.Fields), multiCloser(closers), nil
}

// writeErrInterval is the minimum interval between warnings about a log line that could not be written.
const writeErrInterval = time.Minute

// fanout hands every log line to each configured output.
//
// It is not io.MultiWriter, for two reasons that are both "the logs stop and nobody is told":
//
//   - MultiWriter returns at the first writer that fails (multiWriter.Write in io/multi.go), so a console
//     whose reader has gone -- the collector closing the pipe -- would take the log file with it, while
//     that file is perfectly writable;
//   - a failed write has nowhere to go: slog discards what a handler returns (_ = l.Handler().Handle(ctx, r)
//     in log/slog's Logger.log), so a full disk, an fd that went away or a file somebody removed looks
//     exactly like a service that had nothing to say. Rotation says this about itself one file over and
//     warns for it; this is the other half of the same promise.
//
// The warning goes to stderr through warnf and is rate-limited: an output that fails usually fails for
// every line, and one warning per line is a flood of its own.
type fanout struct {
	writers []io.Writer

	// clock is an injectable time source, which makes the rate limiting testable
	clock func() time.Time

	mu        sync.Mutex
	lastErrAt time.Time
}

// Write hands p to every output, including the ones after a failure, and reports the first failure.
//
// What it returns is the failing writer's own count and error, which keeps io.Writer's contract even
// though slog discards it.
func (f *fanout) Write(p []byte) (int, error) {
	n, err := len(p), error(nil)
	for _, w := range f.writers {
		wn, werr := w.Write(p)
		if werr != nil && err == nil {
			n, err = wn, werr
		}
	}
	if err != nil {
		f.report(err)
	}
	return n, err
}

// report warns about a failed write, at most once per writeErrInterval.
func (f *fanout) report(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := f.clock()
	if f.lastErrAt.IsZero() || now.Sub(f.lastErrAt) >= writeErrInterval {
		f.lastErrAt = now
		warnf("write: a log line could not be written, err=[%v]", err)
	}
}

// identity are the framework's default per-line fields: which application and which process the line came from.
//
// The names are OTel's semantic conventions with the dots written as underscores -- service.name becomes
// service_name. Tracing is held to those conventions anyway, and a dot is not allowed in a Prometheus label name
// and means a nested path in several log backends, so one rule ("semconv, underscores where a dot cannot go")
// gives the same name in all three signals: xmetric attaches service_name / service_version as constant labels.
//
// A side benefit of host_name over a bare host: in xgin's access log, host is the request's Host header, and the
// same name would mean two hosts in one line.
func identity() []slog.Attr {
	hostname, _ := os.Hostname()
	return []slog.Attr{
		slog.String("service_name", xapp.Name()),
		slog.String("service_version", xapp.Version()),
		slog.String("host_name", hostname),
		slog.Int("process_pid", os.Getpid()),
	}
}

// withStatic attaches the static fields to a logger: base first, a name in fields wins, and the rest follow sorted by name; a field with an empty string value is not written.
//
// They are attached with With: the JSON / text handler serializes them right there, so every later log line only copies a ready-made stretch of bytes.
func withStatic(l *slog.Logger, base []slog.Attr, fields map[string]string) *slog.Logger {
	attrs := make([]any, 0, len(base)+len(fields))
	add := func(a slog.Attr) {
		if a.Value.Kind() != slog.KindString || a.Value.String() != "" {
			attrs = append(attrs, a)
		}
	}
	for _, a := range base {
		if v, ok := fields[a.Key]; ok {
			a = slog.String(a.Key, v)
		}
		add(a)
	}
	extra := make([]string, 0, len(fields))
	for k := range fields {
		if !slices.ContainsFunc(base, func(a slog.Attr) bool { return a.Key == k }) {
			extra = append(extra, k)
		}
	}
	slices.Sort(extra)
	for _, k := range extra {
		add(slog.String(k, fields[k]))
	}
	if len(attrs) == 0 {
		return l
	}
	return l.With(attrs...)
}

// parseLocation parses an IANA time zone name. Empty means follow the local time zone.
func parseLocation(name string) (*time.Location, error) {
	if name == "" {
		return nil, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, xerror.Newf("xlog", "config",
			"unknown Timezone=[%s]; images without /usr/share/zoneinfo need `import _ \"time/tzdata\"` in your main package: %w",
			name, err)
	}
	return loc, nil
}

// inLocation converts the record time into the given time zone. It returns nil when loc is nil,
// which leaves HandlerOptions.ReplaceAttr empty and keeps even one redundant function call off the hot path.
func inLocation(loc *time.Location) func([]string, slog.Attr) slog.Attr {
	if loc == nil {
		return nil
	}
	return func(groups []string, a slog.Attr) slog.Attr {
		// Only the top-level time field is touched: a time.Time the business logged is its data and must not have its time zone changed.
		if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
			a.Value = slog.TimeValue(a.Value.Time().In(loc))
		}
		return a
	}
}

// handlerFor picks the handler constructor by format.
// It only looks at the format and does not touch the output, so a malformed format surfaces before the log file is opened.
func handlerFor(format string) (func(io.Writer, *slog.HandlerOptions) slog.Handler, error) {
	switch strings.ToLower(format) {
	case FormatText:
		return func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewTextHandler(w, o) }, nil
	case FormatJSON, "":
		return func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewJSONHandler(w, o) }, nil
	default:
		return nil, xerror.Newf("xlog", "config",
			"unknown log format Format=[%s], expected %s or %s", format, FormatJSON, FormatText)
	}
}

// newFileWriter builds the file writer, creating the directory when it does not exist.
func newFileWriter(c FileConfig) (io.WriteCloser, error) {
	// The permission is parsed before the directory is created: a malformed Perm must not leave an empty directory behind.
	perm, err := parsePerm(c.Perm)
	if err != nil {
		return nil, err
	}
	// A failure to create the directory is an environment problem (no permission, a read-only disk) while the configuration itself is valid, so the op is new rather than config.
	if c.Path != "" {
		if err := os.MkdirAll(c.Path, 0o755); err != nil {
			return nil, xerror.Newf("xlog", "new", "create log dir failed Path=[%s]: %w", c.Path, err)
		}
	}
	return newRotateWriter(filepath.Join(c.Path, c.Name), c.MaxAge, c.RotateTime, perm)
}

// parseLevel parses the log level. An unrecognized value is an error rather than a fallback to the default --
// a malformed configuration should be known at startup, not discovered in production with the wrong log level.
func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, xerror.Newf("xlog", "config",
			"unknown log level Level=[%s], expected debug / info / warn / error", s)
	}
}

// parsePerm parses an octal permission string, accepting "0644", "644" and YAML 1.2's "0o644".
func parsePerm(s string) (os.FileMode, error) {
	if s == "" {
		return defaultLogFilePerm, nil
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0o"), 8, 32)
	if err != nil {
		return 0, xerror.Newf("xlog", "config",
			"malformed log file permission Perm=[%s], expected an octal string such as \"0644\": %w", s, err)
	}
	return os.FileMode(v), nil
}

type multiCloser []io.Closer

func (m multiCloser) Close() error {
	var first error
	for _, c := range m {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ---- registration ----

// location is the log time zone in effect, rewritten every time logging is installed. nil means follow the local time zone.
var location atomic.Pointer[time.Location]

// Location returns the time zone the log timestamps use.
//
// It is for business code formatting times of its own that should line up with the logs, so the time zone name in
// the configuration does not have to be copied into the code. Without XLog.Timezone configured it returns time.Local.
//
//	t.In(xlog.Location()).Format(time.RFC3339)
func Location() *time.Location {
	if loc := location.Load(); loc != nil {
		return loc
	}
	return time.Local
}

// init puts logging first up and last down, so every other component's start and stop logs get written.
func init() {
	xhook.BeforeStart(initXLog, xhook.At(xhook.StageLog))
	xhook.BeforeStop(closeXLog) // the stage follows the start hook above
}

// closer is filled in by initXLog and used by closeXLog to close the writer, and swapOnClose says whether the
// global logger has to be replaced once that writer is gone, see closeXLog.
var (
	closer      io.Closer
	swapOnClose bool
)

// custom is the backend UseHandler was given, nil meaning build from the XLog configuration; installed means logging is already set up.
var (
	custom    atomic.Pointer[slog.Handler]
	installed atomic.Bool
)

// UseHandler sends the logs to a handler of your own (a slog bridge for zap, a company logging SDK, ...).
// Call it before xone.Run, as the first line of main for example.
//
// xlog still wraps it and installs it as slog.Default(): the trace_id, the fields from AddKV / CtxWithKV and the
// error log counting are all still there, the framework's access logs and SQL logs still go into it, and it merely does the final writing.
//
// Everything in XLog except Fields (the per-line fields, which are still carried) decides how xlog itself writes and
// has no effect at all; writing any of it fails startup, so nobody believes a Level: debug took. The level, the format and the destination are all decided by that handler.
// Passing nil restores building from the XLog configuration.
//
// Calling it after logging is set up (xone.Run has already passed the logging stage) has no effect and only logs a WARN.
func UseHandler(h slog.Handler) {
	if installed.Load() {
		slog.Warn("xlog.UseHandler called after logging was installed, ignored; call it before xone.Run")
		return
	}
	if h == nil {
		custom.Store(nil)
		return
	}
	custom.Store(&h)
}

// initXLog reads the configuration and then sets logging up.
func initXLog(context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return err
	}
	if h := custom.Load(); h != nil {
		// Fields are not the handler's business and are carried with any backend; the other items decide how xlog itself writes.
		// A block written without changing a single default (only Level: info, say) is not a conflict: that is what it means anyway.
		output := c
		output.Fields = nil
		if !reflect.DeepEqual(output, DefaultConfig()) {
			return xerror.Newf("xlog", "config",
				"%s has no effect when xlog.UseHandler is set: level, format and outputs are up to your handler; only %s.Fields applies",
				ConfigKey, ConfigKey)
		}
		closer = nil
		swapOnClose = false // the handler is the caller's: xlog neither closes it nor replaces it
		location.Store(nil) // the timestamps are decided by that handler, so xlog has no time zone
		slog.SetDefault(withStatic(slog.New(newCtxHandler(*h)), identity(), c.Fields))
		installed.Store(true)
		return nil
	}
	return install(c)
}

// install builds the logger from the configuration and installs it as the standard library's global default.
func install(c Config) error {
	l, cl, err := build(c, identity())
	if err != nil {
		return err
	}
	closer = cl
	swapOnClose = !c.Console && c.File.Enable
	// New has already validated, so this cannot fail again. Storing nil (follow the local time zone) when nothing is
	// configured matters too: with a time zone configured by a previous Run in the same process, not storing would leave that one in place.
	loc, _ := parseLocation(c.Timezone)
	location.Store(loc)
	// Installed as the standard library's global default logger: business code uses slog.Info / slog.InfoContext
	// directly and does not have to know this package. This is the same pattern as slog.SetDefault.
	slog.SetDefault(l)
	installed.Store(true)
	return nil
}

// closeXLog closes the log file.
//
// Writing is unbuffered (every log line is written straight to the file), so there is nothing to flush here;
// it only hands the fd back.
//
// Logging is closed last while logs keep coming after it -- the stop hooks abandoned after the timeout, the requests
// still in flight -- and they are exactly the ones that matter most for investigating "why did it not exit cleanly".
// Where they go from here depends on what was configured, which is what swapOnClose records:
//
//   - file output alone: they would be written into a closed file and vanish without a word, so the global logger is
//     replaced by one writing to stderr;
//   - the console on: they still reach stdout, and replacing would only take their trace_id, their per-request
//     fields and the configured level and format with them;
//   - no output configured at all: "I want no logs" must not turn into logs on stderr the moment the service stops.
func closeXLog(context.Context) error {
	installed.Store(false) // so UseHandler still works on another Run in the same process (common in tests)
	if swapOnClose {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	}
	if closer == nil {
		return nil
	}
	return closer.Close()
}
