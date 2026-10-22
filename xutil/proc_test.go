package xutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutableName_IsTheProgramsOwnFileName(t *testing.T) {
	// The point of the fallback is that it tells services apart, so what matters is that it is the
	// program's own name and carries no directory with it.
	got := ExecutableName()
	if got == "" {
		t.Fatal("the running test binary has a name, so this must not be empty")
	}
	if strings.ContainsRune(got, filepath.Separator) {
		t.Errorf("a path would be useless as a client id or a service name, got=%q", got)
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Base(path); got != want {
		t.Errorf("got=%q, want the executable's base name %q", got, want)
	}
}
