// Package conf is the example of "the business's own configuration block".
//
// It has nothing to initialize, so the whole package does one thing: reads the MyApp block into a
// package-level variable. Reading is one line of xconfig.Unmarshal, called at any time before Start
// -- this example calls it in main.
//
// A package of its own rather than part of main: other packages want the configuration too, and here
// anybody can import it.
package conf

import "github.com/jupiter-eng/xone/xconfig"

// Config is the business's own configuration
type Config struct {
	// Greeting is the greeting printed each round
	Greeting string `yaml:"Greeting"`

	// Rounds is how many rounds to run before finishing. It defaults to 3
	Rounds int `yaml:"Rounds"`
}

// DefaultConfig pre-fills the defaults in the struct; a field the file does not set stays as it is
func DefaultConfig() Config {
	return Config{Greeting: "hello", Rounds: 3}
}

// conf is the configuration in effect
var conf = DefaultConfig()

// Load reads the MyApp block. Called once in main, after which C() works everywhere
func Load() error { return xconfig.Unmarshal("MyApp", &conf) }

// C returns the final configuration
func C() Config { return conf }
