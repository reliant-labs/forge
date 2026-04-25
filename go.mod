module github.com/reliant-labs/forge

go 1.26.2

replace github.com/reliant-labs/forge/pkg => ./pkg

require (
	github.com/fsnotify/fsnotify v1.9.0
	github.com/go-delve/delve v1.26.1
	github.com/jackc/pgx/v5 v5.9.1
	github.com/jinzhu/inflection v1.0.0
	github.com/spf13/cobra v1.10.2
	go.opentelemetry.io/otel v1.43.0
	go.opentelemetry.io/otel/metric v1.43.0
	go.opentelemetry.io/otel/trace v1.43.0
	go.yaml.in/yaml/v3 v3.0.4
	golang.org/x/text v0.29.0
	golang.org/x/tools v0.36.0
	google.golang.org/protobuf v1.36.10
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cilium/ebpf v0.11.0 // indirect
	github.com/frankban/quicktest v1.14.6 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	golang.org/x/arch v0.11.0 // indirect
	golang.org/x/exp v0.0.0-20230224173230-c95f2b4c22f2 // indirect
	golang.org/x/mod v0.27.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.36.0 // indirect
	golang.org/x/telemetry v0.0.0-20250807160809-1a19826ec488 // indirect
)
