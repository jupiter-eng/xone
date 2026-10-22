package xlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/logext"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xonetest"
)

// fileCfg builds a configuration writing only to a file and returns the configuration together with the log file path.
func fileCfg(t *testing.T) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	c := DefaultConfig()
	c.Console = false
	c.File = FileConfig{Enable: true, Path: dir, Name: "app.log",
		RotateTime: 24 * time.Hour, MaxAge: 7 * 24 * time.Hour, Perm: "0644"}
	return c, filepath.Join(dir, "app.log")
}

// readLines reads each JSON line out of the log file.
func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log file failed: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("the log line is not valid JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestNew_LevelParsing(t *testing.T) {
	for _, s := range []string{"debug", "info", "warn", "warning", "error", "INFO", " info ", ""} {
		c := DefaultConfig()
		c.Console = false
		c.Level = s
		if _, _, err := New(c); err != nil {
			t.Errorf("the level %q must be accepted, got=%v", s, err)
		}
	}
}

func TestNew_FailsFastOnLevelTypo(t *testing.T) {
	c := DefaultConfig()
	c.Level = "verbose"
	_, _, err := New(c)
	if err == nil {
		t.Fatal("an unrecognized level must be an error rather than a quiet fallback to info")
	}
	if !strings.Contains(err.Error(), "verbose") {
		t.Errorf("the error must echo the malformed value, got=%v", err)
	}
	assertConfigOp(t, err)
}

func TestNew_FailsFastOnFormatTypo(t *testing.T) {
	c := DefaultConfig()
	c.Format = "xml"
	_, _, err := New(c)
	if err == nil {
		t.Fatal("an unrecognized format must be an error")
	}
	assertConfigOp(t, err)
}

// assertConfigOp: a malformed configuration is reported as config, not new, so alerts and investigations can tell a configuration problem from an environment one by the op.
func assertConfigOp(t *testing.T, err error) {
	t.Helper()
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Module != "xlog" || xe.Op != "config" {
		t.Errorf("a malformed configuration must be reported as xlog config, got=%v", err)
	}
}

func TestNew_LeavesNoOpenLogFileOnFailure(t *testing.T) {
	// Format validation used to come after opening the file: New returned an error while the log file was already
	// created with its fd open, and the caller had no Closer to close -- that fd and its symbolic link simply stayed
	// there. Every configuration item has to be validated before any file is touched.
	c, _ := fileCfg(t)
	c.Format = "xml"
	if _, _, err := New(c); err == nil {
		t.Fatal("an unrecognized format must be an error")
	}

	entries, err := os.ReadDir(c.File.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("a failed New must leave no file behind, got=%v", names)
	}
}

func TestNew_FailsFastOnRotationPeriodUnderOneMinute(t *testing.T) {
	// The time suffix in the file name goes down to the minute. With RotateTime written as 0s, truncate returns the
	// value unchanged and the result is one file per minute; written as 30s, two periods land in the same file name
	// and it still rotates once a minute -- both are "configure A and get B", and silently so.
	for _, d := range []time.Duration{0, 30 * time.Second, -time.Hour} {
		c, _ := fileCfg(t)
		c.File.RotateTime = d
		_, _, err := New(c)
		if err == nil || !strings.Contains(err.Error(), "RotateTime") {
			t.Errorf("RotateTime=%v must be an error naming the field, got=%v", d, err)
		}
	}
	c, _ := fileCfg(t)
	c.File.RotateTime = time.Minute
	if _, cl, err := New(c); err != nil {
		t.Errorf("a minute is the finest valid granularity and must not be an error: %v", err)
	} else {
		cl.Close()
	}
}

func TestNew_MaxAgeNegativeFailsFast(t *testing.T) {
	// 0 means no cleanup; a negative value is most likely a mistake, and treating it as "no cleanup" would slowly fill the disk.
	c, _ := fileCfg(t)
	c.File.MaxAge = -time.Hour
	if _, _, err := New(c); err == nil || !strings.Contains(err.Error(), "MaxAge") {
		t.Errorf("a negative MaxAge must be an error naming the field, got=%v", err)
	}
}

func TestParsePerm_OctalNotations(t *testing.T) {
	// Measured with yaml.v3: an unquoted Perm: 0644 going into a string field is still "0644";
	// YAML 1.2's 0o644 spelling must be accepted too.
	for _, s := range []string{"0644", "644", "0o644"} {
		if got, err := parsePerm(s); err != nil || got != 0o644 {
			t.Errorf("parsePerm(%q)=%o err=%v, want 644", s, got, err)
		}
	}
}

