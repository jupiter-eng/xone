module github.com/jupiter-eng/xone

go 1.23.0

require go.yaml.in/yaml/v3 v3.0.4

// v1.0.0 shipped only the root module tag; the integration modules (xgin, xgorm, ...)
// were never tagged, so that version cannot be installed as a set. Use v1.0.1 or later.
retract v1.0.0
