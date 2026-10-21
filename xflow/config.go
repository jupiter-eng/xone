package xflow

import (
	"context"
	"fmt"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xhook"
)

// ConfigKey is this module's top-level key in the config file.
const ConfigKey = "XFlow"

// Config is the flow orchestration configuration.
type Config struct {
	// Monitor says whether the monitoring callbacks are enabled. It defaults to on.
	//
	// With it off, Execute makes no monitoring callback at all, which is really zero overhead.
	Monitor bool `yaml:"Monitor"`

	// RollbackTimeout is the total budget for rolling every step back. It defaults to 30s.
	//
	// A rollback does not inherit the caller's context (a request timing out would otherwise make every compensation
	// fail); it is limited by this setting instead, so compensation logic cannot hang the shutdown sequence
	// indefinitely. A Rollback that ignores the ctx cannot hang it either: once the budget is reached it is no longer
	// waited for and is recorded in RollbackErrors, as is every step that never got a turn; the abandoned step's goroutine keeps running in the background.
	RollbackTimeout time.Duration `yaml:"RollbackTimeout"`
}

// DefaultConfig holds every default in one place.
func DefaultConfig() Config {
	return Config{Monitor: true, RollbackTimeout: 30 * time.Second}
}

// Validate is called once by the framework after the configuration is loaded, before anything is built.
//
// This module has no resources to initialize and only one thing to do here: a RollbackTimeout of 0 would make every
// rollback time out the moment it starts and skip every compensation, while the flow itself looks entirely normal.
// A misconfiguration like that fails no initialization and can only be caught here.
func (c Config) Validate() error {
	if c.RollbackTimeout <= 0 {
		return fmt.Errorf("RollbackTimeout must be > 0, got=%v", c.RollbackTimeout)
	}
	return nil
}

var cfg = DefaultConfig()

// init has no resources to initialize; it only reads the configuration -- Unmarshal calls Validate along the way.
func init() {
	xhook.BeforeStart(loadConfig, xhook.At(xhook.StageLog))
}

// loadConfig decodes into a fresh set of defaults and only swaps it in once validation passes.
//
// Decoding straight into cfg would carry values from a previous Run in the same process into the next one (fields
// left unset would not return to their defaults), and a failed validation would already have written the invalid value (a RollbackTimeout: 0, say) onto cfg.
func loadConfig(context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return err
	}
	cfg = c
	return nil
}
