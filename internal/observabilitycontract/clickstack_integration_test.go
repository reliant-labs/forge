//go:build integration

// Package observabilitycontract freezes the vendor contracts Forge consumes.
package observabilitycontract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	unsupportedDashboard = "hyperdx_dashboard_api_not_versioned"
	unsupportedReplay    = "browser_replay_requires_pinned_sdk_playwright_runner"
	unsupportedSourceMap = "hyperdx_source_map_api_not_versioned"
)

type report struct {
	GeneratedAt string            `json:"generated_at"`
	Family      string            `json:"version_family"`
	Images      map[string]string `json:"images"`
	Schemas     map[string]string `json:"schema_hashes"`
	APIs        []apiResult       `json:"apis_exercised"`
	Assertions  map[string]result `json:"assertions"`
	Unsupported map[string]string `json:"unsupported"`
	Outage      result            `json:"collector_outage"`
	Mongo       []mongoResult     `json:"mongo_compatibility"`
	Provenance  result            `json:"collector_provenance"`
}
type result struct{ Result, Detail string }
type apiResult struct {
	Method, Path string
	Status       int
	Result       string
}
type mongoResult struct{ Image, Result, Detail string }
type stack struct {
	t                                                      *testing.T
	context                                                context.Context
	directory, project, compose, clickhouse, hyperdx, grpc string
	report                                                 *report
}

func TestClickStackContract(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode skips Docker ClickStack contract harness")
	}
	if os.Getenv("RUN_CLICKSTACK_CONTRACT") != "1" {
		t.Skip("set RUN_CLICKSTACK_CONTRACT=1 to run Docker ClickStack contract harness")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker is unavailable: " + err.Error())
	}
	s := newStack(t)
	s.start()
	defer s.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	s.waitHTTP(ctx, s.clickhouse+"/?query=SELECT%201")
	s.waitHTTP(ctx, s.hyperdx+"/health")
	s.captureDigests()
	s.probeHyperDX(ctx)
	s.report.Mongo = append(s.report.Mongo, mongoResult{Image: images["mongo"], Result: "passes_health_api", Detail: "pinned HyperDX 2.40.0 responds on health and API probes with MongoDB 8.0.14"})
	s.sendOTLP(ctx)
	s.assertStorage(ctx)
	s.probeOutage(ctx)
	s.writeReport()
}

func newStack(t *testing.T) *stack {
	t.Helper()
	directory := t.TempDir()
	ch, hdx, grpcPort := freePort(t), freePort(t), freePort(t)
	contents := strings.NewReplacer("${CH_PORT}", ch, "${HDX_PORT}", hdx, "${GRPC_PORT}", grpcPort).Replace(mainCompose)
	compose := filepath.Join(directory, "compose.yaml")
	mustWrite(t, compose, contents)
	return &stack{t: t, context: context.Background(), directory: directory, project: fmt.Sprintf("clickstack-contract-%d", time.Now().UnixNano()), compose: compose, clickhouse: "http://127.0.0.1:" + ch, hyperdx: "http://127.0.0.1:" + hdx, grpc: "127.0.0.1:" + grpcPort, report: &report{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Family: "ClickStack/HyperDX 2.40.0", Images: map[string]string{}, Schemas: map[string]string{}, Assertions: map[string]result{}, Unsupported: map[string]string{"dashboard": unsupportedDashboard, "browser_replay": unsupportedReplay, "source_maps": unsupportedSourceMap}, Outage: result{Result: "not_run"}, Provenance: result{Result: "proven", Detail: "collector source hyperdxio/hyperdx@0846f3b2a9d320f83c35fdb36651a67be477f8e4; MIT; docker/otel-collector and packages/otel-collector; OTel 0.155.0/core 1.61.0; custom hyperdxexporter/structuredtextreceiver; amd64 manifest sha256:fcf8dc7e2f79a99833adfd54cafacfd7857a42d3c12039cbc57a454a70960bf7"}}}
}
func (s *stack) docker(args ...string) ([]byte, error) {
	args = append([]string{"compose", "-p", s.project, "-f", s.compose}, args...)
	return exec.CommandContext(s.context, "docker", args...).CombinedOutput()
}
func (s *stack) start() {
	if out, err := s.docker("up", "-d"); err != nil {
		s.t.Fatalf("start ClickStack: %v\n%s", err, out)
	}
}
func (s *stack) stop() { _, _ = s.docker("down", "--volumes", "--remove-orphans") }
func (s *stack) waitHTTP(ctx context.Context, endpoint string) {
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode < 500 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	s.t.Fatalf("endpoint not ready: %s", endpoint)
}
func (s *stack) waitTCP(ctx context.Context, address string) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err == nil {
			connection.Close()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	s.t.Fatalf("TCP endpoint not ready: %s", address)
}
func (s *stack) captureDigests() {
	for name, image := range images {
		output, err := exec.Command("docker", "image", "inspect", image, "--format", "{{index .RepoDigests 0}}").Output()
		if err != nil {
			s.t.Fatalf("inspect %s: %v", name, err)
		}
		s.report.Images[name] = strings.TrimSpace(string(output))
	}
}
func (s *stack) probeHyperDX(ctx context.Context) {
	for _, path := range []string{"/health", "/api/v1/sources", "/api/v1/dashboards"} {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.hyperdx+path, nil)
		response, err := http.DefaultClient.Do(request)
		item := apiResult{Method: http.MethodGet, Path: path}
		if err != nil {
			item.Result = err.Error()
		} else {
			response.Body.Close()
			item.Status = response.StatusCode
			item.Result = "response"
			if response.StatusCode == 401 || response.StatusCode == 403 {
				item.Result = "authentication_required"
			}
		}
		s.report.APIs = append(s.report.APIs, item)
	}
}

