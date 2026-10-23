package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// covLogLine GETs /probe/log to write one business log line at the given level
func covLogLine(t *testing.T, p *harness.Process, level, msg string) {
	t.Helper()
	if r := p.Get(t, "/probe/log?level="+level+"&msg="+msg); r.Status != 200 {
		t.Fatalf("GET /probe/log: %v", r)
	}
}

// xlog/README.md, XLog.File:
//
//	Name        the real file is app.log.<time suffix>, with a symbolic link of that name pointing at the current one
//	RotateTime  the rotation period, aligned to the local time zone, 1m at the least; the suffix goes down to the minute
//	MaxAge      a cleanup at the start and one at every rotation, deleting the app.log.<time suffix> files it named itself alone,
//	            and never touching app.log.bak / app.log.1.gz
//	Perm        a string parsed as octal
//
// RotateTime is set to its lower bound of 1m and a real rotation across a minute is waited for, so
// this case takes a minute and more
func TestCoverage_LogRotatesByRotateTime_MaxAgePrunesOwnFiles_PermApplied(t *testing.T) {
	requireSlow(t, "the next minute boundary, RotateTime being 1m at the least")
	t.Parallel()

	dir := t.TempDir()
	old := []string{"app.log.20200101", "app.log.2020010100", "app.log.202001010000"} // the day, hour and minute granularities, all older than MaxAge
	foreign := []string{"app.log.bak", "app.log.1.gz", "app.log.20200101.gz"}         // not named the way we name them, and never touched
	for _, n := range append(append([]string{}, old...), foreign...) {
		covWrite(t, dir, n, "old\n")
		past := time.Now().Add(-30 * 24 * time.Hour)
		_ = os.Chtimes(filepath.Join(dir, n), past, past)
	}

	p := harness.Start(t, harness.Options{Overlay: fmt.Sprintf(`XLog:
  File:
    Enable: true
    Path: %q
    RotateTime: 1m
    MaxAge: 1h
    Perm: "0600"
`, dir)})
	started := time.Now()
	exists := func(n string) bool { _, err := os.Lstat(filepath.Join(dir, n)); return err == nil }

	for _, n := range old {
		if exists(n) {
			t.Errorf("MaxAge: 1h: %s is older than that and should be cleaned up at the start", n)
		}
	}
	for _, n := range foreign {
		if !exists(n) {
			t.Errorf("%s is no app.log.<time suffix> and should not be deleted", n)
		}
	}

	current := func() string {
		t.Helper()
		target, err := os.Readlink(filepath.Join(dir, "app.log"))
		if err != nil {
			t.Fatalf("app.log should be a symbolic link pointing at the current file: %v", err)
		}
		return target
	}
	first := current()
	if want := "app.log." + started.Format("200601021504"); first != want && first != "app.log."+started.Add(-time.Minute).Format("200601021504") {
		t.Errorf("with RotateTime: 1m the file name suffix should go down to the minute (%s), and the symbolic link points at %s", want, first)
	}
	fi, err := os.Stat(filepath.Join(dir, first))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("with Perm: \"0600\" the log file's permissions should be 0600, got %o", fi.Mode().Perm())
	}

	// One more cleanup after a rotation: a file that aged out after the start is deleted at the next rotation
	covWrite(t, dir, "app.log.200001010000", "old\n")

	// Wait until the minute boundary is crossed and then write one line to trigger the rotation (which happens on a write)
	deadline := time.Now().Add(75 * time.Second)
	var second string
	for time.Now().Before(deadline) {
		covLogLine(t, p, "info", "rotate-probe")
		if second = current(); second != first {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if second == first {
		t.Fatalf("RotateTime: 1m: no rotation in 75s, and the symbolic link still points at %s", first)
	}
	rotatedAt := time.Now()
	if !strings.HasPrefix(second, "app.log.") || len(second) != len("app.log.200601021504") {
		t.Errorf("the file name after a rotation should be app.log.<year month day hour minute>, got %s", second)
	}
	if !exists(first) {
		t.Errorf("after the rotation the previous file %s has not reached MaxAge and should be kept", first)
	}
	logs, err := harness.ReadLogFile(filepath.Join(dir, "app.log"))
	if err != nil || len(logs) == 0 {
		t.Errorf("the new file the symbolic link points at after the rotation should hold log lines, got %d (%v)", len(logs), err)
	}
	// The cleanup at a rotation runs in the background; give it a moment
	for i := 0; i < 100 && exists("app.log.200001010000"); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if exists("app.log.200001010000") {
		t.Errorf("MaxAge: every rotation cleans up once, and a file that aged out after the start is still there after the rotation")
	}
	for _, n := range foreign {
		if !exists(n) {
			t.Errorf("the cleanup at a rotation should not touch %s either", n)
		}
	}
	t.Logf("numbers: %s -> %s, rotated %v after the start (across the minute boundary)", first, second, rotatedAt.Sub(started).Round(time.Millisecond))

	t.Run("a RotateTime under 1m fails the start", func(t *testing.T) {
		t.Parallel()
		stderr := covStartupError(t, harness.Options{Overlay: fmt.Sprintf("XLog:\n  File:\n    Enable: true\n    Path: %q\n    RotateTime: 30s\n", t.TempDir())})
		faultMustContain(t, "the start error for RotateTime: 30s", stderr, "xlog", "RotateTime")
	})
	t.Run("the permissions default to 0644", func(t *testing.T) {
		t.Parallel()
		d := t.TempDir()
		harness.Start(t, harness.Options{Overlay: fmt.Sprintf("XLog:\n  File:\n    Enable: true\n    Path: %q\n", d)})
		target, err := os.Readlink(filepath.Join(d, "app.log"))
		if err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(d, target))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("Perm defaults to 0644, got %o", fi.Mode().Perm())
		}
		if len(target) != len("app.log.20060102") {
			t.Errorf("RotateTime defaults to 24h and the suffix should be the year, month and day, got %s", target)
		}
	})
}

