package middleware

// Masking of sensitive values in requests and responses. The implementation and the word list
// live in internal/web; this file only forwards to them. There is one word list per process: a
// word added here is honored by xgin too, and one added through the identically named function in
// xgin/middleware is honored here.

import (
	"log/slog"
	"net/http"

	"github.com/jupiter-eng/xone/internal/web"
)

// Redacted is what every masked value is written as.
const Redacted = "***REDACTED***"

// AddSensitiveFields appends sensitive words, insensitive to case and separators.
//
// The rule is the one the default list follows: a field whose name contains the word is masked,
// in bodies and in request headers alike. Adding id_card, for instance, masks idCard,
// user_id_card and X-Id-Card.
func AddSensitiveFields(fields ...string) { web.AddSensitiveFields(fields...) }

// AddSensitiveHeaders appends request headers masked by exact name, insensitive to case.
//
// It is for headers whose name contains no sensitive word while their content is a credential,
// X-Tenant-Id for example. A header whose name does contain a sensitive word needs no entry: it
// is masked by default.
func AddSensitiveHeaders(headers ...string) { web.AddSensitiveHeaders(headers...) }

// RedactBody masks a request body according to its Content-Type.
//
// Content-Type is lowercased first: RFC 9110 makes that header case-insensitive, and comparing it
// literally would keep "Application/JSON" out of the JSON branch.
//
// JSON is detected as "contains json" rather than matching application/json exactly:
// application/vnd.api+json, application/problem+json and text/json are all JSON, and an exact
// match would let them through untouched. Detecting it wrongly costs nothing — the branch that
// fails to parse JSON masks the whole thing anyway.
func RedactBody(body []byte, contentType string) string { return web.RedactBody(body, contentType) }

// RedactHeaders masks request headers and returns a value ready to hand to slog.
//
// It returns a slog.Value rather than a serialized string: handing over a string would have slog
// escape it once more as an ordinary string field, and the log would read
//
//	"request_headers":"{\"Authorization\":[\"***REDACTED***\"]}"
//
// which has to be unwrapped from a string before the JSON can be parsed at all. With a
// slog.Value, serialization happens exactly once, the field is an ordinary nested object in the
// log, and the json.Marshal is saved along the way (measured: the whole log line goes from
// 2777ns/19 allocs to 1630ns/9 allocs).
//
// A multi-valued header is joined into one comma-separated string rather than an array: a field
// name that is a string on one log line and an array on the next is rejected outright when the
// logging system builds its index.
//
// Two rules, either one masks: the name is on the list (the default list plus
// AddSensitiveHeaders), or the name contains a sensitive word (the same word list the body fields
// use). A list is never complete — Proxy-Authorization was missing from it once — so the word
// list is the backstop.
func RedactHeaders(h http.Header) slog.Value { return web.RedactHeaders(h) }
