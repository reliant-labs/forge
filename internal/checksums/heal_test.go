// Tests for the LOUD auto-heal contract.
//
// FRICTION cp-forge fr-2c1c2328c7: a hand-edit to pkg/app/bootstrap.go
// happened to hash equal to a PRIOR render in checksum history, so
// generate classified it as stale codegen and silently reverted it —
// the round's only silent destruction of user work. The auto-heal
// behavior itself is correct and load-bearing (it is what lets `forge
// upgrade` distinguish stale codegen from genuine user edits — see
// checksums_test.go), but it must never be silent, and the user needs
// a strict opt-out for the hash-collision-with-history case.
//
// Contract pinned here:
//
//   - Healing is LOUD: overwriting on-disk content that matches a
//     historical-but-not-latest render fires HealNoticeFn once per file
//     per run, for both Tier-1 and Tier-2 writers.
//   - No notice when nothing is destroyed: on-disk == current hash
//     (ordinary regen), on-disk == the new render (no byte change), or
//     a true hand-edit (write skipped entirely).
//   - DisableAutoHeal (the `forge generate --no-heal` escape hatch)
//     treats a historical match as a hand-edit: IsFileModified reports
//     true, the write is skipped, and CheckTier1Drift reports the file
//     as drift flagged HistoricalMatch so the guard message can say so.
package checksums

import (
	"os"
	"path/filepath"
	"testing"
)

// captureHealNotices swaps HealNoticeFn for a recorder and returns the
// recorded paths slice pointer + a restore func.
func captureHealNotices(t *testing.T) *[]string {
	t.Helper()
	var got []string
	orig := HealNoticeFn
	HealNoticeFn = func(relPath string) { got = append(got, relPath) }
	t.Cleanup(func() {
		HealNoticeFn = orig
		ResetPerRunState()
	})
	ResetPerRunState()
	return &got
}

func TestWriteGeneratedFile_HealIsLoud(t *testing.T) {
	renderV1 := []byte("package app // v1\n")
	renderV2 := []byte("package app // v2\n")
	renderV3 := []byte("package app // v3\n")
	handEdit := []byte("package app // user edit\n")

	tests := []struct {
		name        string
		onDisk      []byte
		write       []byte
		wantWrote   bool
		wantNotices int
	}{
		{
			name:        "on-disk matches current hash — ordinary regen, no notice",
			onDisk:      renderV2,
			write:       renderV3,
			wantWrote:   true,
			wantNotices: 0,
		},
		{
			name:        "on-disk matches historical hash — heal, LOUD",
			onDisk:      renderV1,
			write:       renderV3,
			wantWrote:   true,
			wantNotices: 1,
		},
		{
			name:        "on-disk matches historical hash but new render is identical — no byte change, no notice",
			onDisk:      renderV1,
			write:       renderV1,
			wantWrote:   true,
			wantNotices: 0,
		},
		{
			name:        "true hand-edit — write skipped, no heal notice",
			onDisk:      handEdit,
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
			cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
			seedHistory(cs, "pkg/app/app_gen.go", renderV1, renderV2)

			full := filepath.Join(root, "pkg", "app", "app_gen.go")
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, tt.onDisk, 0o644); err != nil {
				t.Fatal(err)
			}

			wrote, err := WriteGeneratedFileTier1(root, "pkg/app/app_gen.go", tt.write, cs, false)
			if err != nil {
				t.Fatal(err)
			}
			if wrote != tt.wantWrote {
				t.Errorf("wrote = %v, want %v", wrote, tt.wantWrote)
			}
			if len(*got) != tt.wantNotices {
				t.Errorf("heal notices = %v, want %d", *got, tt.wantNotices)
			}
			if tt.wantNotices > 0 && (*got)[0] != "pkg/app/app_gen.go" {
				t.Errorf("heal notice named %q, want the relPath", (*got)[0])
			}
		})
	}
}

