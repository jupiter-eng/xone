# xtrace

Tracing: sets up OpenTelemetry's global TracerProvider and Propagator, with business code using the native `otel.Tracer("...")`.

- xgin / xecho / xgorm / xredis / xhttp carry it, so using any one of them needs no separate import
- With an upstream, the upstream's sampling decision is followed; without one, `SampleRatio` decides
- Custom headers (such as `X-Request-Id`) are forwarded from the configuration, optionally only to named domains
- Forwarded headers and `baggage` are accepted only from a trusted peer
- No exporter is bundled: to report, call `xtrace.AddSpanProcessor(...)` and the framework Shuts it down on exit

## Quick start

```yaml
# conf/application.yml
XApp:
  Name: shop.order.api     # this is service.name
XTrace:
  SampleRatio: 0.1         # root Spans only; with an upstream the upstream decides
  ForwardHeaders: [X-Request-Id]
```

```go
func main() {
	// The framework bundles no reporting: attach an exporter yourself, before xone.Run
	exp, err := otlptracegrpc.New(context.Background(),
		otlptracegrpc.WithEndpoint(os.Getenv("OTLP_ENDPOINT")), otlptracegrpc.WithInsecure())
	if err != nil {
		log.Fatal(err)
	}
	xtrace.AddSpanProcessor(sdktrace.NewBatchSpanProcessor(exp)) // Shut down by the framework on exit

	xone.MustRun(xgin.New().WithRoutes(routes))
}

func riskCheck(ctx context.Context, orderID string) {
	// The inbound Span is already open thanks to xgin; to break out a further step, use the native OpenTelemetry API
	ctx, span := otel.Tracer("order").Start(ctx, "risk_check")
	defer span.End()
	span.SetAttributes(attribute.String("order.id", orderID))
	_, _ = xhttp.R(ctx).Get("http://risk.internal/api/v1/check") // traceparent is carried downstream automatically
}
```

## Configuration

```yaml
XTrace:
  Enable: true             # on by default. With it off there are no Spans and traceparent / baggage are no longer forwarded, while ForwardHeaders keeps working
  Console: false           # print Spans to standard output, for local debugging; off by default
  SampleRatio: 1           # the sampling ratio of root Spans, [0, 1], 1 by default; with an upstream its sampled bit always decides
  ShutdownTimeout: 5s      # the upper bound on waiting for the export to finish on exit, 5s by default, must be > 0, and never exceeds the framework's stop budget
  ForwardHeaders:          # headers forwarded to every downstream, none by default
    - X-Request-Id
  ForwardHeaderRules:      # headers sent only to matching domains, none by default
    - Domains: ["api.internal.com", "*.trusted.com"]
      Headers: ["X-Internal-Token"]
```

- `Domains` accepts only the two spellings `api.internal.com` and `*.trusted.com`; `*.trusted.com` matches a subdomain
  at any depth and **does not match the bare domain**. Every other spelling with a `*`, and the same header appearing in
  both `ForwardHeaders` and `ForwardHeaderRules`, fails while the configuration is read.
- `XGin.Trace` / `XHttp.Trace` only decide whether Spans are opened; forwarding keeps working with them off.
- Processors registered with `AddSpanProcessor` receive no Span while `Enable: false`, and are Shut down on exit all the same.

## API

| Function | Description |
|---|---|
| `AddSpanProcessor(sp sdktrace.SpanProcessor)` | Attaches a processor for reporting, called before `xone.Run`; one called after initialization is attached immediately. Passing nil panics outright |
| `ForwardHeaderFromContext(ctx, key) string` | Takes one forwarded header value, case-insensitively |
| `ForwardHeadersFromContext(ctx) map[string]string` | Takes every forwarded header (a copy) |
| `Transport{Next}` | An `http.RoundTripper` writing the target Host into the ctx, which is what lets `ForwardHeaderRules` match by domain. It injects no header itself, so `Next` has to be the layer calling the global Propagator (such as `otelhttp.NewTransport(...)`). xhttp already wraps it; this is for an `http.Client` of your own |
| `WithTargetHost(ctx, host)` / `TargetHostFromContext(ctx)` | Writes / reads the target Host yourself outside HTTP, with the same effect as `Transport` |
| `New(ctx, cfg, procs...) (*Tracing, io.Closer, error)` | The pure constructor: touches no globals, and `(*Tracing).Install()` makes it process-wide |

