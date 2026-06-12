package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/templates"
)

// TestMigrateTemplateTiers locks the Tier-1→Tier-2 checksum migration:
// entries for paths whose template has been reclassified scaffold-once
// flip to tier=2 with the fork flag cleared, and nothing else moves.
func TestMigrateTemplateTiers(t *testing.T) {
	tier2 := map[string]bool{
		"Dockerfile":                     true,
		"Taskfile.yml":                   true,
		".gitignore":                     true,
		".golangci.yml":                  true,
		"frontends/web/src/app/page.tsx": true,
	}

	cs := &generator.FileChecksums{Files: map[string]checksums.FileChecksumEntry{
		// Forked Tier-1 entry on a reclassified path: the canonical
		// cp-forge case (user --accept-ed a starter to escape the guard).
		"Dockerfile": {Hash: "aa", Tier: 1, Forked: true, Accepted: true},
		// Non-forked Tier-1 entry on a reclassified path.
		"Taskfile.yml": {Hash: "bb", Tier: 1},
		// Legacy unset tier (0) on a reclassified path — pre-tier
		// checksums are Tier-1-equivalent and must migrate too.
		".golangci.yml": {Hash: "cc", Forked: true},
		// Already tier=2 AND forked: a deliberate Tier-2 ownership
		// transfer. Must be left alone entirely.
		"frontends/web/src/app/page.tsx": {Hash: "dd", Tier: 2, Forked: true},
		// Tier-1 path NOT in the reclassified set: untouched.
		"cmd/server.go": {Hash: "ee", Tier: 1, Forked: true},
		// Untracked-path noise in the set ('.gitignore' has no entry):
		// must not invent an entry.
	}}

	migrated := migrateTemplateTiers(cs, tier2)

	wantMigrated := []string{".golangci.yml", "Dockerfile", "Taskfile.yml"}
	if len(migrated) != len(wantMigrated) {
		t.Fatalf("migrated %d entries, want %d: %+v", len(migrated), len(wantMigrated), migrated)
	}
	for i, want := range wantMigrated {
		if migrated[i].path != want {
			t.Errorf("migrated[%d].path = %q, want %q (sorted order)", i, migrated[i].path, want)
		}
	}
	if !migrated[1].wasForked || migrated[2].wasForked {
		t.Errorf("wasForked flags wrong: %+v", migrated)
	}

	for path, want := range map[string]checksums.FileChecksumEntry{
		"Dockerfile":                     {Hash: "aa", Tier: 2},
		"Taskfile.yml":                   {Hash: "bb", Tier: 2},
		".golangci.yml":                  {Hash: "cc", Tier: 2},
		"frontends/web/src/app/page.tsx": {Hash: "dd", Tier: 2, Forked: true},
		"cmd/server.go":                  {Hash: "ee", Tier: 1, Forked: true},
	} {
		got := cs.Files[path]
		if got.Tier != want.Tier || got.Forked != want.Forked || got.Accepted != want.Accepted || got.Hash != want.Hash {
			t.Errorf("%s: got {hash:%s tier:%d forked:%v accepted:%v}, want {hash:%s tier:%d forked:%v accepted:%v}",
				path, got.Hash, got.Tier, got.Forked, got.Accepted, want.Hash, want.Tier, want.Forked, want.Accepted)
		}
	}
	if _, invented := cs.Files[".gitignore"]; invented {
		t.Error("migration invented an entry for an untracked path")
	}

	// Idempotency: a second pass is a no-op.
	if again := migrateTemplateTiers(cs, tier2); len(again) != 0 {
		t.Errorf("second migration pass flipped %d entries, want 0: %+v", len(again), again)
	}
}

// TestMigrateTemplateTiersNilAndEmpty covers the degenerate inputs the
// pipeline step can hand over (no checksums yet / fresh project).
func TestMigrateTemplateTiersNilAndEmpty(t *testing.T) {
	if got := migrateTemplateTiers(nil, map[string]bool{"Dockerfile": true}); got != nil {
		t.Errorf("nil cs: got %+v, want nil", got)
	}
	cs := &generator.FileChecksums{}
	if got := migrateTemplateTiers(cs, map[string]bool{"Dockerfile": true}); got != nil {
		t.Errorf("empty cs: got %+v, want nil", got)
	}
}

