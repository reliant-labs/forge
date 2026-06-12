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

func TestForceScope_LimitsForceToFlaggedPaths(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState()
	t.Cleanup(ResetPerRunState)

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}

	// Two tracked, hand-edited files. The guard flagged only flagged.go.
	for _, rel := range []string{"flagged.go", "unflagged.go"} {
		cs.RecordFile(rel, []byte("// as generated\n"))
		entry := cs.Files[rel]
		entry.Tier = 1
		cs.Files[rel] = entry
		if err := os.WriteFile(filepath.Join(root, rel), []byte("// hand edit "+rel+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

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
	if string(got) != "// hand edit unflagged.go\n" {
		t.Errorf("unflagged hand-edit destroyed; got %q", got)
	}
}

func TestForceScope_NilScopeKeepsLegacyUnscopedForce(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState() // clears any scope — nil means unscoped
	t.Cleanup(ResetPerRunState)

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	cs.RecordFile("a.go", []byte("// as generated\n"))
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("// hand edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

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
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	cs.RecordFile("a.go", []byte("// as generated\n"))
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("// hand edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

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
