// Tests for --force tier scoping (journey fr-a04f8c0609).
//
// `forge generate --force` is the documented recovery from a Tier-1
// stomp-guard trip. It must mean "discard the edits the guard just
// reported" — not "force-overwrite anything any emitter touches this
// run". The journey saw a --force recovery from an unrelated Tier-1
// trip re-scaffold Tier-2 frontend files the user never edited, with
// "(your edits discarded)" printed for each.
package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/templates"
)

// navTemplateData is the minimal FrontendTemplateData the nav.tsx
// template renders with.
func navTemplateData() templates.FrontendTemplateData {
	return templates.FrontendTemplateData{FrontendName: "web", ProjectName: "demo"}
}

// TestStepCheckTier1Drift_ForceIsScopedToFlaggedFiles drives the real
// guard step with ctx.Force and verifies the installed force scope:
// the drifted Tier-1 file the guard reported is force-overwritable,
// while a hand-edited certified file the guard did NOT flag (here a
// Tier-2-managed starter, which scanProjectDrift exempts by design)
// survives a force=true write through the same chokepoint.
func TestStepCheckTier1Drift_ForceIsScopedToFlaggedFiles(t *testing.T) {
	checksums.ResetPerRunState()
	t.Cleanup(checksums.ResetPerRunState)
	checksums.ResetSkipWrite()

	dir := t.TempDir()
	mustWriteScopeFile(t, filepath.Join(dir, "proto", "services", "api", "v1", "api.proto"), "syntax = \"proto3\";\n")

	cs := &checksums.FileChecksums{}

	// Drifted Tier-1 file — the guard flags this one. Marker carries
	// the as-generated hash; body is the hand-edit → Modified.
	const flagged = "pkg/app/wire_gen.go"
	flaggedBytes, ok := checksums.StampWithValue(flagged,
		[]byte("package app // hand-edited\n"),
		checksums.BodyHash([]byte("package app // as generated\n")))
	if !ok {
		t.Fatal("wire_gen.go should be stampable")
	}
	mustWriteScopeFile(t, filepath.Join(dir, flagged), string(flaggedBytes))

	// Hand-edited certified file the guard does NOT flag: Taskfile.yml
	// is in generator.Tier2ManagedPaths, so scanProjectDrift exempts it
	// (edits there are sanctioned). An emitter can still target such a
	// path through the plain WriteGeneratedFile chokepoint, and --force
	// must not clobber it.
	const unflagged = "Taskfile.yml"
	if !tier2MigratedPaths[unflagged] {
		t.Fatalf("%s is no longer Tier-2-managed; pick another exempt path for this test", unflagged)
	}
	const tier2Edit = "# precious user task definitions\n"
	unflaggedBytes, ok := checksums.StampWithValue(unflagged,
		[]byte(tier2Edit),
		checksums.BodyHash([]byte("# as scaffolded\n")))
	if !ok {
		t.Fatal("Taskfile.yml should be stampable")
	}
	mustWriteScopeFile(t, filepath.Join(dir, unflagged), string(unflaggedBytes))

	ctx := &pipelineContext{ProjectDir: dir, AbsPath: dir, Checksums: cs, Force: true}
	if err := stepCheckTier1Drift(ctx); err != nil {
		t.Fatalf("stepCheckTier1Drift with --force should proceed; got: %v", err)
	}

	// The flagged file is force-overwritable.
	wrote, err := checksums.WriteGeneratedFile(dir, flagged, []byte("package app // regen\n"), cs, true)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Errorf("--force must overwrite the guard-flagged file %s", flagged)
	}

	// The unflagged hand-edit survives a force=true write.
	wrote, err = checksums.WriteGeneratedFile(dir, unflagged, []byte("# re-scaffold\n"), cs, true)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Errorf("--force overwrote %s, which the guard never flagged", unflagged)
	}
	got, _ := os.ReadFile(filepath.Join(dir, unflagged))
	if string(got) != string(unflaggedBytes) {
		t.Errorf("unflagged hand-edit destroyed under --force; got %q", got)
	}
}

