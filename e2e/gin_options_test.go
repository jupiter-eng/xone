package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// xgin/README.md, XGin.TrustedProxies: "which proxies' X-Forwarded-For / X-Real-IP are trusted,
// none of them by default", "a malformed range fails the start outright" and "it also decides
// whether the forwarded headers are taken".
// "trusting none by default" and "trusting 127.0.0.1" are tested in functional_trace_test.go; what
// is added here is "configured, with the direct peer not among them", X-Real-IP, and a malformed
// range
func TestCoverage_TrustedProxiesHonorForwardedHeadersOnlyFromTrustedPeer(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	t.Run("10.0.0.0/8 configured, with the direct 127.0.0.1 not in it", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		p := harness.Start(t, harness.Options{Downstream: stub.URL, Overlay: "XGin:\n  TrustedProxies: [\"10.0.0.0/8\"]\n"})
		r := p.Get(t, "/proxy", "X-Forwarded-For", "198.51.100.7", "X-Real-IP", "198.51.100.8", "X-Request-Id", "rid-not-trusted")
		if l := accessLog(t, p, traceIDOf(t, r)); l.Str("client_ip") != "127.0.0.1" {
			t.Errorf("with the direct peer not in TrustedProxies, client_ip should be 127.0.0.1 and neither X-Forwarded-For nor X-Real-IP should be taken, got %q", l.Str("client_ip"))
		}
		if got := stub.Last(t).Header.Get("X-Request-Id"); got != "" {
			t.Errorf("with the direct peer untrusted, X-Request-Id should not be forwarded, and the downstream received %q", got)
		}
	})

	t.Run("with 127.0.0.1 trusted, X-Real-IP is taken as well", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: "XGin:\n  TrustedProxies: [\"127.0.0.1\"]\n"})
		r := p.Get(t, "/ping", "X-Real-IP", "203.0.113.9")
		if l := accessLog(t, p, traceIDOf(t, r)); l.Str("client_ip") != "203.0.113.9" {
			t.Errorf("with the direct peer trusted, client_ip comes from X-Real-IP, 203.0.113.9, got %q", l.Str("client_ip"))
		}
	})

	t.Run("a malformed range fails the start", func(t *testing.T) {
		t.Parallel()
		stderr := covStartupError(t, harness.Options{Overlay: "XGin:\n  TrustedProxies: [\"10.0.0.0/33\"]\n"})
		faultMustContain(t, "the start error for a malformed TrustedProxies", stderr, "xgin", "10.0.0.0/33")
	})
}

// covSlowHeader connects to the service and sends half a request header (and with drip, one header
// line every 200ms afterwards, never sending the empty line that ends them), and measures how long
// the server takes to break the connection. It returns how long that took and the bytes received
// before it
func covSlowHeader(t *testing.T, p *harness.Process, drip bool, limit time.Duration) (time.Duration, string) {
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	start := time.Now()
	if _, err := io.WriteString(conn, "GET /ping HTTP/1.1\r\nHost: slowloris\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	stop := make(chan struct{})
	defer close(stop)
	if drip {
		go func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				case <-time.After(200 * time.Millisecond):
				}
				if _, err := fmt.Fprintf(conn, "X-Drip-%d: x\r\n", i); err != nil {
					return
				}
			}
		}()
	}
	_ = conn.SetReadDeadline(time.Now().Add(limit))
	got, err := io.ReadAll(conn)
	took := time.Since(start)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("xgin/README.md, XGin.ReadHeaderTimeout: a connection sending half a request header should be broken, and after %v it is still open", limit)
	}
	return took, string(got)
}

// xgin/README.md, XGin.ReadHeaderTimeout: "the main defense against a slow-connection attack", 10s
// by default.
// A request header not received whole within that time breaks the connection -- and squeezing it
// out byte by byte (slowloris) is no different: what it governs is the total time to receive the
// whole header and not the gap between two reads
func TestCoverage_ReadHeaderTimeoutDropsConnsWithIncompleteHeaders(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	for _, c := range []struct {
		name    string
		overlay string
		want    time.Duration
		drip    bool
	}{
		{"1s configured, sending half a header and then nothing", "XGin:\n  ReadHeaderTimeout: 1s\n", time.Second, false},
		{"1s configured, squeezing one header line out every 200ms", "XGin:\n  ReadHeaderTimeout: 1s\n", time.Second, true},
		{"the default 10s", "", 10 * time.Second, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := harness.Start(t, harness.Options{Overlay: c.overlay})
			took, got := covSlowHeader(t, p, c.drip, c.want+5*time.Second)
			if took < c.want-50*time.Millisecond || took > c.want+faultSlack {
				t.Errorf("with ReadHeaderTimeout %v the connection should be broken at about %v, got %v", c.want, c.want, took)
			}
			// The service carries on afterwards: a slow connection does not hold it up
			if r := p.Get(t, "/ping"); r.Status != http.StatusOK {
				t.Errorf("the service should answer as usual once the slow connection is broken, got %v", r)
			}
			t.Logf("numbers: %s: broken after %v, with %q received before that", c.name, took.Round(time.Millisecond), firstLine(got))
		})
	}
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\r\n")
	return l
}