// TestTier2ManagedPathsContents locks the registry-derived migration set:
// the starter-class files cp-forge forked must be present; files that
// `forge generate` re-renders every run (Tier-1) must NOT be.
func TestTier2ManagedPathsContents(t *testing.T) {
	set := generator.Tier2ManagedPaths()

	for _, want := range []string{
		"Dockerfile",
		"Taskfile.yml",
		"docker-compose.yml",
		".golangci.yml",
		".gitignore",
		"pkg/middleware/middleware.go",
		// One-shot .github scaffolds written only at `forge new` time.
		".github/CODEOWNERS",
		".github/pull_request_template.md",
	} {
		if !set[want] {
			t.Errorf("Tier2ManagedPaths() missing %q", want)
		}
	}

	for _, reject := range []string{
		// Regenerated every run by stepRegenerateInfra — honest Tier-1.
		"cmd/server.go",
		"cmd/main.go",
		"cmd/db.go",
		"cmd/version.go",
		"cmd/otel.go",
		"buf.yaml",
		"deploy/alloy-config.alloy",
		// Re-rendered by the generate-time CI step when enabled.
		".github/workflows/e2e.yml",
		".github/dependabot.yml",
		".github/workflows/ci.yml",
	} {
		if set[reject] {
			t.Errorf("Tier2ManagedPaths() must not contain %q (Tier-1 / generate-owned)", reject)
		}
	}
}

// TestEmitTier2OnceIfMissing_ForkedSurvivesForce: fork is stickier than
// --force for Tier-2 scaffolds too. A forked page.tsx (full user
// rewrite) must survive `forge generate --force` — --force means
// "discard current-run hand-edits on files forge owns", not "undo my
// recorded ownership transfer".
func TestEmitTier2OnceIfMissing_DisownedSurvivesForce(t *testing.T) {
	dir := t.TempDir()
	rel := "frontends/web/src/app/page.tsx"
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	userContent := "// my fully rewritten page\n"
	if err := os.WriteFile(full, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cs := &generator.FileChecksums{Files: map[string]checksums.FileChecksumEntry{
		rel: {Hash: "abc", Tier: 2, Disowned: true},
	}}

	if err := emitTier2OnceIfMissing(dir, rel, "nextjs/src/app/page.tsx.tmpl",
		templates.FrontendTemplateData{FrontendName: "web", ProjectName: "demo"}, cs, true); err != nil {
		t.Fatalf("emitTier2OnceIfMissing: %v", err)
	}

	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != userContent {
		t.Errorf("disowned Tier-2 file was clobbered by force:\n%s", got)
	}

	// Sanity: a NON-disowned existing file IS re-scaffolded under force
	// (the pre-existing documented semantics).
	delete(cs.Files, rel)
	if err := emitTier2OnceIfMissing(dir, rel, "nextjs/src/app/page.tsx.tmpl",
		templates.FrontendTemplateData{FrontendName: "web", ProjectName: "demo"}, cs, true); err != nil {
		t.Fatalf("emitTier2OnceIfMissing (non-disowned): %v", err)
	}
	got, _ = os.ReadFile(full)
	if string(got) == userContent {
		t.Error("non-disowned Tier-2 file should be re-scaffolded under --force")
	}
}

// TestMigrateLegacyForks pins the belt-and-braces pipeline migration:
// every legacy `forked: true` entry converts to disowned (tier=2 +
// marker), inheriting the fork timestamp as the disowned-since time;
// everything else is untouched.
func TestMigrateLegacyForks(t *testing.T) {
	cs := &generator.FileChecksums{Files: map[string]checksums.FileChecksumEntry{
		"pkg/app/bootstrap.go": {Hash: "a", Tier: 1, Forked: true, Accepted: true, ForkedAt: "2026-01-01T00:00:00Z"},
		"pkg/app/wire_gen.go":  {Hash: "b", Tier: 1, Forked: true}, // no timestamp recorded
		"pkg/app/app_gen.go":   {Hash: "c", Tier: 1},
		"internal/x/svc.go":    {Hash: "d", Tier: 2},
	}}

	converted := migrateLegacyForks(cs)
	want := []string{"pkg/app/bootstrap.go", "pkg/app/wire_gen.go"}
	if len(converted) != 2 || converted[0] != want[0] || converted[1] != want[1] {
		t.Fatalf("converted = %v, want %v (sorted)", converted, want)
	}

	e := cs.Files["pkg/app/bootstrap.go"]
	if e.Tier != 2 || !e.Disowned || e.Forked || e.Accepted || e.ForkedAt != "" {
		t.Errorf("bootstrap.go = %+v, want clean disowned shape", e)
	}
	if e.DisownedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("bootstrap.go DisownedAt = %q, want inherited ForkedAt", e.DisownedAt)
	}
	if e := cs.Files["pkg/app/wire_gen.go"]; !e.Disowned || e.DisownedAt != "" {
		t.Errorf("wire_gen.go = %+v, want disowned with empty (unknown) since", e)
	}
	if e := cs.Files["pkg/app/app_gen.go"]; e.Disowned || e.Tier != 1 {
		t.Errorf("app_gen.go touched by legacy-fork migration: %+v", e)
	}
	if e := cs.Files["internal/x/svc.go"]; e.Disowned || e.Tier != 2 {
		t.Errorf("ordinary starter touched by legacy-fork migration: %+v", e)
	}

	// Idempotent: a second pass converts nothing.
	if again := migrateLegacyForks(cs); len(again) != 0 {
		t.Errorf("second migrateLegacyForks pass converted %v, want nothing", again)
	}
}