func TestWriteGeneratedFile_HealNoticeDedupedPerRun(t *testing.T) {
	got := captureHealNotices(t)
	ResetSkipWrite()
	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	renderV1 := []byte("package app // v1\n")
	renderV2 := []byte("package app // v2\n")
	seedHistory(cs, "a.go", renderV1, renderV2)
	if err := os.WriteFile(filepath.Join(root, "a.go"), renderV1, 0o644); err != nil {
		t.Fatal(err)
	}

	// Two writes in the same run (different emitters can touch the same
	// path) — one notice.
	for i := 0; i < 2; i++ {
		// Restore the historical content between writes so both calls see
		// the heal condition.
		if err := os.WriteFile(filepath.Join(root, "a.go"), renderV1, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := WriteGeneratedFile(root, "a.go", []byte("package app // v3\n"), cs, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(*got) != 1 {
		t.Errorf("heal notices = %v, want exactly 1 (per-run dedupe)", *got)
	}

	// A new run (ResetPerRunState) notices again.
	ResetPerRunState()
	if err := os.WriteFile(filepath.Join(root, "a.go"), renderV1, 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-seed: the previous writes moved Hash to v3 and v1 is still in
	// history, so the heal condition holds.
	if _, err := WriteGeneratedFile(root, "a.go", []byte("package app // v4\n"), cs, false); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 {
		t.Errorf("heal notices after new run = %v, want 2", *got)
	}
}

func TestWriteGeneratedFileTier2_HealIsLoud(t *testing.T) {
	got := captureHealNotices(t)
	ResetTier2State()
	defer ResetTier2State()
	ResetSkipWrite()
	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}

	scaffoldV1 := []byte("// scaffold v1\n")
	scaffoldV2 := []byte("// scaffold v2\n")
	if _, err := WriteGeneratedFileTier2(root, "svc.go", scaffoldV1, cs, false); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteGeneratedFileTier2(root, "svc.go", scaffoldV2, cs, false); err != nil {
		t.Fatal(err)
	}
	// Revert on-disk to the historical render — e.g. the user restored an
	// old version, or a hand-edit collides with a prior render's hash.
	if err := os.WriteFile(filepath.Join(root, "svc.go"), scaffoldV1, 0o644); err != nil {
		t.Fatal(err)
	}

	wrote, err := WriteGeneratedFileTier2(root, "svc.go", []byte("// scaffold v3\n"), cs, false)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("Tier-2 historical-match write should proceed (auto-heal)")
	}
	if len(*got) != 1 || (*got)[0] != "svc.go" {
		t.Errorf("Tier-2 heal must be loud; notices = %v", *got)
	}
}

func TestDisableAutoHeal_TreatsHistoricalAsHandEdit(t *testing.T) {
	got := captureHealNotices(t)
	ResetSkipWrite()
	DisableAutoHeal = true
	t.Cleanup(func() { DisableAutoHeal = false })

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	renderV1 := []byte("package app // v1\n")
	renderV2 := []byte("package app // v2\n")
	seedHistory(cs, "pkg/app/app_gen.go", renderV1, renderV2)
	full := filepath.Join(root, "pkg", "app", "app_gen.go")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, renderV1, 0o644); err != nil {
		t.Fatal(err)
	}

	// IsFileModified: historical match counts as a hand-edit under --no-heal.
	if !cs.IsFileModified(root, "pkg/app/app_gen.go") {
		t.Error("IsFileModified = false under DisableAutoHeal; historical match must count as modified")
	}

	// The write is skipped, content preserved, no heal notice.
	wrote, err := WriteGeneratedFile(root, "pkg/app/app_gen.go", []byte("package app // v3\n"), cs, false)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Error("write proceeded under DisableAutoHeal on a historical match")
	}
	if content, _ := os.ReadFile(full); string(content) != string(renderV1) {
		t.Errorf("on-disk content not preserved under --no-heal; got %q", content)
	}
	if len(*got) != 0 {
		t.Errorf("no heal notice expected under DisableAutoHeal; got %v", *got)
	}

	// The stomp guard reports the file, flagged as a historical match.
	drift := cs.CheckTier1Drift(root)
	if len(drift) != 1 {
		t.Fatalf("CheckTier1Drift under DisableAutoHeal = %d entries, want 1", len(drift))
	}
	if !drift[0].HistoricalMatch {
		t.Error("drift entry should be flagged HistoricalMatch so the guard message can explain --no-heal semantics")
	}
}
