package xapp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xonetest"
)

func TestRegister_OnlyReadsConfigBuildsNothing(t *testing.T) {
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xapp" {
			got = &e
		}
	}
	if got == nil {
		t.Fatal("no start hook is registered, so nobody reads the App block")
	}
	if got.Stage != hook.StageLog {
		t.Errorf("tracing takes App.Name as its service name, so this block must be read earlier, got=%v", got.Stage)
	}
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xapp" {
			t.Error("this package has nothing to close, and registering a stop hook would put it in the shutdown sequence")
		}
	}
}

func TestNameVersion(t *testing.T) {
	old := cfg
	t.Cleanup(func() { cfg = old })

	cfg = DefaultConfig()
	if Name() != "" || Version() != "" {
		t.Errorf("the default must be empty, got=%q %q", Name(), Version())
	}
	cfg = Config{Name: "xone.demo.app", Version: "v1.2.0"}
	if Name() != "xone.demo.app" || Version() != "v1.2.0" {
		t.Errorf("the configured values must be read, got=%q %q", Name(), Version())
	}
}

func keepCfg(t *testing.T) {
	t.Helper()
	old := cfg
	t.Cleanup(func() { cfg = old })
}

func TestLoadConfig_ReadsAppBlock(t *testing.T) {
	// The service name and version are taken by tracing and metrics as service.name / service.version,
	// and without this block being read the whole service is anonymous on the dashboards.
	keepCfg(t)
	cfg = DefaultConfig()
	xonetest.UseConfigYAML(t, "XApp:\n  Name: xone.demo.app\n  Version: v1.2.0\n")

	if err := loadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if Name() != "xone.demo.app" || Version() != "v1.2.0" {
		t.Errorf("the configuration was not read, got=%q %q", Name(), Version())
	}
}

func TestLoadConfig_StaysEmptyWhenUnset(t *testing.T) {
	keepCfg(t)
	cfg = DefaultConfig()
	xonetest.UseConfigYAML(t, "XLog:\n  Level: info\n")

	if err := loadConfig(context.Background()); err != nil {
		t.Fatalf("not configuring it must not be an error: %v", err)
	}
	if Name() != "" || Version() != "" {
		t.Errorf("with nothing configured it must be empty, got=%q %q", Name(), Version())
	}
}

func TestLoadConfig_BadConfigFailsStartup(t *testing.T) {
	keepCfg(t)
	xonetest.UseConfigYAML(t, "XApp:\n  Nmae: demo\n")

	if err := loadConfig(context.Background()); err == nil {
		t.Fatal("a misspelled field must fail startup, otherwise the service name stays empty and nobody knows")
	}
}

func TestLoadConfig_DoesNotCarryPreviousValue(t *testing.T) {
	// A second Run in the same process (common in tests): the fields this run leaves unset must return to their
	// defaults rather than carrying over the previous service name.
	keepCfg(t)
	cfg = Config{Name: "last.run", Version: "v0"}
	xonetest.UseConfigYAML(t, "XApp:\n  Version: v1.2.0\n")

	if err := loadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if Name() != "" || Version() != "v1.2.0" {
		t.Errorf("only this run's configured values may remain, got=%q %q", Name(), Version())
	}
}

func TestLoadConfig_DecodeFailureKeepsCurrentValue(t *testing.T) {
	keepCfg(t)
	cfg = Config{Name: "kept"}
	xonetest.UseConfigYAML(t, "XApp:\n  Name: half\n  Version: [1, 2]\n")

	if err := loadConfig(context.Background()); err == nil {
		t.Fatal("a wrong type must be an error")
	}
	if Name() != "kept" || Version() != "" {
		t.Errorf("on failure the existing values must be left alone, got=%q %q", Name(), Version())
	}
}

func TestLoadConfig_LoadsBeforeReadingIfNotLoaded(t *testing.T) {
	// Reading early must also give the values from the file, not a silent set of defaults.
	keepCfg(t)
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte("XApp:\n  Name: early\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	t.Cleanup(config.Reset)
	t.Setenv(config.EnvKey, p)

	if err := loadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if Name() != "early" {
		t.Errorf("the values from the file must be read, got=%q", Name())
	}
}
