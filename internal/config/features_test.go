package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func boolPtr(v bool) *bool { return &v }

// TestFeaturesConfig_ZeroValue_StableEnabled locks in the
// "absent features block → stable features ON" backwards-compat
// promise. Experimental features (Ingress, ExternalBuilds,
// Operators, StrictWiring) are explicitly OFF on a zero value and are
// covered by TestFeaturesConfig_ZeroValue_ExperimentalDisabled below.
func TestFeaturesConfig_ZeroValue_AllEnabled(t *testing.T) {
	var f FeaturesConfig

	methods := []struct {
		name string
		fn   func() bool
	}{
		{"ORMEnabled", f.ORMEnabled},
		{"CodegenEnabled", f.CodegenEnabled},
		{"MigrationsEnabled", f.MigrationsEnabled},
		{"CIEnabled", f.CIEnabled},
		{"BuildEnabled", f.BuildEnabled},
		{"ContractsEnabled", f.ContractsEnabled},
		{"DocsEnabled", f.DocsEnabled},
		{"FrontendEnabled", f.FrontendEnabled},
		{"ObservabilityEnabled", f.ObservabilityEnabled},
		{"HotReloadEnabled", f.HotReloadEnabled},
		{"PacksEnabled", f.PacksEnabled},
		{"DeployEnabled", f.DeployEnabled},
	}
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			if got := m.fn(); !got {
				t.Errorf("%s() on zero-value = %v, want true", m.name, got)
			}
		})
	}
}

// TestFeaturesConfig_ZeroValue_ExperimentalDisabled locks in the
// inverse: experimental features are OFF on a zero-value config
// (opt-in only). Keeps the default-OFF semantic asserted at the
// schema layer so refactors that flip the field type catch the
// regression here.
func TestFeaturesConfig_ZeroValue_ExperimentalDisabled(t *testing.T) {
	var f FeaturesConfig
	methods := []struct {
		name string
		fn   func() bool
	}{
		{"IngressEnabled", f.IngressEnabled},
		{"ExternalBuildsEnabled", f.ExternalBuildsEnabled},
		{"OperatorsEnabled", f.OperatorsEnabled},
		{"StrictWiringEnabled", f.StrictWiringEnabled},
	}
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			if got := m.fn(); got {
				t.Errorf("%s() on zero-value = %v, want false (experimental default OFF)", m.name, got)
			}
		})
	}
}

func TestFeaturesConfig_ExplicitlyTrue(t *testing.T) {
	f := FeaturesConfig{
		ORM:           boolPtr(true),
		Codegen:       boolPtr(true),
		Migrations:    boolPtr(true),
		CI:            boolPtr(true),
		Contracts:     boolPtr(true),
		Docs:          boolPtr(true),
		Frontend:      boolPtr(true),
		Observability: boolPtr(true),
		HotReload:     boolPtr(true),
		Deploy:        boolPtr(true),
	}

	methods := []struct {
		name string
		fn   func() bool
	}{
		{"ORMEnabled", f.ORMEnabled},
		{"CodegenEnabled", f.CodegenEnabled},
		{"MigrationsEnabled", f.MigrationsEnabled},
		{"CIEnabled", f.CIEnabled},
		{"DeployEnabled", f.DeployEnabled},
		{"ContractsEnabled", f.ContractsEnabled},
		{"DocsEnabled", f.DocsEnabled},
		{"FrontendEnabled", f.FrontendEnabled},
		{"ObservabilityEnabled", f.ObservabilityEnabled},
		{"HotReloadEnabled", f.HotReloadEnabled},
	}
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			if got := m.fn(); !got {
				t.Errorf("%s() with explicit true = %v, want true", m.name, got)
			}
		})
	}
}

func TestFeaturesConfig_ExplicitlyFalse(t *testing.T) {
	f := FeaturesConfig{
		ORM:           boolPtr(false),
		Codegen:       boolPtr(false),
		Migrations:    boolPtr(false),
		CI:            boolPtr(false),
		Build:         boolPtr(false),
		Contracts:     boolPtr(false),
		Docs:          boolPtr(false),
		Frontend:      boolPtr(false),
		Observability: boolPtr(false),
		HotReload:     boolPtr(false),
		Packs:         boolPtr(false),
		Deploy:        boolPtr(false),
		// Experimental fields are plain bool: zero value IS the "off"
		// case, so we don't need to set them.
	}

	methods := []struct {
		name string
		fn   func() bool
	}{
		{"ORMEnabled", f.ORMEnabled},
		{"CodegenEnabled", f.CodegenEnabled},
		{"MigrationsEnabled", f.MigrationsEnabled},
		{"CIEnabled", f.CIEnabled},
		{"BuildEnabled", f.BuildEnabled},
		{"DeployEnabled", f.DeployEnabled},
		{"ContractsEnabled", f.ContractsEnabled},
		{"DocsEnabled", f.DocsEnabled},
		{"FrontendEnabled", f.FrontendEnabled},
		{"ObservabilityEnabled", f.ObservabilityEnabled},
		{"HotReloadEnabled", f.HotReloadEnabled},
		{"PacksEnabled", f.PacksEnabled},
		{"IngressEnabled", f.IngressEnabled},
	}
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			if got := m.fn(); got {
				t.Errorf("%s() with explicit false = %v, want false", m.name, got)
			}
		})
	}
}

