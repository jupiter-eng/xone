package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/labstack/echo/v4"

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

func TestAddSensitiveNames_WritesTheSharedRegistry(t *testing.T) {
	// Names match by "equals", so a probe that only contains the name stays unmasked
	name := fmt.Sprintf("fwd_probe_name_%d", probes.Add(1))
	body := []byte(`{"` + name + `":"` + secret + `","x_` + name + `":"kept"}`)
	if !strings.Contains(web.RedactBody(body, "application/json"), secret) {
		t.Fatal("precondition: the probe name must not be sensitive before it is added")
	}

	AddSensitiveNames(name)

	got := web.RedactBody(body, "application/json")
	if strings.Contains(got, secret) {
		t.Errorf("AddSensitiveNames did not reach the shared registry: %s", got)
	}
	if !strings.Contains(got, "kept") {
		t.Errorf("a field that only contains the name must be kept: %s", got)
	}
}

// sensitiveNameChildEnv marks the child process TestLog_SensitiveNameKeepsEchoErrorMessage runs
// itself in
const sensitiveNameChildEnv = "XONE_TEST_SENSITIVE_NAME_CHILD"

func TestLog_SensitiveNameKeepsEchoErrorMessage(t *testing.T) {
	// It runs in a child process: the word list is per process and only grows, and code registered
	// here would change what every other test in this package reads in its errors field
	// (code=404, message=Not Found)
	if os.Getenv(sensitiveNameChildEnv) != "1" {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(self, "-test.run=^TestLog_SensitiveNameKeepsEchoErrorMessage$", "-test.count=1")
		cmd.Env = append(os.Environ(), sensitiveNameChildEnv+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("the child process failed: %v\n%s", err, out)
		}
		return
	}

	// The case the issue reported: with code added through AddSensitiveFields, every echo error
	// reached the log as "code=***REDACTED***" with the message and the cause gone
	AddSensitiveNames("code")
	lines := capture(t)
	serve(t, get("/hello?code="+secret+"&country_code=CN"), []echo.MiddlewareFunc{Log(WithQuery(true))},
		func(c echo.Context) error {
			return echo.NewHTTPError(400, "invalid tenant ref").SetInternal(errors.New("db down"))
		})

	a := accessLogs(lines())
	if len(a) != 1 {
		t.Fatalf("exactly one access log is expected, got=%v", a)
	}
	if got, want := a[0]["errors"], "code="+Redacted+", message=invalid tenant ref, internal=db down"; got != want {
		t.Errorf("errors=%v, want %q", got, want)
	}
	q, _ := a[0]["query"].(string)
	if strings.Contains(q, secret) || !strings.Contains(q, "country_code=CN") {
		t.Errorf("only code may be masked in the query, got=%q", q)
	}
}
