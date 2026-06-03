// Tests for the Tier-1 stomp-guard scoping helper.
//
// FRICTION 2026-06-02: cp-forge dogfood pass — the stomp guard hard-
// failed on drift to pkg/app/migrate.go for agents whose port targeted
// internal/proxy/, blocking concurrent lane work. These tests pin the
// fix: drift on a path whose emitter step is gated OFF is filtered out
// of the in-scope set and surfaced as a warning instead of an error.
package cli

import (
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestTier1OwnerGateRegistry pins the canonical mapping between
// Tier-1 emitter paths and their owning gate. New emitters added to
// the registry must have a corresponding row here so the table stays
// truthful as a documentation surface.
func TestTier1OwnerGateRegistry(t *testing.T) {
	cases := []struct {
		path        string
		wantMapped  bool
		description string
	}{
		{"pkg/app/migrate.go", true, "migrate.go is gated on database driver"},
		{"db/embed.go", true, "db/embed.go is gated on database driver"},
		{"pkg/app/bootstrap.go", true, "bootstrap.go is gated on any entrypoint"},
		{"pkg/app/testing.go", true, "testing.go is gated on any entrypoint"},
		{"pkg/app/wire_gen.go", true, "wire_gen.go is gated on any entrypoint"},
		// glob entries — exercise the path/filepath.Match wiring.
		{"handlers/billing/handlers_crud_gen.go", true, "handlers/<svc>/handlers_crud_gen.go is gated on codegen+services"},
		{"handlers/users/handlers_crud_gen.go", true, "second svc still matches the same glob"},
		{"pkg/middleware/auth_gen.go", true, "pkg/middleware/*_gen.go is gated on codegen+services"},
		{"pkg/middleware/tenant_gen.go", true, "second middleware still matches the same prefix"},
		{"frontends/admin/src/hooks/users-hooks.ts", true, "frontend hook glob is gated on frontend+services"},
		// Unknown paths fall through to nil → caller treats as in-scope.
		// This preserves loud-fail behavior for new emitters until they
		// get a registry entry.
		{"cmd/server.go", false, "server.go has no entry — fail-closed by design"},
		{"handlers/billing/something_else.go", false, "non-_crud_gen handler files don't match the glob"},
		{"frontends/admin/src/pages/users.tsx", false, "frontend pages aren't in the hooks glob"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := tier1OwnerGate(tc.path)
			if tc.wantMapped && got == nil {
				t.Errorf("tier1OwnerGate(%q) = nil, want mapped (%s)", tc.path, tc.description)
			}
			if !tc.wantMapped && got != nil {
				t.Errorf("tier1OwnerGate(%q) returned a gate; want nil (%s)", tc.path, tc.description)
			}
		})
	}
}

// TestFilterTier1DriftInScope_GateOffFiltersDrift exercises the actual
// FRICTION case: a project without a database driver should not see
// pkg/app/migrate.go drift block its generate run. The drift is
// classified as out-of-scope, in-scope is empty.
func TestFilterTier1DriftInScope_GateOffFiltersDrift(t *testing.T) {
	// gateMigrateHasDriver returns false when cfg.Database.Driver is
	// empty (cli's gate helper short-circuits to false on a nil cfg).
	ctx := &pipelineContext{
		Cfg: &config.ProjectConfig{
			Name: "cli-only-project",
			// no Database.Driver — gateMigrateHasDriver returns false
		},
	}
	if gateMigrateHasDriver(ctx) {
		t.Fatalf("gateMigrateHasDriver should be false for a cfg with no driver")
	}
	drift := []driftStub{
		{path: "pkg/app/migrate.go"},
		{path: "db/embed.go"},
	}
	inScope, outOfScope := filterTier1DriftInScope(ctx, drift, func(d driftStub) string { return d.path })
	if len(inScope) != 0 {
		t.Errorf("inScope = %d entries, want 0 (gated-off emitter shouldn't block stomp guard)", len(inScope))
	}
	if len(outOfScope) != 2 {
		t.Errorf("outOfScope = %d entries, want 2", len(outOfScope))
	}
}

// TestFilterTier1DriftInScope_GateOnKeepsDriftInScope pins the
// loud-fail path: when the emitter's gate IS true (driver configured),
// drift on its file stays in-scope so the user is forced to confront
// the conflict.
func TestFilterTier1DriftInScope_GateOnKeepsDriftInScope(t *testing.T) {
	ctx := &pipelineContext{
		Cfg: &config.ProjectConfig{
			Name: "with-driver",
			Database: config.DatabaseConfig{Driver: "postgres"},
			Features: config.FeaturesConfig{
				Migrations: configBoolPtr(true),
			},
		},
	}
	if !gateMigrateHasDriver(ctx) {
		t.Fatalf("gateMigrateHasDriver should be true for a cfg with driver=postgres")
	}
	drift := []driftStub{{path: "pkg/app/migrate.go"}}
	inScope, outOfScope := filterTier1DriftInScope(ctx, drift, func(d driftStub) string { return d.path })
	if len(inScope) != 1 {
		t.Errorf("inScope = %d entries, want 1 (gate-on emitter must still fail loudly)", len(inScope))
	}
	if len(outOfScope) != 0 {
		t.Errorf("outOfScope = %d entries, want 0", len(outOfScope))
	}
}

// TestFilterTier1DriftInScope_UnknownPathStaysInScope pins the
// fail-closed semantics for unregistered emitters: an emitter we don't
// know about keeps its drift in-scope so adding a new Tier-1 emitter
// without registering it here doesn't accidentally silence drift.
func TestFilterTier1DriftInScope_UnknownPathStaysInScope(t *testing.T) {
	ctx := &pipelineContext{} // empty — all gates fall to defaults
	drift := []driftStub{
		{path: "cmd/server.go"}, // not in registry
	}
	inScope, outOfScope := filterTier1DriftInScope(ctx, drift, func(d driftStub) string { return d.path })
	if len(inScope) != 1 {
		t.Errorf("unknown-emitter drift should stay in-scope; inScope = %d, want 1", len(inScope))
	}
	if len(outOfScope) != 0 {
		t.Errorf("unknown-emitter drift should not be out-of-scope; outOfScope = %d, want 0", len(outOfScope))
	}
}

// driftStub is a minimal generic-friendly stand-in for
// checksums.Tier1DriftEntry so this file doesn't import the canonical
// checksums type just for path-extraction tests.
type driftStub struct{ path string }

// configBoolPtr returns a *bool — config feature flags are pointer-
// typed in ProjectConfig so unset distinguishes from "explicit false".
func configBoolPtr(b bool) *bool { return &b }
