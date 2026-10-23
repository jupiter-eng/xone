package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

// A body that came back exactly this long is taken as one that was cut: the reader and the writer
// both stop at the cap, so "as long as the cap" means "there may have been more". A body that was
// exactly that long to begin with is treated as cut too, which costs it the fast path and nothing
// else.
const (
	// maxRequestBody is the upper bound on how much of a request body is recorded
	maxRequestBody = 256 * 1024
	// MaxResponseBody is the upper bound on how much of a response body is recorded, far smaller
	// than the request's: a response is usually much larger, and the first few KB are generally
	// enough to track something down. The writer truncating the response lives in the integrations
	// and truncates by this
	MaxResponseBody = 4 * 1024
)

// AccessLog is what the access log records: which paths are skipped, and whether the body, the
// query string, the request headers and the response headers are recorded.
//
// Its zero value works and records nothing extra. Each integration's logging middleware holds one
// and fills it from its own options (xgin/middleware's WithSkipPaths, WithBody and so on).
type AccessLog struct {
	skipExact  map[string]bool
	skipPrefix []string

	ReqBody    bool
	RespBody   bool
	Query      bool
	ReqHeader  bool
	RespHeader bool
}

// SkipPaths adds paths that are not written to the access log: one ending in / matches by prefix
// (/health/ matches /health/live) and the rest match exactly.
func (l *AccessLog) SkipPaths(paths ...string) {
	for _, p := range paths {
		if strings.HasSuffix(p, "/") {
			l.skipPrefix = append(l.skipPrefix, p)
		} else {
			if l.skipExact == nil {
				l.skipExact = map[string]bool{}
			}
			l.skipExact[p] = true
		}
	}
}

// Skip reports whether this request is skipped: its path is on the skip list, or logging at Info
// level is off.
//
// With the level off it is let through outright. The whole of what the middleware does -- buffering
// the request body, wrapping the ResponseWriter to truncate the response, and the redaction and
// serialization afterwards -- exists for one purpose: assembling this one line of log. By the time
// slog decides about the level itself, the price has been paid in full and only the result is
// thrown away.
//
// Decided per request rather than once at construction: the level may change at run time.
func (l *AccessLog) Skip(r *http.Request) bool {
	return l.skipPath(r.URL.Path) || !slog.Default().Enabled(r.Context(), slog.LevelInfo)
}

func (l *AccessLog) skipPath(path string) bool {
	if l.skipExact[path] {
		return true
	}
	return slices.ContainsFunc(l.skipPrefix, func(p string) bool { return strings.HasPrefix(path, p) })
}

// Access is what the access log records at the end of one request that has to be taken out of the
// framework.
//
// The route, the status code, the bytes written, the client IP and the errors are measured by the
// integration and handed in rather than guessed at here: how they are taken differs per framework
// (gin's route is an empty string when nothing matched and its Size is -1 when no response body was
// written; echo's error is only rendered into a response after the middleware returns). The rest is
// taken from the Request.
type Access struct {
	Request  *http.Request
	Ctx      context.Context // the ctx this log is written with (trace_id and xlog.AddKV's fields come from it); nil takes the Request's
	Route    string          // the route template, unmatched when nothing matched
	Status   int
	Elapsed  time.Duration
	ClientIP string
	BytesOut int // the bytes of response body written, headers excluded; 0 when not one byte was written

	RespHeader http.Header // for recording the response headers and deciding the response body's Content-Type
	ReqBody    []byte      // what SnapshotBody took while ReqBody is on
	RespBody   []byte      // the response body truncated while RespBody is on, MaxResponseBody bytes at most
	Errors     string      // the errors the framework recorded, left out when empty; the redaction rules are in RedactText
}

// Log records a as one line of access log (at Info level, with request completed as the message).
//
// The fields' names and their order are what the indexes and the dashboards on the logging platform
// go by, and changing one puts a whole set out of step:
// method route path status elapsed_ms client_ip host proto user_agent bytes_in bytes_out, followed
// as the switches say by request_headers query response_headers request_body response_body, and
// errors last.
func (l *AccessLog) Log(a *Access) {
	r := a.Request
	// slog.Attr directly rather than alternating keys and values: the latter boxes every value into
	// an any for slog to take apart again, measured at 6 allocations more per access log (8 with the
	// bodies recorded).
	// The slice is assembled here and handed to slog here rather than returned from a function:
	// returning it would put it on the heap
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("route", a.Route), // unmatched when nothing matched; the real path is in path
		// Path alone, with no query string: in a request such as GET /login?token=... the credential
		// is in the URL. RequestURI() or URL.String() both look like "recording a bit more of the
		// log" while really writing the credential into it in plaintext
		slog.String("path", r.URL.Path),
		slog.Int("status", a.Status),
		// The unit is in the field name: slog's JSON writes a Duration as an integer of nanoseconds,
		// and 51130 does not read as 51µs.
		// A float goes through json.Marshal in slog's JSON, measured at 2 allocations and about 0.5µs
		// more per line -- integer microseconds would save that, but a log is read by people and
		// milliseconds come easier
		slog.Float64("elapsed_ms", millis(a.Elapsed)),
		slog.String("client_ip", a.ClientIP),
		slog.String("host", r.Host),
		slog.String("proto", r.Proto),
		slog.String("user_agent", r.UserAgent()),
		// The Content-Length of the request headers; a chunked upload has none and records -1, as
		// net/http's convention has it
		slog.Int64("bytes_in", r.ContentLength),
		slog.Int("bytes_out", a.BytesOut),
	}
	if l.ReqHeader {
		// It is a slog.Value already, and slog.Any would box it once more
		attrs = append(attrs, slog.Attr{Key: "request_headers", Value: RedactHeaders(r.Header)})
	}
	if l.Query && r.URL.RawQuery != "" {
		attrs = append(attrs, slog.String("query", redactForm(r.URL.RawQuery)))
	}
	if l.RespHeader {
		attrs = append(attrs, slog.Attr{Key: "response_headers", Value: RedactHeaders(a.RespHeader)})
	}
	if l.ReqBody {
		attrs = append(attrs, slog.String("request_body",
			redactBody(a.ReqBody, r.Header.Get("Content-Type"), len(a.ReqBody) >= maxRequestBody)))
	}
	if l.RespBody {
		ct := a.RespHeader.Get("Content-Type")
		if isText(ct) && len(a.RespBody) > 0 {
			body := encodedOmitted(a.RespHeader)
			if body == "" {
				body = redactBody(a.RespBody, ct, len(a.RespBody) >= MaxResponseBody)
			}
			attrs = append(attrs, slog.String("response_body", body))
		}
	}
	if a.Errors != "" {
		attrs = append(attrs, slog.String("errors", RedactText(a.Errors)))
	}

	ctx := a.Ctx
	if ctx == nil {
		ctx = r.Context()
	}
	slog.LogAttrs(ctx, slog.LevelInfo, "request completed", attrs...)
}

