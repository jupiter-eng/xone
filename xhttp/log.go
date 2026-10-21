package xhttp

import (
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
)

// installLog records one log line per logical request (Config.Log).
//
// It hangs on OnSuccess / OnError for the same reason as the metrics: they are called once after every retry has
// finished (measured on resty v2.17.2 with RetryCount=2 and the connection cut: 3 attempts sent, OnError called once, Attempt=3).
// What is recorded is the method, host, path, status code, duration and attempt count; the query string, the fragment,
// the userinfo, the headers and the body are not -- a query string often holds a token or a signature, the same principle that keeps the query string out of the outbound Span's url.full.
//
// resty logs on the retry path itself too: one WARN per failed attempt (..., Attempt N) and one ERROR once they are used up,
// whose content is the error on the line here. With Log on, neither is printed any more (restyLogger's quiet, set on the client in New),
// so one logical request is one line here; the per-attempt detail is in the outbound Spans (one Span per attempt).
// When the caller replaces it with their own SetLogger, resty's logs follow resty's rules and this does not interfere.
//
// What is not recorded (measured on resty v2.17.2): a request resty rejects before sending it (a GET with multipart, say)
// goes through OnInvalid and calls neither of these two callbacks.
func installLog(client *resty.Client, slow time.Duration) {
	markStart(client)
	client.OnSuccess(func(_ *resty.Client, resp *resty.Response) {
		if resp != nil && resp.Request != nil {
			logRequest(resp.Request, resp, nil, slow)
		}
	})
	client.OnError(func(req *resty.Request, err error) {
		if req == nil {
			return
		}
		// An error after a response was received (a failing SetResult, say) is wrapped by resty in a ResponseError.
		var resp *resty.Response
		var re *resty.ResponseError
		if errors.As(err, &re) {
			resp, err = re.Response, re.Err
		}
		logRequest(req, resp, err, slow)
	})
}

// logRequest records one logical request.
//
// A failure is no usable response (err is non-nil) or a 5xx from the downstream; a 4xx is a business answer and is recorded at INFO as usual.
// A non-nil err includes cases where a response did arrive: a 200 whose SetResult cannot be parsed, a 3xx under NoRedirectPolicy.
// One that is both slow and failed is recorded as a failure (carrying the error).
// The ctx is the caller's: the outbound Span opens inside the transport and has ended by now, so the span_id is the caller's.
func logRequest(req *resty.Request, resp *resty.Response, err error, slow time.Duration) {
	ctx := req.Context()
	status, fallback := 0, sinceAttempt(req)
	if resp != nil {
		status, fallback = resp.StatusCode(), resp.Time()
	}
	took := elapsed(req, fallback)
	failed := err != nil || status >= 500
	isSlow := slow > 0 && took > slow
	if !failed && !isSlow && !slog.Default().Enabled(ctx, slog.LevelInfo) {
		return
	}

	host, path := target(req)
	attrs := []any{"method", req.Method, "host", host, "path", path, "status", status,
		"elapsed_ms", ms(took), "attempts", req.Attempt}
	if err != nil {
		attrs = append(attrs, "error", scrubError(err))
	}

	switch {
	case failed:
		slog.WarnContext(ctx, "http request failed", attrs...)
	case isSlow:
		slog.WarnContext(ctx, "slow http request", append(attrs, "threshold_ms", ms(slow))...)
	default:
		slog.InfoContext(ctx, "http request", attrs...)
	}
}

// sinceAttempt is how long this attempt has taken. For one that failed before anything went out (an SRV lookup that found nothing) Request.Time is the zero value,
// and time.Since would be 292 years, logged as elapsed_ms 9223372036854.775, so 0 is recorded instead.
func sinceAttempt(req *resty.Request) time.Duration {
	if req.Time.IsZero() {
		return 0
	}
	return time.Since(req.Time)
}

// ---- URLs in the logs ----
//
// Every URL xhttp writes out (the error in a request log, resty's own logs, the outbound Span's url.full)
// keeps only the scheme, the host and the path: the query string, the fragment and the userinfo are always removed. A query string often holds a token or a signature.

