package config

import (
	"testing"

	"go.yaml.in/yaml/v3"
)

// legacyFullForgeYAML is what `forge new demo --service users
// --frontend web` wrote BEFORE the minimal-scaffold change: every
// derived default materialized explicitly. The derivation contract is
// that loading the minimal file must be semantically identical to
// loading this one.
const legacyFullForgeYAML = `
name: demo
module_path: example.com/demo
kind: service
version: 0.1.0
forge_version: v0.0.0-test
hot_reload: true
services:
    - name: users
      type: go_service
      path: handlers/users
      port: 8080
frontends:
    - name: web
      type: nextjs
      path: frontends/web
      port: 3000
database:
    driver: postgres
    migrations_dir: db/migrations
    sqlc_enabled: false
    migration_safety:
        enabled: true
        unsafe_add_column: error
        destructive_change: error
        volatile_default: warn
ci:
    provider: github
    lint:
        golangci: true
        buf: true
        buf_breaking: true
        frontend: true
        migration_safety: true
    test:
        race: true
        coverage: false
    vuln_scan:
        go: true
        docker: true
        npm: true
deploy:
    provider: github
docker:
    registry: ghcr.io
k8s:
    kcl_dir: deploy/kcl
lint:
    contract: true
    frontend:
        css_health: true
        no_important: warn
        no_inline_styles: warn
contracts:
    strict: true
    allow_exported_vars: false
    allow_exported_funcs: false
    exclude: []
auth:
    provider: none
features:
    orm: true
    codegen: true
    migrations: true
    ci: true
    build: true
    contracts: true
    docs: true
    frontend: true
    observability: true
    hot_reload: true
    packs: true
    starters: true
`

// minimalForgeYAML is the post-change scaffold output for the same
// project shape.
const minimalForgeYAML = `
name: demo
module_path: example.com/demo
forge_version: v0.0.0-test
services:
    - name: users
      type: go_service
      path: handlers/users
      port: 8080
frontends:
    - name: web
      type: nextjs
      path: frontends/web
      port: 3000
`

// TestDerivedDefaults_MinimalEquivalentToLegacyFull is the load-side
// contract of the minimal scaffold: a minimal forge.yaml must resolve
// to the same effective configuration as the legacy fully-materialized
// one — same feature states, same section blocks, same hot-reload.
func TestDerivedDefaults_MinimalEquivalentToLegacyFull(t *testing.T) {
	legacy, err := LoadStrict([]byte(legacyFullForgeYAML), "legacy.yaml")
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	minimal, err := LoadStrict([]byte(minimalForgeYAML), "minimal.yaml")
	if err != nil {
		t.Fatalf("load minimal: %v", err)
	}

	// Features resolve identically.
	lf, mf := legacy.Features.EffectiveFeatures(), minimal.Features.EffectiveFeatures()
	for name, want := range lf {
		if mf[name] != want {
			t.Errorf("feature %q: minimal resolves %v, legacy resolves %v", name, mf[name], want)
		}
	}

	// Section blocks resolve identically (compare canonical YAML).
	sections := map[string][2]any{
		"database":  {legacy.Database, minimal.Database},
		"ci":        {legacy.CI, minimal.CI},
		"deploy":    {legacy.Deploy, minimal.Deploy},
		"docker":    {legacy.Docker, minimal.Docker},
		"k8s":       {legacy.K8s, minimal.K8s},
		"lint":      {legacy.Lint, minimal.Lint},
		"contracts": {legacy.Contracts, minimal.Contracts},
		"auth":      {legacy.Auth, minimal.Auth},
	}
	for name, pair := range sections {
		a, _ := yaml.Marshal(pair[0])
		b, _ := yaml.Marshal(pair[1])
		if string(a) != string(b) {
			t.Errorf("section %q diverges:\nlegacy:\n%s\nminimal:\n%s", name, a, b)
		}
	}

	if legacy.EffectiveHotReload() != minimal.EffectiveHotReload() {
		t.Errorf("EffectiveHotReload: legacy %v, minimal %v",
			legacy.EffectiveHotReload(), minimal.EffectiveHotReload())
	}
	if legacy.EffectiveKind() != minimal.EffectiveKind() {
		t.Errorf("EffectiveKind: legacy %q, minimal %q", legacy.EffectiveKind(), minimal.EffectiveKind())
	}
}

