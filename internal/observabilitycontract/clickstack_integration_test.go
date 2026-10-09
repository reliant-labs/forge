//go:build integration

// Package observabilitycontract freezes the vendor contracts Forge consumes.
package observabilitycontract

import (
	"context"
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
	t                                                             *testing.T
	context                                                       context.Context
	directory, project, compose, clickhouse, hyperdx, grpc, runID string
	report                                                        *report
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
	defer s.stop()
	s.start()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	s.waitHTTP(ctx, s.clickhouse+"/?query=SELECT%201")
	s.waitHTTP(ctx, s.hyperdx+"/health")
	s.captureDigests()
	s.probeHyperDX(ctx)
	s.report.Mongo = append(s.report.Mongo, mongoResult{Image: Images["mongo"], Result: "startup_health_passed", Detail: "pinned HyperDX responds to the strict health probe with MongoDB 8.0.14; state/restart compatibility remains not_proven"})
	s.sendOTLP(ctx)
	s.assertStorage(ctx)
	s.probeOutage(ctx)
	s.writeReport()
}

func newStack(t *testing.T) *stack {
	t.Helper()
	directory := t.TempDir()
	ch, hdx, grpcPort := freePort(t), freePort(t), freePort(t)
	contents := strings.NewReplacer("${CH_PORT}", ch, "${HDX_PORT}", hdx, "${GRPC_PORT}", grpcPort).Replace(ComposeTemplate)
	compose := filepath.Join(directory, "compose.yaml")
	mustWrite(t, compose, contents)
	return &stack{t: t, context: context.Background(), directory: directory, project: fmt.Sprintf("clickstack-contract-%d", time.Now().UnixNano()), runID: fmt.Sprintf("run-%d", time.Now().UnixNano()), compose: compose, clickhouse: "http://127.0.0.1:" + ch, hyperdx: "http://127.0.0.1:" + hdx, grpc: "127.0.0.1:" + grpcPort, report: &report{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Family: "ClickStack/HyperDX " + HyperDXVersion, Images: map[string]string{}, Schemas: map[string]string{}, Assertions: map[string]result{}, Unsupported: map[string]string{"dashboard": unsupportedDashboard, "browser_replay": unsupportedReplay, "source_maps": unsupportedSourceMap}, Outage: result{Result: "not_run"}, Provenance: result{Result: "proven", Detail: "collector source hyperdxio/hyperdx@0846f3b2a9d320f83c35fdb36651a67be477f8e4; MIT; docker/otel-collector and packages/otel-collector; OTel 0.155.0/core 1.61.0; custom hyperdxexporter/structuredtextreceiver; amd64 manifest sha256:fcf8dc7e2f79a99833adfd54cafacfd7857a42d3c12039cbc57a454a70960bf7"}}}
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
	for name, image := range Images {
		output, err := exec.Command("docker", "image", "inspect", image, "--format", "{{index .RepoDigests 0}}").Output()
		if err != nil {
			s.t.Fatalf("inspect %s: %v", name, err)
		}
		runtime := strings.TrimSpace(string(output))
		if PinnedDigest(runtime) != PinnedDigest(image) {
			s.t.Fatalf("%s digest drift: got %s, want %s", name, runtime, image)
		}
		s.report.Images[name] = runtime
	}
	version, err := s.docker("exec", "hyperdx", "sh", "-c", "grep -o '\"version\": \"[^\"]*\"' /app/packages/app/node_modules/@hyperdx/api/package.json | head -1")
	if err != nil || !strings.Contains(string(version), HyperDXVersion) {
		s.t.Fatalf("HyperDX CODE_VERSION %s not found: %v %s", HyperDXVersion, err, version)
	}
	s.report.Assertions["hyperdx_code_version"] = result{Result: "proven", Detail: "runtime @hyperdx/api package identifies CODE_VERSION " + HyperDXVersion}
	logs, err := s.docker("logs", "otel-collector")
	if err != nil {
		s.t.Fatalf("collector logs: %v", err)
	}
	s.report.Provenance.Detail += "; startup_log=" + strings.TrimSpace(string(logs))
}
func (s *stack) probeHyperDX(ctx context.Context) {
	for _, path := range []string{"/health", "/api/v1/sources", "/api/v1/dashboards"} {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.hyperdx+path, nil)
		response, err := http.DefaultClient.Do(request)
		item := apiResult{Method: http.MethodGet, Path: path}
		if err != nil {
			item.Result = "transport_error: " + err.Error()
		} else {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			item.Status = response.StatusCode
			switch {
			case path == "/health" && response.StatusCode == http.StatusOK && len(body) > 0:
				item.Result = "healthy"
			case response.StatusCode == http.StatusNotFound:
				item.Result = "route_not_present"
			case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
				item.Result = "authentication_required"
			default:
				item.Result = "unsupported_status"
			}
		}
		s.report.APIs = append(s.report.APIs, item)
	}
	if s.report.APIs[0].Result != "healthy" {
		s.t.Fatalf("HyperDX health contract failed: %+v", s.report.APIs[0])
	}
}

