package web

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsText(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/json":         true,
		"text/plain; charset=utf8": true,
		"application/xml":          true,
		"image/png":                false,
		"application/octet-stream": false,
		"":                         false,
	} {
		if got := isText(ct); got != want {
			t.Errorf("isText(%q)=%v want %v", ct, got, want)
		}
	}
}

// countingBody records how many bytes were read, to make clear how much was really buffered
type countingBody struct {
	r      io.Reader
	read   int
	closed bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += n
	return n, err
}
func (b *countingBody) Close() error { b.closed = true; return nil }

func TestSnapshotBody_BuffersOnlyPrefix(t *testing.T) {
	// maxRequestBody bounds "how much is logged" and must not casually become "how much of the
	// request body is buffered". Read whole, a large upload would lie in memory and the handler
	// could not begin until all of it had landed
	const total = 3 * maxRequestBody
	body := &countingBody{r: bytes.NewReader(bytes.Repeat([]byte("x"), total))}
	req := httptest.NewRequest("POST", "/", nil)
	req.Body, req.GetBody = body, nil
	req.Header.Set("Content-Type", "application/json")

	got := SnapshotBody(req)

	if len(got) != maxRequestBody {
		t.Errorf("the log must keep the first %d bytes alone, got=%d", maxRequestBody, len(got))
	}
	if body.read > maxRequestBody+4096 {
		t.Errorf("only the prefix may be read, %d bytes were read (of %d)", body.read, total)
	}

	// Downstream still has to read the whole request body
	rest, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != total {
		t.Errorf("downstream must get the whole request body of %d bytes, got=%d", total, len(rest))
	}
}

func TestSnapshotBody_CloseReachesOriginalBody(t *testing.T) {
	// Swapping in an io.NopCloser would swallow http.Request's close semantics
	body := &countingBody{r: bytes.NewReader([]byte(`{"a":1}`))}
	req := httptest.NewRequest("POST", "/", nil)
	req.Body, req.GetBody = body, nil
	req.Header.Set("Content-Type", "application/json")

	SnapshotBody(req)
	if err := req.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !body.closed {
		t.Error("Close must land on the original body")
	}
}

// partialThenEOF returns "some data plus an error" first and EOF on the next call.
// That is valid io.Reader behavior, and it is what a truncated request looks like at the network
// layer.
type partialThenEOF struct {
	data []byte
	done bool
	err  error
}

func (r *partialThenEOF) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), r.err
}
func (r *partialThenEOF) Close() error { return nil }

func TestSnapshotBody_PrereadErrorPassedDownstream(t *testing.T) {
	// Putting only the bytes back would have downstream read "the prefix plus EOF" -- a truncated
	// request looking exactly like a complete one, from which the business layer concludes "it all
	// arrived"
	boom := errors.New("connection reset by peer")
	req := httptest.NewRequest("POST", "/", nil)
	req.Body, req.GetBody = &partialThenEOF{data: []byte("partial"), err: boom}, nil
	req.Header.Set("Content-Type", "application/json")

	SnapshotBody(req)

	got, err := io.ReadAll(req.Body)
	if string(got) != "partial" {
		t.Errorf("the bytes already read have to go back downstream, got=%q", got)
	}
	if !errors.Is(err, boom) {
		t.Errorf("the error the read-ahead hit has to go back downstream too, got=%v", err)
	}
}

func TestSnapshotBody_EmptyWithoutBody(t *testing.T) {
	if got := SnapshotBody(nil); got != nil {
		t.Errorf("a nil request must return nil, got=%v", got)
	}
	req := httptest.NewRequest("GET", "/hello", nil)
	req.Body = http.NoBody
	if got := SnapshotBody(req); got != nil {
		t.Errorf("NoBody must return nil, got=%v", got)
	}
	req2 := httptest.NewRequest("GET", "/hello", nil)
	req2.Body = nil
	if got := SnapshotBody(req2); got != nil {
		t.Errorf("no body must return nil, got=%v", got)
	}
}

func TestSnapshotBody_MediaTypeCaseInsensitive(t *testing.T) {
	// A media type is case-insensitive under RFC 9110. Compared literally, a Multipart/Form-Data
	// upload goes around the check and the whole file reaches the log
	for _, ct := range []string{"Multipart/Form-Data; boundary=x", "Application/Octet-Stream"} {
		req := httptest.NewRequest("POST", "/hello", strings.NewReader("file contents"))
		req.Header.Set("Content-Type", ct)
		if got := string(SnapshotBody(req)); !strings.Contains(got, "omitted") {
			t.Errorf("a body with Content-Type=%q must not be read into the log, got=%q", ct, got)
		}
	}
}

