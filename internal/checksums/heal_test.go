// Tests for the LOUD auto-heal contract.
//
// FRICTION cp-forge fr-2c1c2328c7: a hand-edit to pkg/app/bootstrap.go
// happened to hash equal to a PRIOR render in the legacy checksum
// history, so generate classified it as stale codegen and silently
// reverted it — the round's only silent destruction of user work. The
// auto-heal behavior itself is correct and load-bearing (it is what
// lets `forge upgrade` regenerate stale codegen without --force), but
// it must never be silent, and the user needs a strict opt-out.
//
// Self-certifying-era contract pinned here:
//
//   - The manifest's current-hash-vs-history distinction is GONE: a
//     verifying marker proves a pristine render of SOME vintage. ANY
//     pristine on-disk body that regeneration actually changes is a
//     heal, and healing is LOUD: HealNoticeFn fires once per file per
//     run.
//   - Notices are DEFERRED: the writer records a pending heal and
//     FlushHealNotices(root) fires the notice only when the FINAL
//     on-disk body differs from the replaced pristine body — so
//     formatting-only churn (goimports converging back) stays silent.
//   - No notice when nothing is destroyed: the new render is
//     body-identical, or a true hand-edit (write skipped entirely).
//   - DisableAutoHeal (the `forge generate --no-heal` escape hatch)
//     treats pristine-but-stale as a hand-edit: the write SKIPS and
//     NoHealSkipFn fires once per file per run instead.
package checksums

import (
	"os"
	"path/filepath"
	"testing"
)

// captureHealNotices swaps HealNoticeFn (and silences NoHealSkipFn) for
// recorders, resets per-run state, and returns the recorded-paths slice
// pointer.
func captureHealNotices(t *testing.T) *[]string {
	t.Helper()
	var got []string
	origHeal, origSkip := HealNoticeFn, NoHealSkipFn
	HealNoticeFn = func(relPath string) { got = append(got, relPath) }
	NoHealSkipFn = func(string) {} // no-op, never nil
	t.Cleanup(func() {
		HealNoticeFn = origHeal
		NoHealSkipFn = origSkip
		ResetPerRunState()
	})
	ResetPerRunState()
	return &got
}

func TestWriteGeneratedFile_HealIsLoud(t *testing.T) {
	renderV1 := []byte("package app // v1\n")
	renderV3 := []byte("package app // v3\n")
	const rel = "pkg/app/app_gen.go"

	stampedV1, _ := Stamp(rel, renderV1)

	tests := []struct {
		name        string
		onDisk      []byte
		write       []byte
		wantWrote   bool
		wantNotices int
	}{
		{
			// The legacy "on-disk matches current hash → silent regen"
			// case has no marker-era equivalent: without history there is
			// no "current vs prior" distinction. Its surviving silent leg
			// is the body-identical regen below.
			name:        "on-disk is the stamped render of the incoming content — no byte change, no notice",
			onDisk:      stampedV1,
			write:       renderV1,
			wantWrote:   true,
			wantNotices: 0,
		},
		{
			name:        "on-disk is a stamped older vintage — heal, LOUD",
			onDisk:      stampedV1,
			write:       renderV3,
			wantWrote:   true,
			wantNotices: 1,
		},
		{
			name:        "true hand-edit — write skipped, no heal notice",
			onDisk:      append(append([]byte{}, stampedV1...), []byte("// user edit\n")...),
			write:       renderV3,
			wantWrote:   false,
			wantNotices: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := captureHealNotices(t)
			ResetSkipWrite()
			root := t.TempDir()
			cs := &FileChecksums{}

			full := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, tt.onDisk, 0o644); err != nil {
				t.Fatal(err)
			}

			wrote, err := WriteGeneratedFileTier1(root, rel, tt.write, cs, false)
			if err != nil {
				t.Fatal(err)
			}
			if wrote != tt.wantWrote {
				t.Errorf("wrote = %v, want %v", wrote, tt.wantWrote)
			}
			// Notices are deferred: nothing fires until the flush.
			if len(*got) != 0 {
				t.Errorf("heal notice fired before FlushHealNotices: %v", *got)
			}
			FlushHealNotices(root)
			if len(*got) != tt.wantNotices {
				t.Errorf("heal notices = %v, want %d", *got, tt.wantNotices)
			}
			if tt.wantNotices > 0 && (*got)[0] != rel {
				t.Errorf("heal notice named %q, want the relPath", (*got)[0])
			}
		})
	}
}

