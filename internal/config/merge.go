package config

import (
	"go.yaml.in/yaml/v3"
)

// merge lays override on top of base and returns the result.
//
// The rules match Spring's (which is where most callers come from):
//
//   - a map: merged recursively, with override's value for a key both hold
//   - a list: replaced whole, not merged element by element
//   - a scalar: override replaces base
//
// Replacing a list whole is the rule most easily misunderstood here, so the reasoning is spelled
// out: merged element by element, base holding [A, B] and override holding [C] would give [C, B] --
// the caller believes they replaced the whole table while only the first entry was replaced and the
// rest comes from another file. Spring replaces a list whole for the same reason.
//
// Appending to a list means writing the whole list out.
//
// "Not written" laid on top changes nothing: an empty file (holding only comments counts) parses
// into a node whose Kind is 0, and `Demo:` and `Addr: ~` are null. The former used to count as a
// scalar and replaced every file before it whole -- one # TODO line in a profile file quietly put
// the whole configuration back to its defaults; the latter had `Demo:` clear base's whole block
// while `Demo: {}` kept it.
// Inside one file a null keeps the default, so laid on top it should keep the value of the
// lower-priority file as well.
// Clearing something means writing the empty value out: `""`, `[]`.
func merge(base, override *yaml.Node) *yaml.Node {
	if absent(override) {
		return base
	}
	if absent(base) {
		return override
	}

	// A document node: one level in before merging
	if base.Kind == yaml.DocumentNode && override.Kind == yaml.DocumentNode {
		if len(base.Content) == 0 {
			return override
		}
		if len(override.Content) == 0 {
			return base
		}
		out := *base
		out.Content = []*yaml.Node{merge(base.Content[0], override.Content[0])}
		return &out
	}

	// Only two maps are merged recursively; everything else is replaced whole
	if base.Kind != yaml.MappingNode || override.Kind != yaml.MappingNode {
		return override
	}
	return mergeMapping(base, override)
}

// mergeMapping merges two map nodes, keeping the order base's keys appeared in and appending the
// keys override adds.
//
// The order is not kept for appearance: the error messages carry line numbers, a disordered set of
// nodes puts "which line" out of step, and the duplicate key check is exactly what points at both
// places by their line numbers.
func mergeMapping(base, override *yaml.Node) *yaml.Node {
	out := *base
	out.Content = nil

	// override's key -> value, deleted once used, and what is left are the ones it adds
	pending := map[string]*yaml.Node{}
	order := make([]*yaml.Node, 0, len(override.Content)/2) // override's key nodes, in the order they were written
	for i := 0; i+1 < len(override.Content); i += 2 {
		k := override.Content[i]
		if _, dup := pending[k.Value]; !dup {
			order = append(order, k)
		}
		pending[k.Value] = override.Content[i+1]
	}

	for i := 0; i+1 < len(base.Content); i += 2 {
		key, val := base.Content[i], base.Content[i+1]
		if ov, ok := pending[key.Value]; ok {
			out.Content = append(out.Content, key, merge(val, ov))
			delete(pending, key.Value)
			continue
		}
		out.Content = append(out.Content, key, val)
	}

	// The keys override alone holds, appended in the order they appear in override
	for _, k := range order {
		if v, ok := pending[k.Value]; ok { // the ones not there were consumed above
			out.Content = append(out.Content, k, v)
		}
	}
	return &out
}

// absent reports whether a node is "nothing was written here": an empty file, or null
func absent(n *yaml.Node) bool {
	return n == nil || n.Kind == 0 || isNull(n)
}

// isNull reports whether a node is null: `Demo:`, `Demo: ~`, `Demo: null`
func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}