func TestFeaturesConfig_Mixed(t *testing.T) {
	f := FeaturesConfig{
		ORM:        boolPtr(true),
		Codegen:    boolPtr(false),
		Migrations: nil, // should default to true
		CI:         boolPtr(false),
		Deploy:     boolPtr(true), // stable: explicit true
		// Contracts, Docs, Frontend, Observability, HotReload all nil
	}

	tests := []struct {
		name string
		fn   func() bool
		want bool
	}{
		{"ORMEnabled (true)", f.ORMEnabled, true},
		{"CodegenEnabled (false)", f.CodegenEnabled, false},
		{"MigrationsEnabled (nil)", f.MigrationsEnabled, true},
		{"CIEnabled (false)", f.CIEnabled, false},
		{"DeployEnabled (true)", f.DeployEnabled, true},
		{"ContractsEnabled (nil)", f.ContractsEnabled, true},
		{"DocsEnabled (nil)", f.DocsEnabled, true},
		{"FrontendEnabled (nil)", f.FrontendEnabled, true},
		{"ObservabilityEnabled (nil)", f.ObservabilityEnabled, true},
		{"HotReloadEnabled (nil)", f.HotReloadEnabled, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fn(); got != tt.want {
				t.Errorf("%s = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestFeaturesConfig_YAMLRoundTrip(t *testing.T) {
	orig := FeaturesConfig{
		ORM:           boolPtr(true),
		Codegen:       boolPtr(false),
		Migrations:    nil,
		CI:            boolPtr(true),
		Contracts:     nil,
		Docs:          boolPtr(true),
		Frontend:      boolPtr(false),
		Observability: boolPtr(true),
		HotReload:     boolPtr(false),
		Deploy:        boolPtr(true),
		Experimental: ExperimentalConfig{
			Ingress: true,
		},
	}

	data, err := yaml.Marshal(&orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got FeaturesConfig
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Explicitly set fields must survive round-trip.
	checks := []struct {
		name string
		ptr  *bool
		want *bool
	}{
		{"ORM", got.ORM, boolPtr(true)},
		{"Codegen", got.Codegen, boolPtr(false)},
		{"Migrations", got.Migrations, nil},
		{"CI", got.CI, boolPtr(true)},
		{"Contracts", got.Contracts, nil},
		{"Docs", got.Docs, boolPtr(true)},
		{"Frontend", got.Frontend, boolPtr(false)},
		{"Observability", got.Observability, boolPtr(true)},
		{"HotReload", got.HotReload, boolPtr(false)},
		{"Deploy", got.Deploy, boolPtr(true)},
	}
	if !got.Experimental.Ingress {
		t.Errorf("Experimental.Ingress round-trip: got false, want true")
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if c.want == nil {
				if c.ptr != nil {
					t.Errorf("%s: got %v, want nil", c.name, *c.ptr)
				}
				return
			}
			if c.ptr == nil {
				t.Fatalf("%s: got nil, want %v", c.name, *c.want)
			}
			if *c.ptr != *c.want {
				t.Errorf("%s: got %v, want %v", c.name, *c.ptr, *c.want)
			}
		})
	}
}

// TestFeaturesConfig_NewFeatures_ZeroValue locks in the additive
// contract for the build/packs/starters fields: a forge.yaml without
// a `features:` block (or with the block but those fields absent)
// must report each feature as enabled. This is the backwards-compat
// promise for projects scaffolded before the field was introduced.
func TestFeaturesConfig_NewFeatures_ZeroValue(t *testing.T) {
	var f FeaturesConfig
	if !f.BuildEnabled() {
		t.Error("BuildEnabled() on zero-value = false, want true")
	}
	if !f.PacksEnabled() {
		t.Error("PacksEnabled() on zero-value = false, want true")
	}
}

// TestFeaturesConfig_NewFeatures_ExplicitFalse covers the
// `features.build/packs: false` opt-out path used by
// `forge new --kind cli/library` and explicit user disabling.
func TestFeaturesConfig_NewFeatures_ExplicitFalse(t *testing.T) {
	f := FeaturesConfig{
		Build: boolPtr(false),
		Packs: boolPtr(false),
	}
	if f.BuildEnabled() {
		t.Error("BuildEnabled() with explicit false = true, want false")
	}
	if f.PacksEnabled() {
		t.Error("PacksEnabled() with explicit false = true, want false")
	}
}

// TestDisabledFeatureError_Format pins the exact user-visible string
// produced by the canonical feature-disabled helper. The wording is
// load-bearing — sub-agents grep for "feature '...' is disabled in
// forge.yaml" to recognise the gate; humans see it in CLI error
// output. A drift here without a deliberate spec change would break
// downstream tooling.
func TestDisabledFeatureError_Format(t *testing.T) {
	// Stable feature uses the historical "is disabled in forge.yaml" idiom.
	err := DisabledFeatureError(FeatureBuild)
	if err == nil {
		t.Fatal("DisabledFeatureError returned nil")
	}
	want := "feature 'build' is disabled in forge.yaml. Set features.build: true to enable."
	if err.Error() != want {
		t.Errorf("DisabledFeatureError text mismatch\n got: %q\nwant: %q", err.Error(), want)
	}
	// Deploy is stable now — its message must use the stable idiom and
	// the top-level YAML path, NOT the experimental nested one.
	depErr := DisabledFeatureError(FeatureDeploy)
	if depErr == nil {
		t.Fatal("DisabledFeatureError(deploy) returned nil")
	}
	depWant := "feature 'deploy' is disabled in forge.yaml. Set features.deploy: true to enable."
	if depErr.Error() != depWant {
		t.Errorf("DisabledFeatureError(deploy) text mismatch\n got: %q\nwant: %q", depErr.Error(), depWant)
	}
	// Experimental feature carries an experimental-flavoured message.
	expErr := DisabledFeatureError(FeatureIngress)
	if expErr == nil {
		t.Fatal("DisabledFeatureError(ingress) returned nil")
	}
	expGot := expErr.Error()
	if !strings.Contains(expGot, "feature 'ingress' is experimental") {
		t.Errorf("experimental DisabledFeatureError missing 'experimental' marker: %q", expGot)
	}
	if !strings.Contains(expGot, "features.experimental.ingress: true") {
		t.Errorf("experimental DisabledFeatureError missing nested opt-in hint: %q", expGot)
	}
}

// TestEffectiveFeatures_MapShape asserts that every Feature*
// constant declared by the config package is keyed in the map
// returned by EffectiveFeatures(). The map is the wire shape
// `forge audit --json | jq '.features.details.resolved'` reads,
// and the additive-extension contract requires every constant to
// be present (sub-agents check `.<feature> == true|false`).
func TestEffectiveFeatures_MapShape(t *testing.T) {
	stable := []string{
		FeatureORM, FeatureCodegen, FeatureMigrations, FeatureCI,
		FeatureBuild, FeatureContracts, FeatureDocs,
		FeatureFrontend, FeatureObservability, FeatureHotReload,
		FeaturePacks, FeatureDeploy,
	}
	var f FeaturesConfig
	resolved := f.EffectiveFeatures()
	wantLen := len(stable) + len(ExperimentalFeatureNames)
	if len(resolved) != wantLen {
		t.Errorf("EffectiveFeatures len = %d, want %d", len(resolved), wantLen)
	}
	for _, name := range stable {
		v, ok := resolved[name]
		if !ok {
			t.Errorf("EffectiveFeatures missing stable key %q", name)
			continue
		}
		if !v {
			t.Errorf("EffectiveFeatures[%q] = false, want true (stable zero-value defaults)", name)
		}
	}
	for _, name := range ExperimentalFeatureNames {
		v, ok := resolved[name]
		if !ok {
			t.Errorf("EffectiveFeatures missing experimental key %q", name)
			continue
		}
		if v {
			t.Errorf("EffectiveFeatures[%q] = true, want false (experimental zero-value defaults)", name)
		}
	}
}

func TestFeaturesConfig_YAMLMissingFeaturesSection(t *testing.T) {
	// A ProjectConfig YAML with no features key at all.
	yamlStr := `
name: testproject
module_path: github.com/test/project
version: "1.0"
`
	var cfg ProjectConfig
	if err := yaml.Unmarshal([]byte(yamlStr), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Stable features should return true (backwards compat defaults).
	// Experimental features are default OFF; they're asserted by
	// TestFeaturesConfig_ZeroValue_ExperimentalDisabled.
	methods := []struct {
		name string
		fn   func() bool
	}{
		{"ORMEnabled", cfg.Features.ORMEnabled},
		{"CodegenEnabled", cfg.Features.CodegenEnabled},
		{"MigrationsEnabled", cfg.Features.MigrationsEnabled},
		{"CIEnabled", cfg.Features.CIEnabled},
		{"BuildEnabled", cfg.Features.BuildEnabled},
		{"ContractsEnabled", cfg.Features.ContractsEnabled},
		{"DocsEnabled", cfg.Features.DocsEnabled},
		{"FrontendEnabled", cfg.Features.FrontendEnabled},
		{"ObservabilityEnabled", cfg.Features.ObservabilityEnabled},
		{"HotReloadEnabled", cfg.Features.HotReloadEnabled},
		{"PacksEnabled", cfg.Features.PacksEnabled},
	}
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			if got := m.fn(); !got {
				t.Errorf("%s() with missing features section = %v, want true", m.name, got)
			}
		})
	}
}

func TestStackConfig_Defaults(t *testing.T) {
	var s StackConfig

	tests := []struct {
		name string
		fn   func() string
		want string
	}{
		{"EffectiveBackendLanguage", s.EffectiveBackendLanguage, "go"},
		{"EffectiveFrontendFramework", s.EffectiveFrontendFramework, "nextjs"},
		{"EffectiveDatabaseDriver", s.EffectiveDatabaseDriver, "postgres"},
		{"EffectiveProtoProvider", s.EffectiveProtoProvider, "buf"},
		{"EffectiveDeployTarget", s.EffectiveDeployTarget, "k8s"},
		{"EffectiveCIProvider", s.EffectiveCIProvider, "github"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fn(); got != tt.want {
				t.Errorf("%s() on zero-value = %q, want %q", tt.name, got, tt.want)
			}
		})
	}

	// IsProtoEnabled defaults to true when nil.
	if got := s.IsProtoEnabled(); !got {
		t.Errorf("IsProtoEnabled() on zero-value = %v, want true", got)
	}
}

