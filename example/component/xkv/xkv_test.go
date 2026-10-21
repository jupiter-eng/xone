package xkv

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/xonetest"
)

// This file is part of the example too: what the pure constructor New buys is that the tests need no
// mock at all, and no framework brought up -- just a clean instance built directly and closed when
// done.

func TestStore_ReadsBackWhatWasWritten(t *testing.T) {
	s, closer, err := New(Config{Path: filepath.Join(t.TempDir(), "kv.json"), FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })

	s.Set("k", "v")
	if got, ok := s.Get("k"); !ok || got != "v" {
		t.Fatalf("what was written must read back, got=%q ok=%v", got, ok)
	}
	if s.Len() != 1 {
		t.Errorf("there must be exactly one key, got=%d", s.Len())
	}
}

func TestStore_FlushesOnCloseAndReadsBackNextTime(t *testing.T) {
	// The background flush interval is set very long, so this write to disk can only have come from
	// Close
	path := filepath.Join(t.TempDir(), "kv.json")
	c := Config{Path: path, FlushInterval: time.Hour}

	s, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	s.Set("k", "v")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	again, closer2, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer2.Close() })
	if got, _ := again.Get("k"); got != "v" {
		t.Fatalf("closing must flush the changes and reopening must read them back, got=%q", got)
	}
}

func TestStore_FailsToBuildOnCorruptDataFile(t *testing.T) {
	// Failing to build must be an error that fails startup on the spot -- rather than quietly
	// starting from an empty store, which would look perfectly fine in production except that the
	// data is gone
	path := filepath.Join(t.TempDir(), "kv.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(Config{Path: path, FlushInterval: time.Hour}); err == nil {
		t.Fatal("a corrupt data file must be an error")
	}
}

func TestRegister_RegistersStartAndStopHooks(t *testing.T) {
	// This is the only wire between this package and the framework: a hook left unregistered shows up
	// as "C() returns nothing" or "the data was not flushed on exit", and nothing else catches it. So
	// the hooks are run the way the framework runs them, rather than calling initXKV / closeXKV
	// directly
	path := filepath.Join(t.TempDir(), "kv.json")
	t.Run("one start and stop", func(t *testing.T) {
		xonetest.UseConfigYAML(t, "XKV:\n  Path: \""+path+"\"\n  FlushInterval: 1h\n")
		xonetest.StartHooks(t) // the stop hooks run when the subtest ends
		if C() == nil {
			t.Fatal("the start hook was not registered and C() returns nothing")
		}
		C().Set("k", "v")
	})

	// The flush interval is 1h, so anything read back can only have been flushed when the stop hook
	// closed it
	s, closer, err := New(Config{Path: path, FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	if got, _ := s.Get("k"); got != "v" {
		t.Errorf("the stop hook was not registered and the data was not flushed on exit, got=%q", got)
	}
}

func TestInitXKV_WalksTheRealFrameworkPath(t *testing.T) {
	// This is why the example exists: read the configuration -> build the instance -> keep it ->
	// close it on exit. These are the lines a caller copies, and they really have to hold together
	dir := t.TempDir()
	path := filepath.Join(dir, "kv.json")
	xonetest.UseConfigYAML(t, "XKV:\n  Path: \""+path+"\"\n  FlushInterval: 50ms\n")

	if err := initXKV(context.Background()); err != nil {
		t.Fatalf("initialization failed: %v", err)
	}
	if C() == nil {
		t.Fatal("the instance was not kept and C() returns nothing")
	}
	if Conf().Path != path {
		t.Errorf("the configuration was not read, got=%+v", Conf())
	}

	C().Set("k", "v")
	if err := closeXKV(context.Background()); err != nil {
		t.Fatalf("closing failed: %v", err)
	}

	// Closing flushed the last changes, so a freshly built one reads them back
	s, closer, err := New(Conf())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	if got, _ := s.Get("k"); got != "v" {
		t.Errorf("nothing landed on disk before exiting, got=%q", got)
	}
}

func TestInitXKV_BadConfigFailsStartup(t *testing.T) {
	xonetest.UseConfigYAML(t, "XKV:\n  Paht: /tmp/kv.json\n")
	if err := initXKV(context.Background()); err == nil {
		t.Fatal("a misspelled field must fail startup")
	}
}

func TestNew_FailsWhenFlushIntervalNotPositive(t *testing.T) {
	// Let through, time.NewTicker panics inside a background goroutine and the process simply dies
	for _, every := range []time.Duration{0, -time.Second} {
		if _, _, err := New(Config{Path: filepath.Join(t.TempDir(), "kv.json"), FlushInterval: every}); err == nil {
			t.Errorf("FlushInterval=%v must fail to build", every)
		}
	}
}

func TestInitXKV_ZeroFlushIntervalFailsStartup(t *testing.T) {
	xonetest.UseConfigYAML(t, "XKV:\n  FlushInterval: 0s\n")
	if err := initXKV(context.Background()); err == nil {
		t.Fatal("a FlushInterval of 0 must fail startup")
	}
}

func TestStore_FailedFlushLosesNoData(t *testing.T) {
	// Flushing used to clear the dirty flag before writing and never hand it back when the write
	// failed: nobody would flush this batch of changes again, and even the last flush in Close would
	// skip it as "no changes"
	dir := filepath.Join(t.TempDir(), "not-created-yet")
	path := filepath.Join(dir, "kv.json")
	s, closer, err := New(Config{Path: path, FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	s.Set("k", "v")
	if err := s.flush(); err == nil {
		t.Fatal("with the directory missing, this flush must fail")
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("once the directory exists, Close must manage to flush: %v", err)
	}

	again, closer2, err := New(Config{Path: path, FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer2.Close() })
	if got, _ := again.Get("k"); got != "v" {
		t.Errorf("the changes whose flush failed once were lost, got=%q", got)
	}
}
