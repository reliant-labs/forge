package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/config"
)

// smoke_flow.go — the APP-FLOW CHECK phase of `forge smoke`.
//
// WHY THIS EXISTS. The built-in smoke probes (smoke.go cloud routes,
// smoke_dev.go dev ports) verify TRANSPORT only: a listener answered, a port
// is bound. That is necessary but NOT sufficient — smoke can be GREEN while
// the app is functionally broken, because no built-in probe can express an
// app-specific end-to-end invariant. The motivating case: `forge smoke dev`
// was GREEN while the managed-daemon flow was broken, because forge had no way
// to assert "every Ready daemon is attached to the gateway".
//
// THE DESIGN. The OWNING SERVICE (the one holding the state internally) asserts
// the invariant and exposes it as an HTTP flow-health endpoint returning 200
// (healthy) / 503 (unhealthy) — status-only in public so it leaks nothing
// sensitive. The app DECLARES that endpoint in forge.yaml (`smoke.flow_checks`)
// and `forge smoke <env>` simply CURLS it, folding the 200/503 into the SAME
// summary + table + --json + exit logic the route probes use. smoke needs only
// a URL + reachability — no DB creds, no privileged command, no auth juggling.
// A 503 (or unreachable) flow endpoint turns smoke RED (exit 1), so a green
// smoke now means the app actually works, not just that its ports are open.
//
// Generic by design: any forge app can declare flow-health endpoints; the
// daemon-attachment endpoint is just reliant/control-plane's instance.

// smokeFlowReason* are the reason classes for the flow-check phase. Stable
// strings (the --json `reason` field keys off them), distinct from the
// route-probe reasons so a CI consumer can tell an app-flow failure apart from
// an ingress failure.
const (
	smokeFlowReasonHealthy   = "flow-healthy"     // PASS: endpoint returned 2xx
	smokeFlowReasonUnhealthy = "flow-unhealthy"   // FAIL: endpoint returned non-2xx (e.g. 503)
	smokeFlowReasonUnreach   = "flow-unreachable" // FAIL: endpoint couldn't be reached
	smokeFlowReasonErr       = "flow-misdeclared" // FAIL: the check declaration is invalid
)

// flowProbe issues one flow-health probe and returns (statusCode, body, err).
// Swapped out in tests so the phase orchestration is unit-testable without a
// live HTTP server. A non-nil err means the endpoint couldn't be reached
// (transport failure) — distinct from a reachable endpoint that answered 503.
type flowProbe func(ctx context.Context, url string, timeout time.Duration) (int, string, error)

// runSmokeFlowChecks probes every declared flow-health endpoint that applies
// to env and returns the projected result rows. Checks scoped out via `envs:`
// are skipped. A nil/empty declaration yields no rows — the route probes alone
// decide the verdict, so existing projects are unaffected.
//
// The probe is injected so the orchestration (which checks run, how 200/503
// map to PASS/FAIL) is testable without a real HTTP server.
func runSmokeFlowChecks(ctx context.Context, env string, checks []config.SmokeFlowCheck, probe flowProbe, timeout time.Duration) []smokeRouteResult {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	applicable := make([]config.SmokeFlowCheck, 0, len(checks))
	for _, c := range checks {
		if c.RunsInEnv(env) {
			applicable = append(applicable, c)
		}
	}
	// Stable order by name so the table is deterministic regardless of
	// forge.yaml ordering.
	sort.SliceStable(applicable, func(i, j int) bool { return applicable[i].Name < applicable[j].Name })

	results := make([]smokeRouteResult, 0, len(applicable))
	for _, c := range applicable {
		results = append(results, probeOneFlowCheck(ctx, c, probe, timeout))
	}
	return results
}

