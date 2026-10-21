package xutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileExist(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	os.WriteFile(f, []byte("x"), 0o600)

	if !FileExist(f) {
		t.Error("an existing file must give true")
	}
	if FileExist(dir) {
		t.Error("a directory is not a file and must give false")
	}
	if FileExist(filepath.Join(dir, "nope")) {
		t.Error("a path that does not exist must give false")
	}
}

func TestDirExist(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	os.WriteFile(f, []byte("x"), 0o600)

	if !DirExist(dir) {
		t.Error("an existing directory must give true")
	}
	if DirExist(f) {
		t.Error("a file is not a directory and must give false")
	}
}
