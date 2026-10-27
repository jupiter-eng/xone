package xtrace

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel/propagation"

	"github.com/jupiter-eng/xone/xerror"
)

// context key
type (
	forwardHeadersKey struct{}
	targetHostKey     struct{}
)

// headerRule is a normalized domain rule.
type headerRule struct {
	domains []string // lowercase domain patterns, supporting *.example.com
	headers []string // normalized header names
}

// HeaderPropagator forwards the configured custom HTTP headers (X-Request-Id, X-Tenant-Id and the like).
//
// It implements propagation.TextMapPropagator, so once it is installed as the global Propagator, components such as
// otelhttp carry these headers automatically, with no business code involved.
//
// There are two modes: global forwarding sends them to every downstream, while a domain rule sends them only to the
// matching ones. Both accept only values from a trusted peer, see Extract.
type HeaderPropagator struct {
	globalHeaders []string
	rules         []headerRule
	allHeaders    []string // global plus rules, deduplicated, used by Extract and Fields

	// warned makes the warning about forwarding headers from an untrusted peer happen only once
	warned atomic.Bool
}

// NewHeaderPropagator creates a HeaderPropagator.
//
// Header names are normalized with http.CanonicalHeaderKey and domains are lowercased.
// A rule whose domains or headers are empty is discarded whole.
//
// Only two spellings of a domain are accepted: the exact api.example.com and the wildcard *.example.com.
// Any other spelling with a * (*example.com, a.*.com, *) is an error outright; the reasoning is in checkDomain.
//
// The same header appearing both in globalHeaders and in a domain rule is an error outright: that configuration
// contradicts itself -- one side says send it to everyone, the other says send it only to these. Guessing either way
// could send an internal identifier to a third party, so it is stopped at startup.
func NewHeaderPropagator(globalHeaders []string, rules []ForwardHeaderRule) (*HeaderPropagator, error) {
	p, err := newHeaderPropagator(globalHeaders, rules)
	if err != nil {
		return nil, xerror.New("xtrace", "config", err)
	}
	return p, nil
}

// newHeaderPropagator is NewHeaderPropagator returning a plain error:
// Config.Validate uses it to check the forwarding configuration too, and that layer's error is wrapped by its caller.
func newHeaderPropagator(globalHeaders []string, rules []ForwardHeaderRule) (*HeaderPropagator, error) {
	normalizedRules, err := normalizeRules(rules)
	if err != nil {
		return nil, fmt.Errorf("invalid ForwardHeaderRules: %w", err)
	}

	restricted := make(map[string]struct{})
	for _, r := range normalizedRules {
		for _, h := range r.headers {
			restricted[h] = struct{}{}
		}
	}

	global, err := canonicalHeaders(globalHeaders)
	if err != nil {
		return nil, fmt.Errorf("invalid ForwardHeaders: %w", err)
	}
	var conflicts []string
	for _, h := range global {
		if _, limited := restricted[h]; limited {
			conflicts = append(conflicts, h)
		}
	}
	if len(conflicts) > 0 {
		slices.Sort(conflicts)
		return nil, fmt.Errorf("header %s appears in both ForwardHeaders and ForwardHeaderRules; "+
			"the former sends it to every domain, the latter only to the listed ones — remove one of them",
			strings.Join(conflicts, ", "))
	}

	return &HeaderPropagator{
		globalHeaders: global,
		rules:         normalizedRules,
		allHeaders:    mergeHeaders(global, normalizedRules),
	}, nil
}

// normalizeRules normalizes the domain rules, discarding whole any rule whose domains or headers are empty.
func normalizeRules(rules []ForwardHeaderRule) ([]headerRule, error) {
	out := make([]headerRule, 0, len(rules))
	for _, r := range rules {
		domains, err := normalizeDomains(r.Domains)
		if err != nil {
			return nil, err
		}
		headers, err := canonicalHeaders(r.Headers)
		if err != nil {
			return nil, err
		}
		if len(domains) == 0 || len(headers) == 0 {
			continue
		}
		out = append(out, headerRule{domains: domains, headers: headers})
	}
	return out, nil
}

// mergeHeaders deduplicates while keeping a stable order: global first, then the rules.
func mergeHeaders(global []string, rules []headerRule) []string {
	all := make([]string, 0, len(global))
	seen := make(map[string]struct{}, len(global))
	add := func(h string) {
		if _, ok := seen[h]; ok {
			return
		}
		seen[h] = struct{}{}
		all = append(all, h)
	}
	for _, h := range global {
		add(h)
	}
	for _, r := range rules {
		for _, h := range r.headers {
			add(h)
		}
	}
	return all
}

