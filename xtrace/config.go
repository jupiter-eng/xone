// Package xtrace sets up OpenTelemetry's global TracerProvider and Propagator from the configuration.
//
// What the caller gets is native OpenTelemetry: business code opens Spans with otel.Tracer("...") directly and does
// not have to know this package. Only two things here are this package's own:
//
//   - AddSpanProcessor -- attaches the exporter that does the reporting (the framework bundles none)
//   - HeaderPropagator -- forwards custom headers such as X-Request-Id, from the configuration
package xtrace

import (
	"fmt"
	"math"
	"time"
)

// ConfigKey is this module's top-level key in the config file.
const ConfigKey = "XTrace"

// ForwardHeaderRule is a per-domain header forwarding rule.
// The matching headers are forwarded only when the target domain matches one of the patterns in Domains.
type ForwardHeaderRule struct {
	// Domains are the domain patterns, supporting exact matches and a wildcard prefix (*.example.com).
	Domains []string `yaml:"Domains"`

	// Headers are the headers to forward under this rule.
	Headers []string `yaml:"Headers"`
}

// Config is the tracing configuration.
type Config struct {
	// Enable says whether tracing is on. It defaults to on.
	//
	// With it off, Spans can still be created (they are noops) and no TraceID is generated any more,
	// while header forwarding keeps working -- those are two different things.
	Enable bool `yaml:"Enable"`

	// Console says whether Spans are printed to standard output, for local debugging only. It defaults to off.
	Console bool `yaml:"Console"`

	// SampleRatio is the sampling ratio for root Spans, in [0, 1]. It defaults to 1. Out of range or NaN fails startup.
	//
	// It only governs traces with no upstream: when an upstream sends a sampling decision (traceparent's sampled bit)
	// that decision is followed, even at a ratio of 1 -- otherwise a trace an upstream deliberately dropped would be picked back up here.
	//
	// Writing 0 means "sample no new trace at all", while Spans are still created and TraceIDs are still generated and
	// forwarded -- which is how to give downstream services a TraceID without storing Spans locally.
	// To produce no Spans at all (a noop provider), use Enable: false, which is a different thing.
	SampleRatio float64 `yaml:"SampleRatio"`

	// ShutdownTimeout is the upper bound on waiting for Spans to be exported during shutdown. It defaults to 5s and must be greater than 0.
	//
	// There has to be an upper bound: with the export endpoint unreachable, Shutdown blocks forever,
	// and without a deadline that is a hang on exit.
	//
	// Likewise, 0 does not mean "no limit" but "do not wait at all": Shutdown receives an already expired context,
	// and the Spans still in the buffer are dropped outright.
	ShutdownTimeout time.Duration `yaml:"ShutdownTimeout"`

	// ForwardHeaders are the headers forwarded to every domain. There are none by default.
	//
	// Only values from a trusted peer are accepted: with xgin that is whatever is in XGin.TrustedProxies (XEcho.TrustedProxies for xecho),
	// which by default trusts only the private ranges (load balancers, a K8s Ingress and Pods) and not a direct connection from the internet. The reasoning is in HeaderPropagator.Extract.
	ForwardHeaders []string `yaml:"ForwardHeaders"`

	// ForwardHeaderRules are the per-domain header forwarding rules. There are none by default.
	//
	// They are for internal identifiers that must not leak: sent only to our own services, never to a third party.
	// As with ForwardHeaders, only values from a trusted peer are accepted.
	ForwardHeaderRules []ForwardHeaderRule `yaml:"ForwardHeaderRules"`
}

// DefaultConfig holds every default in one place.
func DefaultConfig() Config {
	return Config{
		Enable:          true,
		SampleRatio:     1,
		ShutdownTimeout: 5 * time.Second,
	}
}

// Validate checks what does not make sense in the configuration itself, the header forwarding rules included.
//
// xconfig.Unmarshal calls it after decoding the XTrace block of the config file, so a misconfigured value fails while
// the config is read; a direct call to New calls it once as well.
//
// It returns a plain error here, which the layer calling it (xconfig.Unmarshal or New) wraps in an xerror once:
// wrapping at every level would nest two layers of xtrace into the message.
func (c Config) Validate() error {
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("ShutdownTimeout must be > 0 "+
			"(0 is not unlimited, it is no wait at all, and buffered spans get dropped), got=%v", c.ShutdownTimeout)
	}
	// NaN compares false against anything, so it needs its own check: without catching it, TraceIDRatioBased
	// would compute some indefinable threshold from it.
	if math.IsNaN(c.SampleRatio) || c.SampleRatio < 0 || c.SampleRatio > 1 {
		return fmt.Errorf("SampleRatio must be within [0, 1], got=%v", c.SampleRatio)
	}
	// A malformed wildcard and a header written on both sides are both configuration deciding who an internal identifier is sent to, so they are caught while the config is read.
	if c.forwardEnabled() {
		if _, err := newHeaderPropagator(c.ForwardHeaders, c.ForwardHeaderRules); err != nil {
			return err
		}
	}
	return nil
}

// forwardEnabled reports whether any header forwarding is configured.
func (c Config) forwardEnabled() bool {
	return len(c.ForwardHeaders) > 0 || len(c.ForwardHeaderRules) > 0
}
