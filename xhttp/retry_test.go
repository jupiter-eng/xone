package xhttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
)

// flaky cuts the connection after reading the body on the 1st request (a transport-layer error) and answers status after that.
// It records the body length received each time.
type flaky struct {
	mu     sync.Mutex
	bodies []int
	status int
}

func newFlaky(t *testing.T, status int) (*flaky, string) {
	f := &flaky{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, len(b))
		first := len(f.bodies) == 1
		f.mu.Unlock()
		if first && f.status == 0 {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(cmpStatus(f.status))
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func cmpStatus(s int) int {
	if s == 0 {
		return http.StatusOK
	}
	return s
}

func (f *flaky) got() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.bodies...)
}

func retrying(t *testing.T, onlyIdempotent bool) *resty.Client {
	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, time.Millisecond, 5*time.Millisecond
	c.RetryOnlyIdempotent = onlyIdempotent
	client, _ := newQuiet(t, c)
	return client
}

const payload = "payload-that-must-not-be-lost"

// resty v2.17.2 rebuilds the http.Request from Request.Body on every attempt (middleware.go createHTTPRequest),
// and an io.Reader was read to the end on the first: the second attempt sends an empty body, the server answers 200, and the caller sees success.
func TestRetry_ConsumedReaderBodyNotResent(t *testing.T) {
	for _, only := range []bool{true, false} {
		for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPost} {
			if only && method == http.MethodPost {
				continue // a POST is not retried at this setting anyway, see TestRetry_ActuallyResends
			}
			f, url := newFlaky(t, 0)
			client := retrying(t, only)
			resp, err := client.R().SetContext(context.Background()).SetBody(strings.NewReader(payload)).Execute(method, url)
			if got := f.got(); len(got) != 1 || got[0] != len(payload) {
				t.Errorf("RetryOnlyIdempotent=%v %s: an io.Reader body can only be sent once, lengths received got=%v", only, method, got)
			}
			if err == nil {
				t.Errorf("RetryOnlyIdempotent=%v %s: the first transport error must reach the caller as it is, got status=%v", only, method, resp.StatusCode())
			}
		}
	}
}

// For contrast: a []byte, string or struct body is complete on every rebuild and is retried as usual.
func TestRetry_RewindableBodyStillRetried(t *testing.T) {
	for name, body := range map[string]any{
		"[]byte": []byte(payload), "string": payload, "map": map[string]string{"k": payload},
		"no body": nil,
	} {
		f, url := newFlaky(t, 0)
		client := retrying(t, true)
		r := client.R().SetContext(context.Background())
		if body != nil {
			r.SetBody(body)
		}
		if _, err := r.Put(url); err != nil {
			t.Errorf("%s: it must succeed after the retry, got=%v", name, err)
		}
		got := f.got()
		if len(got) != 2 || got[0] != got[1] {
			t.Errorf("%s: it must retry once with both bodies the same length, got=%v", name, got)
		}
	}
}

// With SetContentLength(true), resty reads the io.Reader into a buffer and sets Body to nil, sending that buffer every time (middleware.go handleRequestBody).
func TestRetry_ReaderBufferedBySetContentLengthStillRetried(t *testing.T) {
	f, url := newFlaky(t, 0)
	client := retrying(t, true)
	if _, err := client.R().SetContext(context.Background()).SetContentLength(true).SetBody(strings.NewReader(payload)).Put(url); err != nil {
		t.Errorf("it must succeed after the retry, got=%v", err)
	}
	if got := f.got(); len(got) != 2 || got[0] != len(payload) || got[1] != len(payload) {
		t.Errorf("it must retry once with a complete body both times, got=%v", got)
	}
}

// resty's AddRetryCondition is an "or": with any one of them returning true, an earlier false does not count
// (v2.17.2 retry.go Backoff). A condition the caller attached for other requests must not put a POST retry back.
func TestRetry_UserConditionCannotReenableNonIdempotentRetry(t *testing.T) {
	f, url := newFlaky(t, 0)
	client := retrying(t, true)
	client.AddRetryCondition(func(_ *resty.Response, err error) bool { return err != nil })
	_, err := client.R().SetContext(context.Background()).SetBody(payload).Post(url)
	if got := f.got(); len(got) != 1 {
		t.Errorf("a POST must not be retried, got %d attempts", len(got))
	}
	if err == nil || strings.Contains(err.Error(), "xhttp") {
		t.Errorf("the caller must get the first transport error, not our veto, got=%v", err)
	}

	// A request-level condition comes first and is blocked just the same.
	f, url = newFlaky(t, 0)
	_, _ = client.R().SetContext(context.Background()).
		AddRetryCondition(func(*resty.Response, error) bool { return true }).Post(url)
	if got := f.got(); len(got) != 1 {
		t.Errorf("a request-level condition must not let a POST retry either, got %d attempts", len(got))
	}
}

// Retrying on a response (a 5xx condition the caller attached) cannot be blocked: resty gives RetryAfter one exit only,
// and vetoing there once a response arrived would hand the caller an error we invented rather than that 503.
// That setting is written as it is in Config.RetryOnlyIdempotent and in the README.
func TestRetry_UserStatusConditionNotVetoedAndNoBogusError(t *testing.T) {
	f, url := newFlaky(t, http.StatusServiceUnavailable)
	client := retrying(t, true)
	client.AddRetryCondition(func(r *resty.Response, _ error) bool { return r.StatusCode() >= 500 })
	resp, err := client.R().SetContext(context.Background()).SetBody(payload).Post(url)
	if err != nil {
		t.Errorf("no error may appear once a response arrived, got=%v", err)
	}
	if resp.StatusCode() != http.StatusServiceUnavailable {
		t.Errorf("the caller must get that last 503, got=%d", resp.StatusCode())
	}
	if got := f.got(); len(got) != 3 {
		t.Errorf("a caller's status-code retry condition must keep working, got %d attempts", len(got))
	}
}

// The exception: with the body an io.Reader already read, it is not retried even when a response arrived -- resending an empty body is far worse than an error.
func TestRetry_UserStatusConditionWithConsumedReaderFailsLoudly(t *testing.T) {
	f, url := newFlaky(t, http.StatusServiceUnavailable)
	client := retrying(t, true)
	client.AddRetryCondition(func(r *resty.Response, _ error) bool { return r.StatusCode() >= 500 })
	resp, err := client.R().SetContext(context.Background()).SetBody(strings.NewReader(payload)).Put(url)
	if got := f.got(); len(got) != 1 {
		t.Errorf("an io.Reader body must not be sent again, got=%v", got)
	}
	if err == nil || !strings.Contains(err.Error(), "io.Reader") {
		t.Errorf("it must say why it was not retried, got=%v", err)
	}
	if resp == nil || resp.StatusCode() != http.StatusServiceUnavailable {
		t.Errorf("the response must still reach the caller")
	}
}