// probeOneFlowCheck curls a single declared flow-health endpoint and projects
// its outcome into a smokeRouteResult so it folds into the shared
// summary/table/json. The check's name + URL are surfaced as the route
// metadata (RouteKind "flow") so the existing output helpers render it without
// special-casing.
func probeOneFlowCheck(ctx context.Context, c config.SmokeFlowCheck, probe flowProbe, timeout time.Duration) smokeRouteResult {
	target := smokeTarget{
		RouteKind: "flow",
		RouteName: c.Name,
		Host:      c.URL,
		ProbeHost: c.URL,
		Path:      "(flow-health)",
	}
	if strings.TrimSpace(c.URL) == "" {
		return smokeRouteResult{
			Target: target,
			Status: smokeStatusFail,
			Reason: smokeFlowReasonErr,
			Detail: fmt.Sprintf("flow check %q declares no url", c.Name),
		}
	}

	statusCode, body, err := probe(ctx, c.URL, timeout)
	summary := flowBodySummary(body)

	switch {
	case err != nil:
		return smokeRouteResult{
			Target: target,
			Status: smokeStatusFail,
			Reason: smokeFlowReasonUnreach,
			Detail: flowDetail("UNREACHABLE", transportErrorDetail(err), c.Description),
		}
	case statusCode >= 200 && statusCode < 300:
		return smokeRouteResult{
			Target:     target,
			Status:     smokeStatusPass,
			Reason:     smokeFlowReasonHealthy,
			StatusCode: statusCode,
			Detail:     flowDetail("HEALTHY", summary, c.Description),
		}
	default:
		return smokeRouteResult{
			Target:     target,
			Status:     smokeStatusFail,
			Reason:     smokeFlowReasonUnhealthy,
			StatusCode: statusCode,
			Detail:     flowDetail("UNHEALTHY", summary, c.Description),
		}
	}
}

// probeFlowHealth is the real flow-health probe: a plain HTTP GET that returns
// the status code + a bounded body prefix. No redirect following (a redirect
// is not a health verdict), and it never errors on a non-2xx — only a
// transport failure (dial/TLS/read) is returned as err, so 503 surfaces as a
// reachable-but-unhealthy result the caller classifies as FAIL.
func probeFlowHealth(ctx context.Context, url string, timeout time.Duration) (int, string, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	buf := make([]byte, 2048)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n]), nil
}

// flowBodySummary trims a flow-health response body to a short single-line
// note so the smoke detail column quotes the aggregate ("2 daemons, 0
// unattached") without dumping the whole JSON. Returns "" when empty.
func flowBodySummary(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	// One line is enough — flow-health bodies are terse JSON / a status line.
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = strings.TrimSpace(body[:i])
	}
	const max = 160
	if len(body) > max {
		body = body[:max] + "…"
	}
	return body
}

// flowDetail composes the smoke detail column as
//
//	<VERDICT> — <live body aggregate>  [<static description>]
//
// The VERDICT (HEALTHY / UNHEALTHY / UNREACHABLE) leads and is the LIVE state.
// The body aggregate is the endpoint's own terse summary ("2 attachments, 0
// stale") — the live evidence. The declared description is a STATIC label of
// what the check asserts; it's bracketed and trails so it can never be misread
// as the live verdict (the earlier "flow UNHEALTHY <description that reads
// healthy>" garble). Any empty part is omitted.
func flowDetail(verdict, bodySummary, description string) string {
	out := verdict
	if bodySummary != "" {
		out += " — " + bodySummary
	}
	if description != "" {
		out += "  [" + description + "]"
	}
	return out
}

// projectFlowChecks loads the project's declared smoke flow-health checks. A
// missing forge.yaml (or no project) yields no checks and no error — smoke
// still runs its route probes. Surfaced as a seam so the smoke paths resolve
// the declaration once.
func projectFlowChecks() []config.SmokeFlowCheck {
	cfg, err := loadProjectConfig()
	if err != nil {
		return nil
	}
	return cfg.Smoke.FlowChecks
}

// flowCheckTimeout derives the flow-health probe timeout from the smoke
// per-probe timeout. A flow-health endpoint runs an internal assertion (a DB
// read, a connection-table scan), so give it a little more headroom than a
// bare route probe, but never less than the configured timeout.
func flowCheckTimeout(probeTimeout time.Duration) time.Duration {
	if probeTimeout <= 0 {
		return 10 * time.Second
	}
	if probeTimeout < 10*time.Second {
		return 10 * time.Second
	}
	return probeTimeout
}

// writeFlowCheckSection prints the app-flow check rows as their own block
// under the route table, so a flow break is visible even when every route
// passed. Called by both smoke paths after the route table. No-op when there
// are no flow rows.
func writeFlowCheckSection(out io.Writer, flowResults []smokeRouteResult) {
	if len(flowResults) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  app-flow checks (curl the owning service's flow-health endpoint):")
	nameW := len("CHECK")
	for _, r := range flowResults {
		nameW = maxInt(nameW, len(r.Target.RouteName))
	}
	fmt.Fprintf(out, "    %-6s  %-*s  %s\n", "RESULT", nameW, "CHECK", "DETAIL")
	for _, r := range flowResults {
		fmt.Fprintf(out, "    %-6s  %-*s  %s\n", string(r.Status), nameW, r.Target.RouteName, smokeReasonLine(r))
	}
}
