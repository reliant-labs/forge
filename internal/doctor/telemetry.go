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
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 5 * time.Second}

// grafanaAddr returns the Grafana host address or a skip result.
func grafanaAddr(env *Environment) (string, *CheckResult) {
	addr, ok := env.GetPort("lgtm", 3000)
	if !ok {
		return "", &CheckResult{Status: StatusSkip, Message: "Grafana port not discovered (docker check may have failed)"}
	}
	return addr, nil
}

// doGet performs a GET request and returns the body bytes.
func doGet(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// CheckPrometheus verifies Prometheus is scraping targets and receiving app metrics.
func CheckPrometheus(ctx context.Context, env *Environment) CheckResult {
	addr, skip := grafanaAddr(env)
	if skip != nil {
		return *skip
	}

	base := "http://" + addr + "/api/datasources/proxy/uid/prometheus/api/v1/query"

	// Query "up" targets.
	upURL := base + "?query=up"
	body, err := doGet(ctx, upURL)
	if err != nil {
		return CheckResult{Status: StatusFail, Message: "Prometheus query failed: " + err.Error(), Evidence: string(body)}
	}

	var upResp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]interface{} `json:"metric"`
				Value  []interface{}          `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &upResp); err != nil {
		return CheckResult{Status: StatusFail, Message: "failed to parse Prometheus response", Evidence: string(body)}
	}
	if upResp.Status != "success" {
		return CheckResult{Status: StatusFail, Message: "Prometheus returned status: " + upResp.Status, Evidence: string(body)}
	}

	upCount := len(upResp.Data.Result)
	if upCount == 0 {
		return CheckResult{Status: StatusFail, Message: "no targets reporting up"}
	}

	// Query go_goroutines to verify app metrics.
	goroutinesURL := base + "?query=go_goroutines"
	goroutineBody, err := doGet(ctx, goroutinesURL)
	goroutineMsg := ""
	if err == nil {
		var grResp struct {
			Data struct {
				Result []struct {
					Value []interface{} `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if json.Unmarshal(goroutineBody, &grResp) == nil && len(grResp.Data.Result) > 0 {
			if vals := grResp.Data.Result[0].Value; len(vals) >= 2 {
				goroutineMsg = fmt.Sprintf(", go_goroutines=%s", vals[1])
			}
		}
	}

	return CheckResult{
		Status:  StatusPass,
		Message: fmt.Sprintf("%d targets up%s", upCount, goroutineMsg),
	}
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
			return CheckResult{
				Status:  StatusWarn,
				Message: "Pyroscope is healthy but could not parse labels",
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