// canonicalHeaders normalizes header names and discards empty ones.
//
// It does not TrimSpace: a header name may not contain spaces anyway, and CanonicalHeaderKey returns a name with
// illegal characters unchanged, so trimming would quietly fix an obvious configuration error instead.
//
// A name that is not a field name is refused outright, as a malformed domain is in checkDomain and for the same
// reason: this configuration decides who internal identifiers are sent to, and getting no error for it is worse
// than it looks. Such a name can never match anything an inbound request carries -- net/http hands over canonical
// names and nothing else -- so it forwards nothing, in silence, while the configuration reads as if it did.
func canonicalHeaders(headers []string) ([]string, error) {
	out := make([]string, 0, len(headers))
	for _, h := range headers {
		if h == "" {
			continue
		}
		if !validHeaderName(h) {
			return nil, fmt.Errorf("header name %q is not a valid HTTP field name, so nothing would ever be forwarded under it", h)
		}
		out = append(out, http.CanonicalHeaderKey(h))
	}
	return out, nil
}

// validHeaderName reports whether h is an HTTP field name, that is RFC 9110's token. An empty name is
// not one; the caller above drops those before asking, since an empty entry in a list is a gap, not a mistake.
func validHeaderName(h string) bool {
	if h == "" {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

func normalizeDomains(domains []string) ([]string, error) {
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		if err := checkDomain(d); err != nil {
			return nil, err
		}
		out = append(out, strings.ToLower(d))
	}
	return out, nil
}

// checkDomain allows a wildcard only at the very front and only when followed by a dot: *.example.com.
//
// The matching used to be "strip a leading * and compare the suffix", under which *trusted.com also matched
// eviltrusted.com -- a domain anybody can register receiving an internal token. Spellings such as * and a.*.com
// likewise do not say what they mean to match. This is configuration deciding who internal credentials are sent to,
// so anything unclear is not guessed at and stops startup outright.
func checkDomain(d string) error {
	if rest, _ := strings.CutPrefix(d, "*."); rest == "" || strings.Contains(rest, "*") {
		return fmt.Errorf("domain pattern %q is not supported, use an exact domain (api.example.com) or a leading *. wildcard (*.example.com)", d)
	}
	return nil
}

// Extract reads every configured header out of the upstream request and stores them in the context.
//
// It does not distinguish domains: what comes in is taken, and who it goes to is Inject's business.
//
// It does distinguish origins: only values from a trusted peer are accepted. Forwarding means "pass the values the
// upstream gave on to the downstream as they are", so who the upstream is, is the trust boundary -- taking everything
// would have an internet client's X-Tenant-Id / X-Internal-Token treated as our own and carried into every internal call.
//
// This package does not know who the peer is; the layer accepting the request does, so the carrier says it:
// it implements
//
//	TrustedPeer() bool
//
// and returns true to count as trusted. xgin's and xecho's tracing middleware judge the directly connected peer by
// their own TrustedProxies, and the carrier they hand over carries this method when the peer is trusted. A carrier
// without it (otelhttp's handler, a MapCarrier assembled by business code) always counts as untrusted: when in doubt,
// do not take it. To take values from a genuinely trusted source such as a message queue, add this method to the carrier.
func (p *HeaderPropagator) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	if len(p.allHeaders) == 0 {
		return ctx
	}
	if !fromTrustedPeer(carrier) {
		p.warnUntrusted(carrier)
		return ctx
	}

	existing := forwardHeadersRaw(ctx)
	var merged map[string]string
	for _, h := range p.allHeaders {
		v := carrier.Get(h)
		if v == "" {
			continue
		}
		if merged == nil {
			merged = make(map[string]string, len(p.allHeaders)+len(existing))
			maps.Copy(merged, existing)
		}
		merged[h] = v
	}

	if merged == nil {
		return ctx
	}
	return context.WithValue(ctx, forwardHeadersKey{}, merged)
}

// fromTrustedPeer reports whether the carrier declares "the peer sending these values is trusted", see Extract.
func fromTrustedPeer(c propagation.TextMapCarrier) bool {
	t, ok := c.(interface{ TrustedPeer() bool })
	return ok && t.TrustedPeer()
}

// warnUntrusted warns once when an untrusted peer brings forwarding headers.
//
// It is the only clue when "forwarding is configured but nothing is forwarded": those values used to be taken
// unconditionally. It warns once only -- anybody on the internet can send these headers, and warning every time would be a way to flood the log.
func (p *HeaderPropagator) warnUntrusted(c propagation.TextMapCarrier) {
	if p.warned.Load() {
		return
	}
	for _, h := range p.allHeaders {
		if c.Get(h) != "" {
			if p.warned.CompareAndSwap(false, true) {
				slog.Warn("xtrace ignored forward headers from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted",
					"header", h)
			}
			return
		}
	}
}

