// Tests for disown friction capture (friction_disown.go): `forge
// disown` and the deprecated `forge generate --accept` alias must each
// record one .forge/friction.jsonl entry per disowned path, the audit
// disowned_files surface must carry the newest recorded reason
// (including legacy area=fork entries), the no-reason nudge must print
// for internal callers, and the friction log must stay untouched
// whenever nothing was actually disowned.
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// loadDisownFrictionEntries reads <root>/.forge/friction.jsonl through
// the production reader so the assertions exercise the same parse path
// consumers use.
func loadDisownFrictionEntries(t *testing.T, root string) []FrictionEntry {
	t.Helper()
	entries, malformed, err := loadFrictionEntries(filepath.Join(root, ".forge", "friction.jsonl"))
	if err != nil {
		t.Fatalf("loadFrictionEntries: %v", err)
	}
	if malformed != 0 {
		t.Fatalf("friction log has %d malformed line(s) — writer emitted a torn/bad record", malformed)
	}
	return entries
}

// requireNoFrictionFile asserts the friction log was never created —
// the "untouched when nothing is disowned" contract. The recorder must
// not even open the file for a zero-path call (O_CREATE would leave an
// empty log behind and make 'was anything recorded?' ambiguous).
func requireNoFrictionFile(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".forge", "friction.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("friction.jsonl exists (stat err=%v); expected no file when nothing was disowned", err)
	}
}

// TestGenerateAccept_RecordsDisownFriction drives the real drift-guard
// step with Accept=true over a project with two drifted Tier-1 files
// and pins the entry shape: one entry per path, severity=note,
// area=disown, source="generate --accept", context=[<relpath>], and
// text = the --reason (or, for internal callers that bypass the cobra
// requirement, the unstated placeholder naming the path).
func TestGenerateAccept_RecordsDisownFriction(t *testing.T) {
	const (
		relWire = "pkg/app/wire_gen.go"
		relCmd  = "cmd/server.go"
	)
	tests := []struct {
		name   string
		reason string
		// wantText returns the expected entry text for a given path.
		wantText func(rel string) string
	}{
		{
			name:     "with reason: --reason text recorded verbatim per path",
			reason:   "needed per-tenant pool sizing the generated wiring can't express",
			wantText: func(string) string { return "needed per-tenant pool sizing the generated wiring can't express" },
		},
		{
			name:     "without reason: per-path unstated placeholder recorded",
			reason:   "",
			wantText: disownReasonUnstatedText,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer checksums.ResetSkipWrite()

			dir := t.TempDir()
			// proto/services/ makes HasServices true so wire_gen.go drift
			// is classified in-scope by the drift scope filter.
			mustWriteScopeFile(t, filepath.Join(dir, "proto", "services", "api", "v1", "api.proto"), "syntax = \"proto3\";\n")

			cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{}}
			for _, rel := range []string{relWire, relCmd} {
				recorded := []byte("package x // as generated: " + rel + "\n")
				mustWriteScopeFile(t, filepath.Join(dir, rel), "package x // hand-edited: "+rel+"\n")
				cs.RecordFile(rel, recorded)
				entry := cs.Files[rel]
				entry.Tier = 1
				cs.Files[rel] = entry
			}

			ctx := &pipelineContext{
				ProjectDir:   dir,
				AbsPath:      dir,
				Checksums:    cs,
				Accept:       true,
				AcceptReason: tt.reason,
			}
			if err := stepCheckTier1Drift(ctx); err != nil {
				t.Fatalf("stepCheckTier1Drift with --accept: %v", err)
			}

			// The accept alias must have DISOWNED both paths.
			for _, rel := range []string{relWire, relCmd} {
				e := cs.Files[rel]
				if e.Tier != 2 || !e.Disowned {
					t.Errorf("%s: entry = %+v, want disowned Tier-2", rel, e)
				}
			}

			entries := loadDisownFrictionEntries(t, dir)
			if len(entries) != 2 {
				t.Fatalf("got %d friction entries, want exactly one per disowned path (2): %+v", len(entries), entries)
			}
			byPath := map[string]FrictionEntry{}
			for _, e := range entries {
				if len(e.Context) != 1 {
					t.Fatalf("entry context = %v, want exactly the disowned relpath", e.Context)
				}
				byPath[e.Context[0]] = e
			}
			for _, rel := range []string{relWire, relCmd} {
				e, ok := byPath[rel]
				if !ok {
					t.Fatalf("no friction entry recorded for %s; got %+v", rel, byPath)
				}
				if e.Severity != "note" {
					t.Errorf("%s: severity = %q, want note", rel, e.Severity)
				}
				if e.Area != frictionAreaDisown {
					t.Errorf("%s: area = %q, want %q", rel, e.Area, frictionAreaDisown)
				}
				if e.Source != "generate --accept" {
					t.Errorf("%s: source = %q, want %q", rel, e.Source, "generate --accept")
				}
				if want := tt.wantText(rel); e.Text != want {
					t.Errorf("%s: text = %q, want %q", rel, e.Text, want)
				}
				if e.Schema != frictionSchemaVersion {
					t.Errorf("%s: schema = %d, want %d", rel, e.Schema, frictionSchemaVersion)
				}
				if !strings.HasPrefix(e.ID, "fr-") {
					t.Errorf("%s: id = %q, want fr- content hash", rel, e.ID)
				}
			}
			// Same reason + same second across two paths must NOT alias to
			// one id — context participates in the hash (friction.go).
			if byPath[relWire].ID == byPath[relCmd].ID {
				t.Errorf("entries for distinct paths share id %q", byPath[relWire].ID)
			}
		})
	}
}

