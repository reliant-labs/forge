// Tests for the typed step-plan refactor of runGeneratePipeline.
//
// (2026-05-06 polish-phase) — guards FORGE_REVIEW_CODEBASE.md Tier 1.1.
//
// Two contracts are pinned here:
//
//  1. The step plan is deterministic — generateSteps() returns the same
//     ordered slice on every call. A test that asserts the names+order
//     prevents accidental drift (re-ordering steps changes generation
//     output in subtle ways: e.g. running goimports BEFORE the rehash
//     step would re-flag every emitted file as user-edited).
//
//  2. Gates are pure — calling step.Gate(ctx) twice returns the same
//     answer and does not mutate ctx. Watch-mode dispatchers and
//     `forge generate --plan` will both call gates without running the
//     associated Run, so any I/O or state mutation in a gate would leak.
package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestGenerateStepsPlanStable pins the step order. If a step is added,
// removed, or reordered, this test fails — an intentional speed bump
// that forces the change to be reviewed against the goimports-vs-rehash
// ordering trap and the "auto-enable multi-tenant rewrites forge.yaml
// mid-pipeline" surprise documented in generate_pipeline.go.
//
// Test bodies and downstream consumers (--plan flag, future watch-mode
// dispatch table) read this list to confirm they're up to date.
func TestGenerateStepsPlanStable(t *testing.T) {
	want := []string{
		"load project config",
		"load checksums",
		"check Tier-1 file-stomp guard",
		"sync forge/pkg dev replace",
		"announce project",
		"pre-codegen contract check",
		"detect proto directories",
		"buf generate (Go stubs)",
		"descriptor extraction",
		"ORM generate (proto/db)",
		"initial migration scaffold",
		"entity-aware migration",
		"TypeScript stubs (frontends)",
		"config loader (proto/config)",
		"parse services + module path",
		"frontend hooks",
		"ensure frontend components",
		"frontend CRUD pages",
		"frontend nav + dashboard",
		"cleanup stale codegen",
		"service stubs",
		"internal/db/ ORM (entity-driven)",
		"CRUD handlers",
		"authorizer",
		"service mocks",
		"internal package contracts",
		"auth middleware",
		"tenant middleware (auto-enable + emit)",
		"webhook routes",
		"pkg/app/bootstrap.go",
		"pkg/app/testing.go",
		"pkg/app/migrate.go",
		"sqlc generate",
		"go mod tidy (gen/)",
		"CI workflows",
		"pack generate hooks",
		"regenerate infra files",
		"per-env deploy config",
		"Grafana dashboards",
		"entity-aware seed data",
		"frontend mocks + transport",
		"go mod tidy (root)",
		"goimports on generated Go",
		"rehash tracked files",
		"refresh ORM output mtimes",
		"post-gen validation",
		"go build (validate generated code)",
	}

	steps := generateSteps()
	if len(steps) != len(want) {
		t.Fatalf("generateSteps() returned %d steps, want %d", len(steps), len(want))
	}
	for i, s := range steps {
		if s.Name != want[i] {
			t.Errorf("steps[%d].Name = %q, want %q", i, s.Name, want[i])
		}
	}
}

// TestGenerateStepsPlanDeterministic verifies generateSteps() returns
// the same names on repeat calls. Catches the (hypothetical) regression
// where a step list is built from a map (Go map iteration is
// randomized).
func TestGenerateStepsPlanDeterministic(t *testing.T) {
	first := stepNames(generateSteps())
	for i := 0; i < 5; i++ {
		next := stepNames(generateSteps())
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("generateSteps() returned different order on call %d:\n first=%v\n next=%v", i+1, first, next)
		}
	}
}

// TestGenerateStepsHaveRunAndGate is a mechanical wellformedness check:
// every step needs a non-nil Gate (for the loop's predicate) and a
// non-nil Run (for the loop's action). Catches a step entry copy-pasted
// without filling in the function fields.
func TestGenerateStepsHaveRunAndGate(t *testing.T) {
	for i, s := range generateSteps() {
		if s.Name == "" {
			t.Errorf("steps[%d] has empty Name", i)
		}
		if s.Gate == nil {
			t.Errorf("steps[%d] %q has nil Gate", i, s.Name)
		}
		if s.Run == nil {
			t.Errorf("steps[%d] %q has nil Run", i, s.Name)
		}
	}
}