// TestFlushHealNotices_FormattingOnlyChurnIsSilent pins the deferral's
// purpose: when the final on-disk body converges back to the replaced
// pristine body (post-write formatters undoing the emitter's raw
// output), no notice fires — nothing was destroyed.
func TestFlushHealNotices_FormattingOnlyChurnIsSilent(t *testing.T) {
	got := captureHealNotices(t)
	ResetSkipWrite()
	root := t.TempDir()
	cs := &FileChecksums{}
	const rel = "a.go"

	renderV1 := []byte("package app // v1\n")
	stampedV1, _ := Stamp(rel, renderV1)
	if err := os.WriteFile(filepath.Join(root, rel), stampedV1, 0o644); err != nil {
		t.Fatal(err)
	}

	// The write replaces pristine v1 with v2 — a pending heal.
	if _, err := WriteGeneratedFile(root, rel, []byte("package app // v2\n"), cs, false); err != nil {
		t.Fatal(err)
	}
	// A "formatter" rewrites the file back to body-equal v1 before the
	// flush (the converged case).
	if err := os.WriteFile(filepath.Join(root, rel), stampedV1, 0o644); err != nil {
		t.Fatal(err)
	}
	FlushHealNotices(root)
	if len(*got) != 0 {
		t.Errorf("formatting-only heal must be silent; notices = %v", *got)
	}
}

