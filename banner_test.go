package xone

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
)

// plain strips the colour codes, leaving the text alone
func plain(s string) string { return regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(s, "") }

func TestPrintBanner_PrintsArtAndVersionOnTerminal(t *testing.T) {
	var b bytes.Buffer
	printBanner(&b, true)
	out := plain(b.String())
	for _, want := range append(bannerText, ":: xone ::", version()) {
		if !strings.Contains(out, want) {
			t.Errorf("the banner must contain %q, got=\n%s", want, out)
		}
	}
}

func TestPrintBanner_WritesNothingWhenNotTerminal(t *testing.T) {
	// A container, a redirection, a log collector downstream: multi-line ASCII art is unparsable
	// garbage in the logging platform
	var b bytes.Buffer
	printBanner(&b, false)
	if b.Len() != 0 {
		t.Errorf("no banner may be written when this is not a terminal, got=%q", b.String())
	}
}

func TestIsTerminal_PipesAndFilesAreNotTerminals(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(w) {
		t.Error("a pipe must not be taken for a terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("an ordinary file must not be taken for a terminal")
	}
}

func TestRun_StderrNotTerminal_NoBanner(t *testing.T) {
	// The call site: Run has to decide by looking at stderr, not print regardless
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := stderr
	stderr = w
	t.Cleanup(func() { stderr = old })

	comps(t)
	if err := Run(Func(func(context.Context) error { return nil }), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
		t.Fatal(err)
	}
	w.Close()
	out, _ := io.ReadAll(r)
	if strings.Contains(plain(string(out)), bannerText[0]) {
		t.Errorf("no banner may be printed while stderr is a pipe, got=%q", out)
	}
}

func TestModuleVersion_ComesFromBuildInfo(t *testing.T) {
	for name, c := range map[string]struct {
		bi   debug.BuildInfo
		want string
	}{
		"released as a dependency":   {debug.BuildInfo{Deps: []*debug.Module{{Path: modulePath, Version: "v0.2.0"}}}, "v0.2.0"},
		"replaced with a local path": {debug.BuildInfo{Deps: []*debug.Module{{Path: modulePath, Version: "v0.2.0", Replace: &debug.Module{Path: "../xone"}}}}, "(devel)"},
		"this repository itself":     {debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "(devel)"}}, "(devel)"},
		"does not depend on it":      {debug.BuildInfo{Main: debug.Module{Path: "example.com/app"}}, "(devel)"},
	} {
		if got := moduleVersion(&c.bi); got != c.want {
			t.Errorf("%s: got=%q want=%q", name, got, c.want)
		}
	}
}

func TestRun_XONE_DEBUGListsStartHookOrder(t *testing.T) {
	var b bytes.Buffer
	old := config.DebugOut
	config.DebugOut = &b
	t.Cleanup(func() { config.DebugOut = old })
	t.Setenv(config.DebugEnvKey, "1")

	r := &recorder{}
	comps(t, comp("db", hook.StageClient, r, nil), comp("log", hook.StageLog, r, nil))
	if err := Run(Func(func(context.Context) error { return nil }), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	i, j := strings.Index(out, "Log"), strings.Index(out, "Client")
	if !strings.Contains(out, "start hooks, in order") || i < 0 || j < 0 || i > j {
		t.Errorf("the start hooks must be listed in the order they run (Log before Client), got=\n%s", out)
	}
}

func TestRun_NoDebugOutputWithoutXONE_DEBUG(t *testing.T) {
	var b bytes.Buffer
	old := config.DebugOut
	config.DebugOut = &b
	t.Cleanup(func() { config.DebugOut = old })
	t.Setenv(config.DebugEnvKey, "")

	comps(t)
	if err := Run(Func(func(context.Context) error { return nil }), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
		t.Fatal(err)
	}
	if b.Len() != 0 {
		t.Errorf("no debug output may appear without XONE_DEBUG, got=\n%s", b.String())
	}
}
