//go:build go1.24

package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/xerror"
)

// 启停的完整用例（超时强断、等 handler、h2c、TLS）在 xgin 里，经真实的 gin 服务跑；这里只钉几条边界

func TestServer_StopBeforeStartSkipsListening(t *testing.T) {
	s := &Server{}
	if err := s.Stop(context.Background(), "xtest"); err != nil {
		t.Fatalf("Stop before Start should be a no-op, got %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Start("xtest", ServerConfig{Host: "127.0.0.1", Port: 1}, http.NotFoundHandler()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start after Stop should return nil without listening, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start after Stop should return immediately")
	}
}

func TestServer_ErrorsCarryTheIntegrationsModule(t *testing.T) {
	// 错误出自谁的 Start，就算谁报的：调用方 xerror.Is(err, "xgin") 要成立
	s := &Server{}
	err := s.Start("xtest", ServerConfig{CertFile: "c", KeyFile: "k", ClientCAFile: "/nonexistent/ca.pem"}, http.NotFoundHandler())
	if !xerror.Is(err, "xtest") {
		t.Fatalf("want an xerror from module xtest, got %v", err)
	}
}

func TestShutdownCtx_LeavesTailForHandlers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	shut, cancelShut := shutdownCtx(ctx)
	defer cancelShut()
	outer, _ := ctx.Deadline()
	inner, _ := shut.Deadline()
	if tail := outer.Sub(inner); tail < 300*time.Millisecond || tail > time.Second {
		t.Errorf("Shutdown should end ~20%% (max 1s) before the caller's deadline, tail=%v", tail)
	}

	shut, cancelShut = shutdownCtx(context.Background())
	defer cancelShut()
	if _, ok := shut.Deadline(); ok {
		t.Error("no caller deadline means no Shutdown deadline either")
	}
}

func TestServer_ErrorLogRoutedToSlog(t *testing.T) {
	// 回归用例：http.Server.ErrorLog 不设时 net/http 写标准库的 log（TLS 握手失败、Accept 出错……）：
	// 进了 slog 也是 INFO，消息是每次都不一样的那一整行，没法按消息检索和告警
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	srv := (&Server{}).newServer("xtest", ServerConfig{}, "127.0.0.1:0", http.NotFoundHandler())
	if srv.ErrorLog == nil {
		t.Fatal("ErrorLog 该接到 slog 上")
	}
	srv.ErrorLog.Printf("http: TLS handshake error from %s: EOF", "203.0.113.9:1234")
	if want := `"level":"WARN","msg":"xtest http server error","error":"http: TLS handshake error from 203.0.113.9:1234: EOF"`; !strings.Contains(buf.String(), want) {
		t.Errorf("该记成 %s，got=%s", want, buf.String())
	}
}

func TestServerTLS_EmptyMinVersionMeansTLS12(t *testing.T) {
	// MinVersion 留空取默认：写明 1.2，不落到 tls.Config 的零值（那是随 Go 版本变的默认）
	cfg, err := ServerConfig{CertFile: "c", KeyFile: "k"}.serverTLS()
	if err != nil || cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("留空该是 TLS 1.2，got=%v err=%v", cfg, err)
	}
}
