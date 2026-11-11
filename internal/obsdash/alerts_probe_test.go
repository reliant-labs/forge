package obsdash

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/alerting"
	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/releasehealth"
)

func probeInput(targets ...string) AlertInput {
	in := AlertInputOf(fixture())
	in.ReleaseProbeTargets = targets
	return in
}

const probeSpanFilter = "SpanAttributes['forge.probe'] != 'true'"

func TestReleaseProbeDefaultsApplyOnlyToAnEnvWithProbes(t *testing.T) {
	if r := rulesByAlert(t, probeInput(), "prod", nil); r["ReleaseProbeFailing"].Alert != "" || r["ReleaseProbeDown"].Alert != "" {
		t.Error("an env with no release probes got a probe alert: it could only ever be silent")
	}
	r := rulesByAlert(t, probeInput("web"), "prod", nil)
	for alert, want := range map[string]struct{ id, severity, forDur string }{
		"ReleaseProbeFailing": {"release-probe-failing", SeverityWarning, "0s"},
		"ReleaseProbeDown":    {"release-probe-down", SeverityCritical, "5m"},
	} {
		got, ok := r[alert]
		if !ok {
			t.Errorf("%s missing from an env with probes", alert)
			continue
		}
		if got.ID != want.id || got.Labels["severity"] != want.severity || got.For != want.forDur {
			t.Errorf("%s = id %q severity %q for %q, want %q %q %q", alert, got.ID, got.Labels["severity"], got.For, want.id, want.severity, want.forDur)
		}
		// probe_failed already judges the release: the alert must not count
		// again as alert_firing, nor hold a later release as a standing critical.
		if got.Labels[releasehealth.LabelReleaseHealth] != releasehealth.ReleaseHealthExclude {
			t.Errorf("%s labels = %v, want %s=%s", alert, got.Labels, releasehealth.LabelReleaseHealth, releasehealth.ReleaseHealthExclude)
		}
		if err := alerting.ValidateExpr(got.Expr); err != nil {
			t.Errorf("%s: the alert guard refuses its own expr: %v", alert, err)
		}
		for _, want := range []string{
			"FROM otel_metrics_sum",
			"MetricName = '" + release.ProbeMetricRequests + "'",
			"ResourceAttributes['deployment.environment.name'] = 'prod'",
			"ResourceAttributes['reliant.source'] = '" + release.ProbeSource + "'",
			"ServiceName IN ('web')",
			// Only ok and failed are samples: throttled (429) is neither, and
			// degraded is the prober's own fault, never the tenant's.
			"Attributes['" + release.ProbeAttrOutcome + "'] IN ('" + release.ProbeOutcomeOK + "', '" + release.ProbeOutcomeFailed + "')",
		} {
			if !strings.Contains(got.Expr, want) {
				t.Errorf("%s does not contain %q:\n%s", alert, want, got.Expr)
			}
		}
		for _, never := range []string{release.ProbeOutcomeThrottled, release.ProbeOutcomeDegraded} {
			if strings.Contains(got.Expr, "'"+never+"'") {
				t.Errorf("%s reads %s probes; they never count:\n%s", alert, never, got.Expr)
			}
		}
	}
	if e := r["ReleaseProbeFailing"].Expr; !strings.Contains(e, "WHERE failed >= 2\n") {
		t.Errorf("ReleaseProbeFailing does not fire on 2 failed probes in the window:\n%s", e)
	}
	if e := r["ReleaseProbeDown"].Expr; !strings.Contains(e, "WHERE samples >= 10 AND failed >= 0.8 * samples\n") {
		t.Errorf("ReleaseProbeDown does not fire on 80%% failed of at least 10 samples:\n%s", e)
	}
	for _, alert := range []string{"ReleaseProbeFailing", "ReleaseProbeDown"} {
		if !strings.Contains(r[alert].Expr, "INTERVAL 5 MINUTE") {
			t.Errorf("%s does not judge a 5 minute window:\n%s", alert, r[alert].Expr)
		}
	}
}