// xlog/README.md, XLog: Level (debug / info / warn / error, info by default), Format (json / text,
// json by default), Timezone (an IANA time zone name, and one that is configured and cannot be
// loaded fails the start outright) and AddSource (whether the code location is recorded, off by
// default)
func TestCoverage_LogLevel_Format_Timezone_AddSource(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	t.Run("Level: warn filters the infos out and keeps warn / error", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: logLevel("warn")})
		covLogLine(t, p, "info", "cov-info")
		covLogLine(t, p, "warn", "cov-warn")
		covLogLine(t, p, "error", "cov-error")
		p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "cov-error" })
		if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "cov-warn" && l.Level() == "WARN" })) != 1 {
			t.Errorf("with Level: warn a WARN log line should be there")
		}
		if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "cov-info" || l.Msg() == "request completed" })); n != 0 {
			t.Errorf("with Level: warn neither an INFO business log line nor an access log line should turn up, got %d", n)
		}
	})

	t.Run("Level: debug lets the debugs through", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: debugLogs})
		covLogLine(t, p, "debug", "cov-debug")
		p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "cov-debug" && l.Level() == "DEBUG" })
	})

	t.Run("the default info filters the debugs out", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{})
		covLogLine(t, p, "debug", "cov-debug")
		covLogLine(t, p, "info", "cov-info")
		p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "cov-info" })
		if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "cov-debug" })) != 0 {
			t.Errorf("with the default info a debug log line should not turn up")
		}
	})

	t.Run("Format: text", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: "XLog:\n  Format: text\n", NonJSON: "XLog.Format: text"})
		covLogLine(t, p, "warn", "cov-text")
		deadline := time.Now().Add(waitFor)
		for !strings.Contains(p.Stdout(), "msg=cov-text") && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		out := p.Stdout()
		if !strings.Contains(out, "level=WARN msg=cov-text") {
			t.Errorf("with Format: text standard output should be key=value text (level=WARN msg=cov-text), and it is not there:\n%s", lastLine(out))
		}
		for _, l := range p.Logs() {
			if l.Stream == "stdout" && l.Msg() != "" {
				t.Errorf("with Format: text there should be no JSON log line on standard output, and there is %s", l.Line)
				break
			}
		}
	})

	t.Run("Timezone: Asia/Shanghai", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: "XLog:\n  Timezone: Asia/Shanghai\n"})
		covLogLine(t, p, "info", "cov-tz")
		l := p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "cov-tz" })
		ts := l.Str("time")
		parsed, err := time.Parse(time.RFC3339Nano, ts)
		if !strings.HasSuffix(ts, "+08:00") || err != nil || time.Since(parsed).Abs() > time.Minute {
			t.Errorf("with Timezone: Asia/Shanghai the timestamp should be rendered in UTC+8 (+08:00) and be the current moment, got %q (%v)", ts, err)
		}
		t.Logf("numbers: the Asia/Shanghai timestamp %s", ts)
	})

	t.Run("a malformed Timezone fails the start", func(t *testing.T) {
		t.Parallel()
		stderr := covStartupError(t, harness.Options{Overlay: "XLog:\n  Timezone: Mars/Olympus\n"})
		faultMustContain(t, "the start error for a malformed Timezone", stderr, "unknown Timezone=[Mars/Olympus]")
	})

	t.Run("AddSource", func(t *testing.T) {
		t.Parallel()
		on := harness.Start(t, harness.Options{Overlay: "XLog:\n  AddSource: true\n"})
		off := harness.Start(t, harness.Options{})
		for _, p := range []*harness.Process{on, off} {
			covLogLine(t, p, "info", "cov-source")
		}
		l := on.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "cov-source" })
		if file := l.Str("source.file"); !strings.HasSuffix(file, "service/probe.go") || l.Str("source.line") == "" {
			t.Errorf("with AddSource: true the code location should be recorded (some line of service/probe.go), got source=%v", l.Fields["source"])
		}
		l = off.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "cov-source" })
		if _, ok := l.Get("source"); ok {
			t.Errorf("AddSource is off by default and the log line should hold no source, got %s", l.Line)
		}
	})
}
