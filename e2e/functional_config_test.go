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

// A malformed configuration keeps the process from coming up, and the error names the item.
// docs/config.md, "The common rules":
//
//	a misspelled field       the start fails rather than ignoring it silently
//	an unclaimed extra block the start fails, with a hint that the matching package may not be imported
//	${VAR}                   required, and the start fails when it is unset
//
// The malformed item is laid over with Options.Overlay (a profile), the way it is written in
// production
func TestFunctional_BadConfigFailsStartupAndNamesTheField(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	unset := "E2E_UNSET_" + strings.ToUpper(harness.NewID())
	for _, c := range []struct {
		name    string
		overlay string
		want    []string // the fragments stderr has to hold
	}{
		{"a misspelled top-level key", "XGinn:\n  Port: 1\n", []string{"XGinn", "not read by anyone", "whether the matching package is imported"}},
		{"a misspelled business top-level block", "Servcie:\n  Table: x\n", []string{"Servcie", "not read by anyone"}},
		{"a misspelled field inside XRedis", "XRedis:\n  Adrr: 127.0.0.1:6379\n", []string{"XRedis", "field Adrr not found"}},
		{"a misspelled field inside XGin", "XGin:\n  Prot: 1\n", []string{"XGin", "field Prot not found"}},
		{"a misspelled nested field", "XLog:\n  File:\n    Enabel: true\n", []string{"XLog", "field Enabel not found"}},
		{"a misspelled field in the business block", "Service:\n  Tabel: x\n", []string{"Service", "field Tabel not found"}},
		{"${VAR} unset", "Service:\n  Downstream: \"${" + unset + "}\"\n", []string{"environment variables not set", unset}},
		{"${VAR} unset (in a framework block, inside a list)", "XGin:\n  TrustedProxies: [\"${" + unset + "}\"]\n", []string{"environment variables not set", unset}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := harness.Start(t, harness.Options{Overlay: c.overlay, NoWait: true})
			exit, ok := p.Wait(30 * time.Second)
			if !ok {
				t.Fatalf("the documentation says this fails the start, and the process is still running after 30s\n%s", p.Output())
			}
			if exit.Code == 0 || exit.Signal != nil {
				t.Fatalf("the documentation says the start fails, got %v\n%s", exit, p.Output())
			}
			stderr := p.Stderr()
			for _, w := range c.want {
				if !strings.Contains(stderr, w) {
					t.Errorf("the error should name the item (holding %q), got stderr:\n%s", w, stderr)
				}
			}
			// Not coming up means not one request was taken: xgin never started listening
			if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgin listening" })) != 0 {
				t.Errorf("the service should not start listening with a malformed configuration, and xgin listening turned up")
			}
			t.Logf("numbers: %s -> it exited after %v (%v); the error: %s", c.name, exit.Uptime, exit, lastLine(stderr))
		})
	}

	// The error for a misspelled field names the file and the line. docs/config.md promises "the file
	// and both line numbers" for a duplicate key, and a line number is there for someone to find that
	// line in the configuration file; what is reported should be that line of that file.
	// The strict decoding used to yaml.Marshal the section into text and decode that, so it reported
	// the line number inside that text (line 70 reported as line 2) and no file name at all -- which
	// left a typo in a profile file with nowhere to look
	wantAt := func(t *testing.T, p *harness.Process, file string, line int) {
		t.Helper()
		if _, ok := p.Wait(30 * time.Second); !ok {
			t.Fatalf("a misspelled field should fail the start, and it is still running after 30s")
		}
		want := fmt.Sprintf("%s:%d: field Prot not found", file, line)
		if !strings.Contains(p.Stderr(), want) {
			t.Errorf("the error should name the file and the line (holding %q), got stderr:\n%s", want, p.Stderr())
		}
		t.Logf("numbers: the error is %s", lastLine(p.Stderr()))
	}
	t.Run("a misspelled field reports the configuration file and that line", func(t *testing.T) {
		t.Parallel()
		base, err := os.ReadFile(filepath.Join(harness.ModuleDir(), "service", "application.yml"))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(base), "\n")
		at := -1
		for i, l := range lines {
			if l == "  Port: ${E2E_PORT}" {
				lines[i], at = "  Prot: ${E2E_PORT}", i+1
			}
		}
		if at < 0 {
			t.Fatal("the Port line of XGin cannot be found in service/application.yml")
		}
		cfg := filepath.Join(t.TempDir(), "typo.yml")
		if err := os.WriteFile(cfg, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		wantAt(t, harness.Start(t, harness.Options{Config: cfg, NoWait: true}), cfg, at)
	})
	t.Run("a typo in a profile file reports the profile file and that line", func(t *testing.T) {
		t.Parallel()
		// The XGin block merges with the one in base, and Prot comes from line 4 of the profile file
		p := harness.Start(t, harness.Options{Overlay: "# overlay\n\nXGin:\n  Prot: 1\n", NoWait: true})
		wantAt(t, p, filepath.Join(p.Dir, "application-e2e.yml"), 4)
	})
}

// lastLine is the last non-empty line, for the logs
func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	return ls[len(ls)-1]
}
