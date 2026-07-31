package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/templates"
)

// GenerateGrafanaDashboards is the standalone entry point callable from both
// `forge project new` (via ProjectGenerator) and `forge generate`. It writes Grafana
// dashboards and provisioning config into deploy/observability/grafana/.
func GenerateGrafanaDashboards(projectName, projectDir string) error {
	dashDir := filepath.Join(projectDir, "deploy", "observability", "grafana", "dashboards")
	provDir := filepath.Join(projectDir, "deploy", "observability", "grafana", "provisioning")
	for _, d := range []string{dashDir, provDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create grafana dir: %w", err)
		}
	}

	// Write provisioning config (static, no templating needed).
	provContent, err := templates.ProjectTemplates().Get("grafana/dashboards.yaml")
	if err != nil {
		return fmt.Errorf("read grafana provisioning template: %w", err)
	}
	if err := os.WriteFile(filepath.Join(provDir, "dashboards.yaml"), provContent, 0o644); err != nil {
		return fmt.Errorf("write dashboards.yaml: %w", err)
	}

	// Write each dashboard JSON, replacing the placeholder with the project name.
	dashboards := []struct {
		name    string
		content string
	}{
		{"overview-dashboard.json", overviewDashboardJSON},
		{"logs-dashboard.json", logsDashboardJSON},
		{"traces-dashboard.json", tracesDashboardJSON},
	}
	for _, d := range dashboards {
		replaced := strings.ReplaceAll(d.content, "{{PROJECT_NAME}}", projectName)
		if err := os.WriteFile(filepath.Join(dashDir, d.name), []byte(replaced), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", d.name, err)
		}
	}

	return nil
}

