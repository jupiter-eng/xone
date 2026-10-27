package reqstate

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func request(remote string) *http.Request {
	r := httptest.NewRequest("GET", "/ping", nil)
	r.RemoteAddr = remote
	return r
}

// ---- State ----

func TestNew_PutsTheStateOnTheContext(t *testing.T) {
	r := request("10.0.0.1:1234")
	ctx, s := New(r.Context(), r, nil, NewRecorder(httptest.NewRecorder()))
	if From(ctx) != s {
		t.Error("the state on the ctx should be the one that was returned")
	}
	// The pointer is what makes a later write (the route template) visible to whoever reads it.
	s.Route = "/users/:id"
	if From(ctx).Route != "/users/:id" {
		t.Error("a write after the fact should be visible through the ctx")
	}
}

func TestFrom_WithoutAStateIsNil(t *testing.T) {
	if From(context.Background()) != nil {
		t.Error("a ctx with no state should yield nil, which the middleware falls back on")
	}
}

type allTrusted struct{}

func (allTrusted) Trusts(string) bool            { return true }
func (allTrusted) ClientIP(*http.Request) string { return "1.2.3.4" }

type noneTrusted struct{}

func (noneTrusted) Trusts(string) bool              { return false }
func (noneTrusted) ClientIP(r *http.Request) string { return "peer" }

func TestNew_DecidesTrustFromTheDirectPeer(t *testing.T) {
	r := request("10.0.0.1:1234")
	_, trusted := New(r.Context(), r, allTrusted{}, nil)
	if !trusted.Trusted {
		t.Error("a trusted peer should be marked as one")
	}
	_, untrusted := New(r.Context(), r, noneTrusted{}, nil)
	if untrusted.Trusted {
		t.Error("an untrusted peer must not be marked")
	}
	// Nobody configured: the safe side is to trust nobody.
	_, none := New(r.Context(), r, nil, nil)
	if none.Trusted {
		t.Error("without a proxy list nothing is trusted")
	}
}

func TestState_ClientIP(t *testing.T) {
	r := request("203.0.113.9:1234")
	_, s := New(r.Context(), r, allTrusted{}, nil)
	if got := s.ClientIP(r); got != "1.2.3.4" {
		t.Errorf("the configured computation should decide, got=%q", got)
	}

	_, plain := New(r.Context(), r, nil, nil)
	if got := plain.ClientIP(r); got != "203.0.113.9" {
		t.Errorf("without one it is the peer itself, got=%q", got)
	}
	// A nil state is what a call from outside a request finds.
	var nilState *State
	if got := nilState.ClientIP(r); got != "203.0.113.9" {
		t.Errorf("a nil state must not panic and falls back to the peer, got=%q", got)
	}
}

// ---- Recorder ----

func TestRecorder_StatusDefaultsTo200(t *testing.T) {
	// net/http sends a 200 for a handler that wrote nothing, so that is what has to be recorded --
	// a 0 in the access log would look like a request that never answered.
	rec := NewRecorder(httptest.NewRecorder())
	if rec.Status() != 200 || rec.Started() {
		t.Errorf("an untouched recorder reports 200 and not started, got=%d %v", rec.Status(), rec.Started())
	}

	rec = NewRecorder(httptest.NewRecorder())
	_, _ = rec.Write([]byte("hi"))
	if rec.Status() != 200 || !rec.Started() {
		t.Errorf("a write with no WriteHeader is a 200, got=%d %v", rec.Status(), rec.Started())
	}
}

func TestRecorder_CountsStatusAndBytes(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := NewRecorder(inner)
	rec.WriteHeader(201)
	_, _ = rec.Write([]byte("hello"))
	_, _ = rec.Write([]byte(" world"))

	if rec.Status() != 201 {
		t.Errorf("the status should be the one that was written, got=%d", rec.Status())
	}
	if rec.Bytes() != len("hello world") {
		t.Errorf("bytes should count the body alone, got=%d", rec.Bytes())
	}
	if inner.Body.String() != "hello world" {
		t.Errorf("everything should still reach the real writer, got=%q", inner.Body.String())
	}
}

func TestRecorder_SecondWriteHeaderIsIgnored(t *testing.T) {
	// net/http ignores it and logs a superfluous WriteHeader; recording the second code would have
	// the access log disagree with what the client received.
	rec := NewRecorder(httptest.NewRecorder())
	rec.WriteHeader(404)
	rec.WriteHeader(500)
	if rec.Status() != 404 {
		t.Errorf("the first status is the one that went out, got=%d", rec.Status())
	}
}

func TestRecorder_CaptureIsCapped(t *testing.T) {
	rec := NewRecorder(httptest.NewRecorder())
	rec.Capture(8)
	_, _ = rec.Write([]byte("0123456789abcdef"))

	if got := string(rec.Body()); got != "01234567" {
		t.Errorf("the captured body should stop at the cap, got=%q", got)
	}
	// Capping what is kept must not cap what is sent.
	if rec.Bytes() != 16 {
		t.Errorf("the whole body should still have been written, got=%d", rec.Bytes())
	}
}