func TestStackConfig_WithValues(t *testing.T) {
	s := StackConfig{
		Backend:  StackBackend{Language: "rust", Framework: "axum"},
		Frontend: StackFrontend{Framework: "svelte"},
		Database: StackDatabase{Driver: "none"},
		Proto:    StackProto{Enabled: boolPtr(false), Provider: "protoc"},
		Deploy:   StackDeploy{Target: "cloudrun", Provider: "gke", Registry: "gcr.io"},
		CI:       StackCI{Provider: "gitlab"},
	}

	stringTests := []struct {
		name string
		fn   func() string
		want string
	}{
		{"EffectiveBackendLanguage", s.EffectiveBackendLanguage, "rust"},
		{"EffectiveFrontendFramework", s.EffectiveFrontendFramework, "svelte"},
		{"EffectiveDatabaseDriver", s.EffectiveDatabaseDriver, "none"},
		{"EffectiveProtoProvider", s.EffectiveProtoProvider, "protoc"},
		{"EffectiveDeployTarget", s.EffectiveDeployTarget, "cloudrun"},
		{"EffectiveCIProvider", s.EffectiveCIProvider, "gitlab"},
	}
	for _, tt := range stringTests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fn(); got != tt.want {
				t.Errorf("%s() = %q, want %q", tt.name, got, tt.want)
			}
		})
	}

	// Proto explicitly disabled.
	if got := s.IsProtoEnabled(); got {
		t.Errorf("IsProtoEnabled() with explicit false = %v, want false", got)
	}
}

