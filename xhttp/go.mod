module github.com/jupiter-eng/xone/xhttp

go 1.25.0

require (
	github.com/go-resty/resty/v2 v2.17.2
	github.com/jupiter-eng/xone v1.4.1
	github.com/jupiter-eng/xone/xmetric v1.4.1
	github.com/jupiter-eng/xone/xtrace v1.4.1
	github.com/prometheus/client_golang v1.24.1
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.71.0
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/sdk v1.46.0
	go.opentelemetry.io/otel/trace v1.46.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/propagators/b3 v1.46.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.46.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// For development: what is built locally and in CI is the code in the repository, not a released version.
// A caller's build ignores a replace in a dependency and looks only at the required version, so this one has no effect on them.
replace (
	github.com/jupiter-eng/xone => ../
	github.com/jupiter-eng/xone/xmetric => ../xmetric
	github.com/jupiter-eng/xone/xtrace => ../xtrace
)
