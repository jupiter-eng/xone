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

// xgin/README.md XGin.TrustedProxies：「信任哪些代理发来的 X-Forwarded-For / X-Real-IP，默认一个都不信」，
// 「写错的网段会直接启动失败」，「它同时决定收不收透传 Header」。
// 默认不信、信 127.0.0.1 这两种 functional_trace_test.go 测过了；这里补「配了、但直连对端不在里面」，
// X-Real-IP，和写错的网段
func TestCoverage_TrustedProxiesHonorForwardedHeadersOnlyFromTrustedPeer(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	t.Run("配了 10.0.0.0/8，直连的 127.0.0.1 不在里面", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		p := harness.Start(t, harness.Options{Downstream: stub.URL, Overlay: "XGin:\n  TrustedProxies: [\"10.0.0.0/8\"]\n"})
		r := p.Get(t, "/proxy", "X-Forwarded-For", "198.51.100.7", "X-Real-IP", "198.51.100.8", "X-Request-Id", "rid-not-trusted")
		if l := accessLog(t, p, traceIDOf(t, r)); l.Str("client_ip") != "127.0.0.1" {
			t.Errorf("直连对端不在 TrustedProxies 里时 client_ip 应是 127.0.0.1，不采信 X-Forwarded-For / X-Real-IP，实际 %q", l.Str("client_ip"))
		}
		if got := stub.Last(t).Header.Get("X-Request-Id"); got != "" {
			t.Errorf("直连对端不可信时 X-Request-Id 不该透传，下游收到了 %q", got)
		}
	})

	t.Run("信 127.0.0.1 时 X-Real-IP 也采信", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: "XGin:\n  TrustedProxies: [\"127.0.0.1\"]\n"})
		r := p.Get(t, "/ping", "X-Real-IP", "203.0.113.9")
		if l := accessLog(t, p, traceIDOf(t, r)); l.Str("client_ip") != "203.0.113.9" {
			t.Errorf("直连对端可信时 client_ip 取 X-Real-IP 的 203.0.113.9，实际 %q", l.Str("client_ip"))
		}
	})

	t.Run("写错的网段启动失败", func(t *testing.T) {
		t.Parallel()
		stderr := covStartupError(t, harness.Options{Overlay: "XGin:\n  TrustedProxies: [\"10.0.0.0/33\"]\n"})
		faultMustContain(t, "TrustedProxies 写错时的启动错误", stderr, "xgin", "10.0.0.0/33")
	})
}

// covSlowHeader 连上服务，发半个请求头（drip 为真时之后每 200ms 再补一行头，永远不发结束的空行），
// 量服务端多久把连接断开。返回断开用了多久和断开之前收到的字节
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
		t.Fatalf("xgin/README.md XGin.ReadHeaderTimeout：发半个请求头的连接应被断开，等了 %v 还连着", limit)
	}
	return took, string(got)
}

// xgin/README.md XGin.ReadHeaderTimeout：「慢连接攻击的主要防线」，默认 10s。
// 请求头在这段时间里没收全，连接就被断开——一个字节一个字节地挤（slowloris）也一样：
// 它管的是收齐整个头的总时长，不是两次收到之间的间隔
func TestCoverage_ReadHeaderTimeoutDropsConnsWithIncompleteHeaders(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	for _, c := range []struct {
		name    string
		overlay string
		want    time.Duration
		drip    bool
	}{
		{"配 1s，发半个头就不动了", "XGin:\n  ReadHeaderTimeout: 1s\n", time.Second, false},
		{"配 1s，每 200ms 挤一行头", "XGin:\n  ReadHeaderTimeout: 1s\n", time.Second, true},
		{"默认 10s", "", 10 * time.Second, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := harness.Start(t, harness.Options{Overlay: c.overlay})
			took, got := covSlowHeader(t, p, c.drip, c.want+5*time.Second)
			if took < c.want-50*time.Millisecond || took > c.want+faultSlack {
				t.Errorf("ReadHeaderTimeout %v：连接应在 %v 左右被断开，实际 %v", c.want, c.want, took)
			}
			// 断开之后服务照常：慢连接占不住服务
			if r := p.Get(t, "/ping"); r.Status != http.StatusOK {
				t.Errorf("慢连接被断开之后服务应照常响应，实际 %v", r)
			}
			t.Logf("数字：%s：%v 后断开，断开前收到 %q", c.name, took.Round(time.Millisecond), firstLine(got))
		})
	}
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\r\n")
	return l
}