func TestNew_FailsFastOnPermTypo(t *testing.T) {
	c, _ := fileCfg(t)
	c.File.Perm = "rw-r--r--"
	_, _, err := New(c)
	if err == nil {
		t.Fatal("a malformed permission must be an error")
	}
	assertConfigOp(t, err)
}

func TestNew_WorksWithAllOutputsDisabled(t *testing.T) {
	c := DefaultConfig()
	c.Console = false
	c.File.Enable = false

	l, closer, err := New(c)
	if err != nil {
		t.Fatalf("\"I simply do not want logs\" is a reasonable choice and must not keep the service from starting: %v", err)
	}
	l.Info("this line is discarded")
	if err := closer.Close(); err != nil {
		t.Errorf("Close must not be an error: %v", err)
	}
}

func TestNew_CloserIsNeverNil(t *testing.T) {
	c := DefaultConfig()
	c.Console = true
	c.File.Enable = false
	_, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if closer == nil {
		t.Fatal("even without file output the Closer must not be nil -- the caller must not have to check")
	}
	_ = closer.Close()
}

func TestNew_WritesFileAndFiltersByLevel(t *testing.T) {
	c, path := fileCfg(t)
	c.Level = "warn"

	l, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	l.Debug("debug must not appear")
	l.Info("info must not appear")
	l.Warn("warn must appear")
	l.Error("error must appear")
	closer.Close()

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("the warn level must let exactly 2 through, got=%d: %v", len(lines), lines)
	}
	if lines[0]["level"] != "WARN" || lines[1]["level"] != "ERROR" {
		t.Errorf("what gets through must be WARN and ERROR, got=%v", lines)
	}
}

func TestNew_TextFormat(t *testing.T) {
	c, path := fileCfg(t)
	c.Format = FormatText

	l, closer, _ := New(c)
	l.Info("hello", "k", "v")
	closer.Close()

	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "k=v") {
		t.Errorf("the text format must output k=v, got=%q", string(b))
	}
}

// ---- ctx scopes ----

func TestScope_FieldsAppearInLog(t *testing.T) {
	c, path := fileCfg(t)
	l, closer, _ := New(c)

	ctx := CtxWithScope(context.Background())
	AddKV(ctx, "user_id", 42)
	AddKVs(ctx, map[string]any{"req_id": "abc", "path": "/api"})
	l.InfoContext(ctx, "processing finished")
	closer.Close()

	got := readLines(t, path)[0]
	if got["user_id"] != float64(42) || got["req_id"] != "abc" || got["path"] != "/api" {
		t.Errorf("every field in the scope must appear in the log, got=%v", got)
	}
}

func TestScope_DeepCallWritesVisibleToOwner(t *testing.T) {
	// This is the point of storing a pointer rather than a value: a business function deep in the call stack cannot
	// reach the *gin.Context and has no way to hand a new context back, yet the fields it writes have to be visible to the access log at the entry point.
	c, path := fileCfg(t)
	l, closer, _ := New(c)

	ctx := CtxWithScope(context.Background())
	func(ctx context.Context) { // a deep business function, holding only the ctx
		AddKV(ctx, "written_deep_down", true)
	}(ctx)
	l.InfoContext(ctx, "the access log at the entry point") // the entry point still uses the original ctx
	closer.Close()

	if got := readLines(t, path)[0]; got["written_deep_down"] != true {
		t.Errorf("a deep write must be visible wherever the same ctx is held, got=%v", got)
	}
}

func TestScope_ReopeningKeepsExistingFields(t *testing.T) {
	ctx := CtxWithScope(context.Background())
	AddKV(ctx, "a", 1)

	ctx2 := CtxWithScope(ctx) // a middleware registered twice, say
	AddKV(ctx2, "b", 2)

	n := 0
	scopeFrom(ctx).Each(func(k string, v any) { n++ })
	if n != 2 {
		t.Errorf("opening it again must be idempotent with both fields present, got=%d", n)
	}
}

func TestScope_DropsAndCountsWithoutScope(t *testing.T) {
	before := DroppedKVCount()
	AddKV(context.Background(), "k", "v")
	AddKVs(context.Background(), map[string]any{"a": 1, "b": 2})

	if got := DroppedKVCount() - before; got != 3 {
		t.Errorf("writes without a scope must be counted, to help investigate \"the field did not appear in the log\", got=%d want=3", got)
	}
}

