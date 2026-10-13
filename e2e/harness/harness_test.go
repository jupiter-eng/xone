package harness

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// harness 自己的测试：故障测试和压测的结论全靠这几样东西说真话，
// 它们本身先得是对的。不连 PG / Redis，但同样只在 XONE_E2E=1 时跑

// echoServer 一个把收到的每一行原样写回的 TCP 服务
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// roundTrip 发一行、读回一行，返回耗时
func roundTrip(t *testing.T, c net.Conn, r *bufio.Reader) time.Duration {
	t.Helper()
	start := time.Now()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("hello\n")); err != nil {
		t.Fatalf("写失败：%v", err)
	}
	line, err := r.ReadString('\n')
	if err != nil || line != "hello\n" {
		t.Fatalf("读回 %q, err=%v", line, err)
	}
	return time.Since(start)
}

func TestProxy_DisconnectDropsConnsRefusesNew_ResumeForwards(t *testing.T) {
	Require(t)
	p := NewProxy(t, echoServer(t))

	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)
	roundTrip(t, c, r)
	if p.Active() != 1 || p.Accepted() != 1 {
		t.Errorf("Active=%d Accepted=%d，want 1 1", p.Active(), p.Accepted())
	}

	p.Cut()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := r.ReadByte(); err == nil {
		t.Error("Cut 之后现有连接还读得到东西")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("Cut 之后现有连接没被断开，只是没数据")
	}
	if c2, err := net.DialTimeout("tcp", p.Addr(), time.Second); err == nil {
		c2.Close()
		t.Error("Cut 之后新连接还连得上")
	}

	p.Restore()
	c3, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatalf("Restore 之后连不上：%v", err)
	}
	defer c3.Close()
	roundTrip(t, c3, bufio.NewReader(c3))
}

func TestProxy_BlackholeStallsConnsNewDialTimesOut_ResumeDropsOldForwardsNew(t *testing.T) {
	Require(t)
	p := NewProxy(t, echoServer(t))
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)
	roundTrip(t, c, r)

	p.Blackhole()
	// 现有连接：写得进去，但等不到回复，也没有被断开（读到的是超时，不是 EOF）
	c.SetDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := c.Write([]byte("hello\n")); err != nil {
		t.Fatalf("黑洞里写现有连接失败：%v", err)
	}
	if _, err := r.ReadByte(); err == nil {
		t.Error("黑洞里现有连接还读得到回复")
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Errorf("黑洞里现有连接应该是等不到回复（超时），实际 %v", err)
	}
	// 新连接：拨号等满自己的超时，不是被拒
	start := time.Now()
	c2, err := net.DialTimeout("tcp", p.Addr(), 300*time.Millisecond)
	if c2 != nil {
		c2.Close()
	}
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Errorf("黑洞里新连接应该拨号超时，实际 err=%v", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("黑洞里拨号 %v 就返回了，没有等满超时", d)
	}

	p.Restore()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := r.ReadByte(); err == nil {
		t.Error("Restore 之后黑洞期间的旧连接应被断开，实际还读得到东西")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("Restore 之后黑洞期间的旧连接没被断开")
	}
	c3, err := net.DialTimeout("tcp", p.Addr(), time.Second)
	if err != nil {
		t.Fatalf("Restore 之后连不上：%v", err)
	}
	defer c3.Close()
	roundTrip(t, c3, bufio.NewReader(c3))

	// 黑洞里 Cut：从「宕机」变成「拒绝连接」
	p.Blackhole()
	p.Cut()
	if _, err := net.DialTimeout("tcp", p.Addr(), time.Second); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("黑洞里 Cut 之后新连接应被拒，实际 err=%v", err)
	}
}

func TestProxy_DelayFromArrivalTimeNotCumulative(t *testing.T) {
	Require(t)
	p := NewProxy(t, echoServer(t))
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)

	p.SetDelay(150 * time.Millisecond)
	if d := roundTrip(t, c, r); d < 150*time.Millisecond || d > time.Second {
		t.Errorf("一次往返 %v，want 约 150ms", d)
	}

	// 一口气发 5 行：每行的延迟各自从到达时算，总共约 150ms 而不是 750ms
	start := time.Now()
	c.Write([]byte(strings.Repeat("hello\n", 5)))
	for range 5 {
		if line, err := r.ReadString('\n'); err != nil || line != "hello\n" {
			t.Fatalf("读回 %q, err=%v", line, err)
		}
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("5 行一共等了 %v，延迟被累加了", d)
	}

	p.SetDelay(0)
	if d := roundTrip(t, c, r); d > 100*time.Millisecond {
		t.Errorf("取消延迟之后一次往返还要 %v", d)
	}
}

