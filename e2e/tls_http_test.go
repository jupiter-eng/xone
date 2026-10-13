package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone/e2e/harness"
	"github.com/jupiter-eng/xone/xgin"
	"github.com/jupiter-eng/xone/xhttp"
	"github.com/jupiter-eng/xone/xonetest"
	"github.com/jupiter-eng/xone/xtls"
)

// tlsStub 一个要求客户端证书的 https 桩：回的 body 是「协议 TLS版本 客户端证书CN」
func tlsStub(t *testing.T, certs *harness.TLSCerts) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn := ""
		if len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		fmt.Fprintf(w, "%s %s %s", r.Proto, tls.VersionName(r.TLS.Version), cn)
	}))
	srv.EnableHTTP2 = true
	srv.TLS = certs.ServerTLS(t, true)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestTLS_XHttp(t *testing.T) {
	harness.Require(t)
	certs := harness.NewTLSCerts(t)
	stub := tlsStub(t, certs)
	ctx := context.Background()

	t.Run("配置文件里的TLS块生效且服务端看到的是TLS和客户端证书", func(t *testing.T) {
		xonetest.UseConfigYAML(t, "XHttp:\n  TLS: "+tlsBlock(xtls.Config{
			Enable: true, CAFile: certs.CAFile, CertFile: certs.ClientCert, KeyFile: certs.ClientKey,
		})+"\n")
		xonetest.StartHooks(t)
		resp, err := xhttp.R(ctx).Get(stub.URL)
		if err != nil || resp.StatusCode() != 200 {
			t.Fatalf("按配置的 CA 和客户端证书该成功：%v %v", resp, err)
		}
		if got := resp.String(); got != "HTTP/2.0 TLS 1.3 "+harness.TLSMTLSUser {
			t.Errorf("服务端应看到 HTTP/2、TLS 和客户端证书 CN=%s，got=%q", harness.TLSMTLSUser, resp.String())
		}
		t.Logf("数字：服务端看到 %s", resp.String())
	})

	for _, c := range []struct {
		name string
		tls  xtls.Config
		want string
	}{
		{"CA不对", xtls.Config{Enable: true, CAFile: certs.OtherCAFile, CertFile: certs.ClientCert, KeyFile: certs.ClientKey},
			"certificate signed by unknown authority"},
		{"ServerName对不上", xtls.Config{Enable: true, CAFile: certs.CAFile, ServerName: "wrong.e2e.internal", CertFile: certs.ClientCert, KeyFile: certs.ClientKey},
			"certificate is valid for"},
		// 下面两条服务端都拒了，但客户端报什么说不准：TLS 1.3 下客户端发完 Finished 就当握手成功、
		// 开始写请求，服务端的告警要等下一次读才到。实测 net/http 报的有时是
		// remote error: tls: certificate required，有时是 write: broken pipe；
		// HTTP/2 的连接只报 http2: client conn could not be established。所以只断言被拒
		{"要客户端证书却没带", xtls.Config{Enable: true, CAFile: certs.CAFile}, ""},
		// 服务端在握手里列出它认的 CA，Go 的客户端手里的证书不是它们签的就不出示
		{"客户端证书不是服务端认的CA签的", xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.StrangerCert, KeyFile: certs.StrangerKey}, ""},
		{"没配TLS块", xtls.Config{}, "certificate signed by unknown authority"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := xhttp.DefaultConfig()
			cfg.TLS, cfg.Metric = c.tls, false
			client, closer, err := xhttp.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			_, err = client.R().SetContext(ctx).Get(stub.URL)
			rejected(t, c.name, err, c.want)
			t.Logf("数字：%s：%v", c.name, err)
		})
	}
}

// ginGet 用给定的客户端 TLS 设置请求一次 /who。h2 为 false 时只说 HTTP/1.1：
// 握手失败时 HTTP/2 的客户端只报 http2: client conn could not be established，看不出原因
func ginGet(certs *harness.TLSCerts, url string, cfg *tls.Config, h2 bool) (string, error) {
	if cfg.RootCAs == nil {
		cfg.RootCAs = certs.Pool
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: h2}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get(url + "/who")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return fmt.Sprintf("%d %s", resp.StatusCode, b), nil
}

func TestTLS_XGin(t *testing.T) {
	harness.Require(t)
	certs := harness.NewTLSCerts(t)
	port := harness.FreePort(t)
	xonetest.UseConfigYAML(t, fmt.Sprintf(`XGin:
  Host: 127.0.0.1
  Port: %d
  TLS:
    CertFile: %q
    KeyFile: %q
    ClientCAFile: %q
    MinVersion: "1.3"
  Log: false
  Metric: false
`, port, certs.ServerCert, certs.ServerKey, certs.CAFile))

	var hits atomic.Int32
	g := xgin.New().WithRoutes(func(e *gin.Engine) {
		e.GET("/who", func(c *gin.Context) {
			hits.Add(1)
			s := c.Request.TLS
			c.String(200, "%s %s %s", c.Request.Proto, tls.VersionName(s.Version), s.PeerCertificates[0].Subject.CommonName)
		})
	})
	done := make(chan error, 1)
	go func() { done <- g.Start(context.Background()) }()
	t.Cleanup(func() {
		_ = g.Stop(context.Background())
		<-done
	})
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		if i == 200 {
			t.Fatal("xgin did not start listening")
		}
		time.Sleep(20 * time.Millisecond)
	}
	base := "https://" + addr

	got, err := ginGet(certs, base, &tls.Config{Certificates: []tls.Certificate{mustPair(t, certs.ClientCert, certs.ClientKey)}}, true)
	if err != nil || got != "200 HTTP/2.0 TLS 1.3 "+harness.TLSMTLSUser {
		t.Fatalf("带着正确的客户端证书该成功，handler 看得到 TLS 1.3 和证书：got=%q err=%v", got, err)
	}
	t.Logf("数字：%s", got)

	for _, c := range []struct {
		name string
		cfg  *tls.Config
		want string
	}{
		{"不带客户端证书", &tls.Config{}, "certificate required"},
		// 服务端在握手里列出 TLS.ClientCAFile 里的 CA，Go 的客户端手里的证书不是它们签的就不出示
		{"客户端证书不是ClientCAFile里的CA签的", &tls.Config{Certificates: []tls.Certificate{mustPair(t, certs.StrangerCert, certs.StrangerKey)}},
			"certificate required"},
		{"TLS1.2的客户端碰上MinVersion1.3", &tls.Config{MaxVersion: tls.VersionTLS12,
			Certificates: []tls.Certificate{mustPair(t, certs.ClientCert, certs.ClientKey)}}, "protocol version"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := hits.Load()
			got, err := ginGet(certs, base, c.cfg, false)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("应被拒并说 %q，got=%q err=%v", c.want, got, err)
			}
			if hits.Load() != before {
				t.Error("被拒的请求不该到 handler")
			}
		})
	}

	t.Run("明文请求打到TLS端口", func(t *testing.T) {
		resp, err := http.Get("http://" + addr + "/who")
		if err != nil {
			t.Fatalf("net/http 对明文请求回 400，而不是断开：%v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 || hits.Load() != 1 {
			t.Errorf("明文请求应得 400 且到不了 handler，got=%d %q hits=%d", resp.StatusCode, b, hits.Load())
		}
		t.Logf("数字：明文请求：%d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	})
}

func mustPair(t *testing.T, cert, key string) tls.Certificate {
	t.Helper()
	c, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
