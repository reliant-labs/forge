//go:build integration

package templates_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/clickstack"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// TestGeneratedProjectDeliversTelemetryToClickStack is the gate for "ClickStack
// on by default, zero manual steps".
//
// It scaffolds a FRESH project, renders its dev env through forge's own KCL
// evaluation (that is where the published ports are decided), brings up the
// GENERATED docker-compose.yml's clickstack service exactly as `forge env up`
// would, then plays a host process: it takes the endpoint from the same
// function `forge env up` uses (clickstack.HostOTLPEnv), sends one known
// trace, three metric families and a log over OTLP/HTTP with no credentials,
// and writes one JSON line to .forge/logs/dev/<service>.log. Every assertion is
// on an EXACT service name, trace id, metric name and value read back from
// ClickHouse. There is no registration step and no key anywhere in this test:
// that is the property under test.
//
// Run: RUN_CLICKSTACK_CONTRACT=1 go test -tags integration -run TestGeneratedProject
// -count=1 -timeout 15m ./internal/templates/
func TestGeneratedProjectDeliversTelemetryToClickStack(t *testing.T) {
	if testing.Short() || os.Getenv("RUN_CLICKSTACK_CONTRACT") != "1" {
		t.Skip("pulls ~2 GB of images and boots a database; set RUN_CLICKSTACK_CONTRACT=1 outside -short")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker unavailable: " + err.Error())
	}

	root := scaffoldForRender(t, "shop", []string{"orders"}, "web")
	w := devWorkloadJSON(t, root)
	stack, ok := w[clickstack.Service]
	if !ok {
		t.Fatalf("a fresh dev env does not run %s: %v", clickstack.Service, w)
	}
	port := func(name string) string {
		m := regexp.MustCompile(`"` + name + `":"(\d+)"`).FindStringSubmatch(stack)
		if m == nil {
			t.Fatalf("the rendered clickstack workload carries no %s:\n%s", name, stack)
		}
		return m[1]
	}
	httpPort, grpcPort, uiPort := port(clickstack.EnvOTLPHTTPPort), port(clickstack.EnvOTLPGRPCPort), port(clickstack.EnvUIPort)

	// `forge env up` creates the log dir before anything writes to it.
	logDir := filepath.Join(root, ".forge", "logs", "dev")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}

	project := fmt.Sprintf("forge-cs-gate-%d", time.Now().UnixNano())
	compose := func(args ...string) ([]byte, error) {
		cmd := exec.Command("docker", append([]string{"compose", "-p", project, "-f", filepath.Join(root, "docker-compose.yml")}, args...)...)
		cmd.Dir = root
		// Exactly what OnCompose.env hands the compose process in `forge env up`.
		cmd.Env = append(os.Environ(),
			clickstack.EnvOTLPHTTPPort+"="+httpPort,
			clickstack.EnvOTLPGRPCPort+"="+grpcPort,
			clickstack.EnvUIPort+"="+uiPort,
		)
		return cmd.CombinedOutput()
	}
	t.Cleanup(func() { _, _ = compose("down", "--volumes", "--remove-orphans") })

	start := time.Now()
	if out, err := compose("up", "-d", "--wait", "--wait-timeout", "300", clickstack.Service); err != nil {
		logs, _ := compose("logs", "--tail", "80", clickstack.Service)
		t.Fatalf("docker compose up --wait clickstack: %v\n%s\n--- logs ---\n%s", err, out, logs)
	}
	t.Logf("clickstack healthy %.0fs after `up --wait`", time.Since(start).Seconds())

	query := func(sql string) string {
		t.Helper()
		out, err := compose("exec", "-T", clickstack.Service, "sh", "-c",
			`clickhouse-client -u "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" --format TSV --query "$1"`, "sh", sql)
		if err != nil {
			t.Fatalf("clickhouse query %q: %v\n%s", sql, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// waitRows polls until the query returns want exactly: ingest is async
	// (the collector batches for 5s), and a bare "row exists" proves nothing.
	waitRows := func(what, sql, want string) {
		t.Helper()
		var got string
		for deadline := time.Now().Add(120 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			if got = query(sql); got == want {
				return
			}
		}
		t.Fatalf("%s: %s\n got  %q\n want %q", what, sql, got, want)
	}

	// ClickHouse must be new enough for contrib's profiles exporter (>= 26.2),
	// so the profiles pipeline can be added without swapping the database.
	var major, minor int
	if _, err := fmt.Sscanf(query("SELECT version()"), "%d.%d", &major, &minor); err != nil {
		t.Fatalf("parse ClickHouse version: %v", err)
	}
	if major < 26 || (major == 26 && minor < 2) {
		t.Errorf("ClickHouse %d.%d is older than the 26.2 profiles needs", major, minor)
	}

	// ── What a host process is handed ────────────────────────────────────
	env := clickstack.HostOTLPEnv(httpPort)
	endpoint := env["OTEL_EXPORTER_OTLP_ENDPOINT"]
	if env["OTEL_EXPORTER_OTLP_PROTOCOL"] != "http/protobuf" || endpoint != "http://127.0.0.1:"+httpPort {
		t.Fatalf("host OTLP env = %v", env)
	}

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	svcOTLP := "gate-otlp-" + run // traces, metrics and the OTLP log
	svcFile := "gate-file-" + run // the file-tailed log
	traceID := bytes16(0xA1)
	spanID := []byte{0xB1, 2, 3, 4, 5, 6, 7, 8}
	fileTrace := bytes16(0xC3)
	fileSpan := []byte{0xD4, 2, 3, 4, 5, 6, 7, 8}

	// ── Traces, metrics, an OTLP log: no key, no header ──────────────────
	res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		kv("service.name", svcOTLP),
		kv("deployment.environment.name", "dev"),
	}}
	now := uint64(time.Now().UnixNano())
	post(t, endpoint+"/v1/traces", &collectortrace.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: res, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			TraceId: traceID, SpanId: spanID, Name: "gate-span", Kind: tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: now, EndTimeUnixNano: now + uint64(time.Millisecond),
		}}}},
	}}})
	post(t, endpoint+"/v1/metrics", &collectormetrics.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: res, ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "gate.gauge", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{
				{TimeUnixNano: now, Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: 7}}}}}},
			{Name: "gate.sum", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				IsMonotonic: true, AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				DataPoints: []*metricspb.NumberDataPoint{{TimeUnixNano: now, Value: &metricspb.NumberDataPoint_AsInt{AsInt: 3}}}}}},
			{Name: "gate.histogram", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
				AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				DataPoints: []*metricspb.HistogramDataPoint{{TimeUnixNano: now, Count: 1, Sum: proto.Float64(2),
					ExplicitBounds: []float64{5}, BucketCounts: []uint64{1, 0}}}}}},
		}}},
	}}})
	post(t, endpoint+"/v1/logs", &collectorlogs.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: res, ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: now, SeverityText: "ERROR", TraceId: traceID, SpanId: spanID,
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "gate-otlp-log"}},
		}}}},
	}}})

	// ── The file log path: exactly what `forge env up` leaves on disk ────
	line, _ := json.Marshal(map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339Nano), "level": "WARN", "msg": "gate-file-log",
		"trace_id": hex.EncodeToString(fileTrace), "span_id": hex.EncodeToString(fileSpan), "procedure": "/shop.v1.Orders/List",
	})
	if err := os.WriteFile(filepath.Join(logDir, svcFile+".log"), append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	// ── Exact assertions ─────────────────────────────────────────────────
	waitRows("trace by exact service name and trace id",
		fmt.Sprintf(`SELECT count() FROM default.otel_traces WHERE ServiceName='%s' AND TraceId='%s' AND SpanId='%s' AND SpanName='gate-span'`,
			svcOTLP, hex.EncodeToString(traceID), hex.EncodeToString(spanID)), "1")
	waitRows("gauge metric: exact name and value",
		fmt.Sprintf(`SELECT MetricName, Value FROM default.otel_metrics_gauge WHERE ServiceName='%s'`, svcOTLP), "gate.gauge\t7")
	waitRows("sum metric: exact name and value",
		fmt.Sprintf(`SELECT MetricName, Value FROM default.otel_metrics_sum WHERE ServiceName='%s'`, svcOTLP), "gate.sum\t3")
	waitRows("histogram metric: exact name, count and sum",
		fmt.Sprintf(`SELECT MetricName, Count, Sum FROM default.otel_metrics_histogram WHERE ServiceName='%s'`, svcOTLP), "gate.histogram\t1\t2")
	waitRows("OTLP log: exact service, body and trace link",
		fmt.Sprintf(`SELECT Body, TraceId, SpanId FROM default.otel_logs WHERE ServiceName='%s'`, svcOTLP),
		"gate-otlp-log\t"+hex.EncodeToString(traceID)+"\t"+hex.EncodeToString(spanID))
	waitRows("file log: service name is the FILE name; body, severity, trace link and env come from the JSON line",
		fmt.Sprintf(`SELECT ServiceName, Body, SeverityText, TraceId, SpanId, ResourceAttributes['deployment.environment.name'] FROM default.otel_logs WHERE ServiceName='%s'`, svcFile),
		strings.Join([]string{svcFile, "gate-file-log", "warn", hex.EncodeToString(fileTrace), hex.EncodeToString(fileSpan), "dev"}, "\t"))

	// ── Pre-created sources and the dashboard provisioner ────────────────
	apiGet := func(path string) string {
		out, err := compose("exec", "-T", clickstack.Service, "curl", "-fsS", "http://localhost:8000"+path)
		if err != nil {
			t.Fatalf("GET %s inside the container: %v\n%s", path, err, out)
		}
		return string(out)
	}
	sources := apiGet("/sources")
	for _, name := range []string{`"name":"Logs"`, `"name":"Traces"`, `"name":"Metrics"`} {
		if !strings.Contains(sources, name) {
			t.Errorf("HyperDX has no pre-created source %s:\n%s", name, sources)
		}
	}
	dash := `{"name":"forge-gate-` + run + `","tiles":[],"tags":["forge"]}`
	if err := os.WriteFile(filepath.Join(root, clickstack.DashboardsDir, "gate.json"), []byte(dash), 0o644); err != nil {
		t.Fatal(err)
	}
	var dashboards string
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		if dashboards = apiGet("/dashboards"); strings.Contains(dashboards, `"name":"forge-gate-`+run+`"`) {
			break
		}
	}
	if !strings.Contains(dashboards, `"name":"forge-gate-`+run+`"`) || !strings.Contains(dashboards, `"provisioned":true`) {
		t.Errorf("a dashboard dropped into %s was not provisioned:\n%s", clickstack.DashboardsDir, dashboards)
	}

	// ── What #603 got wrong ──────────────────────────────────────────────
	// ClickHouse is not published, and its passwordless `default` user is gone.
	if out, err := compose("port", clickstack.Service, "8123"); err == nil && strings.TrimSpace(string(out)) != "" && !strings.HasPrefix(strings.TrimSpace(string(out)), ":0") {
		t.Errorf("ClickHouse 8123 is published to the host: %s", out)
	}
	// clickhouse-client inside the container reads CLICKHOUSE_USER/PASSWORD from
	// the environment, so the credential-free probe must be an explicit request
	// that names no user: the passwordless `default` user must be gone.
	if out, err := compose("exec", "-T", clickstack.Service, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"http://127.0.0.1:8123/?query=SELECT%201"); err != nil || strings.TrimSpace(string(out)) != "401" {
		t.Errorf("an anonymous ClickHouse request returned %q (err %v), want 401", out, err)
	}
	if out, err := compose("exec", "-T", clickstack.Service, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"http://127.0.0.1:8123/?user=default&query=SELECT%201"); err != nil || strings.TrimSpace(string(out)) != "401" {
		t.Errorf("ClickHouse's passwordless default user still works: %q (err %v)", out, err)
	}
	// Every published port is on loopback.
	ps, err := compose("ps", "--format", "json", clickstack.Service)
	if err != nil {
		t.Fatalf("compose ps: %v\n%s", err, ps)
	}
	published := 0
	for _, l := range bytes.Split(bytes.TrimSpace(ps), []byte("\n")) {
		var c struct {
			Publishers []struct {
				URL           string
				PublishedPort int
			}
		}
		if err := json.Unmarshal(l, &c); err != nil {
			t.Fatalf("parse compose ps: %v\n%s", err, l)
		}
		for _, p := range c.Publishers {
			if p.PublishedPort == 0 {
				continue
			}
			published++
			if p.URL != "127.0.0.1" {
				t.Errorf("port %d is published on %q, want 127.0.0.1", p.PublishedPort, p.URL)
			}
		}
	}
	if published < 3 {
		t.Errorf("expected the UI and both OTLP ports to be published, saw %d", published)
	}

	// Footprint, for the PR: the whole stack is this one container.
	if out, err := exec.Command("docker", "stats", "--no-stream", "--format", "{{.MemUsage}}",
		strings.TrimSpace(containerID(t, compose))).CombinedOutput(); err == nil {
		t.Logf("clickstack resident memory (docker stats): %s", strings.TrimSpace(string(out)))
	}
}

func containerID(t *testing.T, compose func(...string) ([]byte, error)) string {
	t.Helper()
	out, err := compose("ps", "-q", clickstack.Service)
	if err != nil {
		t.Fatalf("compose ps -q: %v\n%s", err, out)
	}
	return string(out)
}

func bytes16(first byte) []byte {
	b := make([]byte, 16)
	b[0] = first
	for i := 1; i < 16; i++ {
		b[i] = byte(i)
	}
	return b
}

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// post sends one OTLP/HTTP protobuf request with NO credentials and requires
// the collector to accept it: the property #603 lacked.
func post(t *testing.T, url string, msg proto.Message) {
	t.Helper()
	body, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var last error
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return
		}
		last = fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			t.Fatalf("POST %s needs credentials (%v): host processes cannot send any", url, last)
		}
	}
	t.Fatalf("POST %s never accepted: %v", url, last)
}