func TestAccessLog_ErrorsFieldIsRedacted(t *testing.T) {
	// Both web integrations write errors through here: an error reported by a driver or a
	// downstream often hides a credential
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	var l AccessLog
	for msg, want := range map[string]string{
		"db down":                       `"errors":"db down"`,
		"login failed password=hunter2": `"errors":"login failed password=` + Redacted + `"`, // the value goes, the sentence stays
	} {
		buf.Reset()
		l.Log(&Access{Request: httptest.NewRequest("GET", "/x", nil), Route: "/x", Status: 500, Errors: msg})
		if !strings.Contains(buf.String(), want) || strings.Contains(buf.String(), "hunter2") {
			t.Errorf("errors must be recorded as %s, got=%s", want, buf.String())
		}
	}
}

// A body as long as the cap is one the access log had to stop at, and the masking has to be told
// so: redactJSON's fast path assumes a complete body, and the cut can take the very character it
// goes by -- a DSN cut before its @ holds no sensitive word and no @ either. See redactBody.
func TestAccessLog_ABodyAsLongAsTheCapIsNotFastPathed(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	// A DSN cut before its @, with the padding worked out so the body is exactly as long as the
	// cap: that is what tells the access log it had to stop here. Nothing in it is a sensitive
	// word and the @ is gone, so the fast path would write it out as it is
	cutDSN := func(cap int) []byte {
		head, tail := `{"pad":"`, `","dsn":"postgres://app:hunter2`
		b := head + strings.Repeat("x", cap-len(head)-len(tail)) + tail
		if len(b) != cap || !strings.HasSuffix(b, "hunter2") {
			t.Fatalf("the case builds a %d-byte body for a cap of %d", len(b), cap)
		}
		return []byte(b)
	}

	t.Run("request", func(t *testing.T) {
		buf.Reset()
		l := AccessLog{ReqBody: true}
		r := httptest.NewRequest("POST", "/x", nil)
		r.Header.Set("Content-Type", "application/json")
		l.Log(&Access{Request: r, ReqBody: cutDSN(maxRequestBody)})
		if strings.Contains(buf.String(), "hunter2") {
			t.Errorf("the password reached the log: %.200s", buf.String())
		}
	})

	t.Run("response", func(t *testing.T) {
		buf.Reset()
		l := AccessLog{RespBody: true}
		h := http.Header{"Content-Type": {"application/json"}}
		l.Log(&Access{Request: httptest.NewRequest("GET", "/x", nil), RespHeader: h, RespBody: cutDSN(MaxResponseBody)})
		if strings.Contains(buf.String(), "hunter2") {
			t.Errorf("the password reached the log: %.200s", buf.String())
		}
	})

	t.Run("a shorter body still takes the fast path", func(t *testing.T) {
		buf.Reset()
		l := AccessLog{RespBody: true}
		h := http.Header{"Content-Type": {"application/json"}}
		l.Log(&Access{Request: httptest.NewRequest("GET", "/x", nil), RespHeader: h, RespBody: []byte(`{"id":1}`)})
		if !strings.Contains(buf.String(), `"response_body":"{\"id\":1}"`) {
			t.Errorf("a complete body must be recorded as it is, got=%s", buf.String())
		}
	})
}

func TestAccessLog_EncodedResponseBodyOmitted(t *testing.T) {
	// Compressed bytes in a log are a run of garbage; identity means no compression
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	l := AccessLog{RespBody: true}
	for enc, want := range map[string]string{
		"gzip":     `"response_body":"[gzip-encoded content omitted]"`,
		" BR ":     `"response_body":"[br-encoded content omitted]"`,
		"identity": `"response_body":"hello"`,
		"":         `"response_body":"hello"`,
	} {
		buf.Reset()
		h := http.Header{"Content-Type": {"text/plain"}}
		if enc != "" {
			h.Set("Content-Encoding", enc)
		}
		l.Log(&Access{Request: httptest.NewRequest("GET", "/x", nil), RespHeader: h, RespBody: []byte("hello")})
		if !strings.Contains(buf.String(), want) {
			t.Errorf("Content-Encoding=%q must record %s, got=%s", enc, want, buf.String())
		}
	}
}
