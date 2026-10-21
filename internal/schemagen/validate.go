package main

import (
	"encoding/json"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// compile hands the generated schema to jsonschema-go, so that the configuration is validated by a
// complete draft-07 implementation rather than a hand-written subset recognizing only the keywords
// this generator happens to emit
func compile(r *root) (*jsonschema.Resolved, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return s.Resolve(nil)
}

// problems reports whether a parsed YAML document passes the schema, returning the list of
// problems when it does not
func problems(rs *jsonschema.Resolved, doc any) []string {
	if err := rs.Validate(doc); err != nil {
		return strings.Split(err.Error(), "\n")
	}
	return nil
}
