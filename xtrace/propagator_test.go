package xtrace

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/propagation"
)

func mustProp(t *testing.T, global []string, rules []ForwardHeaderRule) *HeaderPropagator {
	t.Helper()
	p, err := NewHeaderPropagator(global, rules)
	if err != nil {
		t.Fatalf("construction failed: %v", err)
	}
	return p
}

// fromTrusted is a carrier declaring "the peer is trusted", standing in for the one xgin hands over after judging by TrustedProxies.
type fromTrusted struct{ propagation.HeaderCarrier }

func (fromTrusted) TrustedPeer() bool { return true }

// trusted wraps request headers as a carrier from a trusted peer.
func trusted(h http.Header) propagation.TextMapCarrier {
	return fromTrusted{propagation.HeaderCarrier(h)}
}

// roundTrip simulates one "receive from a trusted upstream, send downstream": Extract then Inject, returning the headers the downstream received.
func roundTrip(p *HeaderPropagator, in http.Header, targetHost string) http.Header {
	ctx := p.Extract(context.Background(), trusted(in))
	if targetHost != "" {
		ctx = WithTargetHost(ctx, targetHost)
	}
	out := http.Header{}
	p.Inject(ctx, propagation.HeaderCarrier(out))
	return out
}

func TestHeaderPropagator_GlobalForwarding(t *testing.T) {
	p := mustProp(t, []string{"X-Request-Id", "X-Tenant-Id"}, nil)
	in := http.Header{"X-Request-Id": {"r1"}, "X-Tenant-Id": {"t1"}, "X-Other": {"o"}}

	out := roundTrip(p, in, "any.example.com")
	if out.Get("X-Request-Id") != "r1" || out.Get("X-Tenant-Id") != "t1" {
		t.Errorf("a configured header must be forwarded, got=%v", out)
	}
	if out.Get("X-Other") != "" {
		t.Errorf("an unconfigured header must not be forwarded, got=%v", out)
	}
}

// declared is a carrier that states whether it is trusted.
type declared struct {
	propagation.HeaderCarrier
	ok bool
}

func (d declared) TrustedPeer() bool { return d.ok }

func TestHeaderPropagator_AcceptsOnlyFromTrustedPeers(t *testing.T) {
	// Regression case. Forwarded values used to be taken from any inbound request at all: an internet client sending
	// an X-Tenant-Id / X-Internal-Token had it treated as "given by the upstream" and carried into every internal call.
	// A carrier without TrustedPeer() (otelhttp's handler, one assembled by business code) is never trusted.
	p := mustProp(t, []string{"X-Tenant-Id"}, []ForwardHeaderRule{
		{Domains: []string{"*.internal.com"}, Headers: []string{"X-Internal-Token"}},
	})
	in := http.Header{"X-Tenant-Id": {"forged"}, "X-Internal-Token": {"forged"}}

	for name, c := range map[string]struct {
		carrier propagation.TextMapCarrier
		want    bool
	}{
		"a plain HeaderCarrier": {propagation.HeaderCarrier(in), false},
		"declared untrusted":    {declared{propagation.HeaderCarrier(in), false}, false},
		"declared trusted":      {declared{propagation.HeaderCarrier(in), true}, true},
	} {
		ctx := WithTargetHost(p.Extract(context.Background(), c.carrier), "api.internal.com")
		out := http.Header{}
		p.Inject(ctx, propagation.HeaderCarrier(out))
		for _, h := range []string{"X-Tenant-Id", "X-Internal-Token"} {
			if got := out.Get(h) != ""; got != c.want {
				t.Errorf("%s: %s forwarded=%v, want %v", name, h, got, c.want)
			}
		}
	}
}

func TestHeaderPropagator_WarnsOnceForUntrustedPeerHeaders(t *testing.T) {
	// It is the only clue when "forwarding is configured but nothing is forwarded"; but anybody on the internet can send
	// these headers, and warning every time would be a way to flood the log.
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	p := mustProp(t, []string{"X-Tenant-Id"}, nil)
	p.Extract(context.Background(), propagation.HeaderCarrier(http.Header{"X-Other": {"x"}}))
	if buf.Len() != 0 {
		t.Fatalf("without a forwarding header there must be no warning, got=%s", buf.String())
	}
	for i := 0; i < 3; i++ {
		p.Extract(context.Background(), propagation.HeaderCarrier(http.Header{"X-Tenant-Id": {"forged"}}))
	}
	if n := strings.Count(buf.String(), "untrusted peer"); n != 1 {
		t.Errorf("it must warn only once, got=%d times: %s", n, buf.String())
	}
	// The message must not name XGin alone: a process using only xecho would add an XGin block to follow it and fail startup with "a config key nobody reads".
	if want := "xtrace ignored forward headers from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted"; !strings.Contains(buf.String(), want) {
		t.Errorf("the warning must read %q, got=%s", want, buf.String())
	}
}

