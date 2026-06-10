package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/generator"
)

// newExplainDriftCtx builds a minimal pipelineContext over a tmpdir
// with one drifted Tier-1 file: recorded render v1, on-disk hand-edit.
func newExplainDriftCtx(t *testing.T, rel string, rendered, onDisk []byte) (*pipelineContext, []checksums.Tier1DriftEntry) {
	t.Helper()
	root := t.TempDir()
	cs := &generator.FileChecksums{Files: map[string]generator.FileChecksumEntry{}}
	cs.RecordFile(rel, rendered)
	entry := cs.Files[rel]
	entry.Tier = 1
	cs.Files[rel] = entry

	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, onDisk, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := &pipelineContext{
		ProjectDir:   root,
		AbsPath:      root,
		ExplainDrift: true,
		Checksums:    cs,
	}
	drift := cs.CheckTier1Drift(root)
	if len(drift) != 1 {
		t.Fatalf("expected 1 drift entry, got %d", len(drift))
	}
	return ctx, drift
}

// TestExplainDrift_EndToEnd walks the full mechanism at unit level:
// prepare redirects the emitter write to a side render (file + entry
// untouched), a rehash-style mutation gets rolled back by the snapshot
// restore, and finish prints a diff and returns the drift error.
func TestExplainDrift_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	checksums.ResetSkipWrite()
	checksums.ResetPerRunState()
	defer checksums.ResetPerRunState()

	const rel = "pkg/app/wire_gen.go"
	rendered := []byte("package app\n\nfunc wireA() {}\n")
	onDisk := []byte("package app\n\nfunc wireA() {}\n\nfunc userEdit() {}\n")
	ctx, drift := newExplainDriftCtx(t, rel, rendered, onDisk)
	originalEntry := ctx.Checksums.Files[rel]

	prepareExplainDrift(ctx, drift)

	// Emitter pass: the fresh render must land in .forge/render/, not
	// over the user's file, and must not touch the checksum entry.
	fresh := []byte("package app\n\nfunc wireA() {}\n\nfunc wireB() {}\n")
	wrote, err := checksums.WriteGeneratedFile(ctx.AbsPath, rel, fresh, ctx.Checksums, false)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("explain-drift run overwrote the drifted file")
	}
	gotOnDisk, _ := os.ReadFile(filepath.Join(ctx.AbsPath, rel))
	if string(gotOnDisk) != string(onDisk) {
		t.Errorf("user content modified: %q", gotOnDisk)
	}
	gotRender, err := os.ReadFile(filepath.Join(ctx.AbsPath, checksums.RenderDir, rel))
	if err != nil || string(gotRender) != string(fresh) {
		t.Errorf("fresh render not parked (err=%v content=%q)", err, gotRender)
	}
	// No merge base for a non-forked path — a stale base would poison a
	// future fork's merge.
	if _, err := os.Stat(filepath.Join(ctx.AbsPath, checksums.RenderBaseDir, rel)); !os.IsNotExist(err) {
		t.Errorf("explain-drift seeded render-base for a non-forked path")
	}

	// Simulate stepRehashTracked blessing the on-disk content; the
	// snapshot restore in finishExplainDrift must undo it.
	mutated := ctx.Checksums.Files[rel]
	mutated.Hash = checksums.Hash(onDisk)
	ctx.Checksums.Files[rel] = mutated

	var diffOut strings.Builder
	printExplainDriftDiffs(&diffOut, ctx)
	out := diffOut.String()
	for _, want := range []string{rel, "func userEdit()", "func wireB()"} {
		if !strings.Contains(out, want) {
			t.Errorf("diff output missing %q; got:\n%s", want, out)
		}
	}

	finishErr := finishExplainDrift(ctx)
	if finishErr == nil {
		t.Fatal("finishExplainDrift must return the drift error")
	}
	if !strings.Contains(finishErr.Error(), "Tier-1 file-stomp guard") {
		t.Errorf("error should carry the standard drift report; got %q", finishErr.Error())
	}
	if got := ctx.Checksums.Files[rel]; got.Hash != originalEntry.Hash {
		t.Errorf("snapshot restore failed: hash=%s want %s (drift would be blessed on save)", got.Hash, originalEntry.Hash)
	}
}

// TestExplainDrift_DiffTruncation pins the bounded-diff contract.
func TestExplainDrift_DiffTruncation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	checksums.ResetSkipWrite()
	checksums.ResetPerRunState()
	defer checksums.ResetPerRunState()

	origCap := explainDriftDiffCapLines
	explainDriftDiffCapLines = 5
	defer func() { explainDriftDiffCapLines = origCap }()

	const rel = "pkg/app/wire_gen.go"
	rendered := []byte("package app\n// r1\n// r2\n// r3\n// r4\n// r5\n// r6\n// r7\n// r8\n")
	onDisk := []byte("package app\n// e1\n// e2\n// e3\n// e4\n// e5\n// e6\n// e7\n// e8\n")
	ctx, drift := newExplainDriftCtx(t, rel, rendered, onDisk)

	prepareExplainDrift(ctx, drift)
	if _, err := checksums.WriteGeneratedFile(ctx.AbsPath, rel, rendered, ctx.Checksums, false); err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	printExplainDriftDiffs(&b, ctx)
	out := b.String()
	if !strings.Contains(out, "diff truncated at 5 lines") {
		t.Errorf("missing truncation note; got:\n%s", out)
	}
	if !strings.Contains(out, checksums.SideRenderRelPath(rel)) {
		t.Errorf("truncation note should point at the full render; got:\n%s", out)
	}
}

// TestExplainDrift_NoRenderProduced: when the drifted file's emitter
// never ran (step preset / gate), the diff section says so instead of
// failing.
func TestExplainDrift_NoRenderProduced(t *testing.T) {
	checksums.ResetPerRunState()
	defer checksums.ResetPerRunState()

	const rel = "pkg/app/wire_gen.go"
	ctx, drift := newExplainDriftCtx(t, rel, []byte("package app\n"), []byte("package app // edit\n"))
	prepareExplainDrift(ctx, drift)
	// No emitter write happens.

	var b strings.Builder
	printExplainDriftDiffs(&b, ctx)
	if !strings.Contains(b.String(), "no fresh render was produced this run") {
		t.Errorf("missing no-render note; got:\n%s", b.String())
	}
}

// TestFinishExplainDrift_NoOpWithoutDrift: plain runs (no flag, or no
// drift) pass through untouched.
func TestFinishExplainDrift_NoOpWithoutDrift(t *testing.T) {
	ctx := &pipelineContext{}
	if err := finishExplainDrift(ctx); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}