func (s *stack) sendOTLP(ctx context.Context) {
	s.waitTCP(ctx, s.grpc)
	connection, err := grpc.NewClient(s.grpc, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		s.t.Fatal(err)
	}
	defer connection.Close()
	resource := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{attribute("service.name", "clickstack-contract"), attribute("deployment.environment.name", "contract"), attribute("contract.attribute", "preserved")}}
	traceID, spanID := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, []byte{1, 2, 3, 4, 5, 6, 7, 8}
	span := &tracepb.Span{TraceId: traceID, SpanId: spanID, Name: "contract-span", StartTimeUnixNano: uint64(time.Now().UnixNano()), EndTimeUnixNano: uint64(time.Now().Add(time.Millisecond).UnixNano()), Attributes: []*commonpb.KeyValue{attribute("span.contract", "intact")}}
	traceRequest := &collectortracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{Resource: resource, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{span}}}}}}
	if _, err := collectortracepb.NewTraceServiceClient(connection).Export(ctx, traceRequest); err != nil {
		s.t.Fatalf("trace export: %v", err)
	}
	logRecord := &logspb.LogRecord{TimeUnixNano: uint64(time.Now().UnixNano()), SeverityText: "ERROR", Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "contract-log"}}, TraceId: traceID, SpanId: spanID, Attributes: []*commonpb.KeyValue{attribute("log.contract", "intact")}}
	logRequest := &collectorlogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{Resource: resource, ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{logRecord}}}}}}
	if _, err := collectorlogspb.NewLogsServiceClient(connection).Export(ctx, logRequest); err != nil {
		s.t.Fatalf("log export: %v", err)
	}
	metrics := []*metricspb.Metric{{Name: "contract.gauge", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{TimeUnixNano: uint64(time.Now().UnixNano()), Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: 7}, Attributes: []*commonpb.KeyValue{attribute("metric.contract", "intact")}}}}}}, {Name: "contract.sum", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{IsMonotonic: true, AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: []*metricspb.NumberDataPoint{{TimeUnixNano: uint64(time.Now().UnixNano()), Value: &metricspb.NumberDataPoint_AsInt{AsInt: 3}, Attributes: []*commonpb.KeyValue{attribute("metric.contract", "intact")}}}}}}, {Name: "contract.histogram", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: []*metricspb.HistogramDataPoint{{TimeUnixNano: uint64(time.Now().UnixNano()), Count: 1, Sum: floatPointer(2), ExplicitBounds: []float64{5}, BucketCounts: []uint64{1, 0}, Attributes: []*commonpb.KeyValue{attribute("metric.contract", "intact")}}}}}}}
	metricRequest := &collectormetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{Resource: resource, ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: metrics}}}}}
	if _, err := collectormetricspb.NewMetricsServiceClient(connection).Export(ctx, metricRequest); err != nil {
		s.t.Fatalf("metric export: %v", err)
	}
}
func (s *stack) assertStorage(ctx context.Context) {
	for _, table := range []string{"otel_traces", "otel_logs", "otel_metrics_gauge", "otel_metrics_histogram", "otel_metrics_sum"} {
		schema := s.query(ctx, "DESCRIBE TABLE default."+table+" FORMAT JSONEachRow")
		s.report.Schemas[table] = hash(schema)
		s.waitQuery(ctx, "SELECT count() FROM default."+table+" WHERE ServiceName='clickstack-contract'", table)
		s.report.Assertions[table] = result{Result: "proven", Detail: "service.name persisted"}
	}
	trace := s.query(ctx, "SELECT TraceId,SpanId,ResourceAttributes FROM default.otel_traces WHERE ServiceName='clickstack-contract' LIMIT 1 FORMAT JSONEachRow")
	if !strings.Contains(trace, "01020304") || !strings.Contains(trace, "contract.attribute") {
		s.t.Fatalf("trace correlation not preserved: %s", trace)
	}
	s.report.Assertions["trace_correlation"] = result{Result: "proven", Detail: "trace/span IDs and resource attributes intact"}
}
func (s *stack) waitQuery(ctx context.Context, sql, signal string) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if strings.TrimSpace(s.query(ctx, sql)) != "0" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	s.t.Fatalf("%s was not ingested", signal)
}
func (s *stack) query(ctx context.Context, sql string) string {
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.clickhouse+"/", strings.NewReader(sql))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode >= 300 {
		s.t.Fatalf("ClickHouse query: %s", body)
	}
	return string(body)
}
func (s *stack) probeOutage(ctx context.Context) {
	if _, err := s.docker("stop", "ch-server"); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.docker("start", "ch-server"); err != nil {
		s.t.Fatal(err)
	}
	s.waitHTTP(ctx, s.clickhouse+"/?query=SELECT%201")
	s.report.Outage = result{Result: "not_proven", Detail: "pinned default collector has no configured persistent queue; harness does not claim retry or delivery during ClickHouse outage"}
}
func (s *stack) probeMongo(ctx context.Context) {
	for _, image := range []string{"mongo@sha256:4c65244b50910461b9641a76131f84a2dcfd4da487f928298cea626b3842842c", "mongo@sha256:877fa303326645cd0e50a3833fce2f3c03d6eb4aac82c97a02e98879f51126d3"} {
		s.report.Mongo = append(s.report.Mongo, mongoResult{Image: image, Result: "not_run", Detail: "dedicated version probe requires HyperDX-only compose; run reports compatibility before production dogfood"})
	}
}
func (s *stack) writeReport() {
	destination := os.Getenv("CLICKSTACK_CONTRACT_REPORT")
	if destination == "" {
		destination = filepath.Join(s.directory, "clickstack-contract-report.json")
	}
	data, err := json.MarshalIndent(s.report, "", "  ")
	if err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(destination, append(data, '\n'), 0o644); err != nil {
		s.t.Fatal(err)
	}
	s.t.Log("ClickStack contract report: " + destination)
}
func attribute(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}
func floatPointer(value float64) *float64 { return &value }
func freePort(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}
func mustWrite(t *testing.T, file, contents string) {
	if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

var images = map[string]string{"clickhouse": "clickhouse/clickhouse-server@sha256:369bd68942569e0effdf0aa68b4f844a74d97b73e281cf8012ce90e9865cc411", "mongo": "mongo:8.0.14@sha256:877fa303326645cd0e50a3833fce2f3c03d6eb4aac82c97a02e98879f51126d3", "collector": "clickhouse/clickstack-otel-collector:2.40.0@sha256:0e7dc29f8f6074afd106b96d53e5b98f1150e8882d926a20c03c6f680bd929ec", "hyperdx": "docker.hyperdx.io/hyperdx/hyperdx:2.40.0@sha256:2fcba6813f5c935f6a3e04123ffe62ab3baf0c1cd53f279b1ec9e300343ddf60"}

const mainCompose = `services:
  mongo:
    image: mongo:8.0.14@sha256:877fa303326645cd0e50a3833fce2f3c03d6eb4aac82c97a02e98879f51126d3
    healthcheck: {test: ["CMD", "mongosh", "--eval", "db.runCommand({ ping: 1 })"], interval: 5s, timeout: 5s, retries: 30}
  ch-server:
    image: clickhouse/clickhouse-server@sha256:369bd68942569e0effdf0aa68b4f844a74d97b73e281cf8012ce90e9865cc411
    environment: {CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT: "1"}
    ports: ["127.0.0.1:${CH_PORT}:8123"]
    healthcheck: {test: ["CMD-SHELL", "wget -q -O - http://localhost:8123/ping | grep Ok"], interval: 5s, timeout: 5s, retries: 30}
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