// covUpload POST /probe/upload，上传 size 字节的随机内容，回服务端看到的 on_disk 和 sha256 是否对得上
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
		t.Fatalf("xgin/README.md XGin.MaxMultipartMemory：不是请求体上限，超出的部分落盘、不会被拒绝；上传 %d 字节实际 %v", size, r)
	}
	var body struct {
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		OnDisk bool   `json:"on_disk"`
	}
	r.JSON(t, &body)
	sum := sha256.Sum256(data)
	if body.Size != int64(size) || body.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("handler 读到的文件应完整（%d 字节），实际 %d 字节、sha256 对不上=%v", size, body.Size, body.SHA256 != hex.EncodeToString(sum[:]))
	}
	return body.OnDisk
}

// xgin/README.md XGin.MaxMultipartMemory：「字节，默认 8MB。不是『请求体上限』，是『超过多少才落盘』：
// 超出的部分写进临时文件，不会被拒绝」。
// 服务端从 FileHeader.Open 拿到 *os.File 就是落了盘（mime/multipart 的写法），见 service/probe.go
func TestCoverage_MaxMultipartMemoryIsSpillThresholdNotBodyLimit(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	const mb = 1 << 20
	t.Run("配 1MB：100KB 留在内存，3MB 落盘，都收下", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: "XGin:\n  MaxMultipartMemory: 1048576\n"})
		if covUpload(t, p, 100<<10) {
			t.Errorf("100KB 小于 MaxMultipartMemory（1MB），应留在内存，实际落了盘")
		}
		if !covUpload(t, p, 3*mb) {
			t.Errorf("3MB 大于 MaxMultipartMemory（1MB），超出的部分应落盘，实际全在内存")
		}
	})
	t.Run("默认 8MB：3MB 留在内存，12MB 落盘", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{})
		if covUpload(t, p, 3*mb) {
			t.Errorf("默认 8MB 时 3MB 应留在内存，实际落了盘：默认值不是文档写的 8MB")
		}
		if !covUpload(t, p, 12*mb) {
			t.Errorf("默认 8MB 时 12MB 应落盘，实际全在内存：默认值不是文档写的 8MB（gin 自己的默认是 32MB）")
		}
	})
}

// xgin/README.md XGin.TLS.CertFile / KeyFile：配上就是 HTTPS，「TLS 模式下 HTTP/2 本来就是自动的」；
// 「与 KeyFile 必须同时配或同时留空，只配一半会启动失败」
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
			t.Errorf("配了证书应是 HTTPS 并自动协商 HTTP/2，实际 status=%d proto=%s tls=%v", resp.StatusCode, resp.Proto, resp.TLS != nil)
		}
		l := p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "xgin listening" })
		if l.Str("tls") != "true" || l.Str("h2c") != "false" {
			t.Errorf("启动日志应记 tls=true h2c=false，实际 %s", l.Line)
		}
		// 明文 HTTP 打到 TLS 端口上：net/http 回 400，不会当成明文服务
		plain := p.Get(t, "/ping")
		if plain.Status != http.StatusBadRequest {
			t.Errorf("明文请求打到 HTTPS 端口应被拒（400），实际 %v", plain)
		}
		t.Logf("数字：HTTPS 协商到 %s，%s；明文请求 → %d", resp.Proto, tls13(resp.TLS.Version), plain.Status)
	})

	t.Run("只配 CertFile 启动失败", func(t *testing.T) {
		t.Parallel()
		cert, _, _ := harness.SelfSignedCert(t, t.TempDir())
		stderr := covStartupError(t, harness.Options{Overlay: fmt.Sprintf("XGin:\n  TLS:\n    CertFile: %q\n", cert)})
		faultMustContain(t, "只配 CertFile 时的启动错误", stderr, "xgin", "TLS.CertFile", "TLS.KeyFile")
	})
}