func TestHeaderPropagator_GlobalForwardingIgnoresDomain(t *testing.T) {
	// Global means global: it is sent even when the target domain is unknown.
	p := mustProp(t, []string{"X-Request-Id"}, nil)
	out := roundTrip(p, http.Header{"X-Request-Id": {"r1"}}, "")
	if out.Get("X-Request-Id") != "r1" {
		t.Errorf("a global header must be forwarded even without a target domain, got=%v", out)
	}
}

func TestHeaderPropagator_DomainRules(t *testing.T) {
	// An internal identifier goes to our own services, never to a third party -- which is the whole reason this rule exists.
	p := mustProp(t, nil, []ForwardHeaderRule{
		{Domains: []string{"api.internal.com", "*.trusted.com"}, Headers: []string{"X-Internal-Token"}},
	})
	in := http.Header{"X-Internal-Token": {"secret"}}

	for _, c := range []struct {
		host string
		want bool
	}{
		{"api.internal.com", true},
		{"api.internal.com:8080", true}, // with a port
		{"API.Internal.com", true},      // case-insensitive
		{"sub.trusted.com", true},       // the wildcard matches
		{"a.b.trusted.com", true},       // a subdomain several levels deep
		{"trusted.com", false},          // *.trusted.com does not match the bare domain
		{"nottrusted.com", false},       // not a suffix means not a match
		{"eviltrusted.com", false},      // a similar suffix but not a subdomain
		{"third-party.com", false},      // a third party
		{"", false},                     // nobody to send to, so better not sent
	} {
		out := roundTrip(p, in, c.host)
		if got := out.Get("X-Internal-Token") != ""; got != c.want {
			t.Errorf("host=%q must forward=%v, got=%v", c.host, c.want, got)
		}
	}
}

func TestNewHeaderPropagator_OnlyLeadingStarDotWildcard(t *testing.T) {
	// Regression case. The matching used to strip a leading * and compare the suffix, under which *trusted.com also
	// matched eviltrusted.com -- an internal token sent to a domain anybody can register.
	// The only correct spelling is *.example.com, and every other spelling with a * is caught at startup.
	for _, d := range []string{"*trusted.com", "a.*.com", "*", "*.", "**.a.com", "*.*.a.com", "api.*"} {
		_, err := NewHeaderPropagator(nil, []ForwardHeaderRule{{Domains: []string{d}, Headers: []string{"X-Internal-Token"}}})
		if err == nil {
			t.Errorf("Domains=%q must be an error", d)
		}
	}
	if _, err := NewHeaderPropagator(nil, []ForwardHeaderRule{{Domains: []string{"*.trusted.com", "api.a.com"}, Headers: []string{"X-T"}}}); err != nil {
		t.Errorf("a valid spelling must not be an error: %v", err)
	}
}

func TestNewHeaderPropagator_ContradictoryConfigFails(t *testing.T) {
	// One side says send it to everyone, the other says send it only to these. Guessing either way could send an internal identifier to a third party.
	_, err := NewHeaderPropagator(
		[]string{"X-Internal-Token", "X-Request-Id"},
		[]ForwardHeaderRule{{Domains: []string{"*.internal.com"}, Headers: []string{"x-internal-token"}}},
	)
	if err == nil {
		t.Fatal("the same header appearing in both places must be an error")
	}
	if !strings.Contains(err.Error(), "X-Internal-Token") {
		t.Errorf("the error must name the header, got=%v", err)
	}
	if strings.Contains(err.Error(), "X-Request-Id") {
		t.Errorf("a header that does not conflict must not appear in the error, got=%v", err)
	}
}

