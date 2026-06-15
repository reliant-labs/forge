// Tests for the --force scope chokepoint.
//
// Journey fr-a04f8c0609: recovering from a Tier-1 stomp-guard trip via
// `forge generate --force` overwrote files the guard never flagged.
// --force means "discard the edits the guard just told me about", not
// "force-overwrite anything any emitter touches this run" — so the
// pipeline now scopes force to the exact path set the guard reported,
// via SetForceScope. When no scope is installed (non-pipeline callers
// like `forge upgrade`, or presets that deliberately skip the guard),
// force keeps its historical unscoped meaning.
package checksums

import (
	"os"
	"path/filepath"
	"testing"
)

// writeHandEdited materializes the "user hand-edited a tracked file"
// state at root/rel: a stamped forge render mutated after stamping, so
// the marker survives but no longer verifies (Verify == Modified).
func writeHandEdited(t *testing.T, root, rel string) []byte {
	t.Helper()
	stamped, ok := Stamp(rel, []byte("// as generated\n"))
	if !ok {
		t.Fatalf("Stamp(%q): unstampable", rel)
	}
	edited := append(stamped, []byte("// hand edit "+rel+"\n")...)
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if Verify(edited) != Modified {
		t.Fatalf("fixture must verify Modified; got %v", Verify(edited))
	}
	return edited
}

func TestForceScope_LimitsForceToFlaggedPaths(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState()
	t.Cleanup(ResetPerRunState)

	root := t.TempDir()
	cs := &FileChecksums{}

	// Two tracked, hand-edited files. The guard flagged only flagged.go.
	writeHandEdited(t, root, "flagged.go")
	unflaggedContent := writeHandEdited(t, root, "unflagged.go")

	SetForceScope([]string{"flagged.go"})

	// force=true on the flagged path overwrites.
	wrote, err := WriteGeneratedFile(root, "flagged.go", []byte("// regen\n"), cs, true)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Error("force=true on a guard-flagged path must overwrite")
	}

	// force=true on a path the guard did NOT flag is inert — the
	// hand-edit survives.
	wrote, err = WriteGeneratedFile(root, "unflagged.go", []byte("// regen\n"), cs, true)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Error("force=true outside the guard-flagged scope overwrote a hand-edited file")
	}
	got, _ := os.ReadFile(filepath.Join(root, "unflagged.go"))
	if string(got) != string(unflaggedContent) {
		t.Errorf("unflagged hand-edit destroyed; got %q", got)
	}
}

func TestForceScope_NilScopeKeepsLegacyUnscopedForce(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState() // clears any scope — nil means unscoped
	t.Cleanup(ResetPerRunState)

	root := t.TempDir()
	cs := &FileChecksums{}
	writeHandEdited(t, root, "a.go")

	wrote, err := WriteGeneratedFile(root, "a.go", []byte("// regen\n"), cs, true)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Error("with no scope installed, force=true must keep its historical unscoped behavior")
	}
}

func TestForceScope_EmptyScopeMakesForceInert(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState()
	t.Cleanup(ResetPerRunState)

	root := t.TempDir()
	cs := &FileChecksums{}
	writeHandEdited(t, root, "a.go")

	// The guard ran and flagged NOTHING (e.g. --force passed on a clean
	// tree) — force must not become a license to overwrite paths that
	// drift later in the run or that the guard never checks.
	SetForceScope(nil)

	wrote, err := WriteGeneratedFile(root, "a.go", []byte("// regen\n"), cs, true)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Error("force=true with an installed-but-empty scope overwrote a hand-edited file")
	}
}
