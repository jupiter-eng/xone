package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The README's getting-started section is the first piece of code a caller copies. Change the public
// API without changing it and the first thing a caller copying it gets is a compile error -- so it is
// compiled here as the code it is.

func TestREADME_QuickStartCodeCompiles(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	src := firstGoBlock(string(readme))
	if src == "" {
		t.Fatal("no ```go block found in README.md; was the getting-started section moved?")
	}
	if !strings.Contains(src, "package main") {
		t.Fatalf("the README's first go block must be a complete main.go, got:\n%s", src)
	}

	// Compiled as a file in a temporary directory: the package is command-line-arguments and the
	// imports resolve against the example module the current directory belongs to, which is the code
	// in this repository
	path := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := exec.Command("go", "vet", path) // vet compiles first and then checks, catching one layer more than build
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("the README's getting-started code does not compile: %v\n%s\n---- code ----\n%s", err, out.String(), src)
	}
}

// firstGoBlock takes the contents of the first ```go block in the markdown, or an empty string if
// there is none
func firstGoBlock(md string) string {
	_, rest, ok := strings.Cut(md, "\n```go\n")
	if !ok {
		return ""
	}
	body, _, ok := strings.Cut(rest, "\n```")
	if !ok {
		return ""
	}
	return body + "\n"
}
