package checksums

import (
	"os"
	"path/filepath"
	"testing"
)

// Disown lifecycle contract (self-certifying era):
//
//   - DisownPaths strips the embedded forge:hash marker from the file
//     (a user-owned file must not advertise forge certification) and
//     records {Reason, DisownedAt} in cs.Disowned (.forge/disowned.json).
//   - While the file exists, NO WriteGeneratedFile* call touches it —
//     not even force=true.
//   - Re-adoption is by deletion: with the file gone, the next Tier-1
//     write re-emits (stamped) and the disowned record is cleared.
//
// The legacy fork-era entry flags (Forked/Accepted/ForkedAt) no longer
// exist — the one-time migration (migrate.go) converts them to plain
// disowned records; see migrate_test.go.

func writeDisownFixture(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDisownPaths_RecordsReasonAndStripsMarker(t *testing.T) {
	orig := nowRFC3339
	nowRFC3339 = func() string { return "2026-06-10T00:00:00Z" }
	defer func() { nowRFC3339 = orig }()
	ResetSkipWrite()

	root := t.TempDir()
	cs := &FileChecksums{}
	const rel = "pkg/app/wire_gen.go"
	const reason = "custom wiring forge cannot express"

	// Pristine Tier-1 render, then a user hand-edit (whole-file replace,
	// marker gone — the common shape right before a disown).
	pristine := []byte("package app // pristine\n")
	if _, err := WriteGeneratedFileTier1(root, rel, pristine, cs, false); err != nil {
		t.Fatal(err)
	}
	userContent := []byte("package app // user edit\n")
	writeDisownFixture(t, root, rel, userContent)

	if err := cs.DisownPaths(root, []string{rel}, reason); err != nil {
		t.Fatalf("DisownPaths: %v", err)
	}
	entry, ok := cs.Disowned[rel]
	if !ok {
		t.Fatalf("no disowned record for %s", rel)
	}
	if entry.Reason != reason {
		t.Errorf("Reason = %q, want %q", entry.Reason, reason)
	}
	if entry.DisownedAt != "2026-06-10T00:00:00Z" {
		t.Errorf("DisownedAt = %q, want pinned timestamp", entry.DisownedAt)
	}
	// The user's bytes are untouched (there was no marker to strip).
	got, _ := os.ReadFile(filepath.Join(root, rel))
	if string(got) != string(userContent) {
		t.Errorf("disown modified the user's content: %q", got)
	}

	// Disowning a STILL-PRISTINE file strips the marker but preserves the
	// body — the file must stop advertising forge certification.
	const rel2 = "pkg/app/bootstrap.go"
	body2 := []byte("package app // taken over while pristine\n")
	if _, err := WriteGeneratedFileTier1(root, rel2, body2, cs, false); err != nil {
		t.Fatal(err)
	}
	if err := cs.DisownPaths(root, []string{rel2}, reason); err != nil {
		t.Fatalf("DisownPaths(pristine): %v", err)
	}
	got2, _ := os.ReadFile(filepath.Join(root, rel2))
	if _, found := ExtractMarker(got2); found {
		t.Errorf("disowned file still carries a forge:hash marker:\n%s", got2)
	}
	if string(got2) != string(body2) {
		t.Errorf("marker strip altered the body; got %q, want %q", got2, body2)
	}
}

func TestDisownPaths_DropsUnstampableRecord(t *testing.T) {
	root := t.TempDir()
	const rel = "config/app.json"
	content := []byte("{\"a\":1}\n")
	writeDisownFixture(t, root, rel, content)
	cs := &FileChecksums{Unstampable: map[string]string{rel: BodyHash(content)}}

	if err := cs.DisownPaths(root, []string{rel}, "ours now"); err != nil {
		t.Fatalf("DisownPaths: %v", err)
	}
	if _, ok := cs.Unstampable[rel]; ok {
		t.Error("scoped fallback record must be dropped on disown")
	}
	if !cs.IsDisowned(rel) {
		t.Error("disown record missing")
	}
}

func TestDisownPaths_MissingFileErrors(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{}
	if err := cs.DisownPaths(root, []string{"gone.go"}, "why"); err == nil {
		t.Fatalf("DisownPaths on a missing file must error (the on-disk content IS the thing being disowned)")
	}
}

func TestWriteGeneratedFile_DisownedSkipsEvenWithForce(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState()
	defer ResetPerRunState()

	root := t.TempDir()
	cs := &FileChecksums{}
	const rel = "pkg/app/wire_gen.go"
	userContent := []byte("package app // user-owned\n")
	writeDisownFixture(t, root, rel, userContent)
	if err := cs.DisownPaths(root, []string{rel}, "user-owned"); err != nil {
		t.Fatal(err)
	}

	for _, force := range []bool{false, true} {
		wrote, err := WriteGeneratedFile(root, rel, []byte("package app // fresh render\n"), cs, force)
		if err != nil {
			t.Fatalf("force=%v: %v", force, err)
		}
		if wrote {
			t.Errorf("force=%v: disowned file must never be written", force)
		}
	}
	// The Tier-1 wrapper takes the same skip path.
	wrote, err := WriteGeneratedFileTier1(root, rel, []byte("package app // fresh render\n"), cs, true)
	if err != nil || wrote {
		t.Errorf("Tier-1 wrapper: wrote=%v err=%v, want skip", wrote, err)
	}
	// The Tier-2 writer skips too (disowned content is the USER's; the
	// Tier-2 preserve-existing check alone would not record the intent).
	wrote, err = WriteGeneratedFileTier2(root, rel, []byte("package app // fresh render\n"), cs, false)
	if err != nil || wrote {
		t.Errorf("Tier-2 writer: wrote=%v err=%v, want skip", wrote, err)
	}

	onDisk, _ := os.ReadFile(filepath.Join(root, rel))
	if string(onDisk) != string(userContent) {
		t.Errorf("disowned file modified: %q", onDisk)
	}
	// No side renders are parked for disowned paths — there is no
	// reconcile-later state.
	if _, err := os.Stat(filepath.Join(root, RenderDir, rel)); !os.IsNotExist(err) {
		t.Errorf(".forge/render side render must not be parked for disowned paths")
	}
}

func TestWriteGeneratedFile_DeletedDisownedFileIsReAdopted(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState()
	defer ResetPerRunState()

	root := t.TempDir()
	cs := &FileChecksums{}
	const rel = "pkg/app/wire_gen.go"
	writeDisownFixture(t, root, rel, []byte("package app // user-owned\n"))
	if err := cs.DisownPaths(root, []string{rel}, "user-owned"); err != nil {
		t.Fatal(err)
	}

	// The documented re-adoption path: delete the file, regenerate.
	if err := os.Remove(filepath.Join(root, rel)); err != nil {
		t.Fatal(err)
	}
	fresh := []byte("package app // pristine render\n")
	wrote, err := WriteGeneratedFileTier1(root, rel, fresh, cs, false)
	if err != nil {
		t.Fatalf("re-adopting write: %v", err)
	}
	if !wrote {
		t.Fatalf("deleted disowned file must be re-emitted")
	}
	onDisk, _ := os.ReadFile(filepath.Join(root, rel))
	wantStamped, _ := Stamp(rel, fresh)
	if string(onDisk) != string(wantStamped) {
		t.Errorf("on-disk = %q, want the stamped pristine render", onDisk)
	}
	if Verify(onDisk) != Pristine {
		t.Errorf("re-adopted file must self-certify; Verify = %v", Verify(onDisk))
	}
	if cs.IsDisowned(rel) {
		t.Errorf("disowned record not cleared after re-adoption: %+v", cs.Disowned[rel])
	}
}

func TestScanTier1Drift_IgnoresDisowned(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{}
	const rel = "pkg/app/wire_gen.go"
	writeDisownFixture(t, root, rel, []byte("package app // user-owned\n"))
	if err := cs.DisownPaths(root, []string{rel}, "user-owned"); err != nil {
		t.Fatal(err)
	}
	// Edit AFTER disowning — still never drift (the marker was stripped,
	// so the scan sees a plain user file).
	writeDisownFixture(t, root, rel, []byte("package app // edited again\n"))
	if drift := ScanTier1Drift(root, cs); len(drift) != 0 {
		t.Errorf("disowned file must never trip the Tier-1 stomp guard; got %+v", drift)
	}

	// Belt-and-braces: even a file that still carries a FAILING marker
	// (e.g. the user restored an old stamped copy from git after the
	// disown) is skipped — the disowned record wins over the marker.
	stamped, _ := Stamp(rel, []byte("package app // old render\n"))
	writeDisownFixture(t, root, rel, append(stamped, []byte("// edited\n")...))
	if drift := ScanTier1Drift(root, cs); len(drift) != 0 {
		t.Errorf("disowned record must override a failing marker; got %+v", drift)
	}

	// And a disowned unstampable record is skipped too.
	const jsonRel = "config/app.json"
	writeDisownFixture(t, root, jsonRel, []byte("{\"edited\":true}\n"))
	cs.Unstampable = map[string]string{jsonRel: BodyHash([]byte("{\"orig\":true}\n"))}
	cs.Disowned[jsonRel] = DisownedEntry{Reason: "user-owned"}
	if drift := ScanTier1Drift(root, cs); len(drift) != 0 {
		t.Errorf("disowned unstampable path must not drift; got %+v", drift)
	}
}

func TestWriteGeneratedFileTier2_ReScaffoldClearsDisownedMarker(t *testing.T) {
	ResetSkipWrite()
	defer ResetPerRunState()

	root := t.TempDir()
	cs := &FileChecksums{}
	const rel = "internal/svc/service.go"
	writeDisownFixture(t, root, rel, []byte("package svc // user\n"))
	if err := cs.DisownPaths(root, []string{rel}, "user-owned"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, rel)); err != nil {
		t.Fatal(err)
	}

	scaffold := []byte("package svc // scaffold\n")
	wrote, err := WriteGeneratedFileTier2(root, rel, scaffold, cs, false)
	if err != nil || !wrote {
		t.Fatalf("Tier-2 re-scaffold of a deleted disowned file: wrote=%v err=%v", wrote, err)
	}
	if cs.IsDisowned(rel) {
		t.Errorf("disowned record not cleared by the Tier-2 re-scaffold: %+v", cs.Disowned[rel])
	}
	// Tier-2 scaffolds are user-owned from birth: no marker.
	got, _ := os.ReadFile(filepath.Join(root, rel))
	if string(got) != string(scaffold) {
		t.Errorf("re-scaffold content = %q, want %q", got, scaffold)
	}
	if _, found := ExtractMarker(got); found {
		t.Errorf("Tier-2 scaffold must not carry a forge:hash marker:\n%s", got)
	}
}