func TestRecorder_WithoutCaptureKeepsNothing(t *testing.T) {
	rec := NewRecorder(httptest.NewRecorder())
	_, _ = rec.Write([]byte("secret"))
	if len(rec.Body()) != 0 {
		t.Errorf("nothing should be kept unless capturing was asked for, got=%q", rec.Body())
	}
}

// flushRecorder answers Flush and nothing else.
type flushRecorder struct {
	http.ResponseWriter
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

func TestRecorder_ForwardsFlush(t *testing.T) {
	// SSE and long polling need it, and beego's context.Response asks the writer it was handed.
	inner := &flushRecorder{ResponseWriter: httptest.NewRecorder()}
	NewRecorder(inner).Flush()
	if !inner.flushed {
		t.Error("Flush should reach the writer underneath")
	}
	// A writer that cannot flush must not panic.
	NewRecorder(writerOnly{httptest.NewRecorder()}).Flush()
}

// writerOnly implements http.ResponseWriter and nothing else.
type writerOnly struct{ http.ResponseWriter }

// hijackRecorder answers Hijack.
type hijackRecorder struct {
	http.ResponseWriter
	called bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.called = true
	return nil, nil, nil
}

func TestRecorder_ForwardsHijack(t *testing.T) {
	// A WebSocket upgrade goes through it; without forwarding, every upgrade would fail.
	inner := &hijackRecorder{ResponseWriter: httptest.NewRecorder()}
	rec := NewRecorder(inner)
	if _, _, err := rec.Hijack(); err != nil {
		t.Fatalf("Hijack should reach the writer underneath: %v", err)
	}
	if !inner.called {
		t.Error("Hijack should reach the writer underneath")
	}
	// Once hijacked the bytes are no longer counted, so the access log must not report a 200
	// nobody sent.
	if !rec.Started() {
		t.Error("a hijacked connection counts as started")
	}

	if _, _, err := NewRecorder(writerOnly{httptest.NewRecorder()}).Hijack(); err == nil {
		t.Error("a writer that cannot hijack should report it rather than panic")
	}
}

func TestRecorder_Unwrap(t *testing.T) {
	// http.ResponseController reaches the real writer through it.
	inner := httptest.NewRecorder()
	if NewRecorder(inner).Unwrap() != http.ResponseWriter(inner) {
		t.Error("Unwrap should hand out the writer underneath")
	}
}

// plainReader hides a reader's WriteTo: io.Copy prefers src.WriteTo over dst.ReadFrom, and both
// strings.Reader and bytes.Reader have one, so without this the path under test is never taken.
type plainReader struct{ io.Reader }

// readerFromRecorder answers ReadFrom.
type readerFromRecorder struct {
	http.ResponseWriter
	used bool
}

func (r *readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.used = true
	return io.Copy(r.ResponseWriter, src)
}

func TestRecorder_ReadFrom(t *testing.T) {
	inner := &readerFromRecorder{ResponseWriter: httptest.NewRecorder()}
	rec := NewRecorder(inner)
	n, err := io.Copy(rec, plainReader{strings.NewReader("hello")})
	if err != nil || n != 5 {
		t.Fatalf("the copy should succeed, got=%d %v", n, err)
	}
	if !inner.used {
		t.Error("the sendfile path underneath should be used")
	}
	if rec.Bytes() != 5 || !rec.Started() {
		t.Errorf("the bytes should still be counted, got=%d %v", rec.Bytes(), rec.Started())
	}
}

func TestRecorder_ReadFromKeepsCapturing(t *testing.T) {
	// The access log reads the captured copy, not the writer. Handed to the sendfile path
	// underneath, the bytes are counted but never seen, and response_body would be empty while
	// bytes_out was right.
	inner := &readerFromRecorder{ResponseWriter: httptest.NewRecorder()}
	rec := NewRecorder(inner)
	rec.Capture(4)

	n, err := io.Copy(rec, plainReader{strings.NewReader("hello")})
	if err != nil || n != 5 {
		t.Fatalf("the copy should succeed, got=%d %v", n, err)
	}
	if string(rec.Body()) != "hell" {
		t.Errorf("the capture should hold the body up to its cap, got=%q", rec.Body())
	}
	if rec.Bytes() != 5 {
		t.Errorf("the bytes should still be counted, got=%d", rec.Bytes())
	}
	if inner.used {
		t.Error("while capturing, the bytes have to travel through Write rather than the sendfile path")
	}
}

func TestRecorder_ReadFromWithoutOneUnderneath(t *testing.T) {
	// io.Copy would find ReadFrom on the recorder itself and recurse without the guard.
	inner := httptest.NewRecorder()
	rec := NewRecorder(writerOnly{inner})
	n, err := io.Copy(rec, plainReader{bytes.NewReader([]byte("hello"))})
	if err != nil || n != 5 {
		t.Fatalf("the copy should succeed, got=%d %v", n, err)
	}
	if rec.Bytes() != 5 {
		t.Errorf("the bytes should be counted, got=%d", rec.Bytes())
	}
	if inner.Body.String() != "hello" {
		t.Errorf("the body should have reached the writer, got=%q", inner.Body.String())
	}
}