// scrubError is the error text with the query string, the fragment and the userinfo removed from the URLs inside it.
//
// Structure comes first: every *url.Error on the error chain (wrapped with %w or joined with errors.Join alike) has its URL replaced by a clean one before rendering.
// The standard library's text is Get "http://user:***@host/x?token=…": … (measured on Go 1.25: only the password becomes ***, the user name stays),
// and that URL is not necessarily absolute: when a redirect policy refuses (NoRedirectPolicy, or FlexibleRedirectPolicy used up)
// what goes in is the Location as it is, which may be a relative /b?token=…; a query string may also hold unescaped spaces.
// Neither of those is reliably found by text, so text is only a fallback (scrubText) for the free text outside the error chain.
func scrubError(err error) string {
	s := err.Error()
	for _, ue := range urlErrors(err) {
		// A *url.Error renders its URL with %q, the same as strconv.Quote.
		if clean := cleanURL(ue.URL); clean != ue.URL {
			s = strings.ReplaceAll(s, strconv.Quote(ue.URL), strconv.Quote(clean))
		}
	}
	return scrubText(s)
}

// urlErrors are every *url.Error on the error chain, including each branch of an errors.Join.
func urlErrors(err error) []*url.Error {
	var out []*url.Error
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if ue, ok := e.(*url.Error); ok {
			out = append(out, ue)
		}
		switch w := e.(type) {
		case interface{ Unwrap() error }:
			walk(w.Unwrap())
		case interface{ Unwrap() []error }:
			for _, e := range w.Unwrap() {
				walk(e)
			}
		}
	}
	walk(err)
	return out
}

// cleanURL removes the query string, the fragment and the userinfo from a URL string, relative ones included.
// One that cannot be parsed (an invalid % escape in the path, say) is cut at the first ? or #, and the userinfo is then removed by the scheme://…@ shape.
func cleanURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return bareURL(u)
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	start := 0
	if i := strings.Index(raw, "://"); i >= 0 {
		start = i + len("://")
	}
	end := len(raw)
	if i := strings.IndexByte(raw[start:], '/'); i >= 0 {
		end = start + i
	}
	if at := strings.LastIndexByte(raw[start:end], '@'); at >= 0 {
		raw = raw[:start] + raw[start+at+1:]
	}
	return raw
}

// bareURL is u with the userinfo, the query string and the fragment removed, leaving u itself untouched.
func bareURL(u *url.URL) string {
	c := *u
	c.User, c.RawQuery, c.ForceQuery, c.Fragment, c.RawFragment = nil, "", false, "", ""
	return c.String()
}

var (
	// quotedQuery is the query string and fragment of a quoted URL (absolute or relative alike): up to the closing quote, not stopping at a space inside.
	// No whitespace is allowed before the question mark, so that a sentence such as "what is this?" is not cut.
	quotedQuery = regexp.MustCompile(`"([^"\s?#]*)[?#][^"]*"`)
	// urlQuery is the query string and fragment of an unquoted http(s) URL, up to whitespace.
	urlQuery = regexp.MustCompile(`(https?://[^\s"?#]*)[?#][^\s"]*`)
	// urlUserinfo is the userinfo of an http(s) URL (user@ or user:***@).
	urlUserinfo = regexp.MustCompile(`(https?://)[^\s"/?#@]*@`)
)

// scrubText removes each URL's query string, fragment and userinfo by text. It is the fallback: whatever has a recognizable structure goes through scrubError first.
// What it does not recognize: a URL with no quotes that does not start with http(s):// (the request line in resty's debug mode, GET /x?token=…, for example).
func scrubText(s string) string {
	s = quotedQuery.ReplaceAllString(s, `"$1"`)
	s = urlQuery.ReplaceAllString(s, "$1")
	return urlUserinfo.ReplaceAllString(s, "$1")
}

// target is the host and path of the last attempt actually sent. For one that failed before a native request was assembled (a middleware before the request failed),
// it falls back to the URL string on the resty request. Only the Host and the path are taken: the userinfo, the query string and the fragment are all dropped.
// The path is taken in its escaped form (EscapedPath): /a%2Fb is not recorded as /a/b, and a newline or a quote in the path does not reach the log as it is.
func target(req *resty.Request) (host, path string) {
	u := (*url.URL)(nil)
	if req.RawRequest != nil {
		u = req.RawRequest.URL
	} else if parsed, err := url.Parse(req.URL); err == nil {
		u = parsed
	}
	if u == nil {
		return "", ""
	}
	return u.Host, u.EscapedPath()
}

// ms converts an elapsed time to milliseconds, keeping microseconds. The field name carries the unit, matching xgorm's and xredis's elapsed_ms.
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