// encodedOmitted is the sentence written into the log when the response body was compressed by
// Content-Encoding (gzip, br and so on), or an empty string when it was not.
//
// A compression middleware (echo's middleware.Gzip, gin-contrib/gzip) sits inside the access log,
// so what is truncated are the compressed bytes: recorded as they are, they would be a run of
// binary garbage, and JSON would not even parse. Decompressing them only to write a log is not
// worth it, so it records one omitted sentence, as an uploaded file does
func encodedOmitted(h http.Header) string {
	enc := strings.ToLower(strings.TrimSpace(h.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return ""
	}
	return "[" + enc + "-encoded content omitted]"
}

// isText reports whether this is a text type fit to be written into the log as it is.
//
// json is matched as a substring, as RedactBody does: application/vnd.api+json and
// application/problem+json are both JSON, and an exact match would leave such response bodies
// unrecorded without a word.
func isText(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "json") ||
		strings.Contains(ct, "text/") ||
		strings.Contains(ct, "xml")
}

// SnapshotBody takes a copy of the request body (the first 256KB at most) without affecting later
// reads.
//
// GetBody comes first: what it returns is a copy and the original Body is untouched.
// Only without one does it fall back to "read it out and put it back".
func SnapshotBody(req *http.Request) []byte {
	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return nil
	}

	// Lowercased first: a media type is case-insensitive under RFC 9110, and compared literally a
	// Multipart/Form-Data upload would go around the checks below and put the whole file into the
	// log
	ct := strings.ToLower(req.Header.Get("Content-Type"))
	// A file upload and a binary stream are not read: the content helps nothing while reading it
	// costs the whole of the memory and the time
	if strings.Contains(ct, "multipart/form-data") {
		return []byte("[multipart/form-data omitted]")
	}
	if strings.Contains(ct, "application/octet-stream") {
		return []byte("[binary content omitted]")
	}

	if req.GetBody != nil {
		if rc, err := req.GetBody(); err == nil {
			defer rc.Close()
			b, _ := io.ReadAll(io.LimitReader(rc, maxRequestBody))
			return b
		}
	}

	// The fallback: only the first maxRequestBody bytes are read and the rest is left in the stream
	// as it is.
	//
	// It must not be read whole. maxRequestBody bounds "how much is logged" and must not casually
	// become "how much of the request body is buffered": a 500MB JSON upload would lie in memory in
	// its entirety, and the handler could not begin until all of it had landed. Writing one line of
	// log does not deserve that price.
	//
	// What was read has to be put back even on a read error: returning halfway would make that half
	// of the request body disappear and hand the handler downstream a body missing its head --
	// writing a log must not be able to change the request itself.
	head, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBody))
	req.Body = &prefixedBody{prefix: head, rest: req.Body, preErr: err}
	if err != nil {
		return nil // an incomplete read is not recorded, while downstream still gets the whole request body and that error
	}
	return head
}

// prefixedBody puts the prefix that was read back in front of the request body, along with the
// error the read-ahead hit.
//
// The error has to go back. A valid Reader may return "some data plus an error" first and EOF on
// the next call -- putting only the bytes back would have downstream read "the prefix plus EOF",
// where a truncated request looks exactly like a complete one and the business layer concludes "it
// all arrived". Writing a log must not be able to change that conclusion.
//
// Close still lands on the original body: that is what really holds the connection, and swapping in
// an io.NopCloser would swallow http.Request's close semantics.
type prefixedBody struct {
	prefix []byte
	off    int
	rest   io.ReadCloser
	preErr error // the error the read-ahead hit, handed downstream once the prefix has been read
}

func (b *prefixedBody) Read(p []byte) (int, error) {
	if b.off < len(b.prefix) {
		n := copy(p, b.prefix[b.off:])
		b.off += n
		return n, nil
	}
	if b.preErr != nil {
		return 0, b.preErr
	}
	return b.rest.Read(p)
}

func (b *prefixedBody) Close() error { return b.rest.Close() }

// millis turns an elapsed time into milliseconds, keeping it to the microsecond
func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
