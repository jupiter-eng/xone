module github.com/jupiter-eng/xone/xmetric

go 1.25.0

require github.com/jupiter-eng/xone v1.4.0

require go.yaml.in/yaml/v3 v3.0.4 // indirect

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_golang v1.24.1
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// For development: what is built locally and in CI is the code in the repository, not a released version.
// A caller's build ignores a replace in a dependency and looks only at the required version, so this one has no effect on them.
replace github.com/jupiter-eng/xone => ../
