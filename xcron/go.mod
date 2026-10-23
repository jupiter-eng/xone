module github.com/jupiter-eng/xone/xcron

go 1.25.0

require (
	github.com/hashicorp/cronexpr v1.1.3
	github.com/jupiter-eng/xone v1.4.0
	github.com/jupiter-eng/xone/xtrace v1.4.0
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/sdk v1.46.0
	go.opentelemetry.io/otel/trace v1.46.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/propagators/b3 v1.46.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.46.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

// For development: what is built locally and in CI is the code in the repository, not a released version.
// A caller's build ignores a replace in a dependency and looks only at the required version, so this one has no effect on them.
replace (
	github.com/jupiter-eng/xone => ../
	github.com/jupiter-eng/xone/xtrace => ../xtrace
)
