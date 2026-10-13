package middleware

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jupiter-eng/xone/internal/web"
)

// 脱敏本身的用例在 internal/web/redact_test.go；这里只验公开的这几个名字确实转到了那边

const secret = "hunter2"

func TestRedacted_SameMarkerAsShared(t *testing.T) {
	// 这里的常量是字面量（go doc 里给使用者看的是值），和 web 的那个必须一字不差
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

// probes 每跑一次换一个探针名：词表只能加不能减，-count=2 的第二轮还用同一个名字的话前提就不成立了。
// 词按「含」匹配，所以数字后面还得收个尾：不然 field_1 会认出 field_10
var probes atomic.Int64

func TestAddSensitive_WritesTheSharedRegistry(t *testing.T) {
	// 词表进程里只有一张：从这里加的词，别的 Web 集成（它们读 internal/web）也得认。
	// 用专门的名字，免得影响别的用例
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