// covUpload POSTs /probe/upload with size bytes of random content and returns whether the server
// saw it on disk, having checked the sha256 matches
func covUpload(t *testing.T, p *harness.Process, size int) (onDisk bool) {
	t.Helper()
	data := make([]byte, size)
	_, _ = rand.Read(data)
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", "blob.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(data)
	_ = w.Close()
	r := p.Do(t, http.MethodPost, "/probe/upload", buf.Bytes(), "Content-Type", w.FormDataContentType())
	if r.Status != http.StatusOK {
		t.Fatalf("xgin/README.md, XGin.MaxMultipartMemory: it is no request body limit, and what goes past it lands on disk rather than being refused; uploading %d bytes gave %v", size, r)
	}
	var body struct {
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		OnDisk bool   `json:"on_disk"`
	}
	r.JSON(t, &body)
	sum := sha256.Sum256(data)
	if body.Size != int64(size) || body.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("the handler should read the whole file (%d bytes), got %d bytes with the sha256 not matching=%v", size, body.Size, body.SHA256 != hex.EncodeToString(sum[:]))
	}
	return body.OnDisk
}

// xgin/README.md, XGin.MaxMultipartMemory: "in bytes, 8MB by default. It is no 'request body
// limit' but 'how much before it spills to disk': what goes past it is written into a temporary
// file and is not refused".
// The server getting an *os.File out of FileHeader.Open is what spilling to disk looks like (the
// way mime/multipart works), see service/probe.go
func TestCoverage_MaxMultipartMemoryIsSpillThresholdNotBodyLimit(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	const mb = 1 << 20
	t.Run("1MB configured: 100KB stays in memory, 3MB spills to disk, and both are taken", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: "XGin:\n  MaxMultipartMemory: 1048576\n"})
		if covUpload(t, p, 100<<10) {
			t.Errorf("100KB is under MaxMultipartMemory (1MB) and should stay in memory, and it spilled to disk")
		}
		if !covUpload(t, p, 3*mb) {
			t.Errorf("3MB is over MaxMultipartMemory (1MB) and what goes past it should spill to disk, and it all stayed in memory")
		}
	})
	t.Run("the default 8MB: 3MB stays in memory and 12MB spills to disk", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{})
		if covUpload(t, p, 3*mb) {
			t.Errorf("with the default 8MB, 3MB should stay in memory, and it spilled to disk: the default is not the documented 8MB")
		}
		if !covUpload(t, p, 12*mb) {
			t.Errorf("with the default 8MB, 12MB should spill to disk, and it all stayed in memory: the default is not the documented 8MB (gin's own is 32MB)")
		}
	})
}

// xgin/README.md, XGin.TLS.CertFile / KeyFile: configuring them makes it HTTPS, and "under TLS,
// HTTP/2 is automatic anyway"; "it has to be configured together with KeyFile or left empty
// together, and configuring one half alone fails the start"
func TestCoverage_CertConfiguredMeansHTTPSWithHTTP2Negotiated(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	t.Run("CertFile + KeyFile", func(t *testing.T) {
		t.Parallel()
		cert, key, pool := harness.SelfSignedCert(t, t.TempDir())
		p := harness.Start(t, harness.Options{NoWait: true, Overlay: fmt.Sprintf("XGin:\n  TLS:\n    CertFile: %q\n    KeyFile: %q\n", cert, key)})
		c := covHTTPSClient(pool)
		defer c.CloseIdleConnections()
		https := fmt.Sprintf("https://127.0.0.1:%d", p.Port)
		p.WaitReadyWith(t, c, https+"/ping", 30*time.Second)

		resp, err := c.Get(https + "/ping")
		if err != nil {
			t.Fatalf("GET %s/ping: %v", https, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 || resp.TLS == nil {
			t.Errorf("with a certificate configured it should be HTTPS with HTTP/2 negotiated automatically, got status=%d proto=%s tls=%v", resp.StatusCode, resp.Proto, resp.TLS != nil)
		}
		l := p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "xgin listening" })
		if l.Str("tls") != "true" || l.Str("h2c") != "false" {
			t.Errorf("the start log should record tls=true h2c=false, got %s", l.Line)
		}
		// Plain HTTP against the TLS port: net/http answers 400 and does not take it for a plaintext service
		plain := p.Get(t, "/ping")
		if plain.Status != http.StatusBadRequest {
			t.Errorf("a plaintext request against the HTTPS port should be refused (400), got %v", plain)
		}
		t.Logf("numbers: HTTPS negotiated %s, %s; the plaintext request -> %d", resp.Proto, tls13(resp.TLS.Version), plain.Status)
	})

	t.Run("CertFile alone fails the start", func(t *testing.T) {
		t.Parallel()
		cert, _, _ := harness.SelfSignedCert(t, t.TempDir())
		stderr := covStartupError(t, harness.Options{Overlay: fmt.Sprintf("XGin:\n  TLS:\n    CertFile: %q\n", cert)})
		faultMustContain(t, "the start error for CertFile alone", stderr, "xgin", "TLS.CertFile", "TLS.KeyFile")
	})
}