func TestReleaseProbeDefaultsAreTunableAndDisablable(t *testing.T) {
	r := rulesByAlert(t, probeInput("web"), "prod", map[string]AlertTuning{
		"release-probe-failing": {Enabled: true, Params: map[string]float64{"failures": 4, "window_minutes": 15}},
		"release-probe-down":    {Enabled: true, Params: map[string]float64{"failure_ratio": 0.5, "min_samples": 20}, Labels: map[string]string{"severity": "warning"}},
	})
	if e := r["ReleaseProbeFailing"].Expr; !strings.Contains(e, "WHERE failed >= 4\n") || !strings.Contains(e, "INTERVAL 15 MINUTE") {
		t.Errorf("failing tuning not applied:\n%s", e)
	}
	if d := r["ReleaseProbeDown"]; !strings.Contains(d.Expr, "WHERE samples >= 20 AND failed >= 0.5 * samples\n") || d.Labels["severity"] != "warning" {
		t.Errorf("down tuning not applied: %+v", d)
	}
	r = rulesByAlert(t, probeInput("web"), "prod", map[string]AlertTuning{"release-probe-down": {Enabled: false}})
	if r["ReleaseProbeDown"].Alert != "" || r["ReleaseProbeFailing"].Alert == "" {
		t.Errorf("disabling release-probe-down = %v", r)
	}
	if err := ValidateAlertTuning(map[string]AlertTuning{"release-probe-failing": {Params: map[string]float64{"failure_ratio": 1}}}); err == nil {
		t.Error("a param of the other probe default is accepted")
	}
}

