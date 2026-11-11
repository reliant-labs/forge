package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/releasehealth"
)

const probeSpanExclusion = "SpanAttributes['forge.probe'] != 'true'"

func withReleaseProbes(e *KCLEntities, targets ...string) *KCLEntities {
	for _, t := range targets {
		e.ReleaseProbes = append(e.ReleaseProbes, release.ShapeReleaseProbe{
			Target: t, Kind: release.ReleaseProbeKindFrontend, Paths: []string{"/"}, Method: "GET",
			RatePerMin: 4, TimeoutSeconds: 10, Source: release.ReleaseProbeSourceDefault,
		})
	}
	return e
}

func alertsByName(got []release.ShapeAlert) map[string]release.ShapeAlert {
	by := map[string]release.ShapeAlert{}
	for _, a := range got {
		by[a.Alert] = a
	}
	return by
}

// A probed env records both probe defaults over its own targets, and its span
// readers exclude probe spans; the same env unprobed records neither and its
// SQL is what it was.
func TestResolveShapeAlerts_ReleaseProbesGetTheProbeDefaults(t *testing.T) {
	dir := writeAlertDefaults(t, alertProject())
	got, _, err := resolveShapeAlerts(context.Background(), io.Discard, dir, "prod", withReleaseProbes(obsEntities(nil), "web", "api"))
	if err != nil {
		t.Fatal(err)
	}
	by := alertsByName(got)
	if len(got) != 20 {
		t.Errorf("got %d alerts, want the 18 defaults plus the 2 probe defaults", len(got))
	}
	for alert, id := range map[string]string{"ReleaseProbeFailing": "release-probe-failing", "ReleaseProbeDown": "release-probe-down"} {
		a, ok := by[alert]
		if !ok {
			t.Errorf("%s missing from a probed env", alert)
			continue
		}
		if a.Source != release.AlertSourceDefault || a.DefaultID != id || a.Labels[releasehealth.LabelReleaseHealth] != releasehealth.ReleaseHealthExclude {
			t.Errorf("%s = %+v", alert, a)
		}
		if !strings.Contains(a.Expr, "ServiceName IN ('api', 'web')") || !strings.Contains(a.Expr, "= 'prod'") {
			t.Errorf("%s is not scoped to the env's targets:\n%s", alert, a.Expr)
		}
	}
	for _, alert := range []string{"FrontendJSErrorSpike", "FrontendLCPPoor"} {
		if !strings.Contains(by[alert].Expr, probeSpanExclusion) {
			t.Errorf("%s reads spans in a probed env without excluding probes:\n%s", alert, by[alert].Expr)
		}
	}

	plain, _, err := resolveShapeAlerts(context.Background(), io.Discard, dir, "prod", obsEntities(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plain {
		if strings.HasPrefix(a.Alert, "ReleaseProbe") {
			t.Errorf("an env with no probes records %s", a.Alert)
		}
		if strings.Contains(a.Expr, "forge.probe") {
			t.Errorf("%s changed for an env with no probes:\n%s", a.Alert, a.Expr)
		}
	}
	if len(plain) != 18 {
		t.Errorf("an env with no probes got %d alerts, want the 18 it always had", len(plain))
	}
}

// A static site: forge generate writes no alert defaults, so it records none
// of the project's defaults, but its probes still get theirs.
func TestResolveShapeAlerts_ProbeDefaultsNeedNoGeneratedDefaults(t *testing.T) {
	site := t.TempDir()
	got, routes, err := resolveShapeAlerts(context.Background(), io.Discard, site, "prod", withReleaseProbes(obsEntities(nil), "web"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range got {
		names = append(names, a.Alert)
	}
	if strings.Join(names, ",") != "ReleaseProbeDown,ReleaseProbeFailing" {
		t.Errorf("a site with a probe records %v, want exactly the two probe defaults", names)
	}
	if len(routes) != 2 || routes[0].Receiver != release.AlertReceiverInApp {
		t.Errorf("routes = %+v, want the default in-app then Slack", routes)
	}
	// Tuned off in the env: nothing.
	got, _, err = resolveShapeAlerts(context.Background(), io.Discard, site, "prod", withReleaseProbes(obsEntities(&AlertsEntity{Defaults: map[string]AlertDefaultEntity{
		"release-probe-failing": {Enabled: false}, "release-probe-down": {Enabled: false},
	}}), "web"))
	if err != nil || got != nil {
		t.Errorf("both probe defaults disabled: got %v %v", got, err)
	}
	// Metrics opted out: nothing for them to read.
	e := withReleaseProbes(obsEntities(nil), "web")
	e.Observability.Policy = &ObservabilityPolicyEntity{Enabled: true, Disabled: []string{"metrics"}}
	if got, _, err = resolveShapeAlerts(context.Background(), io.Discard, site, "prod", e); err != nil || got != nil {
		t.Errorf("metrics opted out: got %v %v", got, err)
	}
	// No probes: nothing, as before.
	if got, _, err = resolveShapeAlerts(context.Background(), io.Discard, site, "prod", obsEntities(nil)); err != nil || got != nil {
		t.Errorf("no probes, no defaults file: got %v %v", got, err)
	}
}

// KCL -> render -> alerts: a hosted static site with no probe declaration
// gets the default probe at its root, and with it both probe alerts; the same
// site with its probe turned off gets none.
func TestReleaseProbeAlertsFollowTheRender(t *testing.T) {
	obs := "    control_plane = forge.ControlPlane {}\n    observability = forge.Observability {}\n"
	site := t.TempDir()

	e, err := renderReleaseProbeEnv(t, obs+probeSite)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got, _, err := resolveShapeAlerts(context.Background(), io.Discard, site, "prod", e)
	if err != nil {
		t.Fatal(err)
	}
	by := alertsByName(got)
	if len(got) != 2 || !strings.Contains(by["ReleaseProbeFailing"].Expr, "ServiceName IN ('web')") || !strings.Contains(by["ReleaseProbeDown"].Expr, "ServiceName IN ('web')") {
		t.Errorf("a hosted static site's default probe records %+v, want both probe alerts over 'web'", got)
	}

	e, err = renderReleaseProbeEnv(t, obs+strings.Replace(probeSite, "runtime = forge.OnHosted {}", "runtime = forge.OnHosted {}\n        release_probe = forge.Probe {enabled = False}", 1))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got, _, err = resolveShapeAlerts(context.Background(), io.Discard, site, "prod", e); err != nil || got != nil {
		t.Errorf("a site whose probe is off records %v (%v), want nothing", got, err)
	}
}