// trustedBaggage is W3C baggage, accepted only from a trusted peer.
//
// Baggage and forwarding headers are the same thing: key-value pairs the upstream gave, carried to the downstream and
// into every call. OTel's own propagation.Baggage accepts them from anybody, so the X-Tenant-Id that ForwardHeaders
// keeps out would be taken as our own and carried into the network as soon as an internet client rewrote it as
// baggage: tenant=... So it recognizes the same marker as HeaderPropagator.Extract: the carrier implements TrustedPeer() and returns true.
//
// Injection is not restricted: the baggage in a ctx either came from a trusted upstream or was written by this process.
type trustedBaggage struct {
	propagation.Baggage

	// warned makes the warning about baggage from an untrusted peer happen only once
	warned atomic.Bool
}

// Extract accepts it only from a trusted peer, see trustedBaggage.
func (b *trustedBaggage) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	if !fromTrustedPeer(carrier) {
		// Warned once only, for the same reason as warnUntrusted.
		if carrier.Get(baggageHeader) != "" && b.warned.CompareAndSwap(false, true) {
			slog.Warn("xtrace ignored baggage from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted")
		}
		return ctx
	}
	return b.Baggage.Extract(ctx, carrier)
}

// baggageHeader is the W3C baggage request header.
const baggageHeader = "baggage"

// Inject writes the forwarded values from the context into the downstream request.
//
// Global headers are injected unconditionally; rule headers only when the target domain matches.
// The target domain is written into the context by Transport -- without it, no domain rule ever matches.
func (p *HeaderPropagator) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	if len(p.allHeaders) == 0 {
		return
	}
	vals := forwardHeadersRaw(ctx)
	if len(vals) == 0 {
		return
	}

	for _, h := range p.globalHeaders {
		if v := vals[h]; v != "" {
			carrier.Set(h, v)
		}
	}

	if len(p.rules) == 0 {
		return
	}
	host := TargetHostFromContext(ctx)
	if host == "" {
		return
	}
	for _, rule := range p.rules {
		if !matchDomains(host, rule.domains) {
			continue
		}
		for _, h := range rule.headers {
			if v := vals[h]; v != "" {
				carrier.Set(h, v)
			}
		}
	}
}

// Fields returns every header this Propagator manages (a copy).
func (p *HeaderPropagator) Fields() []string { return slices.Clone(p.allHeaders) }

// matchDomains reports whether host matches any domain pattern, supporting exact matches and *.example.com.
//
// *.example.com matches a subdomain at any depth (a.example.com, a.b.example.com) but not example.com itself:
// to include the bare domain, write example.com as well.
// The patterns have already been through checkDomain when constructed, so one with a * necessarily starts with *.
func matchDomains(host string, patterns []string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host // no port
	}
	h = strings.ToLower(h)

	for _, pattern := range patterns {
		if parent, ok := strings.CutPrefix(pattern, "*."); ok {
			// What is compared is ". plus the parent domain", so eviltrusted.com does not match *.trusted.com
			if strings.HasSuffix(h, "."+parent) {
				return true
			}
			continue
		}
		if h == pattern {
			return true
		}
	}
	return false
}

// WithTargetHost writes the target request's Host into the context, for the domain rules to filter on.
func WithTargetHost(ctx context.Context, host string) context.Context {
	return context.WithValue(ctx, targetHostKey{}, host)
}

// TargetHostFromContext takes the target request's Host back out, or an empty string when there is none.
func TargetHostFromContext(ctx context.Context) string {
	host, _ := ctx.Value(targetHostKey{}).(string)
	return host
}

func forwardHeadersRaw(ctx context.Context) map[string]string {
	m, _ := ctx.Value(forwardHeadersKey{}).(map[string]string)
	return m
}

// ForwardHeadersFromContext returns every forwarded header key-value pair (a copy).
func ForwardHeadersFromContext(ctx context.Context) map[string]string {
	return maps.Clone(forwardHeadersRaw(ctx)) // Extract only stores a non-empty map, so it is nil when there is none
}

// ForwardHeaderFromContext takes the value of one header, case-insensitively.
func ForwardHeaderFromContext(ctx context.Context, key string) string {
	return forwardHeadersRaw(ctx)[http.CanonicalHeaderKey(key)]
}