func TestReleaseProbeAlertNamesAreReservedAndFollowTheMetricsOptOut(t *testing.T) {
	names := map[string]bool{}
	for _, n := range AlertDefaultAlertNames() {
		names[n] = true
	}
	for _, n := range []string{"ReleaseProbeFailing", "ReleaseProbeDown"} {
		if !names[n] {
			t.Errorf("%s is not reserved: a user rule could shadow it", n)
		}
	}
	_, skipped, err := ResolveAlertDefaultsFor(probeInput("web"), "prod", nil, []string{"metadata"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range skipped {
		if strings.HasPrefix(s.ID, "release-probe") {
			t.Errorf("opting out of metadata skipped %s", s.ID)
		}
	}
	_, skipped, _ = ResolveAlertDefaultsFor(probeInput("web"), "prod", nil, []string{"metrics"})
	got := map[string]bool{}
	for _, s := range skipped {
		got[s.ID] = true
	}
	if !got["release-probe-failing"] || !got["release-probe-down"] {
		t.Errorf("opting out of metrics did not skip the probe defaults: %v", skipped)
	}
}

// The generated catalog is a fact about the project; probes are a fact about
// one env, so the catalog never lists them (as it never lists the targets).
func TestReleaseProbeDefaultsAreNotInTheProjectCatalog(t *testing.T) {
	for _, d := range AlertDefaults(AlertInputOf(fixture())).Defaults {
		if strings.HasPrefix(d.ID, "release-probe") {
			t.Errorf("the project catalog lists %s", d.ID)
		}
	}
}

// A project with no generated alert defaults (a static site: forge generate
// writes none) still gets the probe defaults: they read only the env's render.
func TestEnvScopedDefaultsNeedNothingButTheEnv(t *testing.T) {
	rules, _, err := ResolveEnvAlertDefaultsFor(AlertInput{ReleaseProbeTargets: []string{"web"}}, "prod", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rules {
		got = append(got, r.Alert)
	}
	if strings.Join(got, ",") != "ReleaseProbeDown,ReleaseProbeFailing" {
		t.Errorf("env-scoped defaults = %v, want exactly the two probe defaults", got)
	}
	rules, _, err = ResolveEnvAlertDefaultsFor(AlertInput{}, "prod", nil, nil)
	if err != nil || len(rules) != 0 {
		t.Errorf("an env with no probes got env-scoped defaults %v (%v)", rules, err)
	}
	if _, _, err := ResolveEnvAlertDefaultsFor(AlertInput{ReleaseProbeTargets: []string{"web"}}, "prod", map[string]AlertTuning{"nope": {}}, nil); err == nil {
		t.Error("an unknown tuning id is accepted")
	}
}

// No other default fires on probe traffic. The span readers exclude
// forge.probe=true in an env that is probed; the rpc RED readers read only the
// rpc.server.* metrics, which a workload never records for a probe; the log
// readers read only Kubernetes events. Without probes, nothing changes (the
// goldens in alerts_test.go pin that text).
func TestOtherDefaultsNeverSeeProbeTraffic(t *testing.T) {
	probed, err := ResolveAlertDefaults(probeInput("web"), "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	plain := byAlert(mustResolve(t, probeInput(), "prod"))
	spanReaders := 0
	for _, r := range probed {
		if strings.HasPrefix(r.ID, "release-probe") {
			continue
		}
		if strings.Contains(r.Expr, "otel_traces") {
			spanReaders++
			if !strings.Contains(r.Expr, probeSpanFilter) {
				t.Errorf("%s reads spans but does not exclude probes:\n%s", r.Alert, r.Expr)
			}
			if strings.Contains(plain[r.Alert].Expr, "forge.probe") {
				t.Errorf("%s excludes probes in an env that has none: its text must not change", r.Alert)
			}
		}
		if strings.Contains(r.Expr, "otel_logs") && !strings.Contains(r.Expr, "k8s.event.reason") {
			t.Errorf("%s reads logs other than Kubernetes events; it must exclude forge.probe=true:\n%s", r.Alert, r.Expr)
		}
		if strings.Contains(r.Expr, "otel_metrics_sum") || strings.Contains(r.Expr, "otel_metrics_histogram") {
			if !strings.Contains(r.Expr, "rpc.server.") {
				t.Errorf("%s reads a request metric other than rpc.server.*; check whether workloads record it for probes:\n%s", r.Alert, r.Expr)
			}
		}
	}
	if spanReaders != 2 {
		t.Errorf("checked %d span-reading defaults, want the 2 frontend ones", spanReaders)
	}
}

func mustResolve(t *testing.T, in AlertInput, env string) []AlertRule {
	t.Helper()
	rules, err := ResolveAlertDefaults(in, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestGoldenReleaseProbeAlertSQL(t *testing.T) {
	var b strings.Builder
	for _, r := range mustResolve(t, probeInput("api", "web"), "prod") {
		if !strings.HasPrefix(r.ID, "release-probe") {
			continue
		}
		b.WriteString("-- " + r.Alert + " (" + r.ID + ") severity=" + r.Labels["severity"] + " release_health=" + r.Labels[releasehealth.LabelReleaseHealth] + " for=" + r.For + " interval=" + r.Interval + "\n")
		b.WriteString(r.Expr + "\n\n")
	}
	golden(t, "alert-defaults-release-probes.sql", []byte(b.String()))
}

// TestReleaseProbeAlertsOnClickHouse runs the probe defaults against a real
// ClickHouse holding the exporter's sum table, with one target per case the
// SQL must get right, and reads the why payload back through pkg/alerting.
//
// Set FORGE_ALERT_CLICKHOUSE to a running container with clickhouse-client
// (password "pw") whose default database has otel_metrics_sum.
func TestReleaseProbeAlertsOnClickHouse(t *testing.T) {
	container := os.Getenv("FORGE_ALERT_CLICKHOUSE")
	if testing.Short() || container == "" {
		t.Skip("needs a ClickHouse container; set FORGE_ALERT_CLICKHOUSE=<container>")
	}
	run := func(sql string) string {
		t.Helper()
		cmd := exec.Command("docker", "exec", "-i", container, "clickhouse-client", "--password", "pw", "--multiquery", "--format", "JSONEachRow", "--output_format_json_quote_64bit_integers=0")
		cmd.Stdin = strings.NewReader(sql)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("clickhouse: %v\n%s\n--- sql ---\n%s", err, out, sql)
		}
		return strings.TrimSpace(string(out))
	}
	run(`TRUNCATE TABLE otel_metrics_sum`)

	// put writes one flush of the prober for a target: count probes of one
	// outcome, `ago` minutes back, as a DELTA sum with source probe, unless
	// the case overrides the env or the source.
	type point struct {
		env, source, target, path, outcome, class, errType string
		ago, count                                         int
	}
	put := func(p point) {
		t.Helper()
		if p.env == "" {
			p.env = "prod"
		}
		if p.source == "" {
			p.source = release.ProbeSource
		}
		attrs := fmt.Sprintf("map('%s','%s','%s','%s','%s','%s','%s','%s')",
			release.ProbeAttrPath, p.path, release.ProbeAttrOutcome, p.outcome,
			release.ProbeAttrStatusClass, p.class, release.ProbeAttrError, p.errType)
		run(fmt.Sprintf(`INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
VALUES (map('deployment.environment.name','%s','reliant.source','%s','service.instance.id','prober-0'), '%s', '%s', %s, now() - INTERVAL %d SECOND, %d, 1, true)`,
			p.env, p.source, p.target, release.ProbeMetricRequests, attrs, p.ago*60+30, p.count))
	}
	okEvery := func(target string, agos ...int) {
		for _, a := range agos {
			put(point{target: target, path: "/", outcome: release.ProbeOutcomeOK, class: "2xx", ago: a, count: 4})
		}
	}

	// healthy: 4 ok a minute.
	okEvery("healthy", 0, 1, 2, 3, 4)
	// blip: one failure among ok probes: below the warning's 2.
	okEvery("blip", 0, 1, 2, 3, 4)
	put(point{target: "blip", path: "/", outcome: release.ProbeOutcomeFailed, errType: "timeout", ago: 2, count: 1})
	// failing: 3 failures (5xx on /pricing twice, a 404 on / once) among many ok -> warning only.
	okEvery("failing", 0, 1, 2, 3, 4)
	put(point{target: "failing", path: "/pricing", outcome: release.ProbeOutcomeFailed, class: "5xx", ago: 3, count: 1})
	put(point{target: "failing", path: "/pricing", outcome: release.ProbeOutcomeFailed, class: "5xx", ago: 1, count: 1})
	put(point{target: "failing", path: "/", outcome: release.ProbeOutcomeFailed, class: "4xx", ago: 2, count: 1})
	// down: 18 of 20 failed (15 5xx, 3 that got no response) -> both.
	for a := 0; a < 5; a++ {
		put(point{target: "down", path: "/", outcome: release.ProbeOutcomeFailed, class: "5xx", ago: a, count: 3})
	}
	for a := 0; a < 3; a++ {
		put(point{target: "down", path: "/", outcome: release.ProbeOutcomeFailed, errType: "connect", ago: a, count: 1})
	}
	put(point{target: "down", path: "/", outcome: release.ProbeOutcomeOK, class: "2xx", ago: 4, count: 2})
	// thin: every probe failed, but 6 samples is too few to call it down -> warning only.
	put(point{target: "thin", path: "/", outcome: release.ProbeOutcomeFailed, class: "5xx", ago: 1, count: 6})
	// throttled: 429s all window long are neither success nor failure.
	for a := 0; a < 5; a++ {
		put(point{target: "throttled", path: "/", outcome: release.ProbeOutcomeThrottled, class: "4xx", ago: a, count: 4})
	}
	// degraded: the prober's canary was failing, so its failures are its own
	// fault: never the tenant's alert.
	for a := 0; a < 5; a++ {
		put(point{target: "degraded", path: "/", outcome: release.ProbeOutcomeDegraded, errType: "connect", ago: a, count: 4})
	}
	// old: failures before the window never count.
	okEvery("old", 0, 1, 2)
	put(point{target: "old", path: "/", outcome: release.ProbeOutcomeFailed, class: "5xx", ago: 8, count: 20})
	// another env's, another source's, and an undeclared target's failures never count.
	put(point{env: "dev", target: "healthy", path: "/", outcome: release.ProbeOutcomeFailed, class: "5xx", ago: 1, count: 20})
	put(point{source: "otlp", target: "healthy", path: "/", outcome: release.ProbeOutcomeFailed, class: "5xx", ago: 1, count: 20})
	put(point{target: "undeclared", path: "/", outcome: release.ProbeOutcomeFailed, class: "5xx", ago: 1, count: 20})

	targets := []string{"blip", "degraded", "down", "failing", "healthy", "old", "thin", "throttled"}
	by := byAlert(mustResolve(t, probeInput(targets...), "prod"))
	rows := func(alert string) map[string]map[string]any {
		t.Helper()
		out := map[string]map[string]any{}
		for _, line := range strings.Split(run(by[alert].Expr), "\n") {
			if line == "" {
				continue
			}
			var row map[string]any
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("%s: %v\n%s", alert, err, line)
			}
			out[row["target"].(string)] = row
		}
		return out
	}
	names := func(m map[string]map[string]any) string {
		var n []string
		for k := range m {
			n = append(n, k)
		}
		return strings.Join(sortedStrs(n), ",")
	}

	failing, down := rows("ReleaseProbeFailing"), rows("ReleaseProbeDown")
	if got := names(failing); got != "down,failing,thin" {
		t.Errorf("ReleaseProbeFailing fired for %q, want down, failing and thin (blip failed once, throttled and degraded never count, old is outside the window)", got)
	}
	if got := names(down); got != "down" {
		t.Errorf("ReleaseProbeDown fired for %q, want only down (thin has too few samples)", got)
	}
	if v := failing["failing"]["value"]; v != float64(3) {
		t.Errorf("failing failures = %v, want 3", v)
	}
	if v := down["down"]["value"]; v != 0.9 {
		t.Errorf("down failure ratio = %v, want 0.9 (18 of 20)", v)
	}

	w := whyOf(t, failing["failing"])
	if w.Detector != alerting.DetectorReleaseProbe || w.Signal != "probe_failures" || w.Unit != "probes" ||
		w.Observed == nil || *w.Observed != 3 || w.Since.IsZero() || w.Window != "5m" ||
		w.Thresholds["failures"] != 2 || w.DedupeKey != "release_probe/failing/-" || len(w.Next) != 2 {
		t.Errorf("failing why = %+v", w)
	}
	if len(w.Context.Findings) != 2 {
		t.Fatalf("failing findings = %+v, want one per path and status (/pricing 5xx, / 4xx)", w.Context.Findings)
	}
	if f := w.Context.Findings[0]; f.Code != release.HealthProbeFailed || f.Ref != "/pricing" || f.Value != 2 || !strings.Contains(f.Message, "/pricing answered 5xx 2 times") {
		t.Errorf("first finding = %+v, want /pricing's 2 5xx (most first)", f)
	}
	wd := whyOf(t, down["down"])
	if wd.Signal != "probe_failure_ratio" || wd.Unit != "fraction" || wd.Observed == nil || *wd.Observed != 0.9 ||
		wd.Thresholds["failure_ratio"] != 0.8 || wd.Thresholds["min_samples"] != 10 {
		t.Errorf("down why = %+v", wd)
	}
	if f := firstFinding(wd, release.HealthProbeFailed); f.Ref != "/" {
		t.Errorf("down's findings = %+v", wd.Context.Findings)
	}
	var sawTransport bool
	for _, f := range wd.Context.Findings {
		if strings.Contains(f.Message, "/ failed: connect 3 times") {
			sawTransport = true
		}
	}
	if !sawTransport {
		t.Errorf("a probe that got no response must say how it failed: %+v", wd.Context.Findings)
	}
	if f := w.Context.Findings[1]; !strings.Contains(f.Message, "/ answered 4xx once,") {
		t.Errorf("a single failure reads %q", f.Message)
	}

	// The span readers' probe exclusion is valid SQL on the exporter's table.
	for _, r := range mustResolve(t, probeInput("web"), "prod") {
		if strings.Contains(r.Expr, probeSpanFilter) {
			run(r.Expr)
		}
	}
}