func TestScope_nil_CtxDoesNotCrash(t *testing.T) {
	//lint:ignore SA1012 nil is passed on purpose to verify it does not panic
	AddKV(nil, "k", "v")
	if ctx := CtxWithScope(nil); ctx == nil {
		t.Error("CtxWithScope(nil) must return a usable context")
	}
}

func TestCtxWithKV_AffectsOnlyDerivedCtx(t *testing.T) {
	c, path := fileCfg(t)
	l, closer, _ := New(c)

	req := CtxWithScope(context.Background())
	AddKV(req, "req_id", "r1")
	a := CtxWithKV(req, map[string]any{"order_id": 1})
	b := CtxWithKV(req, map[string]any{"order_id": 2})
	AddKV(a, "only_a", true) // written into a itself, not flowing back to req and not bleeding into b

	l.InfoContext(a, "a")
	l.InfoContext(b, "b")
	l.InfoContext(req, "req")
	closer.Close()

	got := readLines(t, path)
	if got[0]["req_id"] != "r1" || got[0]["order_id"] != float64(1) || got[0]["only_a"] != true {
		t.Errorf("a must carry the parent fields, its own order_id and the later write, got=%v", got[0])
	}
	if got[1]["req_id"] != "r1" || got[1]["order_id"] != float64(2) || got[1]["only_a"] != nil {
		t.Errorf("b carries only the parent fields and its own order_id, got=%v", got[1])
	}
	if got[2]["order_id"] != nil || got[2]["only_a"] != nil {
		t.Errorf("the parent ctx must not be polluted by the derived fields, got=%v", got[2])
	}
}

func TestCtxWithKV_PassedValueWinsOnSameKey_WorksWithoutScope(t *testing.T) {
	c, path := fileCfg(t)
	l, closer, _ := New(c)

	parent := CtxWithScope(context.Background())
	AddKV(parent, "k", "old")
	l.InfoContext(CtxWithKV(parent, map[string]any{"k": "new"}), "override")
	l.InfoContext(CtxWithKV(context.Background(), map[string]any{"k": "bare"}), "no scope")
	closer.Close()

	got := readLines(t, path)
	if got[0]["k"] != "new" {
		t.Errorf("a key of the same name must take the value passed in, got=%v", got[0])
	}
	if got[1]["k"] != "bare" {
		t.Errorf("the fields must be carried even when the parent ctx has no scope, got=%v", got[1])
	}
	var kept any
	scopeFrom(parent).Each(func(_ string, v any) { kept = v })
	if kept != "old" {
		t.Errorf("the parent ctx's value must not be changed, got=%v", kept)
	}
}

// ---- the trace extractor ----

func TestTraceExtractor(t *testing.T) {
	t.Cleanup(func() { SetTraceExtractor(nil) })

	c, path := fileCfg(t)
	l, closer, _ := New(c)

	SetTraceExtractor(func(ctx context.Context) (string, string) {
		return "trace-1", "span-1"
	})
	l.InfoContext(context.Background(), "a log with a trace")
	closer.Close()

	got := readLines(t, path)[0]
	if got["trace_id"] != "trace-1" || got["span_id"] != "span-1" {
		t.Errorf("the injected trace identifiers must appear in the log, got=%v", got)
	}
}

func TestTraceExtractor_NoFieldsWhenNotInjected(t *testing.T) {
	SetTraceExtractor(nil)

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	l.InfoContext(context.Background(), "no trace")
	closer.Close()

	if got := readLines(t, path)[0]; got["trace_id"] != nil {
		t.Errorf("without an injected extractor there must be no trace field, got=%v", got)
	}
}

func TestTraceExtractor_NoFieldsForEmptyTraceID(t *testing.T) {
	t.Cleanup(func() { SetTraceExtractor(nil) })
	SetTraceExtractor(func(context.Context) (string, string) { return "", "" })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	l.InfoContext(context.Background(), "the trace was not sampled")
	closer.Close()

	if got := readLines(t, path)[0]; got["trace_id"] != nil {
		t.Errorf("nothing extractable must not write empty fields, got=%v", got)
	}
}