// overviewDashboardJSON is the Grafana dashboard JSON for application overview
// metrics: request rate, error rate, latency percentiles, per-procedure
// breakdown, and Go runtime stats.
//
// METRIC SOURCE (verified against the runtime, 2026-07): the RED panels
// query the otelconnect SERVER interceptor's instruments — the only RPC
// metrics the scaffolded runtime emits. cmd-tree-serve wires
// otelconnect.NewInterceptor() into the chain (observe.Chain receives no
// Meter, so observe.MetricsInterceptor is a pass-through), and otelconnect
// v0.7.x emits `rpc.server.duration` as an Int64Histogram in MILLISECONDS
// with attributes rpc.system ("connect_rpc" | "grpc" | "grpc_web"),
// rpc.service, rpc.method, and a per-protocol status attribute:
//   - connect_rpc: rpc.connect_rpc.error_code (string, ERRORS ONLY;
//     successes carry no code attribute)
//   - grpc / grpc_web: rpc.grpc.status_code / rpc.grpc_web.status_code
//     (int, ALWAYS present; 0 = success)
//
// Both ingestion paths (OTLP push -> lgtm collector -> Prometheus, and the
// /metrics Prometheus-exporter scrape) translate that to
// `rpc_server_duration_milliseconds_{bucket,sum,count}` with underscored
// labels; the OTLP path stamps job=<service.name> (the generated
// ServiceName constant = the project name). The previous queries hit
// `http_server_request_duration_seconds`, which nothing emits.
var overviewDashboardJSON = `{
  "uid": "forge-overview",
  "title": "Forge — Application Overview",
  "tags": ["forge", "auto-generated"],
  "editable": true,
  "schemaVersion": 39,
  "timezone": "browser",
  "refresh": "30s",
  "time": { "from": "now-1h", "to": "now" },
  "fiscalYearStartMonth": 0,
  "templating": {
    "list": [
      {
        "name": "datasource",
        "type": "datasource",
        "query": "prometheus",
        "current": { "selected": true, "text": "default", "value": "default" },
        "hide": 0,
        "includeAll": false,
        "multi": false,
        "refresh": 1
      }
    ]
  },
  "panels": [
    {
      "title": "Request Rate",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 8, "x": 0, "y": 0 },
      "datasource": { "type": "prometheus", "uid": "${datasource}" },
      "fieldConfig": {
        "defaults": {
          "unit": "reqps",
          "color": { "mode": "palette-classic" }
        },
        "overrides": []
      },
      "options": { "legend": { "displayMode": "list" }, "tooltip": { "mode": "multi" } },
      "targets": [
        {
          "expr": "sum(rate(rpc_server_duration_milliseconds_count{job=\"{{PROJECT_NAME}}\"}[5m]))",
          "legendFormat": "req/s",
          "refId": "A"
        }
      ]
    },
    {
      "title": "Error Rate",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 8, "x": 8, "y": 0 },
      "datasource": { "type": "prometheus", "uid": "${datasource}" },
      "fieldConfig": {
        "defaults": {
          "unit": "percentunit",
          "color": { "mode": "fixed", "fixedColor": "red" },
          "min": 0,
          "max": 1
        },
        "overrides": []
      },
      "options": { "legend": { "displayMode": "list" }, "tooltip": { "mode": "multi" } },
      "targets": [
        {
          "expr": "1 - (sum(rate(rpc_server_duration_milliseconds_count{job=\"{{PROJECT_NAME}}\",rpc_connect_rpc_error_code=\"\",rpc_grpc_status_code=~\"0|\",rpc_grpc_web_status_code=~\"0|\"}[5m])) / sum(rate(rpc_server_duration_milliseconds_count{job=\"{{PROJECT_NAME}}\"}[5m])))",
          "legendFormat": "error %",
          "refId": "A"
        }
      ]
    },
    {
      "title": "Latency Percentiles",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 8, "x": 16, "y": 0 },
      "datasource": { "type": "prometheus", "uid": "${datasource}" },
      "fieldConfig": {
        "defaults": {
          "unit": "ms",
          "color": { "mode": "palette-classic" }
        },
        "overrides": []
      },
      "options": { "legend": { "displayMode": "list" }, "tooltip": { "mode": "multi" } },
      "targets": [
        {
          "expr": "histogram_quantile(0.50, sum(rate(rpc_server_duration_milliseconds_bucket{job=\"{{PROJECT_NAME}}\"}[5m])) by (le))",
          "legendFormat": "p50",
          "refId": "A"
        },
        {
          "expr": "histogram_quantile(0.95, sum(rate(rpc_server_duration_milliseconds_bucket{job=\"{{PROJECT_NAME}}\"}[5m])) by (le))",
          "legendFormat": "p95",
          "refId": "B"
        },
        {
          "expr": "histogram_quantile(0.99, sum(rate(rpc_server_duration_milliseconds_bucket{job=\"{{PROJECT_NAME}}\"}[5m])) by (le))",
          "legendFormat": "p99",
          "refId": "C"
        }
      ]
    },
    {
      "title": "Requests by Procedure",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 8 },
      "datasource": { "type": "prometheus", "uid": "${datasource}" },
      "fieldConfig": {
        "defaults": {
          "unit": "reqps",
          "color": { "mode": "palette-classic" }
        },
        "overrides": []
      },
      "options": { "legend": { "displayMode": "table", "placement": "right" }, "tooltip": { "mode": "multi" } },
      "targets": [
        {
          "expr": "sum by (rpc_service, rpc_method) (rate(rpc_server_duration_milliseconds_count{job=\"{{PROJECT_NAME}}\"}[5m]))",
          "legendFormat": "{{ rpc_service }}/{{ rpc_method }}",
          "refId": "A"
        }
      ]
    },
    {
      "title": "Top Errors by Code",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 8 },
      "datasource": { "type": "prometheus", "uid": "${datasource}" },
      "fieldConfig": {
        "defaults": {
          "unit": "reqps",
          "color": { "mode": "palette-classic" }
        },
        "overrides": []
      },
      "options": { "legend": { "displayMode": "table", "placement": "right" }, "tooltip": { "mode": "multi" } },
      "targets": [
        {
          "expr": "sum by (rpc_connect_rpc_error_code) (rate(rpc_server_duration_milliseconds_count{job=\"{{PROJECT_NAME}}\",rpc_connect_rpc_error_code!=\"\"}[5m]))",
          "legendFormat": "connect {{ rpc_connect_rpc_error_code }}",
          "refId": "A"
        },
        {
          "expr": "sum by (rpc_grpc_status_code) (rate(rpc_server_duration_milliseconds_count{job=\"{{PROJECT_NAME}}\",rpc_grpc_status_code!~\"0|\"}[5m]))",
          "legendFormat": "grpc {{ rpc_grpc_status_code }}",
          "refId": "B"
        },
        {
          "expr": "sum by (rpc_grpc_web_status_code) (rate(rpc_server_duration_milliseconds_count{job=\"{{PROJECT_NAME}}\",rpc_grpc_web_status_code!~\"0|\"}[5m]))",
          "legendFormat": "grpc-web {{ rpc_grpc_web_status_code }}",
          "refId": "C"
        }
      ]
    },
    {
      "title": "Goroutines",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 8, "x": 0, "y": 16 },
      "datasource": { "type": "prometheus", "uid": "${datasource}" },
      "fieldConfig": {
        "defaults": {
          "unit": "short",
          "color": { "mode": "fixed", "fixedColor": "blue" }
        },
        "overrides": []
      },
      "options": { "legend": { "displayMode": "list" }, "tooltip": { "mode": "single" } },
      "targets": [
        {
          "expr": "go_goroutines{job=\"{{PROJECT_NAME}}\"}",
          "legendFormat": "goroutines",
          "refId": "A"
        }
      ]
    },
    {
      "title": "Heap Usage",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 8, "x": 8, "y": 16 },
      "datasource": { "type": "prometheus", "uid": "${datasource}" },
      "fieldConfig": {
        "defaults": {
          "unit": "bytes",
          "color": { "mode": "fixed", "fixedColor": "orange" }
        },
        "overrides": []
      },
      "options": { "legend": { "displayMode": "list" }, "tooltip": { "mode": "single" } },
      "targets": [
        {
          "expr": "go_memstats_heap_inuse_bytes{job=\"{{PROJECT_NAME}}\"}",
          "legendFormat": "heap in-use",
          "refId": "A"
        },
        {
          "expr": "go_memstats_heap_alloc_bytes{job=\"{{PROJECT_NAME}}\"}",
          "legendFormat": "heap alloc",
          "refId": "B"
        }
      ]
    },
    {
      "title": "GC Pause Duration",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 8, "x": 16, "y": 16 },
      "datasource": { "type": "prometheus", "uid": "${datasource}" },
      "fieldConfig": {
        "defaults": {
          "unit": "s",
          "color": { "mode": "fixed", "fixedColor": "purple" }
        },
        "overrides": []
      },
      "options": { "legend": { "displayMode": "list" }, "tooltip": { "mode": "single" } },
      "targets": [
        {
          "expr": "rate(go_gc_duration_seconds_sum{job=\"{{PROJECT_NAME}}\"}[5m])",
          "legendFormat": "gc pause rate",
          "refId": "A"
        }
      ]
    }
  ]
}`