// TestDerivedDefaults_RealProjectShapesMatchExplicit pins the
// derivation rules against the shapes of the two reference production
// projects (cp-forge, kalshi-trader): kind=service, services+frontends
// non-empty, postgres database. Both spell out features: orm, codegen,
// migrations, ci, contracts, docs, frontend, observability, hot_reload
// — all true — and leave build/packs/starters absent (historically →
// enabled). Derivation must resolve every one of those to the same
// value, so deleting their features: block would be a no-op.
func TestDerivedDefaults_RealProjectShapesMatchExplicit(t *testing.T) {
	shape := &ProjectConfig{
		Kind:      ProjectKindService,
		Services:  []ServiceConfig{{Name: "api"}},
		Frontends: []FrontendConfig{{Name: "web"}},
		Database:  DatabaseConfig{Driver: "postgres"},
	}
	derived := DeriveFeatureDefaults(shape)
	for _, name := range []FeatureName{
		FeatureORM, FeatureCodegen, FeatureMigrations, FeatureCI,
		FeatureBuild, FeatureContracts, FeatureDocs, FeatureFrontend,
		FeatureObservability, FeatureHotReload, FeaturePacks, FeatureStarters,
	} {
		if !derived[name] {
			t.Errorf("reference-shape derivation: feature %q = false, want true (must match the explicit `true` in cp-forge/kalshi forge.yaml)", name)
		}
	}
}

// TestNormalizeForWrite_RoundTripStaysMinimal: load(minimal) → write
// must not materialize derived defaults back into the file.
func TestNormalizeForWrite_RoundTripStaysMinimal(t *testing.T) {
	cfg, err := LoadStrict([]byte(minimalForgeYAML), "minimal.yaml")
	if err != nil {
		t.Fatalf("load minimal: %v", err)
	}
	out, err := yaml.Marshal(NormalizeForWrite(cfg))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reparsed map[string]any
	if err := yaml.Unmarshal(out, &reparsed); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	allowed := map[string]bool{
		"name": true, "module_path": true, "forge_version": true,
		"services": true, "frontends": true,
	}
	for key := range reparsed {
		if !allowed[key] {
			t.Errorf("round-trip materialized derived key %q:\n%s", key, out)
		}
	}
}

// TestNormalizeForWrite_OverridesSurvive: values that DIFFER from
// derivation must round-trip — normalization only strips boilerplate.
func TestNormalizeForWrite_OverridesSurvive(t *testing.T) {
	src := minimalForgeYAML + `
features:
    ci: false
database:
    driver: sqlite
    migrations_dir: db/migrations
`
	cfg, err := LoadStrict([]byte(src), "override.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	out, err := yaml.Marshal(NormalizeForWrite(cfg))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	re, err := LoadStrict(out, "roundtrip.yaml")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if re.Features.CIEnabled() {
		t.Errorf("explicit features.ci: false lost in round-trip:\n%s", out)
	}
	if re.Database.Driver != "sqlite" {
		t.Errorf("explicit database.driver: sqlite lost in round-trip (got %q):\n%s", re.Database.Driver, out)
	}
}

// TestNormalizeForWrite_DisableMigrationsSurvivesEmptyDatabase guards
// the subtle case: a scaffold-time `--disable migrations` on a config
// whose database section is still absent. Derivation against the RAW
// (unfilled) shape would say migrations=off (no driver) and wrongly
// strip the explicit false; derivation must run against the effective
// (filled) shape instead.
func TestNormalizeForWrite_DisableMigrationsSurvivesEmptyDatabase(t *testing.T) {
	off := false
	cfg := &ProjectConfig{
		Name:       "demo",
		ModulePath: "example.com/demo",
		Features:   FeaturesConfig{Migrations: &off},
	}
	out := NormalizeForWrite(cfg)
	if out.Features.Migrations == nil || *out.Features.Migrations {
		t.Error("explicit migrations: false was stripped — NormalizeForWrite must derive against the filled (postgres) shape")
	}
}

// TestDeriveFeatureDefaults_PerKindMatrix pins the per-kind derivation
// matrix that replaced the scaffold-time explicit features block.
func TestDeriveFeatureDefaults_PerKindMatrix(t *testing.T) {
	cases := []struct {
		name string
		cfg  *ProjectConfig
		want map[FeatureName]bool
	}{
		{
			name: "service with db, no frontends",
			cfg: &ProjectConfig{
				Kind:     ProjectKindService,
				Database: DatabaseConfig{Driver: "postgres"},
			},
			want: map[FeatureName]bool{
				FeatureORM: true, FeatureCodegen: true, FeatureMigrations: true,
				FeatureCI: true, FeatureBuild: true, FeatureContracts: true,
				FeatureDocs: true, FeatureFrontend: false, FeatureObservability: true,
				FeatureHotReload: true, FeaturePacks: true, FeatureStarters: true,
			},
		},
		{
			name: "service with driver none",
			cfg: &ProjectConfig{
				Kind:     ProjectKindService,
				Database: DatabaseConfig{Driver: "none"},
			},
			want: map[FeatureName]bool{
				FeatureORM: false, FeatureMigrations: false, FeatureCodegen: true,
			},
		},
		{
			name: "cli",
			cfg:  &ProjectConfig{Kind: ProjectKindCLI},
			want: map[FeatureName]bool{
				FeatureORM: false, FeatureCodegen: false, FeatureMigrations: false,
				FeatureCI: true, FeatureBuild: true, FeatureContracts: true,
				FeatureDocs: true, FeatureFrontend: false, FeatureObservability: false,
				FeatureHotReload: false, FeaturePacks: false, FeatureStarters: false,
			},
		},
		{
			name: "library",
			cfg:  &ProjectConfig{Kind: ProjectKindLibrary},
			want: map[FeatureName]bool{
				FeatureCI: false, FeatureBuild: false, FeatureContracts: true,
				FeatureDocs: true, FeatureCodegen: false,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveFeatureDefaults(tc.cfg)
			for name, want := range tc.want {
				if got[name] != want {
					t.Errorf("%s: derived %q = %v, want %v", tc.name, name, got[name], want)
				}
			}
		})
	}
}