func TestHandler_GroupKeepsCtxFields(t *testing.T) {
	// slog's With goes through WithAttrs, and WithGroup prefixes the attributes that follow.
	// A user's group is for business fields, while trace_id is the identity of the whole record and has to stay at the top level.
	t.Cleanup(func() { SetTraceExtractor(nil) })
	SetTraceExtractor(func(context.Context) (string, string) { return "t1", "s1" })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	ctx := CtxWithScope(context.Background())
	AddKV(ctx, "uid", "u9")
	l.With("fixed_field", "x").WithGroup("g").InfoContext(ctx, "msg", "business_field", "y")
	closer.Close()

	got := readLines(t, path)[0]
	if got["trace_id"] != "t1" || got["span_id"] != "s1" {
		t.Errorf("the trace fields must be at the top level, got=%v", got)
	}
	if got["uid"] != "u9" {
		t.Errorf("the scope fields must be at the top level, got=%v", got)
	}
	if got["fixed_field"] != "x" {
		t.Errorf("an attribute attached with With before the group must stay at the top level, got=%v", got)
	}
	g, ok := got["g"].(map[string]any)
	if !ok || g["business_field"] != "y" {
		t.Errorf("only the business fields after the group may land in the group, got=%v", got)
	}
	if _, dup := g["trace_id"]; dup {
		t.Errorf("the trace fields must not also appear inside the group, got=%v", got)
	}
}

func TestHandler_NestedGroups(t *testing.T) {
	// The replayed call chain has to keep its order, or nested groups end up in the wrong place.
	t.Cleanup(func() { SetTraceExtractor(nil) })
	SetTraceExtractor(func(context.Context) (string, string) { return "t1", "" })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	l.WithGroup("a").With("inner", 1).WithGroup("b").InfoContext(context.Background(), "msg", "leaf", 2)
	closer.Close()

	got := readLines(t, path)[0]
	if got["trace_id"] != "t1" {
		t.Errorf("the trace fields must be at the top level, got=%v", got)
	}
	a, ok := got["a"].(map[string]any)
	if !ok {
		t.Fatalf("the a group is missing, got=%v", got)
	}
	if a["inner"] != float64(1) {
		t.Errorf("a.inner must be 1, got=%v", got)
	}
	b, ok := a["b"].(map[string]any)
	if !ok || b["leaf"] != float64(2) {
		t.Errorf("a.b.leaf must be 2, got=%v", got)
	}
}

func TestHandler_FastPathWithoutGroup(t *testing.T) {
	// Without a group ever opened the attributes are at the top level anyway, and it must not degrade to rebuilding a handler per record.
	h := newCtxHandler(slog.NewJSONHandler(io.Discard, nil))
	w := h.WithAttrs([]slog.Attr{slog.String("a", "1")}).(*ctxHandler)
	if w.grouped {
		t.Error("calling WithAttrs alone must not trigger a replay")
	}
	if same := h.WithAttrs(nil); same != slog.Handler(h) {
		t.Error("empty attributes must be returned unchanged")
	}
	if same := h.WithGroup(""); same != slog.Handler(h) {
		t.Error("an empty group name must be returned unchanged")
	}
}

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.Level != "info" || c.Format != FormatJSON {
		t.Errorf("the defaults must be info + json, got=%+v", c)
	}
	if !c.Console {
		t.Error("the console must be on by default")
	}
	if c.File.Enable {
		t.Error("file output must be off by default")
	}
	if c.File.RotateTime != 24*time.Hour || c.File.MaxAge != 7*24*time.Hour {
		t.Errorf("rotation must default to one day with seven days of retention, got=%+v", c.File)
	}
}

func TestHandler_SiblingDerivationsIndependent(t *testing.T) {
	// Two branches derived from the same parent logger must not share the call chain's underlying array,
	// or the one derived later would overwrite the last link of the one derived first.
	t.Cleanup(func() { SetTraceExtractor(nil) })
	SetTraceExtractor(func(context.Context) (string, string) { return "t1", "" })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	parent := l.With("shared", "p")
	a := parent.WithGroup("a")
	b := parent.WithGroup("b")
	a.InfoContext(context.Background(), "msg", "x", 1)
	b.InfoContext(context.Background(), "msg", "x", 2)
	closer.Close()

	lines := readLines(t, path)
	for i, want := range []string{"a", "b"} {
		got := lines[i]
		if got["shared"] != "p" || got["trace_id"] != "t1" {
			t.Errorf("the top-level fields of line %d are wrong, got=%v", i, got)
		}
		g, ok := got[want].(map[string]any)
		if !ok || g["x"] != float64(i+1) {
			t.Errorf("line %d must land in the %s group, got=%v", i, want, got)
		}
	}
}

