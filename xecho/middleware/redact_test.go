package middleware

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jupiter-eng/xone/internal/web"
)

// The masking itself is covered in internal/web/redact_test.go; this file only checks that
// these exported names really forward there.

const secret = "hunter2"

func TestRedacted_SameMarkerAsShared(t *testing.T) {
	// The constant here is a literal (go doc shows the caller its value), so it has to match
	// web's word for word.
	if Redacted != web.Redacted {
		t.Fatalf("Redacted=%q, internal/web writes %q", Redacted, web.Redacted)
	}
}

func TestRedactForwards_ReachSharedImplementation(t *testing.T) {
	if got := RedactBody([]byte(`{"password":"`+secret+`"}`), "application/json"); strings.Contains(got, secret) {
		t.Errorf("RedactBody leaked: %s", got)
	}
	if got := RedactHeaders(http.Header{"Authorization": {secret}}).String(); strings.Contains(got, secret) {
		t.Errorf("RedactHeaders leaked: %s", got)
	}
}

// probes gives every run a fresh probe name: the word list only grows, so reusing a name on the
// second round of -count=2 would break the precondition. Words match by containment, so the
// number needs a terminator after it, or field_1 would also match field_10.
var probes atomic.Int64

func TestAddSensitive_WritesTheSharedRegistry(t *testing.T) {
	// There is one word list per process: a word added here has to be honored by xgin as well
	// (it reads internal/web too). Dedicated names keep this out of other tests' way.
	n := probes.Add(1)
	field, header := fmt.Sprintf("fwd_probe_field_%d_end", n), fmt.Sprintf("X-Fwd-Probe-%d", n)
	body := []byte(`{"` + field + `":"` + secret + `"}`)
	h := http.Header{header: {secret}}
	if !strings.Contains(web.RedactBody(body, "application/json"), secret) ||
		!strings.Contains(web.RedactHeaders(h).String(), secret) {
		t.Fatal("precondition: the probe names must not be sensitive before they are added")
	}

	AddSensitiveFields(field)
	AddSensitiveHeaders(header)

	if got := web.RedactBody(body, "application/json"); strings.Contains(got, secret) {
		t.Errorf("AddSensitiveFields did not reach the shared registry: %s", got)
	}
	if got := web.RedactHeaders(h).String(); strings.Contains(got, secret) {
		t.Errorf("AddSensitiveHeaders did not reach the shared registry: %s", got)
	}
}