func tls13(v uint16) string {
	if v == 0x0304 {
		return "TLS 1.3"
	}
	return fmt.Sprintf("TLS 0x%04x", v)
}

// xgin/README.md XGin.UseH2C：「非 TLS 下启用 HTTP/2，只认先验知识」；
// 「h2c 连接和 HTTP/1.1 一样受优雅退出管：Shutdown 等在途请求做完」。
// 没开时说 h2c 的客户端连不上（服务端只说 HTTP/1.1）
func TestCoverage_UseH2COn_PriorKnowledgeH2CWorks_InFlightDrainedOnShutdown(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	t.Run("没开：h2c 客户端说不通", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{})
		c := covH2CClient()
		defer c.CloseIdleConnections()
		if resp, err := c.Get(p.URL("/ping")); err == nil {
			resp.Body.Close()
			t.Errorf("UseH2C 默认关，先验知识的 h2c 请求不该成功，实际 %s %d", resp.Proto, resp.StatusCode)
		}
	})

	t.Run("打开：h2c 可用，SIGTERM 时在途的 h2c 请求做完才退出", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: "XGin:\n  UseH2C: true\n"})
		c := covH2CClient()
		defer c.CloseIdleConnections()
		resp, err := c.Get(p.URL("/ping"))
		if err != nil {
			t.Fatalf("UseH2C: true 时先验知识的 h2c 应能用：%v", err)
		}
		resp.Body.Close()
		if resp.ProtoMajor != 2 {
			t.Fatalf("UseH2C: true 时应是 HTTP/2，实际 %s", resp.Proto)
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
		waitMetrics(t, p, "e2e_slow_inflight 到 1（请求进了 handler）", func(m harness.Metrics) bool { return m.Sum("e2e_slow_inflight") == 1 })
		sent := time.Now()
		p.Signal(syscall.SIGTERM)

		var r result
		select {
		case r = <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("在途的 h2c 请求 15s 没回来")
		}
		if r.err != nil || r.status != http.StatusOK || r.proto != "HTTP/2.0" {
			t.Errorf("xgin/README.md：h2c 连接受优雅退出管，在途请求应做完（200，HTTP/2.0），实际 status=%d proto=%s err=%v", r.status, r.proto, r.err)
		}
		exit, ok := p.Wait(20 * time.Second)
		if !ok || exit.Code != 0 {
			t.Fatalf("SIGTERM 之后应以 0 退出，实际 %v（ok=%v）", exit, ok)
		}
		if exit.SinceSignal < time.Second {
			t.Errorf("在途的 h2c 请求还剩约 1.2s，进程却在信号之后 %v 就退出了：Shutdown 没等 h2c 连接（x/net h2c.NewHandler 的老毛病）", exit.SinceSignal)
		}
		t.Logf("数字：h2c 在途请求 %v 后做完，进程在信号之后 %v 退出", time.Since(sent).Round(time.Millisecond), exit.SinceSignal.Round(time.Millisecond))
	})
}