func TestRegister_RegistrationMatchesFramework(t *testing.T) {
	// This is the only wire between this package and the framework: a missing hook registration or a wrong stage shows
	// up as "the config has no effect" or "it is ready later than what uses it", and nothing else catches it.
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xlog" {
			got = &e
			break
		}
	}
	if got == nil {
		t.Fatal("no start hook was registered")
	}
	if got.Stage != hook.StageLog {
		t.Errorf("logging must be first up and last down, or the other components' start and stop logs are lost, got=%v", got.Stage)
	}

	var stopped bool
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xlog" {
			stopped = true
		}
	}
	if stopped != true {
		t.Errorf("the stop hook registration is wrong, got=%v", stopped)
	}
}

func TestTraceIDs(t *testing.T) {
	// It is for packages that need the trace identifiers without depending on OpenTelemetry (xmetric uses it for exemplars).
	t.Cleanup(func() { SetTraceExtractor(nil) })

	if id, sp := TraceIDs(context.Background()); id != "" || sp != "" {
		t.Errorf("without an injected extractor it must return empty strings, got=%q %q", id, sp)
	}
	SetTraceExtractor(func(context.Context) (string, string) { return "t1", "s1" })
	if id, sp := TraceIDs(context.Background()); id != "t1" || sp != "s1" {
		t.Errorf("it must return what the extractor gives, got=%q %q", id, sp)
	}
	if id, sp := TraceIDs(nil); id != "" || sp != "" { //nolint:staticcheck // nil is passed on purpose
		t.Errorf("a nil ctx must not panic, got=%q %q", id, sp)
	}
}

func TestAddObserver(t *testing.T) {
	// xmetric counts error logs through it, without xlog having to know about Prometheus the other way round.
	t.Cleanup(logext.ResetObservers())

	var seen []string
	AddObserver(func(_ context.Context, r slog.Record) { seen = append(seen, r.Level.String()+":"+r.Message) })
	AddObserver(nil) // ignored, must not blow up

	c, _ := fileCfg(t)
	c.Level = "warn"
	l, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("filtered out by level")
	l.Warn("a warning")
	l.Error("an error")
	closer.Close()

	want := []string{"WARN:a warning", "ERROR:an error"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("an observer must only receive the logs actually written, got=%v want=%v", seen, want)
	}
}

func TestAddObserver_PanicDoesNotInterruptLogging(t *testing.T) {
	t.Cleanup(logext.ResetObservers())

	AddObserver(func(context.Context, slog.Record) { panic("boom") })
	var reached bool
	AddObserver(func(context.Context, slog.Record) { reached = true })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	l.Error("something went wrong")
	closer.Close()

	if !reached {
		t.Error("one observer panicking must not affect the observers after it")
	}
	if got := readLines(t, path)[0]; got["msg"] != "something went wrong" {
		t.Errorf("the observer panicked, and the log itself must still be written, got=%v", got)
	}
}

