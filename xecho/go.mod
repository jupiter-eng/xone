module github.com/jupiter-eng/xone/xecho

go 1.25.0

// 开发用：本地和 CI 编的都是仓库里的代码，不是已发布的版本。
// 使用者的构建会忽略依赖里的 replace、只看 require 的版本，这里的 replace 对他们不起作用
replace (
	github.com/jupiter-eng/xone => ../
	github.com/jupiter-eng/xone/xmetric => ../xmetric
	github.com/jupiter-eng/xone/xtrace => ../xtrace
)

require (
	github.com/jupiter-eng/xone v1.0.2
	github.com/jupiter-eng/xone/xmetric v1.0.2
	github.com/jupiter-eng/xone/xtrace v1.0.2
	github.com/labstack/echo/v4 v4.16.0
	github.com/prometheus/client_golang v1.24.1
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/sdk v1.46.0
	go.opentelemetry.io/otel/trace v1.46.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/labstack/gommon v0.5.0 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasttemplate v1.2.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/propagators/b3 v1.46.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.46.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
