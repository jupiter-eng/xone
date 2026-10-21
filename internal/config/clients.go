package config

import (
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xerror"
)

const (
	clientsKey = "Clients"

	// DefaultClientName is the name the single-instance spelling is normalized to, which is the one
	// C() takes without an argument. Normalizing and taking have to use the same name, so it is
	// written in xclient alone; the direction is this package depending on xclient, because the
	// other way round would pull yaml into xclient indirectly and it would no longer have zero
	// third-party dependencies
	DefaultClientName = xclient.DefaultName
)

// UnmarshalClients decodes a configuration block that "supports one instance and several alike"
// and records that the block has been read.
//
// With the whole block unconfigured (or empty) it returns nil and no error. The other rules are
// Unmarshal's: it loads first when nothing has been loaded, and calls Validate() error once per
// instance when the instance type implements it.
func UnmarshalClients[C any](key string, defaults func() C) (map[string]C, error) {
	mu.Lock()
	defer mu.Unlock()

	node, err := sectionLocked(key)
	if err != nil || node == nil {
		return nil, err
	}
	out, err := decodeClients(node, defaults)
	if err != nil {
		return nil, xerror.Newf("xconfig", "config", "invalid config %s: %w", key, err)
	}
	return out, nil
}

// decodeClients decides which spelling to decode by whether Clients is there:
//
//	XGorm:                  # one instance, with the fields written directly and the name default
//	  DSN: "${DB_DSN}"
//
//	XGorm:                  # several instances, written by name
//	  Clients:
//	    default: {DSN: "${DB_DSN}"}
//	    report:  {DSN: "${REPORT_DSN}"}
//
// Mixing the two is an error outright: there is no answer to "which one is default, then" that does
// not surprise somebody.
//
// In both spellings every instance has the defaults spread over it before it is decoded, so a field
// the file does not set keeps its default -- an instance type needs no UnmarshalYAML of its own.
func decodeClients[C any](n *yaml.Node, defaults func() C) (map[string]C, error) {
	list := valueOf(n, clientsKey)
	if list == nil {
		single, err := decodeClient(n, defaults)
		if err != nil {
			return nil, err
		}
		return map[string]C{DefaultClientName: single}, nil
	}

	// Mixing is recognized here rather than read out of a decoding error.
	// Left to the decoder, one misspelled field inside an instance (Clients.default.DSNN) would be
	// reported as "cannot mix" as well -- while that configuration mixes nothing, and the caller
	// would follow the sentence to change something that was never wrong.
	if stray := keysExcept(n, clientsKey); len(stray) > 0 {
		return nil, fmt.Errorf("cannot mix the single- and multi-instance forms: with %s present, "+
			"%s belong to no instance — move them into one, or drop %s and use the single-instance form",
			clientsKey, strings.Join(stray, ", "), clientsKey)
	}
	if isEmptyNode(list) {
		return nil, fmt.Errorf("%s is empty: either list instances under it, or remove the whole block", clientsKey)
	}
	if list.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: %s must map instance names to their config", newChecker().at(list), clientsKey)
	}

	// The defaults are spread over each instance before it is decoded: decoding straight into a
	// map[string]C starts a map's value from the zero value and every unset field becomes it. The
	// problems of all the instances are gathered and reported together
	var errs []error
	out := make(map[string]C, len(list.Content)/2)
	for i := 0; i+1 < len(list.Content); i += 2 {
		name := list.Content[i].Value
		c, err := decodeClient(list.Content[i+1], defaults)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.%s: %w", clientsKey, name, err))
		}
		out[name] = c
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// decodeClient spreads the defaults and decodes one instance strictly.
// A name written alone (report: with nothing under it) is null, and decoding leaves the defaults
func decodeClient[C any](n *yaml.Node, defaults func() C) (C, error) {
	c := defaults()
	if err := DecodeStrict(n, &c); err != nil {
		return c, err
	}
	// Validate's error gets the instance's file and line in front of it: it can say which field is
	// wrong while not knowing where it was read from, and with several instances merged across
	// several files, a field name alone is hard to find
	if v, ok := any(&c).(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return c, fmt.Errorf("%s: %w", newChecker().at(n), err)
		}
	}
	return c, nil
}

// valueOf is the value for key inside a mapping, or nil when there is none
func valueOf(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	if i := keyIndex(node, key); i >= 0 {
		return node.Content[i+1]
	}
	return nil
}

// keysExcept lists the keys of a mapping except for except, keeping the order they were written
// in
func keysExcept(node *yaml.Node, except string) []string {
	var out []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		if k := node.Content[i].Value; k != except {
			out = append(out, k)
		}
	}
	return out
}