func (s *stack) sendOTLP(ctx context.Context) {
	s.waitTCP(ctx, s.grpc)
	connection, err := grpc.NewClient(s.grpc, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		s.t.Fatal(err)
	}
	defer connection.Close()
	resource := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{attribute("service.name", "clickstack-contract-"+s.runID), attribute("deployment.environment.name", "contract"), attribute("contract.attribute", "preserved")}}
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
func (s *stack) serviceName() string { return "clickstack-contract-" + s.runID }
func (s *stack) assertStorage(ctx context.Context) {
	for _, table := range []string{"otel_traces", "otel_logs", "otel_metrics_gauge", "otel_metrics_histogram", "otel_metrics_sum"} {
		schema := s.query(ctx, "DESCRIBE TABLE default."+table+" FORMAT JSONEachRow")
		s.report.Schemas[table] = SchemaHash(schema)
		s.waitQuery(ctx, "SELECT count() FROM default."+table+" WHERE ServiceName='"+s.serviceName()+"'", table)
	}
	checks := map[string]string{
		"otel_logs":              "SELECT Body,SeverityText,TraceId,SpanId,LogAttributes,ResourceAttributes FROM default.otel_logs WHERE ServiceName='" + s.serviceName() + "' LIMIT 1 FORMAT JSONEachRow",
		"otel_traces":            "SELECT SpanName,TraceId,SpanId,StatusCode,SpanAttributes,ResourceAttributes FROM default.otel_traces WHERE ServiceName='" + s.serviceName() + "' LIMIT 1 FORMAT JSONEachRow",
		"otel_metrics_gauge":     "SELECT MetricName,Value,Attributes,ResourceAttributes FROM default.otel_metrics_gauge WHERE ServiceName='" + s.serviceName() + "' LIMIT 1 FORMAT JSONEachRow",
		"otel_metrics_sum":       "SELECT MetricName,Value,IsMonotonic,Attributes,ResourceAttributes FROM default.otel_metrics_sum WHERE ServiceName='" + s.serviceName() + "' LIMIT 1 FORMAT JSONEachRow",
		"otel_metrics_histogram": "SELECT MetricName,Count,Sum,Attributes,ResourceAttributes FROM default.otel_metrics_histogram WHERE ServiceName='" + s.serviceName() + "' LIMIT 1 FORMAT JSONEachRow",
	}
	for table, sql := range checks {
		value := s.query(ctx, sql)
		if !strings.Contains(value, "contract") || (table == "otel_logs" && (!strings.Contains(value, "error") || !strings.Contains(value, "contract-log"))) || (table == "otel_traces" && (!strings.Contains(value, "contract-span") || !strings.Contains(value, "01020304"))) {
			s.t.Fatalf("%s content contract failed: %s", table, value)
		}
		s.report.Assertions[table] = result{Result: "proven", Detail: "signal payload, attributes, and identity preserved"}
	}
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
	before := strings.TrimSpace(s.query(ctx, "SELECT count() FROM default.otel_logs WHERE ServiceName='"+s.serviceName()+"'"))
	if _, err := s.docker("stop", "ch-server"); err != nil {
		s.t.Fatal(err)
	}
	s.sendOTLP(ctx)
	if _, err := s.docker("start", "ch-server"); err != nil {
		s.t.Fatal(err)
	}
	s.waitHTTP(ctx, s.clickhouse+"/?query=SELECT%201")
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		after := strings.TrimSpace(s.query(ctx, "SELECT count() FROM default.otel_logs WHERE ServiceName='"+s.serviceName()+"'"))
		if after != before {
			s.report.Outage = result{Result: "memory_retry_delivered", Detail: "telemetry accepted while ClickHouse was stopped appeared after restart; persistent queue remains not_proven"}
			return
		}
		time.Sleep(time.Second)
	}
	s.report.Outage = result{Result: "dropped_or_not_observed", Detail: "telemetry was sent while ClickHouse was stopped and did not appear within 45s of restart; persistent queue remains not_proven"}
}
func (s *stack) writeReport() {
	data, err := json.MarshalIndent(s.report, "", "  ")
	if err != nil {
		s.t.Fatal(err)
	}
	data = append(data, '\n')
	destination := os.Getenv("CLICKSTACK_CONTRACT_REPORT")
	if destination == "" {
		destination = filepath.Join(s.directory, "clickstack-contract-report.json")
	}
	if err := os.WriteFile(destination, data, 0o644); err != nil {
		s.t.Fatal(err)
	}
	if os.Getenv("UPDATE_CLICKSTACK_CONTRACT") == "1" {
		destination = filepath.Join("..", "..", "testdata", "clickstack", "contract-2.40.0.json")
		if err := os.WriteFile(destination, data, 0o644); err != nil {
			s.t.Fatal(err)
		}
	} else {
		s.compareGolden(data)
	}
	s.t.Logf("ClickStack contract report: %s\n%s", destination, data)
}
func (s *stack) compareGolden(runtime []byte) {
	golden, err := os.ReadFile(filepath.Join("..", "..", "testdata", "clickstack", "contract-2.40.0.json"))
	if err != nil {
		s.t.Fatalf("read golden contract (set UPDATE_CLICKSTACK_CONTRACT=1 to create): %v", err)
	}
	var got, want report
	if json.Unmarshal(runtime, &got) != nil || json.Unmarshal(golden, &want) != nil {
		s.t.Fatal("invalid ClickStack contract JSON")
	}
	if got.Family != want.Family || !equalStrings(got.Images, want.Images) || !equalStrings(got.Schemas, want.Schemas) {
		s.t.Fatalf("ClickStack critical contract drift; set UPDATE_CLICKSTACK_CONTRACT=1 after review\ngot=%s\nwant=%s", runtime, golden)
	}
}
func equalStrings(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
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
