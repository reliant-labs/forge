package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 5 * time.Second}

// clickhouseAddr returns a direct ClickHouse HTTP endpoint. The official
// ClickStack collector writes the schemas queried below, so status does not
// rely on an unversioned HyperDX query API.
func clickhouseAddr(env *Environment) (string, *CheckResult) {
	addr, ok := env.GetPort("clickhouse", 8123)
	if !ok {
		return "", &CheckResult{Status: StatusUnknown, Message: "ClickHouse HTTP port not published by the compose stack — could not query this signal"}
	}
	return addr, nil
}

func clickhouseQuery(ctx context.Context, addr, query string) ([]byte, error) {
	return doGet(ctx, "http://"+addr+"/?query="+url.QueryEscape(query+" FORMAT JSON"))
}

func clickhouseCount(ctx context.Context, env *Environment, table, service string) (int, []byte, *CheckResult) {
	addr, result := clickhouseAddr(env)
	if result != nil {
		return 0, nil, result
	}
	body, err := clickhouseQuery(ctx, addr, fmt.Sprintf("SELECT count() AS count FROM default.%s WHERE ServiceName = %q", table, service))
	if err != nil {
		return 0, body, &CheckResult{Status: StatusFail, Message: "ClickHouse query failed: " + err.Error(), Evidence: string(body)}
	}
	var response struct {
		Data []struct {
			Count int `json:"count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil || len(response.Data) != 1 {
		message := "failed to parse ClickHouse response"
		if err != nil {
			message += ": " + err.Error()
		}
		return 0, body, &CheckResult{Status: StatusUnknown, Message: message, Evidence: string(body)}
	}
	return response.Data[0].Count, body, nil
}

// doGet performs a GET request and returns the body bytes.
func doGet(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// CheckPrometheus retains its public name for callers, but now verifies that
// the official ClickStack collector wrote this app's metric points to
// ClickHouse. It deliberately queries storage directly instead of inventing a
// dependency on HyperDX's unversioned search API.
func CheckPrometheus(ctx context.Context, env *Environment) CheckResult {
	count, _, result := clickhouseCount(ctx, env, "otel_metrics_sum", env.ProjectName)
	if result != nil {
		return *result
	}
	if count == 0 {
		return CheckResult{Status: StatusWarn, Message: "ClickStack has no metric points from " + env.ProjectName + " yet"}
	}
	return CheckResult{Status: StatusPass, Message: fmt.Sprintf("%d ClickStack metric point(s) from %s", count, env.ProjectName)}
}

// prometheusAppVerdict turns "which metric names carry job=<project>" into
// the check's result. Split from the HTTP so the verdict is testable alone.
func prometheusAppVerdict(project string, upTargets int, series []promSample) CheckResult {
	if len(series) == 0 {
		return CheckResult{
			Status: StatusWarn,
			Message: fmt.Sprintf("Prometheus is up (%d scrape target(s)) but holds no metrics from %s — nothing is exporting to it; "+
				"check the app's OTEL_EXPORTER_OTLP_ENDPOINT (metrics are pushed about once a minute)", upTargets, project),
		}
	}
	names := make([]string, 0, len(series))
	for _, s := range series {
		if n, _ := s.Metric["__name__"].(string); n != "" {
			names = append(names, n)
		}
	}
	// The RPC histogram first: it is the series that says requests are
	// being measured, rather than that a connection pool exists.
	sort.Slice(names, func(i, j int) bool {
		ri, rj := strings.HasPrefix(names[i], "rpc_"), strings.HasPrefix(names[j], "rpc_")
		if ri != rj {
			return ri
		}
		return names[i] < names[j]
	})
	shown := names
	if len(shown) > 3 {
		shown = shown[:3]
	}
	return CheckResult{
		Status:  StatusPass,
		Message: fmt.Sprintf("%d metric(s) from %s (%s)", len(series), project, strings.Join(shown, ", ")),
	}
}

// promSample is one element of a Prometheus instant-vector result.
type promSample struct {
	Metric map[string]interface{} `json:"metric"`
	Value  []interface{}          `json:"value"`
}

// parsePromVector decodes a Prometheus instant-query response.
func parsePromVector(body []byte) ([]promSample, error) {
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []promSample `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("status %q", resp.Status)
	}
	return resp.Data.Result, nil
}

// CheckTempo retains its public name and verifies ClickStack trace ingestion.
func CheckTempo(ctx context.Context, env *Environment) CheckResult {
	count, _, result := clickhouseCount(ctx, env, "otel_traces", env.ProjectName)
	if result != nil {
		return *result
	}
	if count == 0 {
		return CheckResult{Status: StatusWarn, Message: "ClickStack has no traces from " + env.ProjectName + " yet"}
	}
	return CheckResult{Status: StatusPass, Message: fmt.Sprintf("%d ClickStack trace(s) from %s", count, env.ProjectName)}
}

// CheckLoki retains its public name and verifies ClickStack log ingestion.
func CheckLoki(ctx context.Context, env *Environment) CheckResult {
	count, _, result := clickhouseCount(ctx, env, "otel_logs", env.ProjectName)
	if result != nil {
		return *result
	}
	if count == 0 {
		return CheckResult{Status: StatusWarn, Message: "ClickStack has no logs from " + env.ProjectName + " yet"}
	}
	return CheckResult{Status: StatusPass, Message: fmt.Sprintf("%d ClickStack log record(s) from %s", count, env.ProjectName)}
}

// CheckPyroscope verifies continuous profiling is working.
func CheckPyroscope(ctx context.Context, env *Environment) CheckResult {
	// Pyroscope remains separate from ClickStack. Query it inside its compose
	// network because local dev does not publish a profile endpoint to the host.
	readyOut, err := env.compose(ctx, "exec", "-w", "/", "pyroscope", "wget", "-qO-", "http://localhost:4040/ready")
	if err != nil {
		return CheckResult{Status: StatusFail, Message: "Pyroscope not reachable", Evidence: strings.TrimSpace(string(readyOut) + "\n" + err.Error())}
	}
	labelsOut, err := env.compose(ctx, "exec", "-w", "/", "pyroscope", "wget", "-qO-", "http://localhost:4040/querier.v1.QuerierService/LabelValues", "--header=Content-Type: application/json", "--post-data={\"name\":\"__service_name__\"}")
	if err != nil {
		return CheckResult{Status: StatusWarn, Message: "Pyroscope is healthy but could not query labels", Evidence: string(labelsOut)}
	}
	return parsePyroscopeLabels(labelsOut, env.ProjectName)
}

// parsePyroscopeProfileTypes checks the Grafana proxy response for profile types.
func parsePyroscopeProfileTypes(body []byte, projectName string) CheckResult {
	// The response is an array of profile type objects.
	var profileTypes []map[string]interface{}
	if err := json.Unmarshal(body, &profileTypes); err != nil {
		// Try as a wrapper object.
		var wrapper struct {
			ProfileTypes []map[string]interface{} `json:"profileTypes"`
		}
		if err2 := json.Unmarshal(body, &wrapper); err2 != nil {
			return CheckResult{Status: StatusFail, Message: "failed to parse Pyroscope response", Evidence: string(body)}
		}
		profileTypes = wrapper.ProfileTypes
	}

	if len(profileTypes) == 0 {
		return CheckResult{
			Status:  StatusWarn,
			Message: "Pyroscope is healthy but no profiles ingested yet",
		}
	}

	return CheckResult{
		Status:  StatusPass,
		Message: fmt.Sprintf("%d profile types available", len(profileTypes)),
	}
}

// parsePyroscopeLabels checks the label-values response for our service.
func parsePyroscopeLabels(body []byte, projectName string) CheckResult {
	body = bytes.TrimSpace(body)

	// Could be a plain array, or wrapped as {"values":[...]} or {"names":[...]} (gRPC-web).
	var labels []string
	if err := json.Unmarshal(body, &labels); err != nil {
		var wrapper struct {
			Values []string `json:"values"`
			Names  []string `json:"names"`
		}
		if err2 := json.Unmarshal(body, &wrapper); err2 != nil {
			// The label response is this check's only evidence about
			// ingestion, so a body in none of the shapes we can decode
			// leaves the question unanswered — Pyroscope may well be
			// receiving profiles. UNDETERMINED, for the same reason
			// grafanaAddr above is: forge could not obtain the fact, which
			// is not a finding about the stack (see the StatusSkip vs
			// StatusUnknown note in doctor.go). Contrast the branch below,
			// where the labels DID decode and are genuinely empty — that
			// is a real "nothing ingested yet" warning.
			return CheckResult{
				Status:   StatusUnknown,
				Message:  "Pyroscope is healthy but its label response could not be parsed — ingestion unknown",
				Evidence: err2.Error(),
			}
		}
		labels = wrapper.Values
		if len(labels) == 0 {
			labels = wrapper.Names
		}
	}

	if len(labels) == 0 {
		return CheckResult{
			Status:  StatusWarn,
			Message: "Pyroscope is healthy but no profiles ingested yet",
		}
	}

	// Check if our service is among the labels.
	found := false
	for _, l := range labels {
		if strings.Contains(l, projectName) {
			found = true
			break
		}
	}

	if found {
		return CheckResult{
			Status:  StatusPass,
			Message: fmt.Sprintf("profiles found for %s", projectName),
		}
	}

	return CheckResult{
		Status:  StatusWarn,
		Message: fmt.Sprintf("Pyroscope has %d services but none match %s", len(labels), projectName),
	}
}
