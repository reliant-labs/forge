package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// The whole path for hounders' `web`: `domains` declared on an OnHosted
// frontend renders into FrontendEntity, is lowered into the StaticSite spec
// the control plane stores, and survives the admission the deploy runs.
//
// MUTATIONS VERIFIED RED:
//   - dropping `"domains"` from _render_frontend → the entity decodes empty;
//   - dropping the Domains assignment in hostedStaticSpec → the published
//     spec carries no domains, which is the silent-drop this whole change
//     exists to make impossible.
func TestFrontendCustomDomains_ReachTheStaticSiteSpec(t *testing.T) {
	e := renderFrontendEnv(t, `[forge.Frontend {
        name = "web"
        path = "web"
        type = "vite"
        domains = ["hounders.club", "www.hounders.club"]
        runtime = forge.OnHosted {}
    }]`, `    control_plane = forge.ControlPlane {}`)

	if len(e.Frontends) != 1 {
		t.Fatalf("frontends = %+v", e.Frontends)
	}
	if got := strings.Join(e.Frontends[0].Domains, ","); got != "hounders.club,www.hounders.club" {
		t.Fatalf("FrontendEntity.Domains = %q, want both names in declaration order", got)
	}

	group, err := buildHostedGroup("prod", e)
	if err != nil || group == nil {
		t.Fatalf("buildHostedGroup: %v", err)
	}
	var spec *v1alpha1.StaticSiteSpec
	for _, s := range group.Services {
		if s.Name == "web" && s.Hosted != nil && s.Hosted.Tier == deploytarget.HostedTierStatic {
			spec = s.Hosted.Static
		}
	}
	if spec == nil {
		t.Fatalf("the hosted frontend was not published as a StaticSite: %+v", group.Services)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"keepReleases":10,"domains":["hounders.club","www.hounders.club"]}`
	if string(raw) != want {
		t.Errorf("published StaticSite spec:\n got %s\nwant %s", raw, want)
	}
	if _, err := deploytarget.PreflightHosted(*group); err != nil {
		t.Errorf("a frontend with custom domains is not admissible: %v", err)
	}
}

// A frontend that declares NO domains publishes the spec it always did — the
// key is absent, not empty, so an existing hosted site's published document
// is byte-identical to what it was before custom domains existed.
func TestFrontendCustomDomains_AbsentWhenNotDeclared(t *testing.T) {
	spec := hostedStaticSpec(FrontendEntity{Name: "web"})
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "domains") {
		t.Errorf("a frontend declaring no domains published %s", raw)
	}
}
