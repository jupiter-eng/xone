// Package coreonly 只 import 根包的程序：单独一个测试二进制，登记表里只有框架自己带来的钩子。
//
// 这里的测试不能再 import 别的 xone 包（哪怕是 xlog、xapp）：那样根包带没带它们、
// 有没有多带 xlog，这里就验不出来了。要用 xlog 的放到 withhandler
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
	// XApp 块跟着框架一起来：没 import xapp 的程序写了 XApp.Name，也不该被当成没人读的 key
	cfg := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(cfg, []byte("XApp:\n  Name: core.only\n  Version: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := xone.Run(xone.Func(func(context.Context) error { return nil }),
		xone.WithConfigPath(cfg), xone.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("只 import 根包、写了 XApp 也该正常启动：%v", err)
	}
}

func TestRun_CoreOnlyLeavesSlogDefaultAlone(t *testing.T) {
	// xlog 不跟着根包来：只用核心（和 xgorm 这类数据集成）的程序，slog.Default 还是它自己设的那个
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
		t.Errorf("只 import 根包，slog.Default 不该被换掉，got=%q", buf.String())
	}
}

func TestRun_XLogWithoutImportNamesTheImport(t *testing.T) {
	// 写了 XLog 却没 import xlog：报错直接给出要加的那一行，不让人猜是哪个包
	cfg := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(cfg, []byte("XLog:\n  Level: debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := xone.Run(xone.Func(func(context.Context) error { return nil }),
		xone.WithConfigPath(cfg), xone.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err == nil || !strings.Contains(err.Error(), `import _ "github.com/jupiter-eng/xone/xlog"`) {
		t.Errorf("该报错并给出 xlog 的 import，got=%v", err)
	}
}
