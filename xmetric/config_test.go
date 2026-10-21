package xmetric

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xonetest"
)

func TestNew_InconsistentConfigFailsAtStartup(t *testing.T) {
	// Without catching them here: out-of-order buckets panic on the first Observe (inside a business request);
	// a non-conforming Namespace is not reported as an error but quietly escaped on export (my-app -> my_app_),
	// so dashboards and alerts searching for the name written in the configuration find nothing.
	for name, mutate := range map[string]func(*Config){
		"buckets out of order":                        func(c *Config) { c.HistogramBuckets = []float64{1, 0.5, 2} },
		"duplicate buckets":                           func(c *Config) { c.HistogramBuckets = []float64{1, 1} },
		"a NaN bucket":                                func(c *Config) { c.HistogramBuckets = []float64{0.1, math.NaN()} },
		"an Inf bucket":                               func(c *Config) { c.HTTPDurationBuckets = []float64{0.1, math.Inf(1)} },
		"an empty list written":                       func(c *Config) { c.HTTPDurationBuckets = []float64{} },
		"HTTP buckets out of order":                   func(c *Config) { c.HTTPDurationBuckets = []float64{2, 1} },
		"Namespace with a hyphen":                     func(c *Config) { c.Namespace = "my-app" },
		"Namespace starting with a digit":             func(c *Config) { c.Namespace = "1app" },
		"Namespace with a colon":                      func(c *Config) { c.Namespace = "app:x" },
		"Namespace in non-ASCII text":                 func(c *Config) { c.Namespace = "\u00e4pp" },
		"constant label name with a hyphen":           func(c *Config) { c.ConstLabels = map[string]string{"my-env": "x"} },
		"constant label name using a reserved prefix": func(c *Config) { c.ConstLabels = map[string]string{"__name": "x"} },
		// Measured: le passes startup as usual and the first HistogramObserve panics inside a business request
		"constant label name le, reserved by histograms":      func(c *Config) { c.ConstLabels = map[string]string{"le": "x"} },
		"constant label name quantile, reserved by summaries": func(c *Config) { c.ConstLabels = map[string]string{"quantile": "x"} },
		// Colliding with a variable label of the framework's own metrics: that metric fails to register and only one error log is written
		"constant label name colliding with the log counter's":    func(c *Config) { c.ConstLabels = map[string]string{"level": "x"} },
		"constant label name colliding with the request metric's": func(c *Config) { c.ConstLabels = map[string]string{"method": "x"} },
		"constant label name colliding with the pool metric's":    func(c *Config) { c.ConstLabels = map[string]string{"name": "x"} },
		// Caught even with the Go runtime metrics turned off: a configuration that fails startup the moment a switch is flipped is better rejected from the start
		"constant label name colliding with go_info's": func(c *Config) { c.ConstLabels = map[string]string{"version": "x"} },
	} {
		c := DefaultConfig()
		c.GoMetrics, c.ProcessMetrics = false, false
		mutate(&c)
		_, closer, err := New(c)
		if err == nil {
			t.Errorf("%s must be an error", name)
			continue
		}
		var xe *xerror.Error
		if !errors.As(err, &xe) || xe.Module != "xmetric" || xe.Op != "config" {
			t.Errorf("%s: it must be an xmetric config error, got %v", name, err)
		}
		if closer != nil {
			t.Errorf("%s: no Closer may be returned on failure", name)
		}
	}
}

func TestNew_ValidConfigPasses(t *testing.T) {
	c := DefaultConfig()
	c.GoMetrics, c.ProcessMetrics = false, false
	c.Namespace = "my_app_2"
	c.ConstLabels = map[string]string{"env": "prod", "_zone": "a"}
	c.HistogramBuckets = []float64{-1, 0, 0.5, 10}
	if _, _, err := New(c); err != nil {
		t.Fatalf("a valid configuration must not be an error: %v", err)
	}
}

func TestNew_DefaultBucketsWhenNoneGiven(t *testing.T) {
	// Both bucket sets of a bare Config{} are nil, and handing that straight to Prometheus would give its own
	// DefBuckets rather than the defaults here -- one "not configured" with two different outcomes.
	m, closer, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if closer == nil {
		t.Error("the Closer must not be nil on success")
	}
	if !slices.Equal(m.cfg.HTTPDurationBuckets, defaultHTTPDurationBuckets) {
		t.Errorf("the HTTP buckets must use the defaults, got %v", m.cfg.HTTPDurationBuckets)
	}
	if len(m.cfg.HistogramBuckets) == 0 {
		t.Error("the business histogram buckets must use the defaults")
	}
}

func TestInitXMetric_EmptyListInConfigFailsStartup(t *testing.T) {
	// A list field is replaced whole: whoever writes [] most likely means "use the defaults", while Prometheus
	// quietly replaces it with its own DefBuckets.
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XMetric:\n  HTTPDurationBuckets: []\n")

	err := initXMetric(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTPDurationBuckets") {
		t.Fatalf("an empty list must fail startup and name the field, got %v", err)
	}
	// It is caught while the config is read (xconfig.Unmarshal calls Validate), not only at New.
	if !xerror.Is(err, "xconfig") {
		t.Errorf("the error must come from the config-reading step, got %v", err)
	}
}