func TestNew_RendersTimeInConfiguredTimezone(t *testing.T) {
	c := DefaultConfig()
	c.Console, c.Format, c.Timezone = false, FormatJSON, "Asia/Shanghai"

	dir := t.TempDir()
	c.File.Enable, c.File.Path, c.File.Name = true, dir, "tz"

	l, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	line := readFile(t, filepath.Join(dir, "tz"))
	var rec struct {
		Time string `json:"time"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("the log line could not be parsed: %v, raw=%s", err, line)
	}
	got, err := time.Parse(time.RFC3339Nano, rec.Time)
	if err != nil {
		t.Fatal(err)
	}
	if _, offset := got.Zone(); offset != 8*3600 {
		t.Errorf("it must be rendered in Asia/Shanghai (+08:00), got=%s offset=%d", rec.Time, offset)
	}
}

func TestNew_FailsWhenTimezoneCannotLoad(t *testing.T) {
	// Quietly falling back to local would mean believing the timestamps are UTC+8 while they are UTC,
	// eight hours off with no hint. A scratch image without the time zone database is exactly that scenario.
	c := DefaultConfig()
	c.Console, c.Timezone = false, "Nowhere/Nothing"

	_, _, err := New(c)
	if err == nil {
		t.Fatal("a time zone that cannot be loaded must be an error")
	}
	if !strings.Contains(err.Error(), "time/tzdata") {
		t.Errorf("the error must say how to fix it, got=%v", err)
	}
}

func TestLocation_LocalTimezoneWhenUnset(t *testing.T) {
	location.Store(nil)
	if got := Location(); got != time.Local {
		t.Errorf("without Timezone configured it must return time.Local, got=%v", got)
	}

	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("this machine has no time zone database")
	}
	location.Store(sh)
	t.Cleanup(func() { location.Store(nil) })
	if got := Location(); got != sh {
		t.Errorf("once configured it must return the configured time zone, got=%v", got)
	}
}

// keepGlobals records the globals install will change and restores them when the test ends.
func keepGlobals(t *testing.T) {
	t.Helper()
	oldLogger, oldCloser, oldLoc := slog.Default(), closer, location.Load()
	t.Cleanup(func() {
		slog.SetDefault(oldLogger)
		closer = oldCloser
		custom.Store(nil)
		installed.Store(false)
		location.Store(oldLoc)
	})
}

func TestInitXLog_InstallsUsableDefaultWhenUnconfigured(t *testing.T) {
	// Logging is the one thing that has to exist even unconfigured: when the configuration itself has a problem,
	// the caller has to be able to see that error.
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")

	if err := initXLog(context.Background()); err != nil {
		t.Fatalf("not configuring it must not be an error: %v", err)
	}
	if slog.Default() == nil {
		t.Fatal("no global logger was installed")
	}
}

func TestInitXLog_ConfigTypoFailsStartup(t *testing.T) {
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XLog:\n  Lvl: debug\n")

	if err := initXLog(context.Background()); err == nil {
		t.Fatal("a misspelled field must fail startup")
	}
}

func TestInitXLog_InvalidValueFailsStartup(t *testing.T) {
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XLog:\n  Level: verbose\n")

	if err := initXLog(context.Background()); err == nil {
		t.Fatal("an unrecognized level must fail startup rather than quietly fall back to some default")
	}
}

func TestInitXLog_ConfigAppliedToGlobalLogger(t *testing.T) {
	keepGlobals(t)
	dir := t.TempDir()
	xonetest.UseConfigYAML(t, "XLog:\n  Level: error\n  Console: false\n  File:\n    Enable: true\n    Path: \""+dir+"\"\n    Name: app.log\n")

	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	slog.Info("this line must not be written")
	slog.Error("this line must be written")

	if err := closeXLog(context.Background()); err != nil {
		t.Fatalf("closing must not be an error: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "app.log"))
	if err != nil {
		t.Fatalf("no file was written at the configured path: %v", err)
	}
	if strings.Contains(string(b), "this line must not be written") {
		t.Error("the Level from the configuration had no effect")
	}
	if !strings.Contains(string(b), "this line must be written") {
		t.Error("what should have been written was not")
	}
}

func TestInitXLog_TimezoneAppliedGlobally(t *testing.T) {
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XLog:\n  Timezone: Asia/Tokyo\n")

	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := location.Load(); got == nil || got.String() != "Asia/Tokyo" {
		t.Errorf("the time zone was not installed, got=%v", got)
	}
}

func TestInitXLog_TimezoneClearedWhenLaterRunLeavesItUnset(t *testing.T) {
	// Another Run in the same process (common in tests): location used to be written only when a time zone was
	// configured, so the previous one stayed and xlog.Location() disagreed with this run's log timestamps.
	keepGlobals(t)
	if _, err := time.LoadLocation("Asia/Tokyo"); err != nil {
		t.Skip("this machine has no time zone database")
	}
	xonetest.UseConfigYAML(t, "XLog:\n  Timezone: Asia/Tokyo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := closeXLog(context.Background()); err != nil {
		t.Fatal(err)
	}

	xonetest.UseConfigYAML(t, "XLog:\n  Level: info\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := Location(); got != time.Local {
		t.Errorf("this run configures no Timezone and must return to time.Local, got=%v", got)
	}
	if err := closeXLog(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The same for the UseHandler branch.
	xonetest.UseConfigYAML(t, "XLog:\n  Timezone: Asia/Tokyo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := closeXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	UseHandler(slog.NewTextHandler(io.Discard, nil))
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := Location(); got != time.Local {
		t.Errorf("with UseHandler xlog has no time zone and must return to time.Local, got=%v", got)
	}
}

func TestCloseXLog_ClosesWithoutInit(t *testing.T) {
	keepGlobals(t)
	closer = nil
	if err := closeXLog(context.Background()); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

func TestCloseXLog_LogsGoToStderrAfterFileClosed(t *testing.T) {
	// Regression case. slog.Default() used to still point at the closed file: the error xone.Run returns, the
	// abandoned stop hooks and the logs of requests in flight all vanished without a word while Console was off.
	keepGlobals(t)
	dir := t.TempDir()
	xonetest.UseConfigYAML(t, "XLog:\n  Console: false\n  File:\n    Enable: true\n    Path: \""+dir+"\"\n    Name: app.log\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}

	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	old := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = old })

	if err := closeXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	slog.Error("shutdown failed")

	b, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "shutdown failed") {
		t.Errorf("logs after the file is closed must go to stderr, got=%q", b)
	}
}

func TestUseHandler_LogsGoToOwnHandlerWithCtxFields(t *testing.T) {
	// With another backend, xlog's own layer is still there: the AddKV fields, the trace_id and the observers must not be lost.
	keepGlobals(t)
	var buf bytes.Buffer
	UseHandler(slog.NewJSONHandler(&buf, nil))
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx := CtxWithScope(context.Background())
	AddKV(ctx, "user_id", "u1")
	slog.InfoContext(ctx, "hello")
	if out := buf.String(); !strings.Contains(out, `"msg":"hello"`) || !strings.Contains(out, `"user_id":"u1"`) {
		t.Errorf("the log must go into the caller's own handler carrying the AddKV fields, got=%q", out)
	}
	if closer != nil {
		t.Errorf("a handler of your own is not xlog's to close, so the closer must be nil")
	}
}

func TestUseHandler_XLogBlockFailsStartup(t *testing.T) {
	// Writing Level: debug while another handler decides the level: it looks like it took and it did not.
	keepGlobals(t)
	UseHandler(slog.NewJSONHandler(io.Discard, nil))
	xonetest.UseConfigYAML(t, "XLog:\n  Level: debug\n")
	err := initXLog(context.Background())
	if err == nil || !strings.Contains(err.Error(), "UseHandler") {
		t.Fatalf("UseHandler together with XLog output settings must fail startup and say why, got=%v", err)
	}
}

func TestUseHandler_DefaultValuedXLogBlockIsFine(t *testing.T) {
	keepGlobals(t)
	UseHandler(slog.NewJSONHandler(io.Discard, nil))
	xonetest.UseConfigYAML(t, "XLog:\n  Level: info\n")
	if err := initXLog(context.Background()); err != nil {
		t.Errorf("writing only the defaults is not a conflict, got=%v", err)
	}
}

func TestUseHandler_LateCallIsIgnored(t *testing.T) {
	// Switching after logging is set up: the earlier logs have gone elsewhere already. It has no effect, but it has to be said.
	keepGlobals(t)
	var buf bytes.Buffer
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Swap the installed logger for one that captures the output, so the WARN is visible.
	slog.SetDefault(slog.New(newCtxHandler(slog.NewJSONHandler(&buf, nil))))
	before := slog.Default().Handler()

	UseHandler(slog.NewJSONHandler(io.Discard, nil))
	if custom.Load() != nil || slog.Default().Handler() != before {
		t.Error("a UseHandler after it is set up must have no effect")
	}
	if !strings.Contains(buf.String(), "xlog.UseHandler called after logging was installed") {
		t.Errorf("calling it too late must log a WARN, got=%q", buf.String())
	}
}

func TestUseHandler_CloseLeavesOwnHandlerInPlace(t *testing.T) {
	// Switching to stderr on close is about not writing into an already closed file; a handler of your own has no such problem and must not be replaced.
	keepGlobals(t)
	var buf bytes.Buffer
	UseHandler(slog.NewJSONHandler(&buf, nil))
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := closeXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	slog.Info("after close")
	if !strings.Contains(buf.String(), "after close") {
		t.Errorf("after closing, the logs must still go into the caller's own handler, got=%q", buf.String())
	}
}

func TestUseHandler_UsableAgainAfterClose(t *testing.T) {
	// A second Run in the same process (common in tests): after the previous round closed, UseHandler has to take effect again.
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := closeXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	UseHandler(slog.NewJSONHandler(io.Discard, nil))
	if custom.Load() == nil {
		t.Error("a UseHandler after closing must take effect")
	}
}

// identityLines runs xapp's and xlog's start hooks on the given yml, logs one line, and reads back every line in the file.
func identityLines(t *testing.T, yml string) []map[string]any {
	t.Helper()
	keepGlobals(t)
	dir := t.TempDir()
	xonetest.UseConfigYAML(t, yml+"  Console: false\n  File:\n    Enable: true\n    Path: \""+dir+"\"\n    Name: app.log\n")
	xonetest.StartHooks(t)
	slog.Info("hello")
	return readLines(t, filepath.Join(dir, "app.log"))
}

func TestIdentity_DefaultFieldsOnEveryLine(t *testing.T) {
	// They are semconv's service.name / host.name / process.pid with underscores for the dots, the same names the
	// metrics carry: a logging platform can filter by service and by instance, with one name per signal.
	lines := identityLines(t, "XApp:\n  Name: order-api\n  Version: v1.2.3\nXLog:\n")
	host, _ := os.Hostname()
	l := lines[len(lines)-1]
	for k, want := range map[string]any{
		"service_name": "order-api", "service_version": "v1.2.3", "host_name": host, "process_pid": float64(os.Getpid()),
	} {
		if l[k] != want {
			t.Errorf("%s must be %v, got=%v", k, want, l[k])
		}
	}
	if _, ok := l["host"]; ok {
		t.Errorf("the machine name is called host_name: host is the request's Host header in xgin's access log, got=%v", l)
	}
}

func TestIdentity_EmptyAppFieldsAreOmitted(t *testing.T) {
	// Without XApp.Name / Version configured, no empty service is written.
	lines := identityLines(t, "XLog:\n")
	l := lines[len(lines)-1]
	if _, ok := l["service_name"]; ok {
		t.Errorf("without XApp.Name configured there must be no service_name, got=%v", l)
	}
	if _, ok := l["service_version"]; ok {
		t.Errorf("without XApp.Version configured there must be no service_version, got=%v", l)
	}
	if l["host_name"] == nil || l["process_pid"] == nil {
		t.Errorf("host_name and process_pid are still there, got=%v", l)
	}
}

func TestFields_ExtraOverrideAndDrop(t *testing.T) {
	t.Setenv("XLOG_TEST_POD", "order-api-7d9f")
	lines := identityLines(t, "XApp:\n  Name: order-api\nXLog:\n  Fields:\n"+
		"    pod: ${XLOG_TEST_POD}\n    zone: ${XLOG_TEST_UNSET_ZONE:}\n    service_name: renamed\n    host_name: \"\"\n")
	l := lines[len(lines)-1]
	if l["pod"] != "order-api-7d9f" {
		t.Errorf("a Fields value must come from the environment, got=%v", l["pod"])
	}
	if _, ok := l["zone"]; ok {
		t.Errorf("${VAR:} is an empty string when unset, and a field with an empty string is not written, got=%v", l)
	}
	if l["service_name"] != "renamed" {
		t.Errorf("a name of the same name must take the Fields value, got=%v", l["service_name"])
	}
	if _, ok := l["host_name"]; ok {
		t.Errorf("writing an empty string drops that default field, got=%v", l)
	}
}

func TestNew_OnlyFieldsNoIdentity(t *testing.T) {
	// New is a pure constructor: it carries only cfg.Fields and does not read XApp or the host name.
	c, path := fileCfg(t)
	c.Fields = map[string]string{"b": "2", "a": "1"}
	l, cl, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello")
	cl.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := string(b)
	if !strings.Contains(line, `"a":"1","b":"2"`) || strings.Contains(line, "host_name") {
		t.Errorf("New carries only Fields (sorted by name), got=%s", line)
	}
}

func TestUseHandler_CarriesIdentityAndFields(t *testing.T) {
	// Another backend still carries the identity fields; Fields are not the handler's business and writing them is not a conflict.
	keepGlobals(t)
	var buf bytes.Buffer
	UseHandler(slog.NewJSONHandler(&buf, nil))
	xonetest.UseConfigYAML(t, "XApp:\n  Name: order-api\nXLog:\n  Fields:\n    zone: az1\n")
	xonetest.StartHooks(t)
	slog.Info("hello")
	out := buf.String()
	if !strings.Contains(out, `"service_name":"order-api"`) || !strings.Contains(out, `"zone":"az1"`) || !strings.Contains(out, `"process_pid":`) {
		t.Errorf("a handler of your own must carry the identity fields and Fields too, got=%s", out)
	}
}
