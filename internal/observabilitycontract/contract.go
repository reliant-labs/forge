// Package observabilitycontract freezes version-pinned ClickStack upgrade contracts.
package observabilitycontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

const HyperDXVersion = "2.40.0"

var Images = map[string]string{
	"clickhouse": "clickhouse/clickhouse-server@sha256:369bd68942569e0effdf0aa68b4f844a74d97b73e281cf8012ce90e9865cc411",
	"mongo":      "mongo:8.0.14@sha256:6eaaf8b194dc2c78a61deae63f1c163c2e55769c2157d6406f517d902694c792",
	"collector":  "clickhouse/clickstack-otel-collector:2.40.0@sha256:0e7dc29f8f6074afd106b96d53e5b98f1150e8882d926a20c03c6f680bd929ec",
	"hyperdx":    "docker.hyperdx.io/hyperdx/hyperdx:2.40.0@sha256:2fcba6813f5c935f6a3e04123ffe62ab3baf0c1cd53f279b1ec9e300343ddf60",
}

// NormalizeSchema canonicalizes ClickHouse JSONEachRow DESCRIBE output before hashing.
func NormalizeSchema(input string) string {
	lines := strings.Split(strings.TrimSpace(input), "\n")
	values := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var value any
		if json.Unmarshal([]byte(line), &value) != nil {
			values = append(values, strings.TrimSpace(line))
			continue
		}
		canonical, _ := json.Marshal(value)
		values = append(values, string(canonical))
	}
	sort.Strings(values)
	return strings.Join(values, "\n") + "\n"
}
func SchemaHash(input string) string {
	sum := sha256.Sum256([]byte(NormalizeSchema(input)))
	return "sha256:" + hex.EncodeToString(sum[:])
}
func PinnedDigest(image string) string {
	index := strings.Index(image, "@sha256:")
	if index < 0 {
		return ""
	}
	return image[index+1:]
}

const ComposeTemplate = `services:
  # All ports bind loopback and ClickHouse's empty default credential is test-only.
  mongo:
    image: mongo:8.0.14@sha256:6eaaf8b194dc2c78a61deae63f1c163c2e55769c2157d6406f517d902694c792
    healthcheck: {test: ["CMD", "mongosh", "--eval", "db.runCommand({ ping: 1 })"], interval: 5s, timeout: 5s, retries: 30}
  ch-server:
    image: clickhouse/clickhouse-server@sha256:369bd68942569e0effdf0aa68b4f844a74d97b73e281cf8012ce90e9865cc411
    environment: {CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT: "1"}
    ports: ["127.0.0.1:${CH_PORT}:8123"]
  otel-collector:
    image: clickhouse/clickstack-otel-collector:2.40.0@sha256:0e7dc29f8f6074afd106b96d53e5b98f1150e8882d926a20c03c6f680bd929ec
    environment: {CLICKHOUSE_ENDPOINT: "tcp://ch-server:9000?dial_timeout=10s", HYPERDX_OTEL_EXPORTER_CLICKHOUSE_DATABASE: default, HYPERDX_OTEL_EXPORTER_CREATE_LEGACY_SCHEMA: "true"}
    ports: ["127.0.0.1:${GRPC_PORT}:4317"]
    restart: on-failure
    depends_on: [ch-server]
  hyperdx:
    image: docker.hyperdx.io/hyperdx/hyperdx:2.40.0@sha256:2fcba6813f5c935f6a3e04123ffe62ab3baf0c1cd53f279b1ec9e300343ddf60
    environment: {HYPERDX_API_KEY: contract-key, HYPERDX_API_PORT: "8000", HYPERDX_APP_PORT: "8080", HYPERDX_APP_URL: http://localhost, FRONTEND_URL: http://localhost:8080, MONGO_URI: mongodb://mongo:27017/hyperdx, SERVER_URL: http://127.0.0.1:8000, OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector:4318, DEFAULT_CONNECTIONS: '[{"name":"Local ClickHouse","host":"http://ch-server:8123","username":"default","password":""}]'}
    ports: ["127.0.0.1:${HDX_PORT}:8000"]
    depends_on: [mongo, ch-server, otel-collector]
`
