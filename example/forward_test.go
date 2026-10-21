package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"

	"github.com/jupiter-eng/xone/xgin"
	"github.com/jupiter-eng/xone/xtrace"
)

// The trust boundary for forwarded headers spans two modules: xgin decides about the directly
// connected peer by TrustedProxies, and xtrace accepts only what was judged trusted. The two meet
// through TrustedPeer() on the carrier, and each one's unit tests see only its own half -- the day
// the method name changes on one side, both sides' tests stay green while forwarding accepts nothing
// at all. This case joins the two halves together.
func TestXGinAndXTrace_ForwardHeadersOnlyFromTrustedPeers(t *testing.T) {
	oldTP, oldProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(oldTP); otel.SetTextMapPropagator(oldProp) })

	tc := xtrace.DefaultConfig()
	tc.ForwardHeaders = []string{"X-Tenant-Id"}
	tr, closer, err := xtrace.New(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	tr.Install()

	for name, c := range map[string]struct {
		trustedProxies []string
		want           string
	}{
		"the peer is in TrustedProxies": {[]string{"127.0.0.1"}, "t1"},
		"nobody is trusted by default":  {nil, ""},
	} {
		if got := forwardedTenant(t, c.trustedProxies); got != c.want {
			t.Errorf("%s: the forwarded value the handler saw=%q, want %q", name, got, c.want)
		}
	}
}

// forwardedTenant starts a real xgin server, makes one call to it from this host carrying an
// X-Tenant-Id, and returns the forwarded value the handler took out of its ctx
func forwardedTenant(t *testing.T, trustedProxies []string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	cfg := xgin.DefaultConfig()
	cfg.Host, cfg.Port, cfg.TrustedProxies = "127.0.0.1", port, trustedProxies
	cfg.Log, cfg.Metric = false, false
	g := xgin.New().WithConfig(cfg).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/tenant", func(c *gin.Context) {
				c.String(http.StatusOK, xtrace.ForwardHeaderFromContext(c.Request.Context(), "X-Tenant-Id"))
			})
		})
	go func() { _ = g.Start(context.Background()) }()
	defer g.Stop(context.Background())

	url := fmt.Sprintf("http://127.0.0.1:%d/tenant", port)
	for i := 0; i < 100; i++ {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("X-Tenant-Id", "t1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(body)
	}
	t.Fatal("the server never came up")
	return ""
}
