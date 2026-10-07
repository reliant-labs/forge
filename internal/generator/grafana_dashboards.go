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
		// Exactly one trailing newline: a dashboard that lacks it is
		// rewritten by pre-commit's end-of-file-fixer, which forge's
		// verify-generated then reports as drift.
		replaced := strings.TrimRight(strings.ReplaceAll(d.content, "{{PROJECT_NAME}}", projectName), "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dashDir, d.name), []byte(replaced), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", d.name, err)
		}
	}

	return nil
}

// overviewDashboardJSON is the Grafana dashboard JSON for application overview
// metrics: request rate, error rate, latency percentiles, the per-procedure
// and per-error-code breakdowns, and the database row beneath them.
//
// METRIC SOURCE (verified against a running scaffold's OTLP export,
// 2026-10): the RED panels query the otelconnect SERVER interceptor's
// instruments — the only RPC metrics the scaffolded runtime emits.
// otelconnect v0.10 follows the current RPC semantic conventions: one
// `rpc.server.call.duration` histogram in SECONDS, with `rpc.method` (the
// full "pkg.Service/Method") and `rpc.response.status_code` ("OK", or the
// upper-case Connect code — "NOT_FOUND", "UNAUTHENTICATED" — on an error).
// The lgtm collector writes it to Prometheus as
// `rpc_server_call_duration_seconds_{bucket,sum,count}` with underscored
// labels, and stamps job=<service.name> (the generated ServiceName constant,
// i.e. the project name).
//
// That replaced v0.7's `rpc.server.duration` in milliseconds, which these
// panels queried after the bump — so every panel was empty while the app
// was exporting. TestGenerateGrafanaDashboards_ValidJSONAndRealSeries pins
// the names.
//
// The bottom row is the database pool and query timing (`go_sql_*`, from
// the instrumented sql driver), which the runtime does emit. It replaced a
// Go-runtime row (go_goroutines, go_memstats_*, go_gc_*) that nothing in the
// OTLP export produces.
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
          "expr": "sum(rate(rpc_server_call_duration_seconds_count{job=\"{{PROJECT_NAME}}\"}[5m]))",
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
          "expr": "sum(rate(rpc_server_call_duration_seconds_count{job=\"{{PROJECT_NAME}}\",rpc_response_status_code!=\"OK\"}[5m])) / sum(rate(rpc_server_call_duration_seconds_count{job=\"{{PROJECT_NAME}}\"}[5m]))",
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
          "unit": "s",
          "color": { "mode": "palette-classic" }
        },
        "overrides": []
      },
      "options": { "legend": { "displayMode": "list" }, "tooltip": { "mode": "multi" } },
      "targets": [
        {
          "expr": "histogram_quantile(0.50, sum(rate(rpc_server_call_duration_seconds_bucket{job=\"{{PROJECT_NAME}}\"}[5m])) by (le))",
          "legendFormat": "p50",
          "refId": "A"
        },
        {
          "expr": "histogram_quantile(0.95, sum(rate(rpc_server_call_duration_seconds_bucket{job=\"{{PROJECT_NAME}}\"}[5m])) by (le))",
          "legendFormat": "p95",
          "refId": "B"
        },
        {
          "expr": "histogram_quantile(0.99, sum(rate(rpc_server_call_duration_seconds_bucket{job=\"{{PROJECT_NAME}}\"}[5m])) by (le))",
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
          "expr": "sum by (rpc_method) (rate(rpc_server_call_duration_seconds_count{job=\"{{PROJECT_NAME}}\"}[5m]))",
          "legendFormat": "{{ rpc_method }}",
          "refId": "A"
        }
      ]
    },
    {
      "title": "Errors by Code",
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
          "expr": "sum by (rpc_response_status_code) (rate(rpc_server_call_duration_seconds_count{job=\"{{PROJECT_NAME}}\",rpc_response_status_code!=\"OK\"}[5m]))",
          "legendFormat": "{{ rpc_response_status_code }}",
          "refId": "A"
        }
      ]
    },
    {
      "title": "Database Queries by Operation",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 8, "x": 0, "y": 16 },
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
          "expr": "sum by (db_operation, db_sql_table) (rate(go_sql_query_timing_milliseconds_count{job=\"{{PROJECT_NAME}}\"}[5m]))",
          "legendFormat": "{{ db_operation }} {{ db_sql_table }}",
          "refId": "A"
        }
      ]
    },
    {
      "title": "Database Query Latency (p95)",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 8, "x": 8, "y": 16 },
      "datasource": { "type": "prometheus", "uid": "${datasource}" },
      "fieldConfig": {
        "defaults": {
          "unit": "ms",
          "color": { "mode": "fixed", "fixedColor": "orange" }
        },
        "overrides": []
      },
      "options": { "legend": { "displayMode": "list" }, "tooltip": { "mode": "multi" } },
      "targets": [
        {
          "expr": "histogram_quantile(0.95, sum(rate(go_sql_query_timing_milliseconds_bucket{job=\"{{PROJECT_NAME}}\"}[5m])) by (le))",
          "legendFormat": "p95",
          "refId": "A"
        }
      ]
    },
    {
      "title": "Database Connections",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 8, "x": 16, "y": 16 },
      "datasource": { "type": "prometheus", "uid": "${datasource}" },
      "fieldConfig": {
        "defaults": {
          "unit": "short",
          "color": { "mode": "palette-classic" }
        },
        "overrides": []
      },
      "options": { "legend": { "displayMode": "list" }, "tooltip": { "mode": "multi" } },
      "targets": [
        {
          "expr": "sum(go_sql_connections_in_use{job=\"{{PROJECT_NAME}}\"})",
          "legendFormat": "in use",
          "refId": "A"
        },
        {
          "expr": "sum(go_sql_connections_idle{job=\"{{PROJECT_NAME}}\"})",
          "legendFormat": "idle",
          "refId": "B"
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
