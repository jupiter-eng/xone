// Package xmetric sets up a Prometheus Registry and a /metrics handler from the configuration, together with a set of
// shortcuts that save the boilerplate of recording metrics.
//
// For full control, Registry() returns the native *prometheus.Registry, so a NewCounterVec plus MustRegister works as
// usual; this package does not stand in the way.
package xmetric

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// ConfigKey is this module's top-level key in the config file.
const ConfigKey = "XMetric"

// defaultHTTPDurationBuckets are the default bucket boundaries for HTTP request duration (in seconds).
//
// They have the same shape as prometheus.DefBuckets, with a 1ms bucket added at the front to make very fast endpoints
// visible. Seconds rather than milliseconds: Prometheus convention records in base units, and the exporters, the
// community dashboards and the alerting templates all work in seconds; mixing units would export two scales for one service.
var defaultHTTPDurationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Config is the metrics configuration.
type Config struct {
	// Namespace is the metric name prefix. There is none by default.
	//
	// It may contain only letters, digits and underscores and must not start with a digit, or startup fails.
	// client_golang v1.24 validates names by UTF-8 rules, so a non-conforming prefix is not reported as an error but
	// quietly escaped on export: measured, my-app was exported as my_app_, 1app became _app_, and non-ASCII text became
	// a string of underscores -- so dashboards and alerts searching for the name written in the configuration find nothing.
	Namespace string `yaml:"Namespace"`

	// ConstLabels are constant labels attached to every metric, including the Go runtime and process metrics. There are none by default.
	//
	// The typical use is telling environments and clusters apart: env: "${ENV:dev}", cluster: "${CLUSTER:local}".
	// Label names follow the same rules as Namespace and additionally must not start with __ (reserved by Prometheus),
	// nor be le, quantile, version (carried by go_info) or a variable label name of the framework's own metrics
	// (level, caller, method, status, route, host, name), or startup fails: a constant label is attached to every
	// metric, and the metric whose own label name it collides with cannot be built at all.
	ConstLabels map[string]string `yaml:"ConstLabels"`

	// HTTPDurationBuckets are the bucket boundaries of the inbound and outbound HTTP duration Histograms (in seconds).
	// They default to [0.001 0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5 5 10].
	//
	// They must be strictly increasing and all finite. Writing an empty list [] fails startup: a list field is replaced
	// whole, and [] does not mean "use the defaults" but is quietly replaced by Prometheus with its own DefBuckets
	// (which lacks the 1ms bucket). For the defaults, leave the field out.
	HTTPDurationBuckets []float64 `yaml:"HTTPDurationBuckets"`

	// HistogramBuckets are the bucket boundaries of the business Histograms the shortcuts build (in seconds).
	// They default to prometheus.DefBuckets. The rules are the same as HTTPDurationBuckets'.
	//
	// Without catching it here, [1, 0.5, 2] would pass startup and then panic on the first HistogramObserve
	// (histogram buckets must be in increasing order) -- inside a business request, not at startup.
	HistogramBuckets []float64 `yaml:"HistogramBuckets"`

	// GoMetrics says whether to collect the Go runtime metrics (goroutine count, GC and so on). It defaults to on.
	GoMetrics bool `yaml:"GoMetrics"`

	// ProcessMetrics says whether to collect the process metrics (CPU, memory, file descriptors and so on). It defaults to on.
	ProcessMetrics bool `yaml:"ProcessMetrics"`

	// LogErrorMetric says whether logs at Error level and above are counted into log_errors_total. It defaults to on.
	//
	// It makes the "error rate", the most common alert, available without waiting for the business to instrument it.
	// What is counted is the logs going through xlog: an application not using xlog gets no error from this, only no count.
	LogErrorMetric bool `yaml:"LogErrorMetric"`
}