func TestStub_RecordsRequestsAndRespondsAsSet(t *testing.T) {
	Require(t)
	s := NewStub(t)
	s.SetStatus(http.StatusTeapot)
	s.SetBody("text/plain", "short and stout")

	req, _ := http.NewRequest(http.MethodGet, s.URL+"/echo?token=abc", nil)
	req.Header.Set("X-Request-Id", "rid-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot || string(body) != "short and stout" {
		t.Errorf("响应 %d %q", resp.StatusCode, body)
	}

	got := s.Last(t)
	if got.Path != "/echo" || got.Query.Get("token") != "abc" || got.Header.Get("X-Request-Id") != "rid-1" {
		t.Errorf("记下的请求不对：%+v", got)
	}
}

func TestLoad_BucketsByStatusAndComputesPercentiles(t *testing.T) {
	Require(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") == "1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		time.Sleep(time.Millisecond)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// 每 4 个请求里 1 个 500
	res := Load(t, LoadSpec{
		Concurrency: 4,
		Requests:    200,
		NewRequest: func(ctx context.Context, i int) (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/?fail=%d", srv.URL, btoi(i%4 == 0)), nil)
		},
	})
	t.Log(res)
	if res.Requests != 200 || res.Errors != 0 || res.Status[200] != 150 || res.Status[500] != 50 || res.OK() != 150 {
		t.Errorf("分桶不对：%v", res)
	}
	if res.P50 <= 0 || res.P50 > res.P90 || res.P90 > res.P99 || res.P99 > res.Max || res.QPS <= 0 {
		t.Errorf("分位数不对：%v", res)
	}

	// 按时长跑：到点就停，发出去的都算上
	res = Load(t, LoadSpec{Concurrency: 2, Duration: 200 * time.Millisecond, URL: srv.URL})
	if res.Requests == 0 || res.Status[200] != res.Requests || res.Elapsed < 200*time.Millisecond {
		t.Errorf("按时长跑不对：%v", res)
	}

	// 连不上的算传输层错误，不进状态码
	srv.Close()
	res = Load(t, LoadSpec{Requests: 3, URL: srv.URL})
	if res.Errors != 3 || len(res.Status) != 0 || len(res.ErrorSamples) == 0 {
		t.Errorf("传输层错误没算对：%v", res)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestParseMetrics_SplitsHistogramIntoBucketsAndCount(t *testing.T) {
	Require(t)
	text := `# TYPE e2e_http_requests_total counter
e2e_http_requests_total{route="/ping",status="200"} 3
e2e_http_requests_total{route="/users/:id",status="404"} 1
# TYPE e2e_order_flow_seconds histogram
e2e_order_flow_seconds_bucket{le="0.1"} 1
e2e_order_flow_seconds_bucket{le="+Inf"} 2
e2e_order_flow_seconds_sum 0.35
e2e_order_flow_seconds_count 2
`
	m, err := ParseMetrics(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Sum("e2e_http_requests_total"); got != 4 {
		t.Errorf("总数 %v，want 4", got)
	}
	if got := m.Sum("e2e_http_requests_total", "route", "/ping"); got != 3 {
		t.Errorf("/ping %v，want 3", got)
	}
	if got := m.Sum("e2e_order_flow_seconds_bucket", "le", "+Inf"); got != 2 {
		t.Errorf("+Inf 桶 %v，want 2", got)
	}
	if got := m.Sum("e2e_order_flow_seconds_count"); got != 2 {
		t.Errorf("count %v，want 2", got)
	}
}

func TestReadProc_ReadsOwnCPUAndMemory(t *testing.T) {
	Require(t)
	s, err := ReadProc(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if s.RSS <= 0 || s.PeakRSS < s.RSS || s.Threads <= 0 {
		t.Errorf("读数不对：%+v", s)
	}
}

func TestResetPeakRSS_PeakRestartsFromCurrentRSS(t *testing.T) {
	Require(t)
	// 先把峰值顶高 256MB，再把这块内存还给操作系统：重置前峰值还记着它，重置后不该再记着
	buf := make([]byte, 256<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}
	high, err := ReadProc(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(buf)
	buf = nil
	debug.FreeOSMemory()

	if err := ResetPeakRSS(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	s, err := ReadProc(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if s.PeakRSS > high.PeakRSS-128<<20 {
		t.Errorf("重置之后峰值应从当前 RSS 算起：重置前峰值 %dMB，重置后峰值 %dMB（当前 RSS %dMB）", high.PeakRSS>>20, s.PeakRSS>>20, s.RSS>>20)
	}
}