// TestGenerateAccept_NoDriftLeavesFrictionUntouched: an --accept run
// with nothing drifted disowns nothing, so the friction log must not
// be created at all.
func TestGenerateAccept_NoDriftLeavesFrictionUntouched(t *testing.T) {
	defer checksums.ResetSkipWrite()
	dir := t.TempDir()
	const rel = "pkg/app/wire_gen.go"
	content := []byte("package app // pristine\n")
	mustWriteScopeFile(t, filepath.Join(dir, rel), string(content))
	cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{}}
	cs.RecordFile(rel, content)
	entry := cs.Files[rel]
	entry.Tier = 1
	cs.Files[rel] = entry

	ctx := &pipelineContext{ProjectDir: dir, AbsPath: dir, Checksums: cs, Accept: true, AcceptReason: "should never be written"}
	if err := stepCheckTier1Drift(ctx); err != nil {
		t.Fatalf("stepCheckTier1Drift: %v", err)
	}
	requireNoFrictionFile(t, dir)
}

// TestDisown_NoFlipLeavesFrictionUntouched: already-disowned paths flip
// nothing, and --dry-run writes nothing — in both cases the friction
// log must stay absent (an entry would claim a disown that never
// happened this run).
func TestDisown_NoFlipLeavesFrictionUntouched(t *testing.T) {
	t.Run("already disowned", func(t *testing.T) {
		const rel = "pkg/app/bootstrap.go"
		cs := &checksums.FileChecksums{
			Files: map[string]checksums.FileChecksumEntry{
				rel: {Hash: "h", Tier: 2, Disowned: true},
			},
		}
		root := withUnforkProjectRoot(t, cs)
		seedDisownFile(t, root, rel, "package app\n")
		if err := runDisown([]string{rel}, "stale reason", false); err != nil {
			t.Fatalf("runDisown: %v", err)
		}
		requireNoFrictionFile(t, root)
	})
	t.Run("dry-run", func(t *testing.T) {
		const rel = "pkg/app/bootstrap.go"
		cs := &checksums.FileChecksums{
			Files: map[string]checksums.FileChecksumEntry{
				rel: {Hash: "h", Tier: 1},
			},
		}
		root := withUnforkProjectRoot(t, cs)
		seedDisownFile(t, root, rel, "package app\n")
		if err := runDisown([]string{rel}, "dry reason", true); err != nil {
			t.Fatalf("runDisown --dry-run: %v", err)
		}
		requireNoFrictionFile(t, root)
	})
}

// TestRecordDisownFriction_Nudge pins the agent-facing UX: no --reason
// ⇒ exactly one loud nudge line naming the flag and the list query; a
// provided reason ⇒ no nudge. Never an interactive prompt — the
// recorder takes a writer, not a reader. (User-facing commands refuse
// up-front without --reason; the nudge covers internal callers.)
func TestRecordDisownFriction_Nudge(t *testing.T) {
	t.Run("missing reason prints the nudge", func(t *testing.T) {
		root := t.TempDir()
		var buf strings.Builder
		recordDisownFriction(root, "disown", "", []string{"pkg/app/wire_gen.go"}, &buf)
		out := buf.String()
		if !strings.Contains(out, disownReasonNudge) {
			t.Errorf("output missing nudge %q; got:\n%s", disownReasonNudge, out)
		}
		if strings.Count(out, "tip: record WHY with --reason") != 1 {
			t.Errorf("nudge must print exactly once per invocation; got:\n%s", out)
		}
	})
	t.Run("provided reason stays quiet", func(t *testing.T) {
		root := t.TempDir()
		var buf strings.Builder
		recordDisownFriction(root, "disown", "documented why", []string{"pkg/app/wire_gen.go"}, &buf)
		if strings.Contains(buf.String(), "tip: record WHY") {
			t.Errorf("nudge printed despite a reason being given:\n%s", buf.String())
		}
	})
	t.Run("zero paths: no write, no nudge", func(t *testing.T) {
		root := t.TempDir()
		var buf strings.Builder
		recordDisownFriction(root, "disown", "", nil, &buf)
		if buf.String() != "" {
			t.Errorf("expected silence for zero disowned paths; got %q", buf.String())
		}
		requireNoFrictionFile(t, root)
	})
}