func TestNewHeaderPropagator_Normalization(t *testing.T) {
	p := mustProp(t, []string{"x-request-id", "", "X-REQUEST-ID"}, []ForwardHeaderRule{
		{Domains: []string{" Example.COM ", ""}, Headers: []string{"x-trace-tag"}},
		{Domains: nil, Headers: []string{"X-Dropped"}}, // no domain, the rule is discarded whole
		{Domains: []string{"a.com"}, Headers: nil},     // no header, the rule is discarded whole
	})

	if want := []string{"X-Request-Id", "X-Trace-Tag"}; !reflect.DeepEqual(p.Fields(), want) {
		t.Errorf("header names must be normalized and deduplicated, got=%v want=%v", p.Fields(), want)
	}
	out := roundTrip(p, http.Header{"X-Trace-Tag": {"v"}}, "example.com")
	if out.Get("X-Trace-Tag") != "v" {
		t.Errorf("domains must be trimmed and lowercased before matching, got=%v", out)
	}
}

func TestHeaderPropagator_FieldsReturnsCopy(t *testing.T) {
	p := mustProp(t, []string{"X-Request-Id"}, nil)
	f := p.Fields()
	f[0] = "changed"
	if p.Fields()[0] != "X-Request-Id" {
		t.Error("Fields must return a copy, so the caller cannot change the internal state")
	}
}

func TestHeaderPropagator_NoOpWithEmptyConfig(t *testing.T) {
	p := mustProp(t, nil, nil)
	ctx := p.Extract(context.Background(), trusted(http.Header{"X-Request-Id": {"r1"}}))
	if ForwardHeadersFromContext(ctx) != nil {
		t.Error("with no header configured nothing may be put into the ctx")
	}
	out := http.Header{}
	p.Inject(ctx, propagation.HeaderCarrier(out))
	if len(out) != 0 {
		t.Errorf("with nothing configured nothing may be injected, got=%v", out)
	}
}

func TestHeaderPropagator_ExtractMergesExistingValues(t *testing.T) {
	// A composite Propagator may hold several HeaderPropagators, and a later one must not overwrite an earlier one.
	p1 := mustProp(t, []string{"X-A"}, nil)
	p2 := mustProp(t, []string{"X-B"}, nil)

	ctx := p1.Extract(context.Background(), trusted(http.Header{"X-A": {"a"}}))
	ctx = p2.Extract(ctx, trusted(http.Header{"X-B": {"b"}}))

	got := ForwardHeadersFromContext(ctx)
	if got["X-A"] != "a" || got["X-B"] != "b" {
		t.Errorf("the results of two Extracts must be merged, got=%v", got)
	}
}

func TestForwardHeaderFromContext(t *testing.T) {
	p := mustProp(t, []string{"X-Request-Id"}, nil)
	ctx := p.Extract(context.Background(), trusted(http.Header{"X-Request-Id": {"r1"}}))

	if got := ForwardHeaderFromContext(ctx, "x-request-id"); got != "r1" {
		t.Errorf("lookup must be case-insensitive, got=%q", got)
	}
	if got := ForwardHeaderFromContext(ctx, "X-Nope"); got != "" {
		t.Errorf("a header that is not there must give an empty string, got=%q", got)
	}
	if got := ForwardHeaderFromContext(context.Background(), "X-Request-Id"); got != "" {
		t.Errorf("an empty ctx must give an empty string, got=%q", got)
	}
}

func TestForwardHeadersFromContext_ReturnsCopy(t *testing.T) {
	p := mustProp(t, []string{"X-Request-Id"}, nil)
	ctx := p.Extract(context.Background(), trusted(http.Header{"X-Request-Id": {"r1"}}))

	m := ForwardHeadersFromContext(ctx)
	m["X-Request-Id"] = "changed"
	if ForwardHeaderFromContext(ctx, "X-Request-Id") != "r1" {
		t.Error("a copy must be returned, so the caller cannot change the values in the ctx")
	}
	if ForwardHeadersFromContext(context.Background()) != nil {
		t.Error("an empty ctx must give nil")
	}
}

func TestHeaderPropagator_EmptyValuesNotInjected(t *testing.T) {
	p := mustProp(t, []string{"X-Request-Id"}, nil)
	out := roundTrip(p, http.Header{"X-Request-Id": {""}}, "example.com")
	if _, ok := out["X-Request-Id"]; ok {
		t.Errorf("without a value from the upstream no empty header may be written downstream, got=%v", out)
	}
}

func TestWithTargetHost(t *testing.T) {
	if got := TargetHostFromContext(context.Background()); got != "" {
		t.Errorf("it must be empty when never set, got=%q", got)
	}
	ctx := WithTargetHost(context.Background(), "a.example.com:80")
	if got := TargetHostFromContext(ctx); got != "a.example.com:80" {
		t.Errorf("what comes back must match what was stored, got=%q", got)
	}
}