func tls13(v uint16) string {
	if v == 0x0304 {
		return "TLS 1.3"
	}
	return fmt.Sprintf("TLS 0x%04x", v)
}

// xgin/README.md, XGin.UseH2C: "HTTP/2 without TLS, with prior knowledge alone";
// "an h2c connection is subject to the graceful exit as HTTP/1.1 is: Shutdown waits for the
// requests in flight".
// Turned off, a client speaking h2c cannot get through (the server speaks HTTP/1.1 alone)
func TestCoverage_UseH2COn_PriorKnowledgeH2CWorks_InFlightDrainedOnShutdown(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	t.Run("off: an h2c client cannot get through", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{})
		c := covH2CClient()
		defer c.CloseIdleConnections()
		if resp, err := c.Get(p.URL("/ping")); err == nil {
			resp.Body.Close()
			t.Errorf("UseH2C is off by default and an h2c request with prior knowledge should not succeed, got %s %d", resp.Proto, resp.StatusCode)
		}
	})

	t.Run("on: h2c works, and an h2c request in flight finishes before the SIGTERM exit", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: "XGin:\n  UseH2C: true\n"})
		c := covH2CClient()
		defer c.CloseIdleConnections()
		resp, err := c.Get(p.URL("/ping"))
		if err != nil {
			t.Fatalf("with UseH2C: true h2c with prior knowledge should work: %v", err)
		}
		resp.Body.Close()
		if resp.ProtoMajor != 2 {
			t.Fatalf("with UseH2C: true it should be HTTP/2, got %s", resp.Proto)
		}

		type result struct {
			status int
			proto  string
			err    error
		}
		done := make(chan result, 1)
		go func() {
			resp, err := c.Get(p.URL("/slow?ms=1500"))
			if err != nil {
				done <- result{err: err}
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			done <- result{status: resp.StatusCode, proto: resp.Proto}
		}()
		waitMetrics(t, p, "e2e_slow_inflight reaching 1 (the request is in the handler)", func(m harness.Metrics) bool { return m.Sum("e2e_slow_inflight") == 1 })
		sent := time.Now()
		p.Signal(syscall.SIGTERM)

		var r result
		select {
		case r = <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("the h2c request in flight did not come back within 15s")
		}
		if r.err != nil || r.status != http.StatusOK || r.proto != "HTTP/2.0" {
			t.Errorf("xgin/README.md: an h2c connection is subject to the graceful exit and a request in flight should finish (200, HTTP/2.0), got status=%d proto=%s err=%v", r.status, r.proto, r.err)
		}
		exit, ok := p.Wait(20 * time.Second)
		if !ok || exit.Code != 0 {
			t.Fatalf("it should exit with 0 after the SIGTERM, got %v (ok=%v)", exit, ok)
		}
		if exit.SinceSignal < time.Second {
			t.Errorf("the h2c request in flight had about 1.2s left and the process exited %v after the signal: Shutdown did not wait for the h2c connection (the old x/net h2c.NewHandler problem)", exit.SinceSignal)
		}
		t.Logf("numbers: the h2c request in flight finished after %v and the process exited %v after the signal", time.Since(sent).Round(time.Millisecond), exit.SinceSignal.Round(time.Millisecond))
	})
}

