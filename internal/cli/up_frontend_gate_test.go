package cli

import (
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/projectstore"
)

// storeFor builds the feature reader `forge env up` uses, with the SAME
// shape-derivation the loader applies — so a case that leaves
// features.frontend unset gets the real derived default, not a
// hand-written one.
func storeFor(t *testing.T, cfg config.ProjectConfig) *projectstore.Store {
	t.Helper()
	config.ApplyDerivedDefaults(&cfg)
	return projectstore.New(&cfg)
}

// TestFrontendPhaseEnabled pins the rule the up orchestrator's frontend
// phase resolves on: an explicit forge.yaml value wins in both
// directions, and with no explicit value the RENDER decides.
//
// The regression it locks out: frontend topology lives in
// deploy/kcl/<env>/, so a project with no `frontends:` block in
// forge.yaml derives features.frontend = false. Gating the phase on that
// derived default silently skipped every KCL-declared frontend — the dev
// server never started, and the only loud symptom was a service that
// waits on a frontend port failing the post-launch readiness gate.
func TestFrontendPhaseEnabled(t *testing.T) {
	declared := &KCLEntities{Frontends: []FrontendEntity{{Name: "web", Path: "web"}}}
	none := &KCLEntities{}
	no, yes := false, true

	tests := []struct {
		name     string
		cfg      config.ProjectConfig
		entities *KCLEntities
		want     bool
	}{
		{
			name:     "kcl-only frontends, no forge.yaml inventory",
			cfg:      config.ProjectConfig{Name: "p", Kind: "service"},
			entities: declared,
			want:     true,
		},
		{
			name:     "backend-only project renders no frontends",
			cfg:      config.ProjectConfig{Name: "p", Kind: "service"},
			entities: none,
			want:     false,
		},
		{
			name:     "explicit off beats a render that declares frontends",
			cfg:      config.ProjectConfig{Name: "p", Kind: "service", Features: config.FeaturesConfig{Frontend: &no}},
			entities: declared,
			want:     false,
		},
		{
			name:     "explicit on beats a render that declares none",
			cfg:      config.ProjectConfig{Name: "p", Kind: "service", Features: config.FeaturesConfig{Frontend: &yes}},
			entities: none,
			want:     true,
		},
		{
			name:     "forge.yaml inventory still enables the phase",
			cfg:      config.ProjectConfig{Name: "p", Kind: "service", Frontends: []config.FrontendConfig{{Name: "web", Path: "frontends/web"}}},
			entities: declared,
			want:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := frontendPhaseEnabled(storeFor(t, tt.cfg), tt.entities); got != tt.want {
				t.Errorf("frontendPhaseEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFrontendPhaseEnabled_NoStore covers the outside-a-project path
// (no forge.yaml loaded): with no config to state an opinion, the render
// is the only source left.
func TestFrontendPhaseEnabled_NoStore(t *testing.T) {
	if !frontendPhaseEnabled(nil, &KCLEntities{Frontends: []FrontendEntity{{Name: "web"}}}) {
		t.Error("frontendPhaseEnabled(nil store, 1 frontend) = false, want true")
	}
	if frontendPhaseEnabled(nil, nil) {
		t.Error("frontendPhaseEnabled(nil store, nil entities) = true, want false")
	}
}
