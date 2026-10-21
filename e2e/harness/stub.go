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

// Stub is a downstream stub server: it records every request it receives, and its status code,
// delay and response body can be changed at any time.
//
//	stub := harness.NewStub(t)
//	p := harness.Start(t, harness.Options{Downstream: stub.URL})
//	p.Get(t, "/proxy?token=abc")
//	req := stub.Last(t)  // req.Query.Get("token") == "abc"
//
// To inject a network fault in front of it, put a Proxy there: harness.NewProxy(t, stub.Addr()),
// and hand "http://" + proxy.Addr() to Options.Downstream
type Stub struct {
	// URL looks like http://127.0.0.1:12345
	URL string
	srv *httptest.Server

	mu          sync.Mutex
	status      int
	delay       time.Duration
	contentType string
	body        []byte
	reqs        []StubRequest
}

// StubRequest is one request the stub received
type StubRequest struct {
	Method   string
	Path     string
	RawQuery string
	Query    url.Values
	Header   http.Header
	Body     []byte
	At       time.Time
}

// NewStub starts a stub answering 200 {"ok":true} by default, and closes it when the test ends
func NewStub(t testing.TB) *Stub {
	t.Helper()
	s := &Stub{status: http.StatusOK, contentType: "application/json", body: []byte(`{"ok":true}`)}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	t.Cleanup(func() {
		// Break the connections first: with a request still waiting in SetDelay, Close would wait for it
		s.srv.CloseClientConnections()
		s.srv.Close()
	})
	return s
}

// Addr is the stub's host:port
func (s *Stub) Addr() string { return strings.TrimPrefix(s.URL, "http://") }

// SetStatus has the responses from here on carry this status code
func (s *Stub) SetStatus(code int) {
	s.mu.Lock()
	s.status = code
	s.mu.Unlock()
}

// SetDelay has every request from here on wait d before it is answered; a request whose ctx ends
// first (the client gave up, the connection broke) is not waited for
func (s *Stub) SetDelay(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

// SetBody is the response body from here on
func (s *Stub) SetBody(contentType, body string) {
	s.mu.Lock()
	s.contentType, s.body = contentType, []byte(body)
	s.mu.Unlock()
}

// Requests are all the requests received so far (recorded on arrival, not once the response has gone out)
func (s *Stub) Requests() []StubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]StubRequest(nil), s.reqs...)
}

// Count is how many requests arrived
func (s *Stub) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

// Last is the last request, and t.Fatals when none arrived
func (s *Stub) Last(t testing.TB) StubRequest {
	t.Helper()
	reqs := s.Requests()
	if len(reqs) == 0 {
		t.Fatal("the stub has not received any request")
	}
	return reqs[len(reqs)-1]
}

// Reset drops the recorded requests
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
