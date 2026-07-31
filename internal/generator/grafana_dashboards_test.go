package generator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerateGrafanaDashboards_ValidJSONAndRealSeries writes the dashboards
// for a sample project and asserts (a) every dashboard is valid JSON after
// the project-name substitution, and (b) the overview RED panels query the
// series the runtime actually emits — otelconnect's
// rpc_server_duration_milliseconds histogram — never the retired
// http_server_request_duration_seconds (which nothing emits; the 2026-07
// audit found the RED row permanently blank).
func TestGenerateGrafanaDashboards_ValidJSONAndRealSeries(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateGrafanaDashboards("sampleproj", dir); err != nil {
		t.Fatalf("GenerateGrafanaDashboards: %v", err)
	}

	dashDir := filepath.Join(dir, "deploy", "observability", "grafana", "dashboards")
	for _, name := range []string{"overview-dashboard.json", "logs-dashboard.json", "traces-dashboard.json"} {
		raw, err := os.ReadFile(filepath.Join(dashDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("%s is not valid JSON: %v", name, err)
		}
		if strings.Contains(string(raw), "{{PROJECT_NAME}}") {
			t.Fatalf("%s still carries the unsubstituted project-name placeholder", name)
		}
	}

	overview, err := os.ReadFile(filepath.Join(dashDir, "overview-dashboard.json"))
	if err != nil {
		t.Fatalf("read overview: %v", err)
	}
	src := string(overview)
	if strings.Contains(src, "http_server_request_duration_seconds") {
		t.Fatalf("overview dashboard queries http_server_request_duration_seconds, which the runtime does not emit:\n%s", src)
	}
	for _, want := range []string{
		// RED panels over the otelconnect server histogram, scoped to the
		// OTLP-pushed copy via job=<service.name>.
		`rpc_server_duration_milliseconds_count{job=\"sampleproj\"}`,
		`rpc_server_duration_milliseconds_bucket{job=\"sampleproj\"}`,
		// Per-procedure breakdown by the otelconnect attributes.
		"sum by (rpc_service, rpc_method)",
		// Error selection: connect errors carry an error_code; grpc/grpc_web
		// carry a status_code (0 = ok, present on success too).
		`rpc_connect_rpc_error_code=\"\"`,
		`rpc_grpc_status_code=~\"0|\"`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("overview dashboard missing %q:\n%s", want, src)
		}
	}
}