// TestStepCheckTier1Drift_ForceWithNoDriftInstallsEmptyScope: --force
// on a clean tree flags nothing, so force must be inert for the rest
// of the run rather than reverting to unscoped clobbering.
func TestStepCheckTier1Drift_ForceWithNoDriftInstallsEmptyScope(t *testing.T) {
	checksums.ResetPerRunState()
	t.Cleanup(checksums.ResetPerRunState)
	checksums.ResetSkipWrite()

	dir := t.TempDir()
	cs := &checksums.FileChecksums{}
	// A hand-edited certified file scanProjectDrift exempts (Tier-2
	// managed) — so the guard flags NOTHING, and the empty force scope
	// must keep a force=true write away from it.
	const rel = "Taskfile.yml"
	if !tier2MigratedPaths[rel] {
		t.Fatalf("%s is no longer Tier-2-managed; pick another exempt path for this test", rel)
	}
	stamped, ok := checksums.StampWithValue(rel,
		[]byte("# user edit\n"),
		checksums.BodyHash([]byte("# as scaffolded\n")))
	if !ok {
		t.Fatal("Taskfile.yml should be stampable")
	}
	mustWriteScopeFile(t, filepath.Join(dir, rel), string(stamped))

	ctx := &pipelineContext{ProjectDir: dir, AbsPath: dir, Checksums: cs, Force: true}
	if err := stepCheckTier1Drift(ctx); err != nil {
		t.Fatalf("clean-tree guard with --force: %v", err)
	}

	wrote, err := checksums.WriteGeneratedFile(dir, rel, []byte("# regen\n"), cs, true)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Error("--force on a clean tree (guard flagged nothing) still overwrote a hand-edited file")
	}
}

// TestEmitTier2OnceIfMissing_NeverClobbersWithoutResetTier2 pins the
// Tier-2 half of the journey: nav.tsx / page.tsx re-scaffolds are
// gated on the --reset-tier2 hook, not on --force. An existing file is
// preserved unless the hook approves.
func TestEmitTier2OnceIfMissing_NeverClobbersWithoutResetTier2(t *testing.T) {
	checksums.ResetTier2State()
	t.Cleanup(checksums.ResetTier2State)

	dir := t.TempDir()
	rel := filepath.Join("frontends", "web", "src", "components", "nav.tsx")
	const userNav = "// fully user-curated nav\n"
	mustWriteScopeFile(t, filepath.Join(dir, rel), userNav)

	cs := &checksums.FileChecksums{}

	// No hook installed (plain run, or plain --force run): preserved.
	if err := emitTier2OnceIfMissing(dir, rel, "nextjs/src/components/nav.tsx.tmpl", navTemplateData(), cs); err != nil {
		t.Fatalf("emitTier2OnceIfMissing: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, rel)); string(got) != userNav {
		t.Fatalf("existing Tier-2 file overwritten without --reset-tier2; got:\n%s", got)
	}

	// Hook denies: still preserved.
	checksums.Tier2OverwriteFn = func(string) bool { return false }
	if err := emitTier2OnceIfMissing(dir, rel, "nextjs/src/components/nav.tsx.tmpl", navTemplateData(), cs); err != nil {
		t.Fatalf("emitTier2OnceIfMissing (hook denies): %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, rel)); string(got) != userNav {
		t.Fatalf("Tier-2 file overwritten despite hook denial; got:\n%s", got)
	}

	// Hook approves (--reset-tier2 --yes shape): re-scaffolded.
	checksums.Tier2OverwriteFn = func(string) bool { return true }
	if err := emitTier2OnceIfMissing(dir, rel, "nextjs/src/components/nav.tsx.tmpl", navTemplateData(), cs); err != nil {
		t.Fatalf("emitTier2OnceIfMissing (hook approves): %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, rel)); string(got) == userNav {
		t.Fatal("--reset-tier2 approval did not re-scaffold the Tier-2 file")
	}
}
