package xmetric

import (
	"strings"
	"sync"
	"time"
)

// durationSuffix is the name suffix of a duration metric.
//
// Prometheus convention has the unit in the metric name, with durations always in seconds and ending in _seconds;
// dashboards and alerting rules infer the unit from that convention.
const durationSuffix = "_seconds"

// ObserveDuration records one duration observation.
//
// Compared to using HistogramObserve directly, it settles the two things most easily got wrong on the caller's
// behalf: which metric type to use (Histogram) and which unit to pass. The argument is a time.Duration, converted
// to seconds internally -- which matches the default buckets (prometheus.DefBuckets, in seconds).
// Passing a value in milliseconds would land every sample in the +Inf bucket and make the quantiles useless.
//
// The metric name gets the _seconds suffix automatically, and one already there is not added twice.
func ObserveDuration(name string, d time.Duration, tags ...Tag) {
	HistogramObserve(durationName(name), d.Seconds(), tags...)
}

// Timer starts timing and returns a function that records the elapsed time from this moment when called.
//
//	defer xmetric.Timer("handle_order")()
//
// The tags are fixed when timing starts. To label by outcome (status=success/failed), use ObserveDuration instead:
//
//	start := time.Now()
//	defer func() { xmetric.ObserveDuration("handle_order", time.Since(start), xmetric.T("status", status)) }()
//
// Calling the returned function again has no effect, as with TrackInFlight: the two have the same signature and the
// same use, and having one idempotent and the other not would be pure memory burden. Calling it explicitly outside
// the defer as well (to "record it now" on an early return) would add one observation and skew count and rate upwards.
func Timer(name string, tags ...Tag) func() {
	start := time.Now()
	var once sync.Once
	return func() {
		once.Do(func() { ObserveDuration(name, time.Since(start), tags...) })
	}
}

// TrackInFlight records "how many are in progress right now" and returns a function that decrements it when called.
//
//	defer xmetric.TrackInFlight("active_requests", xmetric.T("api", "/order"))()
//
// GaugeInc / GaugeDec have to come in pairs, and an early return or a panic path makes a Dec very easy to miss --
// missing one leaves the count permanently high, with no error reported. Leaving it to a defer is naturally correct.
//
// Calling the returned function again has no effect, which keeps the count from being decremented through zero.
func TrackInFlight(name string, tags ...Tag) func() {
	GaugeInc(name, tags...)
	var once sync.Once
	return func() {
		once.Do(func() { GaugeDec(name, tags...) })
	}
}

// durationName adds the _seconds suffix by Prometheus convention.
func durationName(name string) string {
	if strings.HasSuffix(name, durationSuffix) {
		return name
	}
	return name + durationSuffix
}
