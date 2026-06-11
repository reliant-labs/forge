// Tests for fork-acceptance friction capture (friction_fork.go):
// `forge generate --accept [--reason]` and `forge generate accept-fork
// [--reason]` must each record one .forge/friction.jsonl entry per
// accepted path, the audit forked_files surface must carry the newest
// recorded reason, the no-reason nudge must print, and the friction
// log must stay untouched whenever nothing was actually accepted.
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// loadForkFrictionEntries reads <root>/.forge/friction.jsonl through
// the production reader so the assertions exercise the same parse path
// consumers use.
func loadForkFrictionEntries(t *testing.T, root string) []FrictionEntry {
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
// the "untouched when nothing is accepted" contract. The recorder must
// not even open the file for a zero-path call (O_CREATE would leave an
// empty log behind and make 'was anything recorded?' ambiguous).
func requireNoFrictionFile(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".forge", "friction.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("friction.jsonl exists (stat err=%v); expected no file when nothing was accepted", err)
	}
}

// TestGenerateAccept_RecordsForkFriction drives the real drift-guard
// step with Accept=true over a project with two drifted Tier-1 files
// and pins the entry shape: one entry per path, severity=note,
// area=fork, source="generate --accept", context=[<relpath>], and text
// = the --reason (or the unstated placeholder naming the path).
func TestGenerateAccept_RecordsForkFriction(t *testing.T) {
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
			wantText: forkReasonUnstatedText,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The accept branch populates the per-run SkipWrite set —
			// package-global; reset so cases don't leak into each other.
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

			entries := loadForkFrictionEntries(t, dir)
			if len(entries) != 2 {
				t.Fatalf("got %d friction entries, want exactly one per accepted path (2): %+v", len(entries), entries)
			}
			byPath := map[string]FrictionEntry{}
			for _, e := range entries {
				if len(e.Context) != 1 {
					t.Fatalf("entry context = %v, want exactly the forked relpath", e.Context)
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
				if e.Area != frictionAreaFork {
					t.Errorf("%s: area = %q, want %q", rel, e.Area, frictionAreaFork)
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
// with nothing drifted accepts nothing, so the friction log must not
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

// TestAcceptFork_RecordsForkFriction pins the accept-fork surface:
// one entry per just-flipped path, source="accept-fork", text from
// --reason (or the unstated placeholder).
func TestAcceptFork_RecordsForkFriction(t *testing.T) {
	tests := []struct {
		name     string
		reason   string
		wantText func(rel string) string
	}{
		{
			name:     "with reason",
			reason:   "long-lived cp-forge bootstrap fork; custom DI ordering",
			wantText: func(string) string { return "long-lived cp-forge bootstrap fork; custom DI ordering" },
		},
		{
			name:     "without reason",
			reason:   "",
			wantText: forkReasonUnstatedText,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := &checksums.FileChecksums{
				Files: map[string]checksums.FileChecksumEntry{
					"pkg/app/bootstrap.go": {Hash: "h1", Tier: 1, Forked: true},
					"pkg/app/wire_gen.go":  {Hash: "h2", Tier: 1, Forked: true},
				},
			}
			root := withUnforkProjectRoot(t, cs)

			args := []string{"pkg/app/bootstrap.go", "pkg/app/wire_gen.go"}
			if err := runAcceptFork(args, false, tt.reason); err != nil {
				t.Fatalf("runAcceptFork: %v", err)
			}

			entries := loadForkFrictionEntries(t, root)
			if len(entries) != 2 {
				t.Fatalf("got %d friction entries, want one per flipped path (2): %+v", len(entries), entries)
			}
			byPath := map[string]FrictionEntry{}
			for _, e := range entries {
				if len(e.Context) != 1 {
					t.Fatalf("entry context = %v, want exactly the forked relpath", e.Context)
				}
				byPath[e.Context[0]] = e
			}
			for _, rel := range args {
				e, ok := byPath[rel]
				if !ok {
					t.Fatalf("no friction entry for %s", rel)
				}
				if e.Severity != "note" || e.Area != frictionAreaFork || e.Source != "accept-fork" {
					t.Errorf("%s: entry = severity %q area %q source %q, want note/%s/accept-fork", rel, e.Severity, e.Area, e.Source, frictionAreaFork)
				}
				if want := tt.wantText(rel); e.Text != want {
					t.Errorf("%s: text = %q, want %q", rel, e.Text, want)
				}
			}
		})
	}
}

// TestAcceptFork_NoFlipLeavesFrictionUntouched: paths that are already
// accepted flip nothing, and --dry-run writes nothing — in both cases
// the friction log must stay absent (an entry would claim an accept
// that never happened this run).
func TestAcceptFork_NoFlipLeavesFrictionUntouched(t *testing.T) {
	t.Run("already accepted", func(t *testing.T) {
		cs := &checksums.FileChecksums{
			Files: map[string]checksums.FileChecksumEntry{
				"pkg/app/bootstrap.go": {Hash: "h", Tier: 1, Forked: true, Accepted: true},
			},
		}
		root := withUnforkProjectRoot(t, cs)
		if err := runAcceptFork([]string{"pkg/app/bootstrap.go"}, false, "stale reason"); err != nil {
			t.Fatalf("runAcceptFork: %v", err)
		}
		requireNoFrictionFile(t, root)
	})
	t.Run("dry-run", func(t *testing.T) {
		cs := &checksums.FileChecksums{
			Files: map[string]checksums.FileChecksumEntry{
				"pkg/app/bootstrap.go": {Hash: "h", Tier: 1, Forked: true},
			},
		}
		root := withUnforkProjectRoot(t, cs)
		if err := runAcceptFork([]string{"pkg/app/bootstrap.go"}, true, "dry reason"); err != nil {
			t.Fatalf("runAcceptFork --dry-run: %v", err)
		}
		requireNoFrictionFile(t, root)
	})
}

// TestRecordForkFriction_Nudge pins the agent-facing UX: no --reason ⇒
// exactly one loud nudge line naming the flag and the list query; a
// provided reason ⇒ no nudge. Never an interactive prompt — the
// recorder takes a writer, not a reader.
func TestRecordForkFriction_Nudge(t *testing.T) {
	t.Run("missing reason prints the nudge", func(t *testing.T) {
		root := t.TempDir()
		var buf strings.Builder
		recordForkFriction(root, "accept-fork", "", []string{"pkg/app/wire_gen.go"}, &buf)
		out := buf.String()
		if !strings.Contains(out, forkReasonNudge) {
			t.Errorf("output missing nudge %q; got:\n%s", forkReasonNudge, out)
		}
		if strings.Count(out, "tip: record WHY with --reason") != 1 {
			t.Errorf("nudge must print exactly once per invocation; got:\n%s", out)
		}
	})
	t.Run("provided reason stays quiet", func(t *testing.T) {
		root := t.TempDir()
		var buf strings.Builder
		recordForkFriction(root, "accept-fork", "documented why", []string{"pkg/app/wire_gen.go"}, &buf)
		if strings.Contains(buf.String(), "tip: record WHY") {
			t.Errorf("nudge printed despite a reason being given:\n%s", buf.String())
		}
	})
	t.Run("zero paths: no write, no nudge", func(t *testing.T) {
		root := t.TempDir()
		var buf strings.Builder
		recordForkFriction(root, "accept-fork", "", nil, &buf)
		if buf.String() != "" {
			t.Errorf("expected silence for zero accepted paths; got %q", buf.String())
		}
		requireNoFrictionFile(t, root)
	})
}

// TestAuditCodegen_ForkedFilesCarriesReason pins the audit surface:
// forked_files rows carry the NEWEST area=fork friction text whose
// context names the path, and rows without any entry stay reason-less.
func TestAuditCodegen_ForkedFilesCarriesReason(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".forge"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Two Tier-1 forks: one with friction history, one without.
	content := []byte("package app // fork\n")
	for _, rel := range []string{"pkg/app/wire_gen.go", "pkg/app/bootstrap.go"} {
		mustWriteScopeFile(t, filepath.Join(dir, rel), string(content))
	}
	csBody := `{
  "forge_version": "test",
  "files": {
    "pkg/app/wire_gen.go": {"hash": "` + checksums.Hash(content) + `", "tier": 1, "forked": true},
    "pkg/app/bootstrap.go": {"hash": "` + checksums.Hash(content) + `", "tier": 1, "forked": true}
  }
}`
	if err := os.WriteFile(filepath.Join(dir, ".forge", "checksums.json"), []byte(csBody), 0o644); err != nil {
		t.Fatal(err)
	}
	// Friction log: an older fork entry, a NEWER one for the same path
	// (must win), and an unrelated non-fork-area entry that must be
	// ignored even though its context matches.
	jsonl := `{"schema":1,"id":"fr-old","recorded_at":"2026-06-01T00:00:00Z","forge_version":"t","severity":"note","area":"fork","source":"generate --accept","context":["pkg/app/wire_gen.go"],"text":"old reason"}
{"schema":1,"id":"fr-new","recorded_at":"2026-06-05T00:00:00Z","forge_version":"t","severity":"note","area":"fork","source":"accept-fork","context":["pkg/app/wire_gen.go"],"text":"newest reason wins"}
{"schema":1,"id":"fr-oth","recorded_at":"2026-06-09T00:00:00Z","forge_version":"t","severity":"p1","area":"codegen","source":"agent","context":["pkg/app/wire_gen.go"],"text":"not a fork reason"}
`
	if err := os.WriteFile(filepath.Join(dir, ".forge", "friction.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}

	cat := auditCodegen(nil, dir)
	forked, ok := cat.Details["forked_files"].([]auditForkedFile)
	if !ok {
		t.Fatalf("forked_files detail missing or wrong shape: %#v", cat.Details["forked_files"])
	}
	if len(forked) != 2 {
		t.Fatalf("forked_files = %+v, want both Tier-1 forks", forked)
	}
	byPath := map[string]auditForkedFile{}
	for _, f := range forked {
		byPath[f.Path] = f
	}
	if got := byPath["pkg/app/wire_gen.go"].Reason; got != "newest reason wins" {
		t.Errorf("wire_gen.go reason = %q, want the newest area=fork entry text", got)
	}
	if got := byPath["pkg/app/bootstrap.go"].Reason; got != "" {
		t.Errorf("bootstrap.go reason = %q, want empty (no friction entry for it)", got)
	}
}
