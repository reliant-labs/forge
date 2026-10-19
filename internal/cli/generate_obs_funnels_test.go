package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/obsdash"
)

func funnelFrontend(env string, funnels ...FunnelEntity) renderedEnvFrontend {
	return renderedEnvFrontend{Env: env, Frontend: FrontendEntity{Name: "web", Telemetry: &FrontendTelemetryEntity{Funnels: funnels}}}
}

func TestDeclaredFunnels_SortedAndDeduplicatedAcrossEnvs(t *testing.T) {
	onboarding := FunnelEntity{ID: "onboarding", Title: "Onboarding", Steps: []string{"a", "b"}, Window: "7d", By: "user"}
	checkout := FunnelEntity{ID: "checkout", Steps: []string{"c", "d"}, Window: "1h", By: "session"}
	got, err := declaredFunnels([]renderedEnvFrontend{
		funnelFrontend("dev", onboarding, checkout),
		funnelFrontend("prod", checkout, onboarding),
		{Env: "prod", Frontend: FrontendEntity{Name: "admin"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "checkout" || got[1].ID != "onboarding" {
		t.Fatalf("funnels = %+v, want checkout then onboarding, once each", got)
	}
	if o := got[1]; o.Title != "Onboarding" || o.Window != "7d" || o.By != obsdash.FunnelByUser || len(o.Steps) != 2 {
		t.Errorf("onboarding = %+v, lost a field", o)
	}
}

func TestDeclaredFunnels_RefusesTwoDefinitionsUnderOneID(t *testing.T) {
	_, err := declaredFunnels([]renderedEnvFrontend{
		funnelFrontend("dev", FunnelEntity{ID: "f", Steps: []string{"a", "b"}, Window: "1h", By: "session"}),
		funnelFrontend("prod", FunnelEntity{ID: "f", Steps: []string{"a", "c"}, Window: "1h", By: "session"}),
	})
	if err == nil || !strings.Contains(err.Error(), `funnel "f" is declared twice`) || !strings.Contains(err.Error(), `"dev"`) || !strings.Contains(err.Error(), `"prod"`) {
		t.Fatalf("err = %v, want a refusal naming the funnel and both envs", err)
	}
}

// TestDeclaredFunnels_FromRealKCL is the whole path: funnels written in an env's
// KCL reach the frontend dashboard as funnel panels.
func TestDeclaredFunnels_FromRealKCL(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; runs in the full tier")
	}
	dir := workloadURLProject(t, "dev", `    project = "acme"
    frontends = [forge.Frontend {
        name = "web", path = "frontends/web", type = "vite", port = 5173
        runtime = forge.OnHost {}
        telemetry = forge.BrowserTelemetry {funnels = [
            forge.Funnel {id = "onboarding", title = "Onboarding", steps = ["signup_started", "first_chat"], window = "7d", by = "user"}
            forge.Funnel {id = "checkout", steps = ["cart_viewed", "paid"]}
        ]}
    }]`)
	if err := os.MkdirAll(filepath.Join(dir, "frontends", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	funnels, err := declaredFunnels(renderedFrontends(dir))
	if err != nil {
		t.Fatal(err)
	}
	spec := obsdash.Build(obsdash.Input{Project: "acme", Frontends: []string{"web"}, Funnels: funnels})
	panels := map[string]obsdash.Panel{}
	for _, d := range spec.Dashboards {
		for _, p := range d.Panels {
			panels[p.ID] = p
		}
	}
	on, ok := panels["funnel-onboarding"]
	if !ok || on.Title != "Onboarding" || on.Funnel == nil || on.Funnel.Window != "7d" || on.Funnel.By != obsdash.FunnelByUser {
		t.Errorf("funnel-onboarding = %+v", on)
	}
	co, ok := panels["funnel-checkout"]
	if !ok || co.Title != "checkout" || co.Funnel == nil || co.Funnel.Window != obsdash.DefaultFunnelWindow || co.Funnel.By != obsdash.DefaultFunnelBy {
		t.Errorf("funnel-checkout = %+v, want the KCL defaults", co)
	}
}
