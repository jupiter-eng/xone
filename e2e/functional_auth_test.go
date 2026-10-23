package e2e

import (
	"net/http"
	"testing"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// v1Token is the credential service/v1.go accepts, spelled out again here: e2e starts the service
// as a process and shares no symbol with it.
const v1Token = "Bearer e2e-token"

// A version prefix with an authentication middleware in front of it is the shape almost every real
// API has, and the rest of this service did not have one. Three promises meet on it:
//
//   - the route label is the whole template, the prefix included, in the access log, the metrics
//     and the span name alike. With the part registered inside the group alone, /api/v1/users/:id
//     and a later /api/v2/users/:id would share one series and one span name;
//   - a request the middleware turned away is recorded under that template as well, not as
//     unmatched: a 401 is an answer on a route this service has, and recording it as unmatched
//     would hide every authentication failure behind the label 404s already use;
//   - the credential the middleware read is masked in the access log.
//
// The route templates asserted everywhere else in e2e are registered at the top level, so none of
// this was covered before.
func TestFunctional_VersionedRouteBehindAuth(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Overlay: payloadLogs, Spans: true})
	const (
		route = "/api/v1/users/:id"
		path  = "/api/v1/users/42"
	)

	for _, c := range []struct {
		what   string
		header []string
		status int
	}{
		{"the credential it accepts", []string{"Authorization", v1Token}, http.StatusOK},
		{"no credential at all", nil, http.StatusUnauthorized},
		{"somebody else's credential", []string{"Authorization", "Bearer not-ours"}, http.StatusForbidden},
	} {
		r := p.Get(t, path, c.header...)
		if r.Status != c.status {
			t.Errorf("%s: it promises %d, got %v", c.what, c.status, r)
			continue
		}
		tid := traceIDOf(t, r)

		l := accessLog(t, p, tid)
		if got := l.Str("route"); got != route {
			t.Errorf("%s: the route label should be the whole template %q, got %q", c.what, route, got)
		}
		if got := l.Str("path"); got != path {
			t.Errorf("%s: the real path belongs in path, got %q", c.what, got)
		}
		if got, _ := l.Get("status"); got != float64(c.status) {
			t.Errorf("%s: status should be %d, got %v", c.what, c.status, got)
		}
		if len(c.header) > 0 {
			if got := l.Str("request_headers.Authorization"); got != redacted {
				t.Errorf("%s: the credential should be masked in the access log, got %q", c.what, got)
			}
		}

		if srv := serverSpan(t, p, tid); srv.Name != "GET "+route || srv.Str("http.route") != route {
			t.Errorf("%s: the span name and http.route should use the whole template, got name=%q route=%q",
				c.what, srv.Name, srv.Str("http.route"))
		}
	}

	m := waitMetrics(t, p, "the three answers counted under the versioned template", func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", route) == 3
	})
	for _, status := range []string{"200", "401", "403"} {
		if got := m.Sum("e2e_http_requests_total", "route", route, "status", status); got != 1 {
			t.Errorf("the %s should be counted once under route=%s, got %v", status, route, got)
		}
	}
}