func TestStackConfig_YAMLRoundTrip(t *testing.T) {
	orig := StackConfig{
		Backend:  StackBackend{Language: "python", Framework: "fastapi"},
		Frontend: StackFrontend{Framework: "react-native"},
		Database: StackDatabase{Driver: "postgres"},
		Proto:    StackProto{Enabled: boolPtr(true), Provider: "buf"},
		Deploy:   StackDeploy{Target: "fly", Provider: "fly", Registry: "registry.fly.io"},
		CI:       StackCI{Provider: "circleci"},
	}

	data, err := yaml.Marshal(&orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got StackConfig
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.EffectiveBackendLanguage() != "python" {
		t.Errorf("round-trip BackendLanguage = %q, want %q", got.EffectiveBackendLanguage(), "python")
	}
	if got.EffectiveFrontendFramework() != "react-native" {
		t.Errorf("round-trip FrontendFramework = %q, want %q", got.EffectiveFrontendFramework(), "react-native")
	}
	if got.EffectiveDatabaseDriver() != "postgres" {
		t.Errorf("round-trip DatabaseDriver = %q, want %q", got.EffectiveDatabaseDriver(), "postgres")
	}
	if !got.IsProtoEnabled() {
		t.Errorf("round-trip IsProtoEnabled = false, want true")
	}
	if got.EffectiveProtoProvider() != "buf" {
		t.Errorf("round-trip ProtoProvider = %q, want %q", got.EffectiveProtoProvider(), "buf")
	}
	if got.EffectiveDeployTarget() != "fly" {
		t.Errorf("round-trip DeployTarget = %q, want %q", got.EffectiveDeployTarget(), "fly")
	}
	if got.EffectiveCIProvider() != "circleci" {
		t.Errorf("round-trip CIProvider = %q, want %q", got.EffectiveCIProvider(), "circleci")
	}
}
