package xtrace

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/jupiter-eng/xone/internal/logext"
	"github.com/jupiter-eng/xone/xapp"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
)

// Tracing is the tracing setup assembled from one configuration.
//
// New only constructs it and does not install it; putting it into OpenTelemetry's globals is Install's job.
// They are separate so a test can get a tracing setup of its own without touching global state.
type Tracing struct {
	// TracerProvider is a noop implementation when tracing is off, never nil.
	TracerProvider oteltrace.TracerProvider

	// Propagator carries the trace identifiers and the forwarded headers across processes.
	Propagator propagation.TextMapPropagator
}

// Install makes this setup the process-wide one.
//
// After that, business code opens Spans with the native otel.Tracer("...") and does not have to know this package.
func (t *Tracing) Install() {
	otel.SetTracerProvider(t.TracerProvider)
	otel.SetTextMapPropagator(t.Propagator)
	// Give the logs a TraceID. xlog does not depend on OpenTelemetry, so this package injects that ability;
	// the injection point is internal/logext, and this package does not import xlog, so a program using only xgorm does not get xlog pulled in.
	logext.SetTraceExtractor(traceIDsFromContext)
}

// New builds the tracing setup from the configuration and touches no globals.
//
// procs are the SpanProcessors to attach. The framework bundles no reporting exporter --
// OTLP alone pulls in over a hundred build dependencies, which not every caller should carry.
//
// The io.Closer returned is never nil; on close it flushes the Spans out and Shuts down every proc, with the wait
// bounded by cfg.ShutdownTimeout. It has to be closed even with tracing off: the procs receive no Span, but the
// connections and goroutines they hold still need cleaning up.
func New(ctx context.Context, cfg Config, procs ...sdktrace.SpanProcessor) (*Tracing, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, xerror.Newf("xtrace", "config", "invalid config: %w", err)
	}

	prop, err := newPropagator(cfg)
	if err != nil {
		return nil, nil, err
	}

	if !cfg.Enable {
		if len(procs) > 0 {
			slog.Warn("xtrace is disabled, the registered SpanProcessors will receive no spans",
				"count", len(procs), "switch", ConfigKey+".Enable")
		}
		// The processors are attached to a provider that is not published: the published one is a noop and never sees a
		// Span, but they are still Shut down on close. AddSpanProcessor promised that this package closes them --
		// what used to be handed back here was a no-op Closer, leaving the exporters' connections and goroutines uncleaned on exit.
		idle := sdktrace.NewTracerProvider(processors(procs)...)
		return &Tracing{TracerProvider: noop.NewTracerProvider(), Propagator: prop},
			&providerCloser{tp: idle, timeout: cfg.ShutdownTimeout}, nil
	}

	res := newResource(ctx)

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(samplerOf(cfg.SampleRatio)),
		sdktrace.WithResource(res),
	}
	if cfg.Console {
		exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, nil, xerror.Newf("xtrace", "new", "create stdout exporter: %w", err)
		}
		// Simple rather than Batch: local debugging wants to see it immediately, not after a batch has filled up.
		opts = append(opts, sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	}
	tp := sdktrace.NewTracerProvider(append(opts, processors(procs)...)...)
	return &Tracing{TracerProvider: tp, Propagator: prop}, &providerCloser{tp: tp, timeout: cfg.ShutdownTimeout}, nil
}

// processors turns each SpanProcessor into a provider option.
func processors(procs []sdktrace.SpanProcessor) []sdktrace.TracerProviderOption {
	opts := make([]sdktrace.TracerProviderOption, 0, len(procs))
	for _, sp := range procs {
		opts = append(opts, sdktrace.WithSpanProcessor(sp))
	}
	return opts
}

// newPropagator assembles the Propagator.
//
// baggage uses trustedBaggage, on the same trust boundary as the forwarded headers;
// traceparent and b3 are only trace identifiers and are accepted from anybody.
//
// Header forwarding is kept even with tracing off: whether X-Request-Id should be carried to the downstream is a
// different question from whether to sample Spans, and turning one off must not silently disable the other.
func newPropagator(cfg Config) (propagation.TextMapPropagator, error) {
	var list []propagation.TextMapPropagator
	if cfg.Enable {
		list = append(list, propagation.TraceContext{}, &trustedBaggage{}, b3.New())
	}
	if cfg.forwardEnabled() {
		hp, err := NewHeaderPropagator(cfg.ForwardHeaders, cfg.ForwardHeaderRules)
		if err != nil {
			return nil, err
		}
		list = append(list, hp)
	}
	return propagation.NewCompositeTextMapPropagator(list...), nil
}

