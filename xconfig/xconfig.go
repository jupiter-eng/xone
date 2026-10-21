// Package xconfig provides the little an integration package needs in order to decode its configuration.
//
// The framework's rule for reading configuration is "defaults pre-filled in the struct, an unknown field is
// an error". Both hold automatically for struct fields, but **a collection cannot hold defaults**: the values
// of a map and the elements of a slice are decoded starting from their zero value, and the framework has
// nothing to pre-fill them with.
//
// A multi-instance Clients block does not have to worry about this: UnmarshalClients spreads the given
// defaults over every instance before decoding. Another collection that needs defaults gets an UnmarshalYAML
// on its element type, which pre-fills the defaults and then decodes:
//
//	func (c *ClientConfig) UnmarshalYAML(n *yaml.Node) error {
//		*c = DefaultClientConfig()
//		type raw ClientConfig // a different type, otherwise this would call itself recursively
//		return xconfig.DecodeStrict(n, (*raw)(c))
//	}
//
// DecodeStrict is required here rather than n.Decode: the latter performs no strict check, so the guarantee
// that "a misspelled field fails startup" would quietly stop holding inside collections.
package xconfig

import (
	"go.yaml.in/yaml/v3"

	"github.com/jupiter-eng/xone/internal/config"
)

// Unmarshal decodes the key block of the config file into into.
//
//	func initXRedis(ctx context.Context) error {
//		c := DefaultConfig()
//		if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
//			return err
//		}
//		...
//	}
//
// into already holds the defaults and a field the file does not set is left unchanged -- which is why no
// pointer fields are needed to tell "not configured" from "configured to the zero value". When the whole
// block is absent, into is left untouched and nil is returned; to tell "not configured" from "configured",
// use Has.
//
// An unrecognized field is an error. If into implements Validate() error, it is called once after decoding.
//
// **It may be called at any time before Start**: in main, before xone.Run, or inside a start hook, all the
// same. The framework looks for the config file and loads it on the first call, so what is read is always the
// final value -- reading early never quietly yields a set of defaults. A block first read inside Start does
// not count as claimed: once the start hooks have run, the framework reports every top-level key nobody read
// as a startup failure.
func Unmarshal(key string, into any) error { return config.Unmarshal(key, into) }

// Has reports whether the config file writes this block, for "initialize only when configured".
//
//	func initXRedis(ctx context.Context) error {
//		if !xconfig.Has(ConfigKey) {
//			return nil // not configured means nothing is built; this module is an optional dependency
//		}
//		...
//	}
//
// As with Unmarshal, asking about a block counts as claiming it: once every start hook has run, the framework
// reports the top-level keys nobody claimed, and the ones that were skipped must not be on that list.
func Has(key string) bool { return config.Has(key) }

// UnmarshalClients decodes a block that supports both a single instance and several, and returns each
// instance by name.
//
//	XGorm:                  # a single instance, written as fields; the name is default
//	  DSN: "${DB_DSN}"
//
//	XGorm:                  # several instances, written by name
//	  Clients:
//	    default: {DSN: "${DB_DSN}"}
//	    report:  {DSN: "${REPORT_DSN}"}
//
//	clients, err := xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)
//
// The presence of Clients decides which form is decoded. Mixing the two is an error outright: there is no
// unsurprising answer to "which one is default" in that case. Writing Clients and leaving it empty is an
// error too.
//
// defaults supplies the defaults for a single instance, and every instance in either form is pre-filled with
// them before decoding, so a field the file does not set keeps its default -- the instance type does not have
// to write an UnmarshalYAML of its own. When the whole block is absent it returns nil; the rest of the rules
// are the same as Unmarshal's (claiming, loading first, Validate).
func UnmarshalClients[C any](key string, defaults func() C) (map[string]C, error) {
	return config.UnmarshalClients(key, defaults)
}

// DecodeStrict decodes a YAML node into v, where an unrecognized field is an error.
//
// Use it only inside a collection element's own UnmarshalYAML (see the package doc): the node is the one yaml
// hands to UnmarshalYAML, so the file and line number in an error still point into the config file.
func DecodeStrict(node *yaml.Node, v any) error { return config.DecodeStrict(node, v) }

// DefaultClientName is the name the single-instance form is normalized to.
const DefaultClientName = config.DefaultClientName
