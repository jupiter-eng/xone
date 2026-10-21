// Package coreonly is a program importing the root package alone: a test binary of its own, whose
// registration board holds nothing but the hooks the framework brings along itself.
//
// The tests here must not import another xone package (not even xlog or xapp): that would make it
// impossible to verify here whether the root package brings them along, and whether it brings xlog
// along as well. Whatever needs xlog goes into withhandler
package coreonly

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone"
)

func TestRun_XAppWorksWithCoreOnly(t *testing.T) {
	// The XApp block comes along with the framework: an XApp.Name written by a program that never
	// imported xapp must not count as a key nobody read
	cfg := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(cfg, []byte("XApp:\n  Name: core.only\n  Version: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := xone.Run(xone.Func(func(context.Context) error { return nil }),
		xone.WithConfigPath(cfg), xone.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("importing the root package alone and writing an XApp must still start as usual: %v", err)
	}
}

func TestRun_CoreOnlyLeavesSlogDefaultAlone(t *testing.T) {
	// xlog does not come along with the root package: in a program using only the core (and data
	// integrations such as xgorm), slog.Default stays the one it set itself
	var buf bytes.Buffer
	mine := slog.New(slog.NewTextHandler(&buf, nil))
	old := slog.Default()
	slog.SetDefault(mine)
	t.Cleanup(func() { slog.SetDefault(old) })
	cfg := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(cfg, []byte("XApp:\n  Name: core.only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := xone.Run(xone.Func(func(context.Context) error { slog.Info("core only"); return nil }), xone.WithConfigPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if slog.Default() != mine || !strings.Contains(buf.String(), "msg=\"core only\"") {
		t.Errorf("importing the root package alone must leave slog.Default alone, got=%q", buf.String())
	}
}

func TestRun_XLogWithoutImportNamesTheImport(t *testing.T) {
	// An XLog written without xlog imported: the error gives the line to add outright, rather than
	// leaving somebody to guess which package it is
	cfg := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(cfg, []byte("XLog:\n  Level: debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := xone.Run(xone.Func(func(context.Context) error { return nil }),
		xone.WithConfigPath(cfg), xone.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err == nil || !strings.Contains(err.Error(), `import _ "github.com/jupiter-eng/xone/xlog"`) {
		t.Errorf("this must fail and give xlog's import, got=%v", err)
	}
}
