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

// 透传 Header 的信任边界横跨两个模块：xgin 按 TrustedProxies 判断直连的对端，
// xtrace 只收被判为可信的那些。两边靠 carrier 上的 TrustedPeer() 接头，
// 各自的单元测试只看得见自己那一半——方法名哪天在一边改了，
// 两边的测试照样全绿，透传却从此一个都不收。这条把两半接起来测。
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
		"对端在 TrustedProxies 里": {[]string{"127.0.0.1"}, "t1"},
		"默认谁都不信":               {nil, ""},
	} {
		if got := forwardedTenant(t, c.trustedProxies); got != c.want {
			t.Errorf("%s：handler 看到的透传值=%q，want %q", name, got, c.want)
		}
	}
}

// forwardedTenant 起一个真的 xgin 服务，从本机带着 X-Tenant-Id 打一次，
// 返回 handler 从 ctx 里取到的透传值
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
	t.Fatal("服务没起来")
	return ""
}