// nameRE is the valid form of a metric name prefix and a label name: the classic Prometheus rules, without a colon
// (colons are reserved for recording rules), so the name appears as it is in every export format.
var nameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// reservedLabels are the names that cannot be used as constant labels, with the value saying who holds each one.
//
// A constant label is attached to every metric, and colliding with some metric's own variable label makes that metric
// impossible to build. Measured (client_golang v1.24.1), what happens without catching it:
//   - le: New passes as usual, and the first HistogramObserve panics inside a business request
//   - quantile: NewSummary panics on the spot
//   - the rest are variable labels of the framework's own metrics: registration fails, one error log is written,
//     and that whole group of metrics is never exported again while the service runs on
//
// Those metrics live in their own modules, which all depend on this one and cannot be referenced the other way round,
// so they are listed here in one place: adding a metric to the framework or changing a variable label updates this list.
var reservedLabels = map[string]string{
	"le":       "histogram buckets",
	"quantile": "summary quantiles",
	"level":    "log_errors_total",
	"caller":   "log_errors_total",
	"method":   "the xgin / xecho / xbeego and xhttp request metrics",
	"status":   "the xgin / xecho / xbeego and xhttp request metrics",
	"route":    "the xgin / xecho / xbeego request metrics",
	"host":     "the xhttp request metrics",
	"name":     "the xgorm, xredis and xcache instance metrics",
	// go_info carries the constant label version, and constant labels are attached to the Go runtime metrics too;
	// measured, a collision makes New report attempted wrapping with already existing label name "version"
	"version": "go_info in the Go runtime metrics",
}

// Validate checks what does not make sense in the configuration itself and fails before any metric is built.
//
// xconfig.Unmarshal calls it after decoding the XMetric block of the config file; a direct call to New calls it once as well.
// It returns a plain error, which the layer calling it wraps in an xerror once.
func (c Config) Validate() error {
	if c.Namespace != "" && !nameRE.MatchString(c.Namespace) {
		return fmt.Errorf("Namespace %q is not a valid metric name prefix: use letters, digits and underscores, not starting with a digit", c.Namespace)
	}
	for k := range c.ConstLabels {
		if !nameRE.MatchString(k) || strings.HasPrefix(k, "__") {
			return fmt.Errorf("ConstLabels key %q is not a valid label name: use letters, digits and underscores, not starting with a digit or __", k)
		}
		if owner, ok := reservedLabels[k]; ok {
			return fmt.Errorf("ConstLabels key %q is reserved as a label of %s, choose another name", k, owner)
		}
	}
	if err := validBuckets(c.HTTPDurationBuckets); err != nil {
		return fmt.Errorf("HTTPDurationBuckets: %w", err)
	}
	if err := validBuckets(c.HistogramBuckets); err != nil {
		return fmt.Errorf("HistogramBuckets: %w", err)
	}
	return nil
}

// validBuckets treats nil as "use the defaults"; anything written must be non-empty, finite and strictly increasing.
func validBuckets(b []float64) error {
	if b == nil {
		return nil
	}
	if len(b) == 0 {
		return errors.New("must not be an empty list, leave the field out to use the defaults")
	}
	for i, v := range b {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("bucket %v is not a finite number", v)
		}
		if i > 0 && v <= b[i-1] {
			return fmt.Errorf("buckets must be strictly increasing, got %v after %v", v, b[i-1])
		}
	}
	return nil
}

// withDefaults fills in the defaults for buckets that were not given (nil).
//
// Calling New with a bare Config{} leaves both bucket sets nil, and handing that to Prometheus would give its own
// DefBuckets rather than the defaults here -- one "not configured" with two different outcomes.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	// A copy is taken: the caller changing their map later must not change an already assembled setup.
	c.ConstLabels = maps.Clone(c.ConstLabels)
	if c.HTTPDurationBuckets == nil {
		c.HTTPDurationBuckets = d.HTTPDurationBuckets
	}
	if c.HistogramBuckets == nil {
		c.HistogramBuckets = d.HistogramBuckets
	}
	return c
}

// DefaultConfig holds every default in one place.
func DefaultConfig() Config {
	return Config{
		HTTPDurationBuckets: slices.Clone(defaultHTTPDurationBuckets),
		HistogramBuckets:    slices.Clone(prometheus.DefBuckets),
		GoMetrics:           true,
		ProcessMetrics:      true,
		LogErrorMetric:      true,
	}
}
