package xhttp

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/jupiter-eng/xone/xmetric"
)

// newDurationHistogram is the outbound request duration histogram.
//
// The bucket boundaries come from xmetric's configuration, so outbound and inbound durations are read on the same scale.
func newDurationHistogram() *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   xmetric.Namespace(),
		Name:        "http_client_request_duration_seconds",
		Help:        "Outbound HTTP request duration",
		Buckets:     xmetric.HTTPDurationBuckets(),
		ConstLabels: xmetric.ConstLabels(),
	}, []string{"method", "host", "status"})
}

// startKey is the context key for the start of a whole logical request.
type startKey struct{}

// markStart records the start of the whole logical request before the first attempt; both the metric and the log compute their duration from it.
// Attaching it once each for the metric and the log is harmless: the start is already in the ctx, and the later one does nothing.
//
// resp.Time() cannot be used: resty resets Request.Time on every attempt, so resp.Time() is only the last attempt's
// duration -- the earlier failed attempts and the backoff waits between them are all left out.
// The count is kept per "one logical request", so the duration has to be as well; otherwise, during an incident,
// the request count rises while the duration does not move at all and the monitoring looks unusually healthy.
func markStart(client *resty.Client) {
	client.OnBeforeRequest(func(_ *resty.Client, req *resty.Request) error {
		if _, ok := req.Context().Value(startKey{}).(time.Time); !ok {
			req.SetContext(context.WithValue(req.Context(), startKey{}, time.Now()))
		}
		return nil
	})
}

// installMetrics attaches the hooks that record the metrics.
//
// It uses OnSuccess / OnError rather than middleware: they are called once after every retry has finished,
// while middleware would record every retry and inflate both the request count and the duration distribution.
func installMetrics(client *resty.Client, hist *prometheus.HistogramVec) {
	markStart(client)
	client.OnSuccess(func(_ *resty.Client, resp *resty.Response) {
		if resp == nil || resp.Request == nil || resp.Request.RawRequest == nil {
			return
		}
		raw := resp.Request.RawRequest
		observe(hist, normalizeMethod(raw.Method), raw.URL.Host, strconv.Itoa(resp.StatusCode()),
			elapsed(resp.Request, resp.Time()))
	})

	client.OnError(func(req *resty.Request, err error) {
		if req == nil || req.RawRequest == nil {
			return
		}
		raw := req.RawRequest
		// The status code is recorded as 0 when there is no response: mixing it in with the real status codes would
		// make an alert such as "the 5xx ratio" show nothing during a network failure.
		status, dur := "0", time.Since(req.Time)
		var re *resty.ResponseError
		if errors.As(err, &re) && re.Response != nil {
			status, dur = strconv.Itoa(re.Response.StatusCode()), re.Response.Time()
		}
		observe(hist, normalizeMethod(raw.Method), raw.URL.Host, status, elapsed(req, dur))
	})
}

// knownMethods are the methods RFC 9110 defines, plus PATCH. The same list as in xgin/middleware.
var knownMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodPost: {}, http.MethodPut: {},
	http.MethodPatch: {}, http.MethodDelete: {}, http.MethodConnect: {},
	http.MethodOptions: {}, http.MethodTrace: {},
}

// methodOther is what an unrecognized method is recorded as.
const methodOther = "OTHER"

// normalizeMethod collapses the method into a fixed set.
//
// A method is a free token, and on the outbound side it comes from business code, often passed on from an upstream
// request, a configuration or a message -- copying it into a label would add a set of time series for every new value,
// with nothing to evict them. Same reasoning as xgin/middleware's normalizeMethod; that one is an internal package of
// another module, and these few lines are better duplicated than turned into a public package.
func normalizeMethod(m string) string {
	if _, ok := knownMethods[m]; ok {
		return m
	}
	return methodOther
}

// elapsed takes the duration of the whole logical request, falling back to this attempt's when the start cannot be found.
func elapsed(req *resty.Request, fallback time.Duration) time.Duration {
	if req == nil {
		return fallback
	}
	if start, ok := req.Context().Value(startKey{}).(time.Time); ok {
		return time.Since(start)
	}
	return fallback
}

func observe(hist *prometheus.HistogramVec, method, host, status string, d time.Duration) {
	hist.WithLabelValues(method, host, status).Observe(d.Seconds())
}