// TestGenerateStepsGatesAreSideEffectFree exercises each gate against a
// realistic spread of contexts and asserts:
//   - calling the gate twice returns the same answer (idempotent)
//   - calling the gate does not mutate any field on the context (pure)
//
// Future watch-mode and --plan callers depend on this: they invoke
// gates without running the associated body, often repeatedly.
func TestGenerateStepsGatesAreSideEffectFree(t *testing.T) {
	cases := []struct {
		name string
		ctx  *pipelineContext
	}{
		{"empty (dir-scan fallback)", &pipelineContext{ProjectDir: ".", AbsPath: "/abs/."}},
		{"with cfg, no protos", &pipelineContext{
			ProjectDir: ".", AbsPath: "/abs/.",
			Cfg: &config.ProjectConfig{Name: "sample"},
		}},
		{"with cfg + services + db", &pipelineContext{
			ProjectDir: ".", AbsPath: "/abs/.",
			Cfg:         &config.ProjectConfig{Name: "sample"},
			HasServices: true, HasDB: true, HasConfig: true,
		}},
		{"with cfg + frontends + packs", &pipelineContext{
			ProjectDir: ".", AbsPath: "/abs/.",
			Cfg: &config.ProjectConfig{
				Name:      "sample",
				Frontends: []config.FrontendConfig{{Name: "web", Type: "nextjs"}},
				Packs:     []string{"audit-log"},
			},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := generateSteps()
			for _, step := range steps {
				snapshot := *tc.ctx
				first := step.Gate(tc.ctx)
				second := step.Gate(tc.ctx)
				if first != second {
					t.Errorf("gate %q non-idempotent: first=%v second=%v", step.Name, first, second)
				}
				if !pipelineContextEqual(&snapshot, tc.ctx) {
					t.Errorf("gate %q mutated context", step.Name)
				}
			}
		})
	}
}

// stepNames extracts the ordered names from a step slice. Helper for
// the determinism test.
func stepNames(steps []GenStep) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

// TestDeriveOrmEnabledMatrix is the golden test guarding step 6's
// ORM-enable probe order. The pre-refactor inline block did:
//
//   1. if hasDB && proto/db has at least one .proto file → ormEnabled=true
//   2. else if internal/db/types.go exists → ormEnabled=true
//   3. else ormEnabled=false
//
// Reordering or skipping a probe changes which projects emit ORM
// wire-up in pkg/app/bootstrap.go, which is hard to spot in a smoke
// test. This matrix pins the exact behavior across every combination
// of probe inputs that mattered pre-refactor.
func TestDeriveOrmEnabledMatrix(t *testing.T) {
	cases := []struct {
		name           string
		hasDB          bool
		makeProtoFile  bool // create proto/db/v1/*.proto?
		makeTypesFile  bool // create internal/db/types.go?
		want           bool
	}{
		{name: "neither: ORM off", want: false},
		{name: "proto/db dir only (hasDB=true), no .proto files: ORM off", hasDB: true, want: false},
		{name: "proto/db dir + .proto file: ORM on (rule 1)", hasDB: true, makeProtoFile: true, want: true},
		{name: "internal/db/types.go only: ORM on (rule 2 fallback)", makeTypesFile: true, want: true},
		{name: "hasDB=false but types.go present: ORM on", makeTypesFile: true, want: true},
		{name: "proto/db with .proto AND types.go: ORM on (rule 1 wins)", hasDB: true, makeProtoFile: true, makeTypesFile: true, want: true},
		{name: "hasDB=true, no proto files, types.go present: ORM on (falls through to rule 2)", hasDB: true, makeTypesFile: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.hasDB {
				if err := os.MkdirAll(filepath.Join(dir, "proto", "db"), 0o755); err != nil {
					t.Fatalf("mkdir proto/db: %v", err)
				}
			}
			if tc.makeProtoFile {
				p := filepath.Join(dir, "proto", "db", "v1")
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatalf("mkdir proto/db/v1: %v", err)
				}
				if err := os.WriteFile(filepath.Join(p, "thing.proto"), []byte("syntax=\"proto3\";\n"), 0o644); err != nil {
					t.Fatalf("write proto: %v", err)
				}
			}
			if tc.makeTypesFile {
				if err := os.MkdirAll(filepath.Join(dir, "internal", "db"), 0o755); err != nil {
					t.Fatalf("mkdir internal/db: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "internal", "db", "types.go"), []byte("package db\n"), 0o644); err != nil {
					t.Fatalf("write types.go: %v", err)
				}
			}
			got, err := deriveOrmEnabled(dir, tc.hasDB)
			if err != nil {
				t.Fatalf("deriveOrmEnabled: %v", err)
			}
			if got != tc.want {
				t.Errorf("deriveOrmEnabled(hasDB=%v, proto=%v, types=%v) = %v, want %v",
					tc.hasDB, tc.makeProtoFile, tc.makeTypesFile, got, tc.want)
			}
		})
	}
}

// pipelineContextEqual compares the fields gates legitimately read from
// the context. We compare scalars directly; for slices/maps we compare
// length to keep the test focused on "did the gate write anything new"
// rather than deep-diffing config payloads.
func pipelineContextEqual(a, b *pipelineContext) bool {
	if a.ProjectDir != b.ProjectDir || a.AbsPath != b.AbsPath || a.Force != b.Force {
		return false
	}
	if a.HasServices != b.HasServices || a.HasAPI != b.HasAPI {
		return false
	}
	if a.HasDB != b.HasDB || a.HasConfig != b.HasConfig {
		return false
	}
	if a.HasWorkers != b.HasWorkers || a.HasOperators != b.HasOperators {
		return false
	}
	if (a.Cfg == nil) != (b.Cfg == nil) {
		return false
	}
	if (a.Checksums == nil) != (b.Checksums == nil) {
		return false
	}
	if len(a.Services) != len(b.Services) {
		return false
	}
	if a.ModulePath != b.ModulePath {
		return false
	}
	if len(a.EntityDefs) != len(b.EntityDefs) {
		return false
	}
	if len(a.ConfigFields) != len(b.ConfigFields) {
		return false
	}
	return true
}
