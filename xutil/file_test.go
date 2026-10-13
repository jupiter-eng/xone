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
		t.Error("存在的文件应返回 true")
	}
	if FileExist(dir) {
		t.Error("目录不是文件，应返回 false")
	}
	if FileExist(filepath.Join(dir, "nope")) {
		t.Error("不存在的路径应返回 false")
	}
}

func TestDirExist(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	os.WriteFile(f, []byte("x"), 0o600)

	if !DirExist(dir) {
		t.Error("存在的目录应返回 true")
	}
	if DirExist(f) {
		t.Error("文件不是目录，应返回 false")
	}
}
