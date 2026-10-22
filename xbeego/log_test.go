package xbeego

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/beego/beego/v2/core/logs"
)

func TestBeeLevel(t *testing.T) {
	// Beego has eight levels and slog four. Emergency / Alert / Critical land on ERROR because
	// slog has nothing above it and a logging platform alerts on ERROR anyway; Notice is nothing a
	// human has to act on and lands on INFO.
	for level, want := range map[int]slog.Level{
		logs.LevelEmergency:     slog.LevelError,
		logs.LevelAlert:         slog.LevelError,
		logs.LevelCritical:      slog.LevelError,
		logs.LevelError:         slog.LevelError,
		logs.LevelWarning:       slog.LevelWarn,
		logs.LevelNotice:        slog.LevelInfo,
		logs.LevelInformational: slog.LevelInfo,
		logs.LevelDebug:         slog.LevelDebug,
	} {
		if got := beeLevel(level); got != want {
			t.Errorf("beego level %d should map to %v, got=%v", level, want, got)
		}
	}
	// An unknown level must not be dropped: recorded as INFO it is still in the log.
	if got := beeLevel(99); got != slog.LevelInfo {
		t.Errorf("an unknown level should still be recorded, got=%v", got)
	}
}

func TestBeeLog_WriteMsg(t *testing.T) {
	lines := captureLog(t)
	if err := (beeLog{}).WriteMsg(&logs.LogMsg{Level: logs.LevelError, Msg: "payload too large"}); err != nil {
		t.Fatal(err)
	}

	l := only(t, lines(), "beego internal log")
	if l["level"] != "ERROR" {
		t.Errorf("the level should be mapped, got=%v", l["level"])
	}
	if l["message"] != "payload too large" {
		t.Errorf("beego's text belongs under message, as xecho's gommon lines do, got=%v", l)
	}
	// Left out rather than written empty: a field that is always there and always blank is worse
	// than none.
	for _, f := range []string{"prefix", "source"} {
		if _, ok := l[f]; ok {
			t.Errorf("%s should be left out when beego did not fill it in, got=%v", f, l)
		}
	}
}

func TestBeeLog_WriteMsgWithPrefixAndSource(t *testing.T) {
	lines := captureLog(t)
	err := (beeLog{}).WriteMsg(&logs.LogMsg{
		Level: logs.LevelWarning, Msg: "slow", Prefix: "HTTP", FilePath: "router.go", LineNumber: 42,
	})
	if err != nil {
		t.Fatal(err)
	}

	l := only(t, lines(), "beego internal log")
	if l["level"] != "WARN" || l["prefix"] != "HTTP" || l["source"] != "router.go:42" {
		t.Errorf("the prefix and the call site should be recorded when beego filled them in, got=%v", l)
	}
}

func TestBeegoLogs_RoutedIntoSlog(t *testing.T) {
	// The point for a service whose logs are collected as JSON: beego writes plain text to the
	// console, and those lines fall out of every field-based query.
	New().WithConfig(configWith(quiet)).Handler() // assembly installs the adapter
	lines := captureLog(t)

	logs.Error("cannot reach %s", "db")

	l := only(t, lines(), "beego internal log")
	if l["level"] != "ERROR" {
		t.Errorf("the level should survive the trip, got=%v", l["level"])
	}
	// Beego hands the format string and its arguments over separately; writing Msg out as it is
	// would log "cannot reach %s".
	if l["message"] != "cannot reach db" {
		t.Errorf("the format string and its arguments should be put together, got=%v", l["message"])
	}
	if src, _ := l["source"].(string); !strings.Contains(src, "log_test.go") {
		t.Errorf("the call site beego recorded should be kept, got=%v", l["source"])
	}
}

func TestBeegoLogs_LevelIsSlogs(t *testing.T) {
	// Beego's own logger passes everything through (its default level is Debug), so XLog.Level is
	// the one place the level is decided -- as it is for every other integration.
	New().WithConfig(configWith(quiet)).Handler()

	lines := captureLog(t) // the default handler level is Info
	logs.Debug("noise")
	logs.Info("kept")

	got := lines()
	for _, l := range got {
		if l["message"] == "noise" {
			t.Errorf("slog's level should have dropped it, got=%v", l)
		}
	}
	if l := only(t, got, "beego internal log"); l["message"] != "kept" {
		t.Errorf("the line above the level should be kept, got=%v", l)
	}
}

func TestRegisterBeeLogAdapter_OnlyOncePerProcess(t *testing.T) {
	// logs.Register panics on a repeated name, which is why it sits inside bindOnce rather than in
	// each assembly.
	New().WithConfig(configWith(quiet)).Handler()
	New().WithConfig(configWith(quiet)).Handler()
	// Reaching here without a panic is the assertion.
}

func TestBeegoLogs_ReplacesBeegosOwnLoggerRatherThanAddingToIt(t *testing.T) {
	// Beego's logger attaches a console adapter of its own the first time anything is written to
	// it, and an application may have set one up as well. Added to rather than replaced, every
	// line would go out twice: once as JSON through slog and once as beego's text on stderr --
	// which no field-based query can read and which doubles the volume.
	//
	// The console adapter captures os.Stdout when it is created, so the swap has to come first.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = old
		useBeeLogAdapter() // put the framework's back for the tests that follow
	})

	logs.Reset()
	if err := logs.SetLogger(logs.AdapterConsole); err != nil {
		t.Fatal(err)
	}
	logs.Info("before")

	// What the framework does on assembly.
	useBeeLogAdapter()
	logs.Info("after")

	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "before") {
		t.Fatalf("the console adapter should have been writing to the pipe, got=%q", got)
	}
	if strings.Contains(got, "after") {
		t.Errorf("beego's own logger should have been replaced, not added to; it still wrote=%q", got)
	}
}
