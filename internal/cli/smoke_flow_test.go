package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/config"
)

// TestRunSmokeFlowChecks_PassAndFail exercises the app-flow-check phase in
// isolation: a declared endpoint that returns 200 → PASS, and one that returns
// 503 → FAIL. This is the core guarantee — a 503 flow endpoint produces a FAIL
// row that turns the smoke run RED.
func TestRunSmokeFlowChecks_PassAndFail(t *testing.T) {
	checks := []config.SmokeFlowCheck{
		{Name: "healthy-flow", URL: "http://svc/flow-health"},
		{Name: "broken-flow", URL: "http://other/flow-health"},
	}
	probe := func(ctx context.Context, url string, timeout time.Duration) (int, string, error) {
		if strings.Contains(url, "other") {
			return 503, `{"status":"unhealthy","daemons":2,"unattached":1}`, nil
		}
		return 200, `{"status":"healthy","daemons":2,"unattached":0}`, nil
	}

	results := runSmokeFlowChecks(context.Background(), "dev", checks, probe, time.Second)
	if len(results) != 2 {
		t.Fatalf("expected 2 flow results, got %d", len(results))
	}
	// Sorted by name: broken-flow first.
	if results[0].Target.RouteName != "broken-flow" || results[0].Status != smokeStatusFail {
		t.Errorf("expected broken-flow FAIL, got %+v", results[0])
	}
	if results[0].Reason != smokeFlowReasonUnhealthy {
		t.Errorf("expected reason %q, got %q", smokeFlowReasonUnhealthy, results[0].Reason)
	}
	if !strings.Contains(results[0].Detail, "unattached") {
		t.Errorf("expected aggregate body quoted in detail, got %q", results[0].Detail)
	}
	if results[1].Target.RouteName != "healthy-flow" || results[1].Status != smokeStatusPass {
		t.Errorf("expected healthy-flow PASS, got %+v", results[1])
	}

	summary := summarizeSmoke(results)
	if !summary.AnyFail || summary.Fail != 1 || summary.Pass != 1 {
		t.Errorf("expected 1 PASS / 1 FAIL / AnyFail, got %+v", summary)
	}
}

// TestRunSmokeFlowChecks_Unreachable verifies a transport failure (endpoint
// down) classifies as a FAIL, not a panic — the dead-endpoint case.
func TestRunSmokeFlowChecks_Unreachable(t *testing.T) {
	checks := []config.SmokeFlowCheck{{Name: "down-flow", URL: "http://nope/flow-health"}}
	probe := func(ctx context.Context, url string, timeout time.Duration) (int, string, error) {
		return 0, "", context.DeadlineExceeded
	}
	results := runSmokeFlowChecks(context.Background(), "dev", checks, probe, time.Second)
	if len(results) != 1 || results[0].Status != smokeStatusFail {
		t.Fatalf("expected 1 FAIL for unreachable endpoint, got %+v", results)
	}
	if results[0].Reason != smokeFlowReasonUnreach {
		t.Errorf("expected reason %q, got %q", smokeFlowReasonUnreach, results[0].Reason)
	}
}

// TestRunSmokeFlowChecks_EnvScoping verifies a check scoped to specific envs
// is skipped in other envs.
func TestRunSmokeFlowChecks_EnvScoping(t *testing.T) {
	checks := []config.SmokeFlowCheck{
		{Name: "dev-only", URL: "http://svc/flow-health", Envs: []string{"dev"}},
		{Name: "prod-only", URL: "http://svc/flow-health", Envs: []string{"prod"}},
	}
	probe := func(ctx context.Context, url string, timeout time.Duration) (int, string, error) {
		return 200, "ok", nil
	}
	results := runSmokeFlowChecks(context.Background(), "dev", checks, probe, time.Second)
	if len(results) != 1 || results[0].Target.RouteName != "dev-only" {
		t.Fatalf("expected only dev-only to run in dev env, got %+v", results)
	}
}

// TestRunSmokeWith_FlowCheckFailsOverall is the integration guarantee: every
// ROUTE passes, but a declared app-flow check returns 503, so the WHOLE smoke
// run exits non-zero. This is the green-while-broken regression the phase
// closes.
func TestRunSmokeWith_FlowCheckFailsOverall(t *testing.T) {
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t, sampleSmokeBundle))

	resolve := func(ctx context.Context, kubeContext, namespace, gateway string) (string, error) {
		return "203.0.113.10", nil
	}
	// Every route PASSes.
	probe := func(ctx context.Context, target smokeTarget, gatewayIP string, timeout time.Duration) smokeRouteResult {
		return smokeRouteResult{Status: smokeStatusPass, Reason: smokeReasonReached, StatusCode: 200, Detail: "ok"}
	}
	// The declared flow endpoint is UNHEALTHY (503).
	flowProbe := func(ctx context.Context, url string, timeout time.Duration) (int, string, error) {
		return 503, `{"status":"unhealthy"}`, nil
	}

	var buf bytes.Buffer
	err := runSmokeWith(context.Background(), "preprod", smokeOptions{
		flowChecks: []config.SmokeFlowCheck{{Name: "daemon-flow", URL: "http://gw/flow-health"}},
		flowProbe:  flowProbe,
	}, resolve, probe, &buf)
	if err == nil {
		t.Fatalf("expected non-nil error: routes pass but flow check 503'd:\n%s", buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "daemon-flow") || !strings.Contains(out, "FAIL") {
		t.Errorf("expected the daemon-flow FAIL row in output:\n%s", out)
	}
	if !strings.Contains(out, "app-flow checks") {
		t.Errorf("expected the app-flow checks section in output:\n%s", out)
	}
}

// TestRunSmokeWith_FlowCheckHealthyStaysGreen verifies that when both routes
// and the flow check pass, smoke stays GREEN (exit 0) and the verdict counts
// the flow check.
func TestRunSmokeWith_FlowCheckHealthyStaysGreen(t *testing.T) {
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t, sampleSmokeBundle))

	resolve := func(ctx context.Context, kubeContext, namespace, gateway string) (string, error) {
		return "203.0.113.10", nil
	}
	probe := func(ctx context.Context, target smokeTarget, gatewayIP string, timeout time.Duration) smokeRouteResult {
		return smokeRouteResult{Status: smokeStatusPass, Reason: smokeReasonReached, StatusCode: 200}
	}
	flowProbe := func(ctx context.Context, url string, timeout time.Duration) (int, string, error) {
		return 200, `{"status":"healthy","daemons":2,"unattached":0}`, nil
	}

	var buf bytes.Buffer
	err := runSmokeWith(context.Background(), "preprod", smokeOptions{
		flowChecks: []config.SmokeFlowCheck{{Name: "daemon-flow", URL: "http://gw/flow-health"}},
		flowProbe:  flowProbe,
	}, resolve, probe, &buf)
	if err != nil {
		t.Fatalf("expected GREEN when routes + flow pass: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "every app-flow check is healthy") {
		t.Errorf("expected healthy combined verdict:\n%s", buf.String())
	}
}
