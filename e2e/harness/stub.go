package harness

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Stub 下游桩服务器：记下收到的每个请求，状态码、延迟、响应体可以随时改。
//
//	stub := harness.NewStub(t)
//	p := harness.Start(t, harness.Options{Downstream: stub.URL})
//	p.Get(t, "/proxy?token=abc")
//	req := stub.Last(t)  // req.Query.Get("token") == "abc"
//
// 要在它前面插一个 Proxy 注入网络故障：harness.NewProxy(t, stub.Addr())，
// 再把 "http://" + proxy.Addr() 交给 Options.Downstream
type Stub struct {
	// URL 形如 http://127.0.0.1:12345
	URL string
	srv *httptest.Server

	mu          sync.Mutex
	status      int
	delay       time.Duration
	contentType string
	body        []byte
	reqs        []StubRequest
}

// StubRequest 桩收到的一个请求
type StubRequest struct {
	Method   string
	Path     string
	RawQuery string
	Query    url.Values
	Header   http.Header
	Body     []byte
	At       time.Time
}

// NewStub 起一个桩，默认 200 {"ok":true}，测试结束时关掉
func NewStub(t testing.TB) *Stub {
	t.Helper()
	s := &Stub{status: http.StatusOK, contentType: "application/json", body: []byte(`{"ok":true}`)}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	t.Cleanup(func() {
		// 先断开连接：还有请求在 SetDelay 里等着的话，Close 会一直等它
		s.srv.CloseClientConnections()
		s.srv.Close()
	})
	return s
}

// Addr 桩的 host:port
func (s *Stub) Addr() string { return strings.TrimPrefix(s.URL, "http://") }

// SetStatus 之后的响应用这个状态码
func (s *Stub) SetStatus(code int) {
	s.mu.Lock()
	s.status = code
	s.mu.Unlock()
}

// SetDelay 之后每个请求等 d 再响应；请求的 ctx 先结束（客户端放弃、连接断开）就不等了
func (s *Stub) SetDelay(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

// SetBody 之后的响应体
func (s *Stub) SetBody(contentType, body string) {
	s.mu.Lock()
	s.contentType, s.body = contentType, []byte(body)
	s.mu.Unlock()
}

// Requests 到目前为止收到的全部请求（收到时就记下，不等响应发完）
func (s *Stub) Requests() []StubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]StubRequest(nil), s.reqs...)
}

// Count 收到了几个请求
func (s *Stub) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

// Last 最后一个请求，一个都没收到时 t.Fatal
func (s *Stub) Last(t testing.TB) StubRequest {
	t.Helper()
	reqs := s.Requests()
	if len(reqs) == 0 {
		t.Fatal("the stub has not received any request")
	}
	return reqs[len(reqs)-1]
}

// Reset 清掉记下的请求
func (s *Stub) Reset() {
	s.mu.Lock()
	s.reqs = nil
	s.mu.Unlock()
}

func (s *Stub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, StubRequest{
		Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Query: r.URL.Query(),
		Header: r.Header.Clone(), Body: body, At: time.Now(),
	})
	status, delay, ct, resp := s.status, s.delay, s.contentType, s.body
	s.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(status)
	_, _ = w.Write(resp)
}
