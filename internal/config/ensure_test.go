package config

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/jupiter-eng/xone/xerror"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fresh goes back to "nothing has been loaded" and clears the environment variables that affect
// locate
func fresh(t *testing.T) {
	t.Helper()
	Reset()
	t.Cleanup(Reset)
	t.Setenv(EnvKey, "")
}

func TestUnmarshal_LoadsFirstIfNotLoaded(t *testing.T) {
	// What this guards against: reading early quietly yields zero values and the service comes up
	// with a set of defaults. The first read now loads first: reading early gets the final value
	// from the file as well
	fresh(t)
	t.Setenv(EnvKey, write(t, "Demo:\n  Addr: from-file\n"))

	c := defaults()
	if err := Unmarshal("Demo", &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "from-file" {
		t.Errorf("what is read must be the value from the file, got=%q", c.Addr)
	}
}

func TestEnsure_ReusesEarlierLoad(t *testing.T) {
	// A block read early has been claimed already, and Run taking over must not clear that claim, or
	// it would be reported as "nobody read it" and fail startup
	fresh(t)
	p := write(t, "Demo:\n  Addr: x\n")
	t.Setenv(EnvKey, p)

	c := defaults()
	if err := Unmarshal("Demo", &c); err != nil {
		t.Fatal(err)
	}
	if err := Ensure("", quiet()); err != nil {
		t.Fatalf("with no file named it must reuse the one already loaded, got=%v", err)
	}
	if err := Ensure(p, quiet()); err != nil {
		t.Fatalf("naming the very file already loaded must not be an error, got=%v", err)
	}
	if got := Unclaimed(); len(got) != 0 {
		t.Errorf("a block read early must count as claimed, got=%v", got)
	}
}

func TestEnsure_SameFileSpelledDifferentlyIsFine(t *testing.T) {
	// They used to be compared as strings: dir/./b.yml and dir/b.yml counted as two files and Run
	// failed to start
	p := write(t, "Demo:\n  Addr: x\n")
	dir := filepath.Dir(p)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, p)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.yml")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	for name, same := range map[string]string{
		"with a ./":         dir + string(filepath.Separator) + "." + string(filepath.Separator) + filepath.Base(p),
		"with a ../":        filepath.Join(dir, "sub") + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(p),
		"a relative path":   rel,
		"through a symlink": link,
	} {
		t.Run(name, func(t *testing.T) {
			fresh(t)
			t.Setenv(EnvKey, p)
			c := defaults()
			if err := Unmarshal("Demo", &c); err != nil {
				t.Fatal(err)
			}
			if err := Ensure(same, quiet()); err != nil {
				t.Errorf("%s and %s are the same file and must not be an error, got=%v", same, p, err)
			}
		})
	}
}

func TestEnsure_FailsWhenNamedFileDiffersFromEarlierLoad(t *testing.T) {
	// Swapping in another one quietly would leave what was read early and what the components read
	// afterwards out of step
	fresh(t)
	t.Setenv(EnvKey, write(t, "Demo:\n  Addr: a\n"))
	c := defaults()
	if err := Unmarshal("Demo", &c); err != nil {
		t.Fatal(err)
	}

	err := Ensure(write(t, "Demo:\n  Addr: b\n"), quiet())
	if err == nil {
		t.Fatal("naming another file must be an error rather than quietly replacing the one already read")
	}
	if !strings.Contains(err.Error(), "--"+ArgKey) || !strings.Contains(err.Error(), EnvKey) {
		t.Errorf("the error has to tell the caller how to fix it, got=%v", err)
	}
}

func TestEnsure_UsesDefaultsWhenNoConfigFile(t *testing.T) {
	// This used to warn once while the configuration stayed at "nothing has been loaded": every
	// integration reading its configuration afterwards reported "read too early", and a service
	// needing no configuration at all could not start
	fresh(t)
	chdir(t, t.TempDir()) // nothing at the conventional paths

	if err := Ensure("", quiet()); err != nil {
		t.Fatalf("with no configuration file it must only warn, got=%v", err)
	}
	c := defaults()
	if err := Unmarshal("Demo", &c); err != nil {
		t.Fatalf("with no configuration file a read must get the defaults, got=%v", err)
	}
	if c != defaults() {
		t.Errorf("the defaults must be kept as they are, got=%+v", c)
	}
}

func TestEnsure_MissingNamedFileIsError(t *testing.T) {
	fresh(t)
	err := Ensure(filepath.Join(t.TempDir(), "nope.yml"), quiet())
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a named file that cannot be found must be an error, got=%v", err)
	}
}

func TestUnmarshal_SameErrorOnEveryReadAfterLoadFailure(t *testing.T) {
	// A malformed configuration must not quietly count as unconfigured on the second read, nor be
	// parsed again on every read
	fresh(t)
	t.Setenv(EnvKey, write(t, "Demo: [\n"))

	c := defaults()
	first := Unmarshal("Demo", &c)
	if first == nil {
		t.Fatal("a malformed configuration must be an error")
	}
	if again := Unmarshal("Demo", &c); !errors.Is(again, first) {
		t.Errorf("the second read must report the same error, got=%v", again)
	}
	if err := Ensure("", quiet()); !errors.Is(err, first) {
		t.Errorf("Run taking over must report the same error too, got=%v", err)
	}
}

func TestReset_RelocatesAndReloadsAfterward(t *testing.T) {
	// The configuration belongs to one Run: the next Run in the same process has to read one of its
	// own
	fresh(t)
	if err := Load(write(t, "Demo:\n  Addr: a\n")); err != nil {
		t.Fatal(err)
	}
	Reset()
	t.Setenv(EnvKey, write(t, "Demo:\n  Addr: b\n"))

	c := defaults()
	if err := Unmarshal("Demo", &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "b" {
		t.Errorf("after Reset the new one must be read, got=%q", c.Addr)
	}
}

func TestUnmarshalAndDecodeStrict_BadTargetIsErrorNotPanic(t *testing.T) {
	// A value passed instead of a pointer, or a nil: both used to panic on the spot (reflect taking
	// Kind on a nil type, yaml writing into a value that cannot be addressed) and the whole process
	// died inside a start hook
	if err := Load(write(t, "Demo:\n  Addr: a:1\n")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(Reset)
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("Addr: a:1\n"), &node); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]any{
		"nil": nil, "value": demoConf{}, "nil pointer": (*demoConf)(nil),
	} {
		check := func(what string, err error) {
			t.Helper()
			var xe *xerror.Error
			if !errors.As(err, &xe) || xe.Module != "xconfig" || xe.Op != "config" ||
				!strings.Contains(err.Error(), "non-nil pointer") {
				t.Errorf("%s(%s) must return an xconfig config error, got=%v", what, name, err)
			}
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Unmarshal(%s) panicked: %v", name, r)
				}
			}()
			check("Unmarshal", Unmarshal("Demo", target))
			check("Unmarshal on an unconfigured block", Unmarshal("NotConfigured", target))
		}()
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("DecodeStrict(%s) panicked: %v", name, r)
				}
			}()
			check("DecodeStrict", DecodeStrict(&node, target))
		}()
	}
}