// xgin/README.md, XGin.LogSkipPaths: "the paths that write no access log: one ending in / matches
// as a prefix and the rest match exactly. With Metric on, the metrics endpoint is added
// automatically and does not have to be written". With a custom MetricPath, the one skipped
// automatically is the custom one
func TestCoverage_LogSkipPathsAndCustomMetricPath(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Overlay: `XGin:
  LogSkipPaths: ["/ping", "/probe/skip/"]
  MetricPath: /internal/metrics
`})
	for _, path := range []string{"/ping", "/probe/skip/a", "/probe/skip/a/b", "/probe/skipx", "/ping/extra", "/internal/metrics", "/metrics"} {
		p.Get(t, path)
	}
	// A landmark: once the last request's access log is there, the ones before it would be out
	sentinel := p.Get(t, "/probe/log?msg=sentinel")
	accessLog(t, p, traceIDOf(t, sentinel))

	for _, path := range []string{"/ping", "/probe/skip/a", "/probe/skip/a/b", "/internal/metrics"} {
		if n := len(covAccessLogs(p, path)); n != 0 {
			t.Errorf("%s is in LogSkipPaths (or is the custom MetricPath) and should write no access log, got %d lines", path, n)
		}
	}
	for _, path := range []string{"/probe/skipx", "/ping/extra", "/metrics"} {
		if n := len(covAccessLogs(p, path)); n != 1 {
			t.Errorf("%s matches none of them (/ping exactly, /probe/skip/ as a prefix) and should have 1 access log line, got %d", path, n)
		}
	}

	m := harness.ScrapeMetrics(t, p.URL("/internal/metrics"))
	if !m.Has("e2e_http_requests_total") {
		t.Errorf("with MetricPath: /internal/metrics the metrics should be mounted there, and there is no e2e_http_requests_total")
	}
	if r := p.Get(t, "/metrics"); r.Status != http.StatusNotFound {
		t.Errorf("with MetricPath changed, /metrics should be gone, got %d", r.Status)
	}

	t.Run("a MetricPath not starting with / fails the start", func(t *testing.T) {
		t.Parallel()
		stderr := covStartupError(t, harness.Options{Overlay: "XGin:\n  MetricPath: metrics\n"})
		faultMustContain(t, "the start error for MetricPath: metrics", stderr, "MetricPath must start with /")
	})
}

// xgin/README.md, XGin.ZHTranslations: "validator's errors are translated into Chinese, see
// xgin/trans for how to use it".
// Turned off, trans.ToZH returns the English error unchanged
func TestCoverage_ZHTranslationsMakesValidationErrorsChinese(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	// The translated messages are what the library produces, and they are written as escapes so that
	// this file itself holds none of that script
	for _, c := range []struct {
		name    string
		overlay string
		want    []string
	}{
		{"off by default: English", "", []string{"Field validation for 'Name' failed on the 'required' tag", "Field validation for 'Age' failed on the 'gte' tag"}},
		{"on: translated", "XGin:\n  ZHTranslations: true\n", []string{"Name\u4e3a\u5fc5\u586b\u5b57\u6bb5", "Age\u5fc5\u987b\u5927\u4e8e\u6216\u7b49\u4e8e1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := harness.Start(t, harness.Options{Overlay: c.overlay})
			r := p.PostJSON(t, "/probe/zh", map[string]any{"age": 0})
			if r.Status != http.StatusBadRequest {
				t.Fatalf("a missing required field should be a 400, got %v", r)
			}
			msg := fmt.Sprint(r.Map(t)["error"])
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("the error should hold %q, got %q", w, msg)
				}
			}
			t.Logf("numbers: %s -> %q", c.name, msg)
		})
	}
}

// A wrong method answers 405 and not 404 (xgin.go: HandleMethodNotAllowed = true, "without it a
// wrong method answers 404");
// as with a 404, a 405's route label in the metrics collapses into unmatched and does not grow with
// the real path (functional_metrics_test.go tests the 404)
func TestCoverage_WrongMethodReturns405_MetricRouteCollapsesToUnmatched(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{})
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/ping"}, {http.MethodDelete, "/users/1"}, {http.MethodDelete, "/users/2"}, {"BREW", "/ping"},
	} {
		r, err := p.Request(context.Background(), c.method, c.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: the route exists and the method is wrong, which should be 405, got %d", c.method, c.path, r.Status)
		}
	}
	m := waitMetrics(t, p, "the 405s counted into the request metrics", func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "status", "405") == 4
	})
	if got := m.Sum("e2e_http_requests_total", "method", "DELETE", "route", "unmatched", "status", "405"); got != 2 {
		t.Errorf("the two DELETE 405s with different ids should count together under route=unmatched, got %v", got)
	}
	if got := m.Sum("e2e_http_requests_total", "method", "OTHER", "route", "unmatched", "status", "405"); got != 1 {
		t.Errorf("the 405 of the unknown method BREW should be recorded as method=OTHER route=unmatched, got %v", got)
	}
	for _, s := range m.Find("e2e_http_requests_total", "status", "405") {
		if s.Labels["route"] != "unmatched" {
			t.Errorf("a 405's route label should be unmatched, got %q", s.Labels["route"])
		}
	}
}