// xgin/README.md XGin.LogSkipPaths：「不记访问日志的路径：以 / 结尾的按前缀匹配，其余精确匹配。
// Metric 开着时指标端点会自动加进来，不用自己写」。MetricPath 自定义时自动跳过的是自定义的那个
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
	// 界碑：最后一个请求的访问日志到了，前面的要打早就打了
	sentinel := p.Get(t, "/probe/log?msg=sentinel")
	accessLog(t, p, traceIDOf(t, sentinel))

	for _, path := range []string{"/ping", "/probe/skip/a", "/probe/skip/a/b", "/internal/metrics"} {
		if n := len(covAccessLogs(p, path)); n != 0 {
			t.Errorf("%s 在 LogSkipPaths 里（或是自定义的 MetricPath），不该有访问日志，实际 %d 条", path, n)
		}
	}
	for _, path := range []string{"/probe/skipx", "/ping/extra", "/metrics"} {
		if n := len(covAccessLogs(p, path)); n != 1 {
			t.Errorf("%s 不匹配任何一条（/ping 精确匹配、/probe/skip/ 按前缀），应有 1 条访问日志，实际 %d 条", path, n)
		}
	}

	m := harness.ScrapeMetrics(t, p.URL("/internal/metrics"))
	if !m.Has("e2e_http_requests_total") {
		t.Errorf("MetricPath: /internal/metrics 时指标应挂在那里，实际没有 e2e_http_requests_total")
	}
	if r := p.Get(t, "/metrics"); r.Status != http.StatusNotFound {
		t.Errorf("MetricPath 换了之后 /metrics 不该还在，实际 %d", r.Status)
	}

	t.Run("MetricPath 不以 / 开头时启动失败", func(t *testing.T) {
		t.Parallel()
		stderr := covStartupError(t, harness.Options{Overlay: "XGin:\n  MetricPath: metrics\n"})
		faultMustContain(t, "MetricPath: metrics 的启动错误", stderr, "MetricPath must start with /")
	})
}

// xgin/README.md XGin.ZHTranslations：「validator 的报错翻成中文，用法见 xgin/trans」。
// 没打开时 trans.ToZH 原样返回英文报错
func TestCoverage_ZHTranslationsMakesValidationErrorsChinese(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	for _, c := range []struct {
		name    string
		overlay string
		want    []string
	}{
		{"默认关：英文", "", []string{"Field validation for 'Name' failed on the 'required' tag", "Field validation for 'Age' failed on the 'gte' tag"}},
		{"打开：中文", "XGin:\n  ZHTranslations: true\n", []string{"Name为必填字段", "Age必须大于或等于1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := harness.Start(t, harness.Options{Overlay: c.overlay})
			r := p.PostJSON(t, "/probe/zh", map[string]any{"age": 0})
			if r.Status != http.StatusBadRequest {
				t.Fatalf("缺必填字段应是 400，实际 %v", r)
			}
			msg := fmt.Sprint(r.Map(t)["error"])
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("报错里应有 %q，实际 %q", w, msg)
				}
			}
			t.Logf("数字：%s → %q", c.name, msg)
		})
	}
}

// 方法不对返回 405 而不是 404（xgin.go：HandleMethodNotAllowed = true，「不开的话，方法不对会返回 404」）；
// 405 和 404 一样，指标的 route 标签收敛成 unmatched，不随真实路径增长（functional_metrics_test.go 测了 404）
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
			t.Errorf("%s %s：路由存在、方法不对，应是 405，实际 %d", c.method, c.path, r.Status)
		}
	}
	m := waitMetrics(t, p, "405 计进请求指标", func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "status", "405") == 4
	})
	if got := m.Sum("e2e_http_requests_total", "method", "DELETE", "route", "unmatched", "status", "405"); got != 2 {
		t.Errorf("两个不同 id 的 DELETE 405 应合并计入 route=unmatched，实际 %v", got)
	}
	if got := m.Sum("e2e_http_requests_total", "method", "OTHER", "route", "unmatched", "status", "405"); got != 1 {
		t.Errorf("不认识的方法 BREW 的 405 应记成 method=OTHER route=unmatched，实际 %v", got)
	}
	for _, s := range m.Find("e2e_http_requests_total", "status", "405") {
		if s.Labels["route"] != "unmatched" {
			t.Errorf("405 的 route 标签应是 unmatched，实际 %q", s.Labels["route"])
		}
	}
}
