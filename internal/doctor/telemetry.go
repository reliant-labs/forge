package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 5 * time.Second}

// grafanaAddr returns the Grafana host address, or an UNDETERMINED result
// when the compose check could not publish one. Undetermined, not skip: the
// telemetry backends ship in the bundled lgtm container, so a missing port
// means forge could not look — not that the question does not apply.
func grafanaAddr(env *Environment) (string, *CheckResult) {
	addr, ok := env.GetPort("lgtm", 3000)
	if !ok {
		return "", &CheckResult{
			Status:  StatusUnknown,
			Message: "Grafana port not published by the compose stack — could not query this signal",
		}
	}
	return addr, nil
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

// CheckPrometheus verifies Prometheus is reachable AND holds this app's
// metrics.
//
// Reachable is not the question. The bundled lgtm image scrapes its own
// collector, so `up` always has a target — the check used to report
// "✓ 1 targets up" on a stack where the app exported nothing at all, which is
// a green on exactly the property it exists to verify. It passes only when
// Prometheus has series stamped with the app's service name (job=<project>,
// what the collector derives from the OTLP resource's service.name — the
// same name the Tempo check searches for).
func CheckPrometheus(ctx context.Context, env *Environment) CheckResult {
	addr, skip := grafanaAddr(env)
	if skip != nil {
		return *skip
	}

	base := "http://" + addr + "/api/datasources/proxy/uid/prometheus/api/v1/query"

	upBody, err := doGet(ctx, base+"?query=up")
	if err != nil {
		return CheckResult{Status: StatusFail, Message: "Prometheus query failed: " + err.Error(), Evidence: string(upBody)}
	}
	up, err := parsePromVector(upBody)
	if err != nil {
		return CheckResult{Status: StatusFail, Message: "failed to parse Prometheus response: " + err.Error(), Evidence: string(upBody)}
	}
	if len(up) == 0 {
		return CheckResult{Status: StatusFail, Message: "no targets reporting up"}
	}

	appQuery := fmt.Sprintf(`count by (__name__) ({job=%q})`, env.ProjectName)
	appBody, err := doGet(ctx, base+"?query="+url.QueryEscape(appQuery))
	if err != nil {
		return CheckResult{Status: StatusUnknown, Message: "Prometheus is up, but the query for this app's metrics failed: " + err.Error(), Evidence: string(appBody)}
	}
	series, err := parsePromVector(appBody)
	if err != nil {
		return CheckResult{Status: StatusUnknown, Message: "Prometheus is up, but its answer about this app's metrics could not be read: " + err.Error(), Evidence: string(appBody)}
	}
	return prometheusAppVerdict(env.ProjectName, len(up), series)
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

// CheckTempo verifies traces are being ingested into Tempo.
func CheckTempo(ctx context.Context, env *Environment) CheckResult {
	addr, skip := grafanaAddr(env)
	if skip != nil {
		return *skip
	}

	searchURL := fmt.Sprintf("http://%s/api/datasources/proxy/uid/tempo/api/search?tags=%s&limit=5",
		addr, url.QueryEscape("service.name="+env.ProjectName))

	body, err := doGet(ctx, searchURL)
	if err != nil {
		return CheckResult{Status: StatusFail, Message: "Tempo query failed: " + err.Error(), Evidence: string(body)}
	}

	var tempoResp struct {
		Traces []struct {
			TraceID         string `json:"traceID"`
			RootServiceName string `json:"rootServiceName"`
			RootTraceName   string `json:"rootTraceName"`
		} `json:"traces"`
		Metrics struct {
			InspectedTraces int `json:"inspectedTraces"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(body, &tempoResp); err != nil {
		return CheckResult{Status: StatusFail, Message: "failed to parse Tempo response", Evidence: string(body)}
	}

	totalTraces := tempoResp.Metrics.InspectedTraces
	if len(tempoResp.Traces) == 0 {
		return CheckResult{
			Status:  StatusWarn,
			Message: "no traces found (send some requests to generate traces)",
		}
	}

	// Collect root trace names for the summary.
	var names []string
	for _, t := range tempoResp.Traces {
		if t.RootTraceName != "" {
			names = append(names, t.RootTraceName)
		}
	}
	namesSummary := ""
	if len(names) > 0 {
		namesSummary = " (" + strings.Join(names, ", ") + ")"
	}

	return CheckResult{
		Status:  StatusPass,
		Message: fmt.Sprintf("%d traces found%s", totalTraces, namesSummary),
	}
}

// CheckLoki verifies logs are being ingested into Loki.
func CheckLoki(ctx context.Context, env *Environment) CheckResult {
	addr, skip := grafanaAddr(env)
	if skip != nil {
		return *skip
	}

	params := url.Values{}
	params.Set("query", `{container=~".*app.*"}`)
	params.Set("limit", "5")
	lokiURL := fmt.Sprintf("http://%s/api/datasources/proxy/uid/loki/loki/api/v1/query_range?%s", addr, params.Encode())

	body, err := doGet(ctx, lokiURL)
	if err != nil {
		return CheckResult{Status: StatusFail, Message: "Loki query failed: " + err.Error(), Evidence: string(body)}
	}

	var lokiResp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]interface{} `json:"stream"`
				Values [][]string             `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &lokiResp); err != nil {
		return CheckResult{Status: StatusFail, Message: "failed to parse Loki response", Evidence: string(body)}
	}

	streamCount := len(lokiResp.Data.Result)
	if streamCount == 0 {
		return CheckResult{
			Status:  StatusWarn,
			Message: "no log streams found (app may not have produced logs yet)",
		}
	}

	var totalLines int
	for _, s := range lokiResp.Data.Result {
		totalLines += len(s.Values)
	}

	return CheckResult{
		Status:  StatusPass,
		Message: fmt.Sprintf("%d log streams, %d lines", streamCount, totalLines),
	}
}

// CheckPyroscope verifies continuous profiling is working.
func CheckPyroscope(ctx context.Context, env *Environment) CheckResult {
	addr, skip := grafanaAddr(env)
	if skip != nil {
		return *skip
	}

	// Try via Grafana datasource proxy first.
	profileURL := fmt.Sprintf("http://%s/api/datasources/proxy/uid/pyroscope/api/v1/profileTypes", addr)
	body, err := doGet(ctx, profileURL)
	if err == nil {
		return parsePyroscopeProfileTypes(body, env.ProjectName)
	}

	// Fallback: check Pyroscope health via docker exec (use curl, not wget).
	readyCmd := exec.CommandContext(ctx, "docker", "compose", "exec", "-w", "/", "lgtm",
		"curl", "-sf", "http://localhost:4040/ready")
	readyCmd.Dir = env.ProjectDir
	readyOut, err := readyCmd.CombinedOutput()
	if err != nil {
		return CheckResult{
			Status:   StatusFail,
			Message:  "Pyroscope not reachable",
			Evidence: strings.TrimSpace(string(readyOut)),
		}
	}

	// Pyroscope is healthy — query via gRPC-web endpoint for label values.
	labelsCmd := exec.CommandContext(ctx, "docker", "compose", "exec", "-w", "/", "lgtm",
		"curl", "-sf", "http://localhost:4040/querier.v1.QuerierService/LabelValues",
		"-H", "Content-Type: application/json",
		"-d", `{"name":"__service_name__"}`)
	labelsCmd.Dir = env.ProjectDir
	labelsOut, err := labelsCmd.CombinedOutput()
	if err != nil {
		return CheckResult{
			Status:  StatusWarn,
			Message: "Pyroscope is healthy but could not query labels",
		}
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
