// Package xapp provides the application's own identity: its name and its version.
//
// These are facts several components share -- tracing wants the name for service.name, and the API docs want it
// for the default title and version. Putting it in any one component's configuration would mean configuring it
// again in the others, and one day they would disagree. So it is a block of its own that anyone can read.
//
//	XApp:
//	  Name: xone.demo.app
//	  Version: v1.2.0
//
// This package initializes nothing; it only claims the XApp block while the configuration is loaded (Profiles and Import inside it are read by the loader itself and never reach here).
package xapp

import (
	"context"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xhook"
)

// ConfigKey is this module's top-level key in the config file.
const ConfigKey = "XApp"

// Config is the application identity.
type Config struct {
	// Name is the application name, preferably globally unique, such as team.system.app.
	Name string `yaml:"Name"`

	// Version is the application version, such as v1.2.0.
	Version string `yaml:"Version"`
}

// DefaultConfig holds the defaults. The name is left empty -- guessing one would be worse than having none:
// traces would fill up with identically named unknowns, which hides who is who instead of showing it.
func DefaultConfig() Config { return Config{} }

var cfg = DefaultConfig()

// init has no resources to initialize; it only reads the XApp block.
//
// It hangs on StageLog: tracing takes XApp.Name as its service name and lives on StageTelemetry, so this block
// has to be read before that.
func init() {
	xhook.BeforeStart(loadConfig, xhook.At(xhook.StageLog))
}

// loadConfig decodes into a fresh set of defaults and only swaps it in on success.
//
// Decoding straight into cfg would carry values from a previous Run in the same process into the next one (fields
// left unset would not return to their defaults), and a failed decode would already have written half of its values onto cfg.
func loadConfig(context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return err
	}
	cfg = c
	return nil
}

// Name returns the application name, or an empty string when it is not configured.
//
// The configuration is read before the other components start, so any start hook reads the final value.
func Name() string { return cfg.Name }

// Version returns the application version, or an empty string when it is not configured.
func Version() string { return cfg.Version }