// TestFeaturesDeploy_TopLevelKeyLoads pins the deploy promotion: a
// forge.yaml that sets `features: deploy: true` must load without an
// unknown-key error (deploy is a stable, top-level feature flag — not
// `features.experimental.deploy`).
func TestFeaturesDeploy_TopLevelKeyLoads(t *testing.T) {
	src := minimalForgeYAML + `
features:
    deploy: true
`
	cfg, err := LoadStrict([]byte(src), "deploy.yaml")
	if err != nil {
		t.Fatalf("LoadStrict with `features: deploy: true`: %v", err)
	}
	if !cfg.Features.DeployEnabled() {
		t.Error("DeployEnabled() = false, want true (explicit features.deploy: true)")
	}
}

// TestDeriveFeatureDefaults_DeployFollowsKind pins the derivation rule
// deploy ⇔ kind == service: a pristine service scaffold has deploy on
// (its deploy/kcl tree is shipped by the scaffold and the generate
// pipeline must emit deploy/kcl/<env>/config_gen.k for the scaffold's
// own main.k import to resolve); cli/library kinds have no deploy
// surface. An explicit false always wins over derivation.
func TestDeriveFeatureDefaults_DeployFollowsKind(t *testing.T) {
	// Fresh service-kind config (minimal scaffold): deploy derives ON.
	svc, err := LoadStrict([]byte(minimalForgeYAML), "svc.yaml")
	if err != nil {
		t.Fatalf("load minimal service: %v", err)
	}
	if !svc.Features.DeployEnabled() {
		t.Error("service kind: DeployEnabled() = false, want true (derived from kind)")
	}

	// CLI and library kinds derive OFF.
	for _, kind := range []string{ProjectKindCLI, ProjectKindLibrary} {
		src := `
name: demo
module_path: example.com/demo
kind: ` + kind + `
`
		cfg, err := LoadStrict([]byte(src), kind+".yaml")
		if err != nil {
			t.Fatalf("load %s: %v", kind, err)
		}
		if cfg.Features.DeployEnabled() {
			t.Errorf("%s kind: DeployEnabled() = true, want false (derived from kind)", kind)
		}
	}

	// Explicit false wins over the service-kind derivation.
	off := minimalForgeYAML + `
features:
    deploy: false
`
	cfg, err := LoadStrict([]byte(off), "off.yaml")
	if err != nil {
		t.Fatalf("load explicit-off: %v", err)
	}
	if cfg.Features.DeployEnabled() {
		t.Error("explicit features.deploy: false on service kind: DeployEnabled() = true, want false")
	}

	// Derivation map carries the deploy key.
	if got := DeriveFeatureDefaults(&ProjectConfig{Kind: ProjectKindService}); !got[FeatureDeploy] {
		t.Error("DeriveFeatureDefaults(service)[deploy] = false, want true")
	}
	if got := DeriveFeatureDefaults(&ProjectConfig{Kind: ProjectKindCLI}); got[FeatureDeploy] {
		t.Error("DeriveFeatureDefaults(cli)[deploy] = true, want false")
	}
}

// TestNormalizeForWrite_DeployDroppedWhenMatchingDerivation: an explicit
// deploy value equal to the kind-derived default is boilerplate and must
// be stripped on write; a differing value survives.
func TestNormalizeForWrite_DeployDroppedWhenMatchingDerivation(t *testing.T) {
	on := true
	off := false
	// service + deploy:true matches derivation → dropped.
	cfg := &ProjectConfig{Name: "d", ModulePath: "x/d", Features: FeaturesConfig{Deploy: &on}}
	if out := NormalizeForWrite(cfg); out.Features.Deploy != nil {
		t.Error("deploy: true on service kind should be dropped (matches derivation)")
	}
	// service + deploy:false differs → survives.
	cfg = &ProjectConfig{Name: "d", ModulePath: "x/d", Features: FeaturesConfig{Deploy: &off}}
	if out := NormalizeForWrite(cfg); out.Features.Deploy == nil || *out.Features.Deploy {
		t.Error("deploy: false on service kind must survive NormalizeForWrite")
	}
}
