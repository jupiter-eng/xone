// Package withhandler is a program using the root package plus xlog.UseHandler. It is kept apart
// from coreonly because it has to import xlog, and inside coreonly that would make "the root
// package does not bring xlog along" impossible to verify
package withhandler

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/xlog"
)

func TestRun_UseHandlerReceivesFrameworkLogs(t *testing.T) {
	// With the backend swapped, the framework's own start and stop logs go into it just as the
	// business logs do, rather than splitting into two paths
	var buf bytes.Buffer
	xlog.UseHandler(slog.NewJSONHandler(&buf, nil))
	t.Cleanup(func() { xlog.UseHandler(nil) })
	cfg := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(cfg, []byte("XApp:\n  Name: core.only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := xone.Run(xone.Func(func(ctx context.Context) error { slog.InfoContext(ctx, "business"); return nil }),
		xone.WithConfigPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"msg":"business"`) || !strings.Contains(out, "xlog.closeXLog") {
		t.Errorf("both the business logs and the framework's stop logs must go into the handler of your own, got=%s", out)
	}
}