// TestAuditCodegen_DisownedFilesCarriesReason pins the audit surface:
// disowned_files rows carry the NEWEST area=disown friction text whose
// context names the path; LEGACY area=fork entries still join (reasons
// recorded by pre-disown forge versions survive the migration); rows
// without any entry stay reason-less; non-disown areas are ignored.
func TestAuditCodegen_DisownedFilesCarriesReason(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".forge"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("package app // user-owned\n")
	for _, rel := range []string{"pkg/app/wire_gen.go", "pkg/app/bootstrap.go", "pkg/app/migrate.go"} {
		mustWriteScopeFile(t, filepath.Join(dir, rel), string(content))
	}
	csBody := `{
  "forge_version": "test",
  "files": {
    "pkg/app/wire_gen.go": {"hash": "` + checksums.Hash(content) + `", "tier": 2, "disowned": true},
    "pkg/app/bootstrap.go": {"hash": "` + checksums.Hash(content) + `", "tier": 2, "disowned": true},
    "pkg/app/migrate.go": {"hash": "` + checksums.Hash(content) + `", "tier": 2, "disowned": true}
  }
}`
	if err := os.WriteFile(filepath.Join(dir, ".forge", "checksums.json"), []byte(csBody), 0o644); err != nil {
		t.Fatal(err)
	}
	// Friction log: an older disown entry, a NEWER one for the same path
	// (must win), a LEGACY area=fork entry for another path (must still
	// join), and an unrelated non-disown-area entry that must be ignored
	// even though its context matches.
	jsonl := `{"schema":1,"id":"fr-old","recorded_at":"2026-06-01T00:00:00Z","forge_version":"t","severity":"note","area":"disown","source":"disown","context":["pkg/app/wire_gen.go"],"text":"old reason"}
{"schema":1,"id":"fr-new","recorded_at":"2026-06-05T00:00:00Z","forge_version":"t","severity":"note","area":"disown","source":"disown","context":["pkg/app/wire_gen.go"],"text":"newest reason wins"}
{"schema":1,"id":"fr-leg","recorded_at":"2026-02-01T00:00:00Z","forge_version":"t","severity":"note","area":"fork","source":"generate --accept","context":["pkg/app/bootstrap.go"],"text":"legacy fork-era reason"}
{"schema":1,"id":"fr-oth","recorded_at":"2026-06-09T00:00:00Z","forge_version":"t","severity":"p1","area":"codegen","source":"agent","context":["pkg/app/wire_gen.go"],"text":"not a disown reason"}
`
	if err := os.WriteFile(filepath.Join(dir, ".forge", "friction.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}

	cat := auditCodegen(nil, dir)
	disowned, ok := cat.Details["disowned_files"].([]auditDisownedFile)
	if !ok {
		t.Fatalf("disowned_files detail missing or wrong shape: %#v", cat.Details["disowned_files"])
	}
	if len(disowned) != 3 {
		t.Fatalf("disowned_files = %+v, want all three disowned entries", disowned)
	}
	byPath := map[string]auditDisownedFile{}
	for _, f := range disowned {
		byPath[f.Path] = f
	}
	if got := byPath["pkg/app/wire_gen.go"].Reason; got != "newest reason wins" {
		t.Errorf("wire_gen.go reason = %q, want the newest area=disown entry text", got)
	}
	if got := byPath["pkg/app/bootstrap.go"].Reason; got != "legacy fork-era reason" {
		t.Errorf("bootstrap.go reason = %q, want the legacy area=fork text to survive the migration", got)
	}
	if got := byPath["pkg/app/migrate.go"].Reason; got != "" {
		t.Errorf("migrate.go reason = %q, want empty (no friction entry for it)", got)
	}
}