// logsDashboardJSON is the Grafana dashboard JSON for log exploration:
// log volume by level, error log table, and audit event table.
var logsDashboardJSON = `{
  "uid": "forge-logs",
  "title": "Forge — Logs",
  "tags": ["forge", "auto-generated"],
  "editable": true,
  "schemaVersion": 39,
  "timezone": "browser",
  "refresh": "30s",
  "time": { "from": "now-1h", "to": "now" },
  "fiscalYearStartMonth": 0,
  "templating": {
    "list": [
      {
        "name": "loki_ds",
        "type": "datasource",
        "query": "loki",
        "current": { "selected": true, "text": "default", "value": "default" },
        "hide": 0,
        "includeAll": false,
        "multi": false,
        "refresh": 1
      }
    ]
  },
  "panels": [
    {
      "title": "Log Volume by Level",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 24, "x": 0, "y": 0 },
      "datasource": { "type": "loki", "uid": "${loki_ds}" },
      "fieldConfig": {
        "defaults": {
          "unit": "short",
          "color": { "mode": "palette-classic" }
        },
        "overrides": [
          { "matcher": { "id": "byName", "options": "error" }, "properties": [{ "id": "color", "value": { "mode": "fixed", "fixedColor": "red" } }] },
          { "matcher": { "id": "byName", "options": "warn" }, "properties": [{ "id": "color", "value": { "mode": "fixed", "fixedColor": "yellow" } }] },
          { "matcher": { "id": "byName", "options": "info" }, "properties": [{ "id": "color", "value": { "mode": "fixed", "fixedColor": "green" } }] }
        ]
      },
      "options": {
        "legend": { "displayMode": "list" },
        "tooltip": { "mode": "multi" },
        "drawStyle": "bars",
        "stacking": { "mode": "normal" }
      },
      "targets": [
        {
          "expr": "sum by (level) (count_over_time({service_name=\"{{PROJECT_NAME}}\"} | json [1m]))",
          "legendFormat": "{{ level }}",
          "refId": "A"
        }
      ]
    },
    {
      "title": "Error Logs",
      "type": "table",
      "gridPos": { "h": 10, "w": 24, "x": 0, "y": 8 },
      "datasource": { "type": "loki", "uid": "${loki_ds}" },
      "options": {
        "showHeader": true,
        "sortBy": [{ "displayName": "Time", "desc": true }]
      },
      "targets": [
        {
          "expr": "{service_name=\"{{PROJECT_NAME}}\"} | json | level = \"error\"",
          "refId": "A"
        }
      ],
      "transformations": [
        {
          "id": "extractFields",
          "options": { "source": "Line" }
        }
      ]
    },
    {
      "title": "Audit Events",
      "type": "table",
      "gridPos": { "h": 10, "w": 24, "x": 0, "y": 18 },
      "datasource": { "type": "loki", "uid": "${loki_ds}" },
      "options": {
        "showHeader": true,
        "sortBy": [{ "displayName": "Time", "desc": true }]
      },
      "targets": [
        {
          "expr": "{service_name=\"{{PROJECT_NAME}}\"} | json | log_type = \"audit\"",
          "refId": "A"
        }
      ],
      "transformations": [
        {
          "id": "extractFields",
          "options": { "source": "Line" }
        }
      ]
    }
  ]
}`

