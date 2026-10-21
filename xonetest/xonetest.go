// Package xonetest is for callers' own tests: swap in a configuration and run the start hooks, without starting the whole service.
//
//	func TestInitXKV(t *testing.T) {
//		xonetest.UseConfigYAML(t, "XKV:\n  Path: /tmp/kv.json\n")
//		xonetest.StartHooks(t) // the matching stop hooks run when the test ends
//		if xkv.C() == nil { ... }
//	}
//
// Configuration and hooks are both process-wide global state, so a test using this package cannot t.Parallel.
//
// This package introduces no new dependencies.
package xonetest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
)

// UseConfig makes this test read the configuration at path: that is what xconfig.Unmarshal reads, by the
// same route xone.Run loads it (Import, profiles and ${VAR} all work as usual).
//
// The test fails outright when loading fails. It is cleared when the test ends, so the next test starts from "not loaded yet".
func UseConfig(t testing.TB, path string) {
	t.Helper()
	config.Reset()
	t.Cleanup(config.Reset)
	if err := config.Load(path); err != nil {
		t.Fatalf("xonetest: load config %s: %v", path, err)
	}
}

// UseConfigYAML is UseConfig with the configuration written inline in the test:
// it is written to an application.yml in a temporary directory and loaded from there.
func UseConfigYAML(t testing.TB, yml string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte(yml), 0o600); err != nil {
		t.Fatalf("xonetest: write config: %v", err)
	}
	UseConfig(t, p)
}

// StartHooks runs every registered start hook once, by stage, in the same order as xone.Run; the stop hooks
// run in reverse when the test ends -- again only those paired with a start hook that succeeded.
//
// It runs every hook registered in this test binary, that is the package under test together with the
// integrations it imports. A failing start hook fails the test, and whatever already succeeded is still closed.
//
// It starts no server, does not take over the stop signals, does not check for config keys nobody reads, and
// puts no deadline on the stop hooks: those are xone.Run's job, and testing them means calling Run directly.
func StartHooks(t testing.TB) {
	t.Helper()
	started := map[int]bool{}
	t.Cleanup(func() {
		for _, e := range hook.StopAfter(started) {
			if err := e.Run(context.Background()); err != nil {
				t.Errorf("xonetest: stop hook %s: %v", e.Name, err)
			}
		}
	})
	for _, e := range hook.Start() {
		if err := e.Run(context.Background()); err != nil {
			t.Fatalf("xonetest: start hook %s: %v", e.Name, err)
		}
		started[e.Seq] = true
	}
}
