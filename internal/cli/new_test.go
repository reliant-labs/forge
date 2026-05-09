package cli

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

func newTestGen() *generator.ProjectGenerator {
	return generator.NewProjectGenerator("test", "/tmp/test", "github.com/example/test")
}

// allFeaturesEnabled returns true when every feature reports enabled (nil = default = enabled).
func allFeaturesEnabled(gen *generator.ProjectGenerator) bool {
	f := gen.Features
	return f.ORMEnabled() &&
		f.CodegenEnabled() &&
		f.MigrationsEnabled() &&
		f.CIEnabled() &&
		f.DeployEnabled() &&
		f.ContractsEnabled() &&
		f.DocsEnabled() &&
		f.FrontendEnabled() &&
		f.ObservabilityEnabled() &&
		f.HotReloadEnabled()
}

func TestApplyDisableFlags_EmptyList(t *testing.T) {
	gen := newTestGen()
	if err := applyDisableFlags(gen, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allFeaturesEnabled(gen) {
		t.Error("expected all features enabled with nil disable list")
	}

	gen = newTestGen()
	if err := applyDisableFlags(gen, []string{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allFeaturesEnabled(gen) {
		t.Error("expected all features enabled with empty disable list")
	}
}

func TestApplyDisableFlags_SingleFeature(t *testing.T) {
	gen := newTestGen()
	if err := applyDisableFlags(gen, []string{"deploy"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gen.Features.DeployEnabled() {
		t.Error("expected deploy to be disabled")
	}
	// Other features remain enabled (nil).
	if !gen.Features.ORMEnabled() {
		t.Error("expected orm to remain enabled")
	}
	if !gen.Features.CIEnabled() {
		t.Error("expected ci to remain enabled")
	}
	if !gen.Features.ObservabilityEnabled() {
		t.Error("expected observability to remain enabled")
	}
}

func TestApplyDisableFlags_MultipleFeatures(t *testing.T) {
	gen := newTestGen()
	if err := applyDisableFlags(gen, []string{"deploy", "ci", "observability"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gen.Features.DeployEnabled() {
		t.Error("expected deploy to be disabled")
	}
	if gen.Features.CIEnabled() {
		t.Error("expected ci to be disabled")
	}
	if gen.Features.ObservabilityEnabled() {
		t.Error("expected observability to be disabled")
	}
	// Others still enabled.
	if !gen.Features.ORMEnabled() {
		t.Error("expected orm to remain enabled")
	}
	if !gen.Features.FrontendEnabled() {
		t.Error("expected frontend to remain enabled")
	}
}

func TestApplyDisableFlags_AllFeatures(t *testing.T) {
	gen := newTestGen()
	all := []string{"orm", "codegen", "migrations", "ci", "deploy", "contracts", "docs", "frontend", "observability", "hot_reload"}
	if err := applyDisableFlags(gen, all); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f := gen.Features
	if f.ORMEnabled() {
		t.Error("orm should be disabled")
	}
	if f.CodegenEnabled() {
		t.Error("codegen should be disabled")
	}
	if f.MigrationsEnabled() {
		t.Error("migrations should be disabled")
	}
	if f.CIEnabled() {
		t.Error("ci should be disabled")
	}
	if f.DeployEnabled() {
		t.Error("deploy should be disabled")
	}
	if f.ContractsEnabled() {
		t.Error("contracts should be disabled")
	}
	if f.DocsEnabled() {
		t.Error("docs should be disabled")
	}
	if f.FrontendEnabled() {
		t.Error("frontend should be disabled")
	}
	if f.ObservabilityEnabled() {
		t.Error("observability should be disabled")
	}
	if f.HotReloadEnabled() {
		t.Error("hot_reload should be disabled")
	}
}

func TestApplyDisableFlags_HotReloadVariants(t *testing.T) {
	variants := []string{"hot_reload", "hot-reload", "hotreload"}
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			gen := newTestGen()
			if err := applyDisableFlags(gen, []string{v}); err != nil {
				t.Fatalf("unexpected error for %q: %v", v, err)
			}
			if gen.Features.HotReloadEnabled() {
				t.Errorf("expected hot_reload disabled via %q", v)
			}
		})
	}
}

func TestApplyDisableFlags_UnknownFeature(t *testing.T) {
	gen := newTestGen()
	err := applyDisableFlags(gen, []string{"bogus"})
	if err == nil {
		t.Fatal("expected error for unknown feature")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should mention the unknown feature name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "valid features") {
		t.Errorf("error should list valid features, got: %v", err)
	}
}

func TestApplyDisableFlags_MixedValidAndInvalid(t *testing.T) {
	gen := newTestGen()
	err := applyDisableFlags(gen, []string{"deploy", "bogus"})
	if err == nil {
		t.Fatal("expected error for unknown feature")
	}
	// "deploy" was processed before the error on "bogus".
	if gen.Features.DeployEnabled() {
		t.Error("expected deploy to be disabled even though a later feature errored")
	}
}

func TestApplyDisableFlags_CaseInsensitive(t *testing.T) {
	gen := newTestGen()
	if err := applyDisableFlags(gen, []string{"DEPLOY", "CI"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gen.Features.DeployEnabled() {
		t.Error("expected DEPLOY (uppercase) to disable deploy")
	}
	if gen.Features.CIEnabled() {
		t.Error("expected CI (uppercase) to disable ci")
	}
}

// TestRunNewKindValidation exercises the `--kind` flag's validation surface
// via the pure validateNewArgs helper. Doing it via runNew would invoke the
// full scaffold (go mod tidy + buf generate + …), which is slow and can
// hang in CI environments without network access.
func TestRunNewKindValidation(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		wantKind string
		wantErr  string
	}{
		{"unknown kind rejected", "framework", "", `invalid --kind "framework"`},
		{"empty becomes service", "", config.ProjectKindService, ""}, // empty == service (back-compat)
		{"service explicit", "service", config.ProjectKindService, ""},
		{"cli explicit", "cli", config.ProjectKindCLI, ""},
		{"library explicit", "library", config.ProjectKindLibrary, ""},
		{"upper-case normalized", "CLI", config.ProjectKindCLI, ""},
		{"trims whitespace", "  service  ", config.ProjectKindService, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotKind, _, _, err := validateNewArgs(tc.kind, "local", "", nil, nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotKind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", gotKind, tc.wantKind)
			}
		})
	}
}

// TestRunNewKindCLIRejectsServiceFlag — `--service` is service-only.
func TestRunNewKindCLIRejectsServiceFlag(t *testing.T) {
	_, _, _, err := validateNewArgs("cli", "local", "", []string{"api"}, nil)
	if err == nil || !strings.Contains(err.Error(), "--service is only meaningful with --kind service") {
		t.Fatalf("expected --service rejection in CLI mode, got: %v", err)
	}
}

// TestValidateNewArgs_BufPlugins covers the --buf-plugins normalization.
func TestValidateNewArgs_BufPlugins(t *testing.T) {
	cases := []struct {
		input       string
		wantPlugins string
		wantErr     bool
	}{
		{"", "local", false}, // default
		{"local", "local", false},
		{"remote", "remote", false},
		{"REMOTE", "remote", false},
		{"  local  ", "local", false},
		{"bogus", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			_, gotPlugins, _, err := validateNewArgs("service", tc.input, "", nil, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotPlugins != tc.wantPlugins {
				t.Fatalf("plugins = %q, want %q", gotPlugins, tc.wantPlugins)
			}
		})
	}
}

func TestApplyDisableFlags_WhitespaceHandling(t *testing.T) {
	gen := newTestGen()
	if err := applyDisableFlags(gen, []string{" deploy "}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gen.Features.DeployEnabled() {
		t.Error("expected ' deploy ' (with whitespace) to disable deploy")
	}
}