// tracesDashboardJSON is the Grafana dashboard JSON for trace exploration:
// trace count, average duration, and a table of slow traces.
var tracesDashboardJSON = `{
  "uid": "forge-traces",
  "title": "Forge — Traces",
  "tags": ["forge", "auto-generated"],
  "editable": true,
  "schemaVersion": 39,
  "timezone": "browser",
  "refresh": "30s",
  "time": { "from": "now-1h", "to": "now" },
  "fiscalYearStartMonth": 0,
  "templating": {
    "list": [
      {
        "name": "tempo_ds",
        "type": "datasource",
        "query": "tempo",
        "current": { "selected": true, "text": "default", "value": "default" },
        "hide": 0,
        "includeAll": false,
        "multi": false,
        "refresh": 1
      },
      {
        "name": "datasource",
        "type": "datasource",
        "query": "prometheus",
        "current": { "selected": true, "text": "default", "value": "default" },
        "hide": 0,
        "includeAll": false,
        "multi": false,
        "refresh": 1
      }
    ]
  },
  "panels": [
    {
      "title": "Trace Count (from spans)",
      "type": "stat",
      "gridPos": { "h": 6, "w": 8, "x": 0, "y": 0 },
      "datasource": { "type": "prometheus", "uid": "${datasource}" },
      "fieldConfig": {
        "defaults": {
          "unit": "short",
          "color": { "mode": "thresholds" },
          "thresholds": { "steps": [{ "color": "green", "value": null }] }
        },
        "overrides": []
      },
      "options": { "graphMode": "area", "textMode": "auto" },
      "targets": [
        {
          "expr": "sum(rate(traces_spanmetrics_calls_total{service=\"{{PROJECT_NAME}}\"}[5m]))",
          "legendFormat": "spans/s",
          "refId": "A"
        }
      ]
    },
    {
      "title": "Average Span Duration",
      "type": "stat",
      "gridPos": { "h": 6, "w": 8, "x": 8, "y": 0 },
      "datasource": { "type": "prometheus", "uid": "${datasource}" },
      "fieldConfig": {
        "defaults": {
          "unit": "s",
          "color": { "mode": "thresholds" },
          "thresholds": { "steps": [{ "color": "green", "value": null }, { "color": "yellow", "value": 0.1 }, { "color": "red", "value": 0.5 }] }
        },
        "overrides": []
      },
      "options": { "graphMode": "area", "textMode": "auto" },
      "targets": [
        {
          "expr": "sum(rate(traces_spanmetrics_latency_sum{service=\"{{PROJECT_NAME}}\"}[5m])) / sum(rate(traces_spanmetrics_latency_count{service=\"{{PROJECT_NAME}}\"}[5m]))",
          "legendFormat": "avg duration",
          "refId": "A"
        }
      ]
    },
    {
      "title": "Span Rate by Operation",
      "type": "timeseries",
      "gridPos": { "h": 6, "w": 8, "x": 16, "y": 0 },
      "datasource": { "type": "prometheus", "uid": "${datasource}" },
      "fieldConfig": {
        "defaults": {
          "unit": "ops",
          "color": { "mode": "palette-classic" }
        },
        "overrides": []
      },
      "options": { "legend": { "displayMode": "table", "placement": "right" }, "tooltip": { "mode": "multi" } },
      "targets": [
        {
          "expr": "sum by (span_name) (rate(traces_spanmetrics_calls_total{service=\"{{PROJECT_NAME}}\"}[5m]))",
          "legendFormat": "{{ span_name }}",
          "refId": "A"
        }
      ]
    },
    {
      "title": "Recent Traces",
      "type": "table",
      "gridPos": { "h": 12, "w": 24, "x": 0, "y": 6 },
      "datasource": { "type": "tempo", "uid": "${tempo_ds}" },
      "options": {
        "showHeader": true,
        "sortBy": [{ "displayName": "Duration", "desc": true }]
      },
      "targets": [
        {
          "queryType": "traceqlSearch",
          "filters": [
            { "id": "service-name", "tag": "service.name", "operator": "=", "value": ["{{PROJECT_NAME}}"], "scope": "resource" },
            { "id": "min-duration", "tag": "duration", "operator": ">", "value": ["100ms"] }
          ],
          "limit": 20,
          "refId": "A"
        }
      ]
    },
    {
      "title": "Error Spans",
      "type": "table",
      "gridPos": { "h": 10, "w": 24, "x": 0, "y": 18 },
      "datasource": { "type": "tempo", "uid": "${tempo_ds}" },
      "options": {
        "showHeader": true,
        "sortBy": [{ "displayName": "Duration", "desc": true }]
      },
      "targets": [
        {
          "queryType": "traceqlSearch",
          "filters": [
            { "id": "service-name", "tag": "service.name", "operator": "=", "value": ["{{PROJECT_NAME}}"], "scope": "resource" },
            { "id": "status", "tag": "status", "operator": "=", "value": ["error"], "scope": "intrinsic" }
          ],
          "limit": 20,
          "refId": "A"
        }
      ]
    }
  ]
}`
