package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withArgs swaps the command line out for the length of the test
func withArgs(t *testing.T, args ...string) {
	t.Helper()
	old := os.Args
	os.Args = append([]string{"svc"}, args...)
	t.Cleanup(func() { os.Args = old })
}

func TestLocate_FlagTakesPrecedence(t *testing.T) {
	withArgs(t, "--config=/a/b.yml")
	t.Setenv(EnvKey, "/from/env.yml")

	if got, _ := locate(); got != "/a/b.yml" {
		t.Errorf("the command line must take precedence over the environment, got=%q", got)
	}
}

func TestLocate_FlagBothForms(t *testing.T) {
	t.Run("with an equals sign", func(t *testing.T) {
		withArgs(t, "--config=/a.yml")
		if got, _ := locate(); got != "/a.yml" {
			t.Errorf("got=%q", got)
		}
	})
	t.Run("with a space", func(t *testing.T) {
		withArgs(t, "--config", "/b.yml")
		if got, _ := locate(); got != "/b.yml" {
			t.Errorf("got=%q", got)
		}
	})
	t.Run("with a single dash", func(t *testing.T) {
		withArgs(t, "-config=/c.yml")
		if got, _ := locate(); got != "/c.yml" {
			t.Errorf("got=%q", got)
		}
	})
}

func TestLocate_LeavesOtherArgsAlone(t *testing.T) {
	// A caller's program has command-line arguments of its own, which the framework must not
	// misread
	withArgs(t, "--port", "8080", "--configx=/x.yml", "--verbose")
	t.Setenv(EnvKey, "")

	if got, _ := locate(); got != "" && got != SearchPaths[0] {
		t.Errorf("another argument must not be taken for the configuration path, got=%q", got)
	}
}

func TestLocate_EnvVarIsSecond(t *testing.T) {
	withArgs(t)
	t.Setenv(EnvKey, "/from/env.yml")

	if got, _ := locate(); got != "/from/env.yml" {
		t.Errorf("got=%q", got)
	}
}

func TestLocate_SearchesConventionalPaths(t *testing.T) {
	withArgs(t)
	t.Setenv(EnvKey, "")

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "conf"), 0o755)
	os.WriteFile(filepath.Join(dir, "conf", "application.yml"), []byte("{}"), 0o600)
	chdir(t, dir)

	if got, _ := locate(); got != "conf/application.yml" {
		t.Errorf("a conventional path must be matched, got=%q", got)
	}
}

func TestLocate_ReturnsEmptyWhenNothingFound(t *testing.T) {
	withArgs(t)
	t.Setenv(EnvKey, "")
	chdir(t, t.TempDir())

	if got, _ := locate(); got != "" {
		t.Errorf("finding none must return an empty string and leave the caller to decide, got=%q", got)
	}
}

// chdir switches the working directory and switches back when the test ends.
//
// testing.T.Chdir is not used: it needs Go 1.24, while the core module's go directive is the
// language version floor for every caller -- raising that floor by two versions for one test helper
// is not worth it.
// The price is going without its check against t.Parallel, so a test using it must not run in
// parallel.
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func TestEnsure_FlagWithoutValueIsError(t *testing.T) {
	// A --config written last with its value forgotten used to be ignored quietly, after which
	// XONE_CONFIG and the conventional paths were tried and another configuration came up -- or one
	// of nothing but defaults
	for _, key := range []string{ArgKey, ProfileArgKey} {
		fresh(t)
		t.Setenv(EnvKey, write(t, "Demo:\n  Addr: from-env\n"))
		withArgs(t, "serve", "--"+key)
		err := Ensure("", quiet())
		if err == nil || !strings.Contains(err.Error(), "--"+key+" needs a value") {
			t.Errorf("--%s with no value must be an error, got=%v", key, err)
		}
	}
}

func TestEnsure_FlagFollowedByAnotherFlagIsError(t *testing.T) {
	// The value is missing just as much when the next flag follows it, and taking that flag as the
	// value points the error at a file nobody named: "config file does not exist: --profile=dev".
	// A value that really begins with a - is written as --config=-weird
	for _, args := range [][]string{
		{"serve", "--" + ArgKey, "--" + ProfileArgKey + "=dev"},
		{"serve", "--" + ArgKey, "-v"},
		{"serve", "--" + ProfileArgKey, "--" + ArgKey + "=/etc/a.yml"},
	} {
		fresh(t)
		withArgs(t, args...)
		err := Ensure("", quiet())
		if err == nil || !strings.Contains(err.Error(), "needs a value") {
			t.Errorf("%v: a flag with the next flag behind it must be an error, got=%v", args[1:], err)
		}
	}
}

func TestLocate_ValueStartingWithDashIsWrittenWithEquals(t *testing.T) {
	// The escape hatch for the spelling above: the = form takes whatever follows it
	withArgs(t, "serve", "--"+ArgKey+"=-weird.yml")
	if got, _ := locate(); got != "-weird.yml" {
		t.Errorf("--%s=-weird.yml should give -weird.yml, got=%q", ArgKey, got)
	}
}