// newResource collects process and host attributes. It uses the caller's ctx: host detection reads files and queries
// network interfaces, and when that is slow a process already exiting should not be waiting here.
//
// A collection error is only a warning and whatever was collected is used, rather than failing startup. resource.New
// hands back the merged result of the remaining detectors (service.name included, see the SDK v1.46 resource/auto.go)
// whenever one detector fails, and the failing one is simply skipped:
//
//   - One wrong character in OTEL_RESOURCE_ATTRIBUTES: ErrPartialResource, and the correctly written entries still
//     take effect (a test pins this down)
//   - Running in a container under a random UID where user.Current finds nothing: the process.owner detector returns
//     an ordinary error rather than ErrPartialResource -- so this does not select by error type and treats every one
//     of them as a warning
//
// These are all descriptive attributes, and keeping the whole service from starting for them is not a fair trade.
//
// service.name's precedence, from lowest to highest: XApp.Name / XApp.Version, OTEL_RESOURCE_ATTRIBUTES,
// OTEL_SERVICE_NAME. resource.New merges in option order with a later name overriding an earlier one (measured), so
// the order is the precedence: an environment variable is the deployer's last word and should override a config file
// baked into the image.
//
// An identity nobody configured produces no attribute at all. The SDK's own resource.WithService is deliberately not
// used: half of it invents a service.name (unknown_service:<executable name>, resource/builtin.go), which looks like
// an answer on a dashboard without being one, and the same reasoning that keeps xapp from guessing a name keeps that
// guess out of the spans. The other half of WithService is worth keeping, so service.instance.id is written here --
// first in the list, which leaves the environment free to override it.
//
// Note that service.name is a required attribute in OTel's semantic conventions, so a backend receiving spans without
// one falls back to whatever it uses for unnamed services. That is the intended trade: "no name" is visible as a gap
// to fix, while unknown_service:<executable name> quietly looks like a real service.
func newResource(ctx context.Context) *resource.Resource {
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceInstanceID(instanceID())),
		resource.WithAttributes(appAttributes()...),
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil {
		slog.WarnContext(ctx, "xtrace some resource attributes could not be detected, continuing without them", "error", err)
	}
	return res
}

// instanceID is this process's service.instance.id, a random UUIDv4 as OTel's own detector produces (its
// defaultServiceInstanceIDDetector, which comes with the service.name guess this package does not want).
//
// It tells one replica from another where host.name and process.pid are not enough -- two Pods of a Deployment on
// one node, or the runs before and after a restart. An error from the random source leaves it out rather than
// failing startup, as with every other attribute here.
func instanceID() string {
	id, err := uuid.NewRandom()
	if err != nil {
		return ""
	}
	return id.String()
}

// appAttributes are the service name and version configured in the XApp block, writing nothing for whichever is unset.
func appAttributes() []attribute.KeyValue {
	var attrs []attribute.KeyValue
	if name := xapp.Name(); name != "" {
		attrs = append(attrs, semconv.ServiceName(name))
	}
	if version := xapp.Version(); version != "" {
		attrs = append(attrs, semconv.ServiceVersion(version))
	}
	return attrs
}

// samplerOf builds a Sampler from the sampling ratio.
//
// ParentBased always wraps it: with an upstream, the upstream decides, and only a root Span is sampled by ratio.
// Full sampling used to be a bare AlwaysSample that ignored the parent Span -- an upstream sending sampled=00 was
// sampled here anyway and propagated downstream as -01 (measured), overturning the upstream's decision.
// TraceIDRatioBased(1) is full sampling in itself, so 1 needs no branch of its own.
func samplerOf(ratio float64) sdktrace.Sampler {
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
}

// traceIDsFromContext takes the trace identifiers out of the ctx's Span, for xlog to inject into the logs.
func traceIDsFromContext(ctx context.Context) (traceID, spanID string) {
	sc := oteltrace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}

// ---- shutdown ----

// providerCloser closes the TracerProvider and with it every SpanProcessor attached to it.
type providerCloser struct {
	tp      *sdktrace.TracerProvider
	timeout time.Duration
}

// Close is the shutdown for standalone use, bounded by ShutdownTimeout.
func (c *providerCloser) Close() error { return c.shutdown(context.Background()) }

// shutdown tightens the caller's ctx by one more layer rather than replacing it.
//
// Same reasoning as xgin's Stop: WithTimeout takes the earlier of the two deadlines, so it neither uses up the whole
// budget the framework gave nor exceeds it. This used to be Background + ShutdownTimeout, which waited the full 5s
// even when the framework had 100ms left (measured).
func (c *providerCloser) shutdown(parent context.Context) error {
	// There has to be a limit: with the export endpoint unreachable, Shutdown blocks forever, and without a deadline that is a hang on exit.
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()

	detach(c)
	if err := c.tp.Shutdown(ctx); err != nil {
		return xerror.Newf("xtrace", "close", "shutdown: %w", err)
	}
	return nil
}

// ---- Transport ----

