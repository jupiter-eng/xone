package middleware

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xecho/internal/peer"
)

const (
	tracerName = "github.com/jupiter-eng/xone/xecho"

	// TraceIDHeader is the trace identifier echoed back in the response, so that one call leads
	// straight to its trace.
	TraceIDHeader = "X-Trace-Id"
)

// Trace opens a server span for every request and joins the trace coming from upstream.
//
// Span names and attributes match xgin's (the error attribute is echo.errors, corresponding to
// xgin's gin.errors).
//
// Forwarded headers (XTrace.ForwardHeaders) and baggage are accepted only from a trusted peer,
// and xecho decides that from the directly connected peer against XEcho.TrustedProxies. Used on
// its own, without xecho's assembly, nobody makes that decision and every peer counts as
// untrusted. Trace identifiers (traceparent and friends) are unaffected.
func Trace() echo.MiddlewareFunc {
	var rs routes
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			r := c.Request()

			// Take the current global Propagator every time: tracing may not be initialized yet
			// when the middleware is constructed.
			ctx := otel.GetTextMapPropagator().Extract(contextOf(c), inbound(c))

			// The method is folded into a fixed set, as it is for metrics: it is a free-form
			// token, and copying it into the span name lets anyone blow up the span names in the
			// tracing backend with CUSTOM1, CUSTOM2. The original goes into method_original (the
			// spelling from OTel's semantic conventions) and stays visible while debugging.
			method := web.NormalizeMethod(r.Method)
			attrs := []attribute.KeyValue{
				attribute.String("http.request.method", method),
				attribute.String("url.path", r.URL.Path),
			}
			if method != r.Method {
				attrs = append(attrs, attribute.String("http.request.method_original", r.Method))
			}

			// The span name starts as the method alone and gains the route template on the way
			// out: xecho mounts this layer on e.Pre, where the router has not run yet and
			// c.Path() is empty.
			ctx, span := otel.Tracer(tracerName).Start(ctx, method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(attrs...),
			)
			// The route and the status code are recorded in a defer: a request aborted with
			// http.ErrAbortHandler passes through this layer carrying a panic, and code written
			// after next(c) would never be reached.
			defer func() {
				route, st := rs.of(c), status(c)
				span.SetName(method + " " + route)
				span.SetAttributes(attribute.String("http.route", route), attribute.Int("http.response.status_code", st))
				// Only 5xx and aborts count as server errors. A 4xx means the client sent
				// something wrong, and marking it as an error floods the trace with "errors"
				// until real failures no longer stand out; the 499 of a client that left is the
				// same story.
				switch {
				case aborted(c):
					span.SetStatus(codes.Error, "handler aborted")
				case st >= 500:
					span.SetStatus(codes.Error, http.StatusText(st))
				}
				if err := errorOf(c); err != nil {
					span.SetAttributes(attribute.String("echo.errors", web.RedactText(err.Error())))
				}
				span.End()
			}()

			withContext(c, ctx)

			// This has to be written before next(c): once the response starts going out, headers
			// can no longer be changed. Error responses carry it too, since e.HTTPErrorHandler
			// does not clear the response headers when writing one.
			if sc := span.SpanContext(); sc.IsValid() {
				c.Response().Header().Set(TraceIDHeader, sc.TraceID().String())
			}

			finish(c, ctx, next(c))
			return nil
		}
	}
}

// Propagate only joins the trace identifiers, baggage and forwarded headers coming from upstream,
// without opening a span.
//
// With XEcho.Trace off, xecho installs this instead of Trace: Trace governs the span only, and an
// upstream traceparent or X-Request-Id still has to reach downstream and the logs — it should not
// break just because this hop records no span. The trusted-peer rules match Trace's.
func Propagate() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			withContext(c, otel.GetTextMapPropagator().Extract(contextOf(c), inbound(c)))
			return next(c)
		}
	}
}

// inbound takes the inbound request headers as the carrier, marked with TrustedPeer when the peer
// is trusted.
//
// xtrace relies on TrustedPeer() on the carrier to know whether the peer sending forwarded
// headers is one of us — it does not know xecho, and only the layer accepting the request knows
// who the peer is. The contract is documented on xtrace.HeaderPropagator.Extract, with tests
// pinning it on both sides.
func inbound(c echo.Context) propagation.TextMapCarrier {
	h := propagation.HeaderCarrier(c.Request().Header)
	if trusted, _ := c.Get(peer.TrustedKey).(bool); trusted {
		return trustedCarrier{h}
	}
	return h
}

// trustedCarrier is the request headers of a trusted peer.
//
// It holds a single map field, so boxing it into an interface allocates nothing — the tracing
// middleware goes through here on every request.
type trustedCarrier struct{ propagation.HeaderCarrier }

// TrustedPeer, see inbound.
func (trustedCarrier) TrustedPeer() bool { return true }