func TestWriteGeneratedFile_HealNoticeDedupedPerRun(t *testing.T) {
	got := captureHealNotices(t)
	ResetSkipWrite()
	root := t.TempDir()
	cs := &FileChecksums{}
	const rel = "a.go"
	renderV1 := []byte("package app // v1\n")
	stampedV1, _ := Stamp(rel, renderV1)

	// Two writes in the same run (different emitters can touch the same
	// path) — one notice. Restore the stale vintage between writes so
	// both calls see the heal condition.
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(filepath.Join(root, rel), stampedV1, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := WriteGeneratedFile(root, rel, []byte("package app // v3\n"), cs, false); err != nil {
			t.Fatal(err)
		}
	}
	FlushHealNotices(root)
	FlushHealNotices(root) // double flush must not re-notice
	if len(*got) != 1 {
		t.Errorf("heal notices = %v, want exactly 1 (per-run dedupe)", *got)
	}

	// A new run (ResetPerRunState) notices again.
	ResetPerRunState()
	if err := os.WriteFile(filepath.Join(root, rel), stampedV1, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteGeneratedFile(root, rel, []byte("package app // v4\n"), cs, false); err != nil {
		t.Fatal(err)
	}
	FlushHealNotices(root)
	if len(*got) != 2 {
		t.Errorf("heal notices after new run = %v, want 2", *got)
	}
}

// TestWriteGeneratedFileTier2_HealIsLoud — Tier-2 writes carry no
// marker, so the legacy "Tier-2 historical match" heal exists in
// exactly one shape now: an existing file whose VERIFYING marker
// certifies it as forge bytes (a template reclassified Tier-1 → Tier-2
// that the user never edited). That file is regenerated once as an
// unmarked scaffold — loudly.
func TestWriteGeneratedFileTier2_HealIsLoud(t *testing.T) {
	got := captureHealNotices(t)
	ResetTier2State()
	defer ResetTier2State()
	ResetSkipWrite()
	root := t.TempDir()
	cs := &FileChecksums{}
	const rel = "svc.go"

	// A pristine Tier-1 render on disk (the pre-reclassification state).
	stampedV1, _ := Stamp(rel, []byte("// scaffold v1\n"))
	if err := os.WriteFile(filepath.Join(root, rel), stampedV1, 0o644); err != nil {
		t.Fatal(err)
	}

	scaffoldV3 := []byte("// scaffold v3\n")
	wrote, err := WriteGeneratedFileTier2(root, rel, scaffoldV3, cs, false)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("Tier-2 write over a VERIFYING marker should proceed (reclassification heal)")
	}
	after, _ := os.ReadFile(filepath.Join(root, rel))
	if string(after) != string(scaffoldV3) {
		t.Errorf("on-disk = %q, want the fresh unmarked scaffold", after)
	}
	if _, found := ExtractMarker(after); found {
		t.Error("reclassified Tier-2 file must lose its marker")
	}
	FlushHealNotices(root)
	if len(*got) != 1 || (*got)[0] != rel {
		t.Errorf("Tier-2 reclassification heal must be loud; notices = %v", *got)
	}

	// An ordinary user-owned Tier-2 file (no marker) is preserved
	// silently — the scaffold-once contract, not a heal.
	const rel2 = "svc2.go"
	if err := os.WriteFile(filepath.Join(root, rel2), []byte("// user code\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrote, err = WriteGeneratedFileTier2(root, rel2, []byte("// new template\n"), cs, false)
	if err != nil || wrote {
		t.Errorf("user-owned Tier-2 file overwritten: wrote=%v err=%v", wrote, err)
	}
	FlushHealNotices(root)
	if len(*got) != 1 {
		t.Errorf("preserving a user-owned Tier-2 file must not notice; got %v", *got)
	}
}

// TestDisableAutoHeal_TreatsStaleAsHandEdit pins the --no-heal strict
// mode: a pristine-but-stale file is treated as a hand-edit — the write
// skips it, content is preserved, and NoHealSkipFn (not HealNoticeFn)
// fires once per file per run.
//
// The legacy assertion that CheckTier1Drift reported the file flagged
// HistoricalMatch has no equivalent: ScanTier1Drift never sees the
// fresh render, so a pristine vintage cannot be distinguished from the
// current one there — the per-write NoHealSkipFn notice is the
// replacement signal.
func TestDisableAutoHeal_TreatsStaleAsHandEdit(t *testing.T) {
	got := captureHealNotices(t)
	var skips []string
	NoHealSkipFn = func(relPath string) { skips = append(skips, relPath) }
	ResetSkipWrite()
	DisableAutoHeal = true
	t.Cleanup(func() { DisableAutoHeal = false })

	root := t.TempDir()
	cs := &FileChecksums{}
	const rel = "pkg/app/app_gen.go"
	renderV1 := []byte("package app // v1\n")
	stampedV1, _ := Stamp(rel, renderV1)
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, stampedV1, 0o644); err != nil {
		t.Fatal(err)
	}

	// The write is skipped, content preserved, no heal notice — the
	// skip notice fires instead, once per run even across two writes.
	for i := 0; i < 2; i++ {
		wrote, err := WriteGeneratedFile(root, rel, []byte("package app // v3\n"), cs, false)
		if err != nil {
			t.Fatal(err)
		}
		if wrote {
			t.Error("write proceeded under DisableAutoHeal on a pristine-but-stale file")
		}
	}
	if content, _ := os.ReadFile(full); string(content) != string(stampedV1) {
		t.Errorf("on-disk content not preserved under --no-heal; got %q", content)
	}
	if len(skips) != 1 || skips[0] != rel {
		t.Errorf("NoHealSkipFn calls = %v, want exactly one naming %s", skips, rel)
	}
	FlushHealNotices(root)
	if len(*got) != 0 {
		t.Errorf("no heal notice expected under DisableAutoHeal; got %v", *got)
	}

	// The file stays pristine, so the stomp guard does NOT flag it.
	if drift := ScanTier1Drift(root, cs); len(drift) != 0 {
		t.Errorf("pristine-but-stale file must not appear as drift; got %+v", drift)
	}
}
