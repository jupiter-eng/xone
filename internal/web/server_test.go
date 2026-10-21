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

// The full start and stop cases (force-closing at the deadline, waiting for the handlers, h2c,
// TLS) live in xgin and run against a real gin server; only a few boundaries are pinned here

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
	// Whoever's Start the error came out of is who reported it: a caller's xerror.Is(err, "xgin")
	// has to hold
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
	// A regression case: with http.Server.ErrorLog unset, net/http writes to the standard library's
	// log (a failed TLS handshake, an Accept error and so on), which reaches slog as INFO with the
	// whole line as the message, different every time, and can be neither searched for nor alerted
	// on
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	srv := (&Server{}).newServer("xtest", ServerConfig{}, "127.0.0.1:0", http.NotFoundHandler())
	if srv.ErrorLog == nil {
		t.Fatal("ErrorLog must be connected to slog")
	}
	srv.ErrorLog.Printf("http: TLS handshake error from %s: EOF", "203.0.113.9:1234")
	if want := `"level":"WARN","msg":"xtest http server error","error":"http: TLS handshake error from 203.0.113.9:1234: EOF"`; !strings.Contains(buf.String(), want) {
		t.Errorf("this must be recorded as %s, got=%s", want, buf.String())
	}
}

func TestServerTLS_EmptyMinVersionMeansTLS12(t *testing.T) {
	// An empty MinVersion takes the default: 1.2 written out, rather than falling through to
	// tls.Config's zero value (which is whatever default the Go version brings)
	cfg, err := ServerConfig{CertFile: "c", KeyFile: "k"}.serverTLS()
	if err != nil || cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("left empty this must be TLS 1.2, got=%v err=%v", cfg, err)
	}
}
