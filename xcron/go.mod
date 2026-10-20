module github.com/jupiter-eng/xone/xcron

go 1.25.0

require (
	github.com/hashicorp/cronexpr v1.1.3
	github.com/jupiter-eng/xone v1.0.3
	github.com/jupiter-eng/xone/xtrace v1.0.3
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

// 开发用：本地和 CI 编的都是仓库里的代码，不是已发布的版本。
// 使用者的构建会忽略依赖里的 replace、只看 require 的版本，这里的 replace 对他们不起作用
replace (
	github.com/jupiter-eng/xone => ../
	github.com/jupiter-eng/xone/xtrace => ../xtrace
)
