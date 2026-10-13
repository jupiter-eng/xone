// Package withhandler 根包加上 xlog.UseHandler 的程序。和 coreonly 分开：这里要 import xlog，
// 放进 coreonly 的话，「根包不带 xlog」就验不出来了
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
	// 换了后端，框架自己的启停日志和业务日志一样写进它，不会分成两条路
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
		t.Errorf("业务日志和框架的停止日志都该写进自己的 handler，got=%s", out)
	}
}
