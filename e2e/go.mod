module github.com/jupiter-eng/xone/e2e

go 1.25.0

// e2e is not released (scripts/release.sh leaves it out), so the replace lines stay in for good:
// it always tests the repository's current code and never some released version
replace (
	github.com/jupiter-eng/xone => ../
	github.com/jupiter-eng/xone/xbeego => ../xbeego
	github.com/jupiter-eng/xone/xcache => ../xcache
	github.com/jupiter-eng/xone/xcron => ../xcron
	github.com/jupiter-eng/xone/xgin => ../xgin
	github.com/jupiter-eng/xone/xginswagger => ../xginswagger
	github.com/jupiter-eng/xone/xgorm => ../xgorm
	github.com/jupiter-eng/xone/xgorm/clickhouse => ../xgorm/clickhouse
	github.com/jupiter-eng/xone/xhttp => ../xhttp
	github.com/jupiter-eng/xone/xkafka => ../xkafka
	github.com/jupiter-eng/xone/xmetric => ../xmetric
	github.com/jupiter-eng/xone/xredis => ../xredis
	github.com/jupiter-eng/xone/xtrace => ../xtrace
)

require (
	github.com/ClickHouse/clickhouse-go/v2 v2.48.0
	github.com/beego/beego/v2 v2.3.8
	github.com/gin-gonic/gin v1.12.0
	github.com/go-sql-driver/mysql v1.10.1
	github.com/jackc/pgx/v5 v5.10.0
	github.com/jupiter-eng/xone v1.2.0
	github.com/jupiter-eng/xone/xbeego v1.2.0
	github.com/jupiter-eng/xone/xcache v1.2.0
	github.com/jupiter-eng/xone/xcron v1.2.0
	github.com/jupiter-eng/xone/xgin v1.2.0
	github.com/jupiter-eng/xone/xginswagger v1.2.0
	github.com/jupiter-eng/xone/xgorm v1.2.0
	github.com/jupiter-eng/xone/xgorm/clickhouse v1.2.0
	github.com/jupiter-eng/xone/xhttp v1.2.0
	github.com/jupiter-eng/xone/xkafka v1.2.0
	github.com/jupiter-eng/xone/xmetric v1.2.0
	github.com/jupiter-eng/xone/xredis v1.2.0
	github.com/jupiter-eng/xone/xtrace v1.2.0
	github.com/prometheus/client_model v0.6.2
	github.com/prometheus/common v0.70.1
	github.com/redis/go-redis/v9 v9.22.0
	github.com/swaggo/swag v1.16.6
	github.com/twmb/franz-go v1.21.7
	github.com/twmb/franz-go/pkg/kmsg v1.13.1
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/sdk v1.46.0
	gorm.io/driver/clickhouse v0.7.0
	gorm.io/driver/mysql v1.6.0
	gorm.io/driver/postgres v1.6.3
	gorm.io/gorm v1.31.2
)

require (
	github.com/ClickHouse/ch-go v0.74.0 // indirect
	github.com/andybalholm/brotli v1.2.2 // indirect
	github.com/go-faster/city v1.0.1 // indirect
	github.com/go-faster/errors v0.7.1 // indirect
	github.com/hashicorp/go-version v1.9.0 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/paulmach/orb v0.13.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.27 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/KyleBanks/depth v1.2.1 // indirect
	github.com/PuerkitoBio/purell v1.1.1 // indirect
	github.com/PuerkitoBio/urlesc v0.0.0-20170810143723-de5bf2ad4578 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bytedance/gopkg v0.1.3 // indirect
	github.com/bytedance/sonic v1.15.0 // indirect
	github.com/bytedance/sonic/loader v0.5.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/dgraph-io/ristretto/v2 v2.4.2 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/gabriel-vasile/mimetype v1.4.15 // indirect
	github.com/gin-contrib/sse v1.1.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-openapi/jsonpointer v0.19.5 // indirect
	github.com/go-openapi/jsonreference v0.19.6 // indirect
	github.com/go-openapi/spec v0.20.4 // indirect
	github.com/go-openapi/swag v0.19.15 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.30.4 // indirect
	github.com/go-resty/resty/v2 v2.17.2 // indirect
	github.com/goccy/go-json v0.10.5 // indirect
	github.com/goccy/go-yaml v1.19.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/cronexpr v1.1.3 // indirect
	github.com/hashicorp/golang-lru v0.5.4 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/leodido/go-urn v1.5.0 // indirect
	github.com/mailru/easyjson v0.7.6 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mitchellh/mapstructure v1.5.0 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/prometheus/client_golang v1.24.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/quic-go/quic-go v0.59.0 // indirect
	github.com/redis/go-redis/extra/rediscmd/v9 v9.22.0 // indirect
	github.com/redis/go-redis/extra/redisotel/v9 v9.22.0 // indirect
	github.com/shiena/ansicolor v0.0.0-20200904210342-c7312218db18 // indirect
	github.com/swaggo/files v1.0.1 // indirect
	github.com/swaggo/gin-swagger v1.6.1 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/ugorji/go/codec v1.3.1 // indirect
	go.mongodb.org/mongo-driver/v2 v2.5.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.71.0 // indirect
	go.opentelemetry.io/contrib/propagators/b3 v1.46.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.46.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/arch v0.22.0 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/tools v0.48.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
