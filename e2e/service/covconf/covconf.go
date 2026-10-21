// Package covconf is the Cov business configuration block in the e2e service, for the
// configuration tests.
//
// It stands against the Service block: Service is read in main before xone.Run (docs/config.md,
// "The common rules - when it is read") while Cov is read in a start hook (the other way of
// docs/guide.md, "Reading your own configuration"). Both should see the final values, with the
// profiles / Imports merged and the ${VAR}s expanded, and GET /probe/config hands the test both at
// once.
//
// Leaving this block out means the defaults and does not count as "unclaimed": reading it in a
// start hook is what claims it
package covconf

import (
	"context"
	"sync"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xhook"
)

// Config holds the few fields the configuration tests use: a scalar, a list (to test that a list is
// replaced whole) and a map (to test the recursive merge)
type Config struct {
	Label  string            `yaml:"Label"`
	Items  []string          `yaml:"Items"`
	Labels map[string]string `yaml:"Labels"`
}

// DefaultConfig are the defaults
func DefaultConfig() Config { return Config{Label: "default", Items: []string{"default"}} }

var (
	mu   sync.RWMutex
	conf = DefaultConfig()
)

func init() { xhook.BeforeStart(load) }

func load(context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal("Cov", &c); err != nil {
		return err
	}
	mu.Lock()
	conf = c
	mu.Unlock()
	return nil
}

// C is the Cov block as the start hook read it
func C() Config {
	mu.RLock()
	defer mu.RUnlock()
	return conf
}
