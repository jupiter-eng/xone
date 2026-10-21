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

// The harness's own tests: what the fault tests and the load tests conclude rests on these few
// pieces telling the truth, so they have to be right first. They touch no PG / Redis, but run only
// with XONE_E2E=1 all the same

// echoServer is a TCP server writing every line it receives straight back
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

// roundTrip sends one line, reads one back and returns how long it took
func roundTrip(t *testing.T, c net.Conn, r *bufio.Reader) time.Duration {
	t.Helper()
	start := time.Now()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	line, err := r.ReadString('\n')
	if err != nil || line != "hello\n" {
		t.Fatalf("read back %q, err=%v", line, err)
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
		t.Errorf("Active=%d Accepted=%d, want 1 1", p.Active(), p.Accepted())
	}

	p.Cut()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := r.ReadByte(); err == nil {
		t.Error("a connection that was already open still reads something after Cut")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("a connection that was already open was not broken by Cut, it merely has no data")
	}
	if c2, err := net.DialTimeout("tcp", p.Addr(), time.Second); err == nil {
		c2.Close()
		t.Error("a new connection still connects after Cut")
	}

	p.Restore()
	c3, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatalf("no connection after Restore: %v", err)
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
	// A connection already open: writes go through, no answer comes back, and it is not broken (a read times out rather than reaching EOF)
	c.SetDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := c.Write([]byte("hello\n")); err != nil {
		t.Fatalf("writing to an open connection in the blackhole failed: %v", err)
	}
	if _, err := r.ReadByte(); err == nil {
		t.Error("an open connection still reads an answer in the blackhole")
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Errorf("an open connection in the blackhole should get no answer (a timeout), got %v", err)
	}
	// A new connection: the dial sits out its own timeout rather than being refused
	start := time.Now()
	c2, err := net.DialTimeout("tcp", p.Addr(), 300*time.Millisecond)
	if c2 != nil {
		c2.Close()
	}
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Errorf("a new connection in the blackhole should time out dialing, got err=%v", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("dialing in the blackhole returned after %v, without sitting out the timeout", d)
	}

	p.Restore()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := r.ReadByte(); err == nil {
		t.Error("an old connection from the blackhole should be broken by Restore, but it still reads something")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("an old connection from the blackhole was not broken by Restore")
	}
	c3, err := net.DialTimeout("tcp", p.Addr(), time.Second)
	if err != nil {
		t.Fatalf("no connection after Restore: %v", err)
	}
	defer c3.Close()
	roundTrip(t, c3, bufio.NewReader(c3))

	// Cut inside the blackhole: "down" turns into "connection refused"
	p.Blackhole()
	p.Cut()
	if _, err := net.DialTimeout("tcp", p.Addr(), time.Second); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("a new connection should be refused after a Cut in the blackhole, got err=%v", err)
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
		t.Errorf("one round trip took %v, want about 150ms", d)
	}

	// Five lines in one go: each line's delay counts from its own arrival, so about 150ms in all rather than 750ms
	start := time.Now()
	c.Write([]byte(strings.Repeat("hello\n", 5)))
	for range 5 {
		if line, err := r.ReadString('\n'); err != nil || line != "hello\n" {
			t.Fatalf("read back %q, err=%v", line, err)
		}
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("5 lines waited %v in all, the delays added up", d)
	}

	p.SetDelay(0)
	if d := roundTrip(t, c, r); d > 100*time.Millisecond {
		t.Errorf("one round trip still takes %v after the delay was taken off", d)
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
		t.Errorf("response %d %q", resp.StatusCode, body)
	}

	got := s.Last(t)
	if got.Path != "/echo" || got.Query.Get("token") != "abc" || got.Header.Get("X-Request-Id") != "rid-1" {
		t.Errorf("the recorded request is wrong: %+v", got)
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

	// One request in every 4 answers 500
	res := Load(t, LoadSpec{
		Concurrency: 4,
		Requests:    200,
		NewRequest: func(ctx context.Context, i int) (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/?fail=%d", srv.URL, btoi(i%4 == 0)), nil)
		},
	})
	t.Log(res)
	if res.Requests != 200 || res.Errors != 0 || res.Status[200] != 150 || res.Status[500] != 50 || res.OK() != 150 {
		t.Errorf("the buckets are wrong: %v", res)
	}
	if res.P50 <= 0 || res.P50 > res.P90 || res.P90 > res.P99 || res.P99 > res.Max || res.QPS <= 0 {
		t.Errorf("the percentiles are wrong: %v", res)
	}

	// Running for a duration: it stops when the time is up and counts everything it sent
	res = Load(t, LoadSpec{Concurrency: 2, Duration: 200 * time.Millisecond, URL: srv.URL})
	if res.Requests == 0 || res.Status[200] != res.Requests || res.Elapsed < 200*time.Millisecond {
		t.Errorf("running for a duration is wrong: %v", res)
	}

	// One that cannot connect is a transport error and lands in no status bucket
	srv.Close()
	res = Load(t, LoadSpec{Requests: 3, URL: srv.URL})
	if res.Errors != 3 || len(res.Status) != 0 || len(res.ErrorSamples) == 0 {
		t.Errorf("the transport errors were not counted right: %v", res)
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
		t.Errorf("the total is %v, want 4", got)
	}
	if got := m.Sum("e2e_http_requests_total", "route", "/ping"); got != 3 {
		t.Errorf("/ping %v, want 3", got)
	}
	if got := m.Sum("e2e_order_flow_seconds_bucket", "le", "+Inf"); got != 2 {
		t.Errorf("the +Inf bucket is %v, want 2", got)
	}
	if got := m.Sum("e2e_order_flow_seconds_count"); got != 2 {
		t.Errorf("count %v, want 2", got)
	}
}

func TestReadProc_ReadsOwnCPUAndMemory(t *testing.T) {
	Require(t)
	s, err := ReadProc(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if s.RSS <= 0 || s.PeakRSS < s.RSS || s.Threads <= 0 {
		t.Errorf("the readings are wrong: %+v", s)
	}
}

func TestResetPeakRSS_PeakRestartsFromCurrentRSS(t *testing.T) {
	Require(t)
	// Push the peak up by 256MB first and then give that memory back to the operating system: before the reset the peak still holds it, and after the reset it should not
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
		t.Errorf("after the reset the peak should start from the current RSS: peak before %dMB, peak after %dMB (current RSS %dMB)", high.PeakRSS>>20, s.PeakRSS>>20, s.RSS>>20)
	}
}
