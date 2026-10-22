package middleware

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xbeego/internal/reqstate"
)

const (
	tracerName = "github.com/jupiter-eng/xone/xbeego"

	// TraceIDHeader is the trace identifier echoed back in the response, so that one call leads
	// straight to its trace.
	TraceIDHeader = "X-Trace-Id"
)

// Trace opens a server span for every request and joins the trace coming from upstream.
//
// Span names and attributes match xgin's and xecho's (the error attribute is beego.errors,
// corresponding to xgin's gin.errors).
//
// Forwarded headers (XTrace.ForwardHeaders) and baggage are accepted only from a trusted peer, and
// xbeego decides that from the directly connected peer against XBeego.TrustedProxies. Used on its
// own, without Capture in front of it, nobody makes that decision and every peer counts as
// untrusted. Trace identifiers (traceparent and friends) are unaffected.
func Trace() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w, r, st := enter(w, r)

			// Take the current global Propagator every time: tracing may not be initialized yet
			// when the middleware is constructed.
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), inbound(r, st))

			// The method is folded into a fixed set, as it is for metrics: it is a free-form token,
			// and copying it into the span name lets anyone blow up the span names in the tracing
			// backend with CUSTOM1, CUSTOM2. The original goes into method_original (the spelling
			// from OTel's semantic conventions) and stays visible while debugging.
			method := web.NormalizeMethod(r.Method)
			attrs := []attribute.KeyValue{
				attribute.String("http.request.method", method),
				attribute.String("url.path", r.URL.Path),
			}
			if method != r.Method {
				attrs = append(attrs, attribute.String("http.request.method_original", r.Method))
			}

			// The span name starts as the method alone and gains the route template on the way out:
			// this layer sits outside beego's router, and the template only exists once beego has
			// matched -- the same shape xecho's Pre chain has.
			ctx, span := otel.Tracer(tracerName).Start(ctx, method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(attrs...),
			)
			inCtx := ctx
			// The route and the status code are recorded in a defer: a request aborted with
			// http.ErrAbortHandler passes through this layer carrying a panic, and code written
			// after next.ServeHTTP would never be reached.
			defer func() {
				route, code := routeOf(st), status(st, inCtx)
				span.SetName(method + " " + route)
				span.SetAttributes(attribute.String("http.route", route), attribute.Int("http.response.status_code", code))
				// Only 5xx and aborts count as server errors. A 4xx means the client sent something
				// wrong, and marking it as an error floods the trace with "errors" until real
				// failures no longer stand out; the 499 of a client that left is the same story.
				switch {
				case st != nil && st.Aborted:
					span.SetStatus(codes.Error, "handler aborted")
				case code >= 500:
					span.SetStatus(codes.Error, http.StatusText(code))
				}
				if st != nil && len(st.Errors) > 0 {
					span.SetAttributes(attribute.String("beego.errors", web.RedactText(strings.Join(st.Errors, "\n"))))
				}
				span.End()
			}()

			// This has to be written before next.ServeHTTP: once the response starts going out,
			// headers can no longer be changed.
			if sc := span.SpanContext(); sc.IsValid() {
				w.Header().Set(TraceIDHeader, sc.TraceID().String())
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Propagate only joins the trace identifiers, baggage and forwarded headers coming from upstream,
// without opening a span.
//
// With XBeego.Trace off, xbeego installs this instead of Trace: Trace governs the span only, and an
// upstream traceparent or X-Request-Id still has to reach downstream and the logs -- it should not
// break just because this hop records no span. The trusted-peer rules match Trace's.
func Propagate() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w, r, st := enter(w, r)
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), inbound(r, st))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// inbound takes the inbound request headers as the carrier, marked with TrustedPeer when the peer
// is trusted.
//
// xtrace relies on TrustedPeer() on the carrier to know whether the peer sending forwarded headers
// is one of us -- it does not know xbeego, and only the layer accepting the request knows who the
// peer is. The contract is documented on xtrace.HeaderPropagator.Extract, with tests pinning it on
// both sides.
func inbound(r *http.Request, st *reqstate.State) propagation.TextMapCarrier {
	h := propagation.HeaderCarrier(r.Header)
	if st != nil && st.Trusted {
		return trustedCarrier{h}
	}
	return h
}

// trustedCarrier is the request headers of a trusted peer.
//
// It holds a single map field, so boxing it into an interface allocates nothing -- the tracing
// middleware goes through here on every request.
type trustedCarrier struct{ propagation.HeaderCarrier }

// TrustedPeer, see inbound.
func (trustedCarrier) TrustedPeer() bool { return true }
