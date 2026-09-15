module github.com/polymetrics-ai/pm-cli

go 1.27.1

require (
	github.com/aws/aws-sdk-go-v2 v1.47.0
	github.com/aws/aws-sdk-go-v2/service/s3 v1.113.0
	github.com/aws/smithy-go v1.28.1
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/duckdb/duckdb-go-bindings v0.10505.0
	github.com/duckdb/duckdb-go/v2 v2.10505.0
	github.com/getkin/kin-openapi v0.149.0
	github.com/gin-gonic/gin v1.12.0
	github.com/jackc/pgx/v5 v5.11.0
	github.com/oapi-codegen/runtime v1.7.0
	github.com/pressly/goose/v3 v3.28.0
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
	github.com/spf13/cobra v1.10.2
	github.com/testcontainers/testcontainers-go v0.44.0
	github.com/zalando/go-keyring v0.2.8
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.71.0
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.46.0
	go.opentelemetry.io/otel/metric v1.46.0
	go.opentelemetry.io/otel/sdk v1.46.0
	go.opentelemetry.io/otel/sdk/metric v1.46.0
	go.opentelemetry.io/otel/trace v1.46.0
	go.temporal.io/api v1.63.4
	go.temporal.io/sdk v1.48.0
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/oauth2 v0.37.0
	golang.org/x/sync v0.23.0
	golang.org/x/time v0.16.0
)

require (
	github.com/duckdb/duckdb-go-bindings/lib/darwin-arm64 v0.10505.0 // indirect
	github.com/duckdb/duckdb-go-bindings/lib/linux-amd64 v0.10505.0 // indirect
	github.com/duckdb/duckdb-go-bindings/lib/linux-arm64 v0.10505.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
)