## Notes

- **The framework bundles no exporter**: without `AddSpanProcessor`, Spans are still generated and `trace_id` still
  reaches the logs, they are simply not reported. For local debugging, set `Console: true`.
- **Forwarded headers and `baggage` are accepted only from a trusted peer**: they are taken only when the directly
  connected peer is in `XGin.TrustedProxies` / `XEcho.TrustedProxies`, which by default trusts only the private ranges
  (load balancers, a K8s Ingress and Pods). `traceparent` / `b3` are accepted from anybody. See
  [observability.md "Propagation and the trust boundary"](../docs/observability.md#propagation-and-the-trust-boundary).
- **With an upstream its sampled bit always decides**, `SampleRatio: 1` included (`ParentBased`, see
  ["Behavior (measured)"](#behavior-measured)). `SampleRatio: 0` means nothing is sampled while TraceIDs are still
  generated and forwarded; to have no Spans at all, use `Enable: false`.
- **`service.name`** comes from `XApp.Name`, with the environment variables `OTEL_RESOURCE_ATTRIBUTES` /
  `OTEL_SERVICE_NAME` overriding it. See ["Behavior (measured)"](#behavior-measured).

## Observability

### Logs

| Message | Level | Fields |
|---|---|---|
| `xtrace ignored forward headers from an untrusted peer, ...` | WARN | `header`; once per process |
| `xtrace ignored baggage from an untrusted peer, ...` | WARN | once per process |
| `xtrace is disabled, the registered SpanProcessors will receive no spans` | WARN | `count`, `switch`; when processors are registered while `Enable: false` |
| `xtrace some resource attributes could not be detected, continuing without them` | WARN | `error`; startup continues |

The global logging conventions (`trace_id` injection, `xlog.AddKV`, the framework's start and stop logs) are in
[`docs/observability.md`](../docs/observability.md#logs).

## Behavior (measured)

The measurement environment and the cross-module summary table are in [`docs/behavior.md`](../docs/behavior.md).

OTel SDK v1.46.0, otelhttp v0.71.0.

**Sampling**: the SDK's `AlwaysSample` ignores an upstream's `sampled=00` and propagates `-01` downstream.
`ParentBased(TraceIDRatioBased(SampleRatio))` is always used here: only root Spans are sampled by ratio, and with an
upstream its sampled bit decides.

**`service.name`'s precedence**, each overriding the one before: OTel's own fallback name
`unknown_service:<executable name>` → `XApp.Name` / `XApp.Version` → `OTEL_RESOURCE_ATTRIBUTES` → `OTEL_SERVICE_NAME`.
Whichever is not configured is not written, so no empty `service.name=""` is written to cover the fallback name.

**A resource collection error is one warning and startup continues with whatever was collected**: one wrong entry in
`OTEL_RESOURCE_ATTRIBUTES`, or a container running under a random UID where the current user cannot be found, does not
keep the service from starting.

**Forwarding accepts values only from a trusted peer.** OTel's own `propagation.Baggage` accepts them from anybody: an
internet client sending an `X-Tenant-Id`, or rewriting it as `baggage: tenant=...`, would have it treated as coming from
our own side and carried into every call inside the network. The rules are in
[`observability.md`](../docs/observability.md#propagation-and-the-trust-boundary).

**A wildcard such as `*trusted.com`**: matched as a literal suffix it would match `eviltrusted.com`, giving an internal
token to a domain anybody can register. So `Domains` accepts only the two spellings `api.internal.com` and
`*.trusted.com`.
