package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func boolPtr(v bool) *bool { return &v }

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
				t.Errorf("%s() on zero-value = %v, want true", m.name, got)
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
		Deploy:        boolPtr(true),
		Contracts:     boolPtr(true),
		Docs:          boolPtr(true),
		Frontend:      boolPtr(true),
		Observability: boolPtr(true),
		HotReload:     boolPtr(true),
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
		Deploy:        boolPtr(false),
		Contracts:     boolPtr(false),
		Docs:          boolPtr(false),
		Frontend:      boolPtr(false),
		Observability: boolPtr(false),
		HotReload:     boolPtr(false),
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
		Deploy:     boolPtr(true),
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
		Deploy:        boolPtr(false),
		Contracts:     nil,
		Docs:          boolPtr(true),
		Frontend:      boolPtr(false),
		Observability: boolPtr(true),
		HotReload:     boolPtr(false),
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
		{"Deploy", got.Deploy, boolPtr(false)},
		{"Contracts", got.Contracts, nil},
		{"Docs", got.Docs, boolPtr(true)},
		{"Frontend", got.Frontend, boolPtr(false)},
		{"Observability", got.Observability, boolPtr(true)},
		{"HotReload", got.HotReload, boolPtr(false)},
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

	// All feature methods should return true (backwards compat defaults).
	methods := []struct {
		name string
		fn   func() bool
	}{
		{"ORMEnabled", cfg.Features.ORMEnabled},
		{"CodegenEnabled", cfg.Features.CodegenEnabled},
		{"MigrationsEnabled", cfg.Features.MigrationsEnabled},
		{"CIEnabled", cfg.Features.CIEnabled},
		{"DeployEnabled", cfg.Features.DeployEnabled},
		{"ContractsEnabled", cfg.Features.ContractsEnabled},
		{"DocsEnabled", cfg.Features.DocsEnabled},
		{"FrontendEnabled", cfg.Features.FrontendEnabled},
		{"ObservabilityEnabled", cfg.Features.ObservabilityEnabled},
		{"HotReloadEnabled", cfg.Features.HotReloadEnabled},
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
		Database: StackDatabase{Driver: "sqlite"},
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
		{"EffectiveDatabaseDriver", s.EffectiveDatabaseDriver, "sqlite"},
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
		Database: StackDatabase{Driver: "mysql"},
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
	if got.EffectiveDatabaseDriver() != "mysql" {
		t.Errorf("round-trip DatabaseDriver = %q, want %q", got.EffectiveDatabaseDriver(), "mysql")
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