// Transport writes the target request's Host into the context so the per-domain forwarding rules can take effect.
//
// Without it, not one header from ForwardHeaderRules is ever injected -- Inject does not know who this request is going to.
//
// It does not inject anything itself; injection is the layer below it: otelhttp calls the global Propagator to inject
// whether or not the TracerProvider is a noop -- so with XTrace.Enable off it injects the forwarded headers and not
// traceparent, which is exactly the wanted behavior. That premise is pinned down by xhttp's tests (which depend on
// otelhttp anyway, so putting it there adds no dependency). For a service that wants no outbound Spans
// (XHttp.Trace: false), xhttp swaps in a layer below this one that only injects and opens no Span, and forwarding keeps working.
type Transport struct {
	// Next is the RoundTripper that actually performs the request; http.DefaultTransport is used when it is nil.
	Next http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
// By convention it does not modify the request passed in: WithContext returns a shallow copy.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.next().RoundTrip(req.WithContext(WithTargetHost(req.Context(), req.URL.Host)))
}

// CloseIdleConnections passes the request to close idle connections on to the underlying transport.
//
// This method has to exist. http.Client.CloseIdleConnections() finds it by type assertion, so wrapping without
// forwarding makes the assertion fail and turns the whole call into a no-op -- and since tracing is on by default,
// by default those idle connections were simply never cleaned up on exit.
func (t *Transport) CloseIdleConnections() {
	if c, ok := t.next().(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// next is the RoundTripper that actually performs the request.
func (t *Transport) next() http.RoundTripper {
	if t.Next == nil {
		return http.DefaultTransport
	}
	return t.Next
}

// ---- registration ----

// The registered SpanProcessors, and the tracing setup installed globally and not yet closed.
// live is nil when nothing is installed yet or it has already been closed. It exists with tracing off too (the
// processors hang on a provider that is not published, receive no Span, and are still Shut down on close).
//
// One lock covers two things: registration and initialization have to exclude each other, or a concurrent
// registration could land in the window between "pending has been taken" and "the provider is installed", and that processor would be swallowed.
var (
	mu      sync.Mutex
	pending []sdktrace.SpanProcessor
	live    *providerCloser
)

// AddSpanProcessor attaches a SpanProcessor, used to report Spans to a remote endpoint.
//
// The framework bundles no exporter: OTLP alone pulls in over a hundred build dependencies, and a service that does
// not report should not pay for it. A service that does imports it and registers it here:
//
//	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(ep), otlptracegrpc.WithInsecure())
//	if err != nil { return err }
//	xtrace.AddSpanProcessor(sdktrace.NewBatchSpanProcessor(exp))
//
// Calling it before xone.Run is enough. One registered after initialization is attached immediately.
// This package Shuts them all down on close, with the wait bounded by XTrace.ShutdownTimeout.
func AddSpanProcessor(sp sdktrace.SpanProcessor) {
	if sp == nil {
		panic("xtrace: SpanProcessor must not be nil")
	}
	mu.Lock()
	defer mu.Unlock()

	if live != nil {
		live.tp.RegisterSpanProcessor(sp)
		return
	}
	pending = append(pending, sp)
}

// detach clears the provider after closing, so a later registration does not attach to a closed instance.
//
// It clears only its own: the instance New built is not necessarily the one installed globally -- when a test wants a
// clean tracing setup, or two configurations exist at once, closing one of them used to wipe the global one as well.
// Every AddSpanProcessor after that would attach to a pending that nobody ever reads again,
// so Spans would still be produced and never reach the reporting endpoint, with no sign of it.
func detach(c *providerCloser) {
	mu.Lock()
	defer mu.Unlock()
	if live == c {
		live = nil
	}
}

// initXTrace reads the configuration, assembles the tracing setup and installs the provider.
func initXTrace(ctx context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return err
	}
	return install(ctx, c)
}

func closeXTrace(ctx context.Context) error {
	mu.Lock()
	c := live
	mu.Unlock()
	if c == nil {
		return nil
	}
	return c.shutdown(ctx)
}

// install takes the pending SpanProcessors, assembles the tracing setup and installs the provider.
//
// The lock is held throughout. Releasing it between taking the pending list and installing the provider would leave
// an AddSpanProcessor landing in that window with neither side: it would see live still nil and append to a pending
// list that has been taken and will never be read again, and would then be dropped silently --
// Spans would still be produced and simply never reach the reporting endpoint, with no sign of it.
// The cost is only that a concurrent registrant waits for initialization to finish, which is what it should wait for anyway.
func install(ctx context.Context, c Config) error {
	mu.Lock()
	defer mu.Unlock()

	procs := pending
	pending = nil

	t, closer, err := New(ctx, c, procs...)
	if err != nil {
		// It was not installed, so the pending list is handed back: the caller most likely fails startup, but should it
		// carry on anyway, these processors must not vanish into thin air.
		pending = procs
		return err
	}
	t.Install()
	live = closer.(*providerCloser) // whether tracing is on or off, this is what New hands back
	return nil
}

// init puts tracing before the clients are ready, or the Spans they emit have nothing to attach to.
func init() {
	xhook.BeforeStart(initXTrace, xhook.At(xhook.StageTelemetry))
	xhook.BeforeStop(closeXTrace) // the stage follows the start hook above
}
