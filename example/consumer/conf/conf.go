// Package conf shows how the business's own configuration block joins the framework.
//
// A top-level key nobody reads fails startup outright (most likely it is misspelled, or the matching
// package was never imported), so a MyApp written in the configuration file needs somebody to read
// it. Reading is one line of xconfig.Unmarshal, called at any time before Start -- this example
// calls it in main.
//
// A package of its own rather than part of main: other packages want the configuration too, and here
// anybody can import it.
package conf

import (
	"fmt"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
)

// Config is the business's own configuration
type Config struct {
	// Topic is the topic to subscribe to
	Topic string `yaml:"Topic"`

	// Workers is how many goroutines handle messages concurrently. It defaults to 4
	Workers int `yaml:"Workers"`

	// MessageTimeout is the upper bound on handling one message. It defaults to 5s.
	// It has to be smaller than the server's share of the stop budget (2/3 of xone.WithStopTimeout,
	// 10s by default), see Settings.Timeout
	MessageTimeout time.Duration `yaml:"MessageTimeout"`
}

// Validate is called by xconfig.Unmarshal once it has read, so a misconfigured value fails at
// startup.
//
// Neither of these two produces an error on its own; they only make the service "do nothing,
// normally": with Workers at 0 not one worker starts and Start returns at once, and with a
// MessageTimeout that is not greater than 0 every message times out the moment it begins and is
// Nacked.
func (c Config) Validate() error {
	if c.Workers <= 0 {
		return fmt.Errorf("Workers must be > 0, got=%d", c.Workers)
	}
	if c.MessageTimeout <= 0 {
		return fmt.Errorf("MessageTimeout must be > 0, got=%v", c.MessageTimeout)
	}
	return nil
}

// DefaultConfig pre-fills the defaults in the struct; a field the file does not set stays as it is
func DefaultConfig() Config {
	return Config{
		Topic:          "orders",
		Workers:        4,
		MessageTimeout: 5 * time.Second,
	}
}

// conf is the configuration in effect
var conf = DefaultConfig()

// Load reads the MyApp block. Called once in main, after which C() works everywhere
func Load() error { return xconfig.Unmarshal("MyApp", &conf) }

// C returns the final configuration
func C() Config { return conf }
