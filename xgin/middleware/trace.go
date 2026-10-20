package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xgin/internal/peer"
)

const (
	tracerName = "github.com/jupiter-eng/xone/xgin"

	// TraceIDHeader is the trace identifier echoed back in the response, so that one call leads
	// straight to its trace.
	TraceIDHeader = "X-Trace-Id"
)

// Trace opens a server span for every request and joins the trace coming from upstream.
//
// Forwarded headers (XTrace.ForwardHeaders) and baggage are accepted only from a trusted peer,
// and xgin decides that from the directly connected peer against XGin.TrustedProxies. Used on its
// own, without xgin's assembly, nobody makes that decision and every peer counts as untrusted.
// Trace identifiers (traceparent and friends) are unaffected.
func Trace() gin.HandlerFunc {
	return func(c *gin.Context) {
		route := routeOf(c)

		// Take the current global Propagator every time: tracing may not be initialized yet when the
		// middleware is constructed.
		ctx := otel.GetTextMapPropagator().Extract(c.Request.Context(), inbound(c))

		// The method is folded into a fixed set, as it is for metrics: it is a free-form token, and
		// copying it into the span name lets anyone blow up the span names in the tracing backend with
		// CUSTOM1, CUSTOM2. The original goes into method_original (the spelling from OTel's semantic
		// conventions) and stays visible while debugging.
		method := web.NormalizeMethod(c.Request.Method)
		attrs := []attribute.KeyValue{
			attribute.String("http.request.method", method),
			attribute.String("http.route", route),
			attribute.String("url.path", c.Request.URL.Path),
		}
		if method != c.Request.Method {
			attrs = append(attrs, attribute.String("http.request.method_original", c.Request.Method))
		}

		ctx, span := otel.Tracer(tracerName).Start(ctx, method+" "+route,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attrs...),
		)
		// The status code is recorded in a defer: a request aborted with http.ErrAbortHandler passes
		// through this layer carrying a panic, and code written after c.Next() would never be reached.
		defer func() {
			st := status(c, ctx)
			span.SetAttributes(attribute.Int("http.response.status_code", st))
			// Only 5xx and aborts count as server errors. A 4xx means the client sent something wrong,
			// and marking it as an error floods the trace with "errors" until real failures no longer
			// stand out; the 499 of a client that left is the same story.
			switch {
			case aborted(c):
				span.SetStatus(codes.Error, "handler aborted")
			case st >= 500:
				span.SetStatus(codes.Error, http.StatusText(st))
			}
			if len(c.Errors) > 0 {
				span.SetAttributes(attribute.String("gin.errors", web.RedactText(c.Errors.String())))
			}
			span.End()
		}()

		c.Request = c.Request.WithContext(ctx)

		// This has to be written before c.Next(): once the response starts going out, headers can no
		// longer be changed.
		if sc := span.SpanContext(); sc.IsValid() {
			c.Header(TraceIDHeader, sc.TraceID().String())
		}

		c.Next()
	}
}

// Propagate only joins the trace identifiers, baggage and forwarded headers coming from upstream,
// without opening a span.
//
// With XGin.Trace off, xgin installs this instead of Trace: Trace governs the span only, and an
// upstream traceparent or X-Request-Id still has to reach downstream and the logs — it should not
// break just because this hop records no span. The trusted-peer rules match Trace's.
func Propagate() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(otel.GetTextMapPropagator().Extract(c.Request.Context(), inbound(c)))
		c.Next()
	}
}

// inbound takes the inbound request headers as the carrier, marked with TrustedPeer when the peer
// is trusted.
//
// xtrace relies on TrustedPeer() on the carrier to know whether the peer sending forwarded headers
// is one of us — it does not know xgin, and only the layer accepting the request knows who the
// peer is. The contract is documented on xtrace.HeaderPropagator.Extract, with tests pinning it on
// both sides and one end-to-end case in example.
func inbound(c *gin.Context) propagation.TextMapCarrier {
	h := propagation.HeaderCarrier(c.Request.Header)
	if c.GetBool(peer.TrustedKey) {
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
