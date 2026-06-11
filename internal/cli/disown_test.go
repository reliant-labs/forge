// Tests for `forge disown <path>... --reason <text>` — see disown.go.
//
// The tests build a synthetic project root (forge.yaml + .forge/
// checksums.json + on-disk files), chdir into it, and drive runDisown
// directly. We avoid spawning the cobra binary so the assertions can
// read / re-decode the checksum file post-write without any process
// boundary.
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// seedDisownFile writes content at root/rel (creating parents).
func seedDisownFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDisown_RequiresReason: --reason is the design-feedback payload;
// the command refuses to run without it.
func TestDisown_RequiresReason(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/wire_gen.go": {Hash: "abc", Tier: 1},
		},
	}
	withUnforkProjectRoot(t, cs)

	for _, reason := range []string{"", "   "} {
		err := runDisown([]string{"pkg/app/wire_gen.go"}, reason, false)
		if err == nil {
			t.Fatalf("reason=%q: expected refusal without --reason; got nil", reason)
		}
		if !strings.Contains(err.Error(), "--reason is required") {
			t.Errorf("error should name the --reason requirement; got %q", err.Error())
		}
	}
}

// TestDisown_FlipsEntryAndRecordsFriction is the happy path: the entry
// flips to Tier-2 + disowned with the on-disk content recorded, and one
// friction entry (area=disown) lands per path with the reason text.
func TestDisown_FlipsEntryAndRecordsFriction(t *testing.T) {
	const rel = "pkg/app/wire_gen.go"
	userContent := "package app // user edit\n"
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			rel: {Hash: "old", History: []string{"old"}, Tier: 1},
		},
	}
	root := withUnforkProjectRoot(t, cs)
	seedDisownFile(t, root, rel, userContent)

	if err := runDisown([]string{rel}, "custom pool wiring forge can't express", false); err != nil {
		t.Fatalf("runDisown: %v", err)
	}

	got, err := checksums.Load(root)
	if err != nil {
		t.Fatalf("re-load checksums: %v", err)
	}
	entry := got.Files[rel]
	if entry.Tier != 2 || !entry.Disowned || entry.DisownedAt == "" {
		t.Errorf("entry = %+v, want {tier:2 disowned:true disowned_at:set}", entry)
	}
	if entry.Hash != checksums.Hash([]byte(userContent)) {
		t.Errorf("hash = %q, want the user's content hash at disown time", entry.Hash)
	}

	// Friction entry: area=disown, context names the path, text is the reason.
	reasons := disownFrictionReasons(root)
	if reasons[rel] != "custom pool wiring forge can't express" {
		t.Errorf("friction reason join = %q, want the --reason text", reasons[rel])
	}
}

// TestDisown_RejectsUnknownTier2AndMissing pins the validation set:
// untracked paths, already-user-owned Tier-2 scaffolds, and tracked
// paths missing from disk are all refused with distinct messages.
func TestDisown_RejectsUnknownTier2AndMissing(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/wire_gen.go":      {Hash: "a", Tier: 1}, // tracked but NOT on disk
			"internal/svc/service.go":  {Hash: "b", Tier: 2}, // ordinary starter
			"handlers/api/handlers.go": {Hash: "c", Tier: 1},
		},
	}
	root := withUnforkProjectRoot(t, cs)
	seedDisownFile(t, root, "internal/svc/service.go", "package svc\n")
	seedDisownFile(t, root, "handlers/api/handlers.go", "package api\n")

	cases := []struct {
		path    string
		wantErr string
	}{
		{"not/tracked.go", "not in .forge/checksums.json"},
		{"internal/svc/service.go", "already user-owned"},
		{"pkg/app/wire_gen.go", "missing on disk"},
	}
	for _, tc := range cases {
		err := runDisown([]string{tc.path}, "why", false)
		if err == nil {
			t.Errorf("%s: expected refusal; got nil", tc.path)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error %q missing %q", tc.path, err.Error(), tc.wantErr)
		}
	}
}

// TestDisown_DryRunPreservesState: targets are listed but nothing is
// written — neither checksums nor the friction log.
func TestDisown_DryRunPreservesState(t *testing.T) {
	const rel = "pkg/app/wire_gen.go"
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			rel: {Hash: "abc", Tier: 1},
		},
	}
	root := withUnforkProjectRoot(t, cs)
	seedDisownFile(t, root, rel, "package app\n")

	if err := runDisown([]string{rel}, "why", true); err != nil {
		t.Fatalf("runDisown --dry-run: %v", err)
	}
	got, _ := checksums.Load(root)
	if got.Files[rel].Disowned {
		t.Errorf("--dry-run mutated the entry on disk")
	}
	if reasons := disownFrictionReasons(root); len(reasons) != 0 {
		t.Errorf("--dry-run wrote friction entries: %v", reasons)
	}
}

// TestDisown_IdempotentOnAlreadyDisowned: re-disowning is a friendly
// no-op, not an error — safe for scripts.
func TestDisown_IdempotentOnAlreadyDisowned(t *testing.T) {
	const rel = "pkg/app/wire_gen.go"
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			rel: {Hash: "abc", Tier: 2, Disowned: true, DisownedAt: "2026-06-01T00:00:00Z"},
		},
	}
	root := withUnforkProjectRoot(t, cs)
	seedDisownFile(t, root, rel, "package app\n")

	if err := runDisown([]string{rel}, "why again", false); err != nil {
		t.Fatalf("runDisown on already-disowned entry should not error; got %v", err)
	}
	got, _ := checksums.Load(root)
	if got.Files[rel].DisownedAt != "2026-06-01T00:00:00Z" {
		t.Errorf("no-op disown must not re-stamp DisownedAt; got %+v", got.Files[rel])
	}
}

// TestDisown_LegacyForkedEntryIsDisownable: a legacy `forked: true`
// entry is Tier-1 by recording; disowning it is exactly the migration
// conversion and must clear the legacy flags.
func TestDisown_LegacyForkedEntryIsDisownable(t *testing.T) {
	const rel = "pkg/app/bootstrap.go"
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			rel: {Hash: "abc", Tier: 1, Forked: true, Accepted: true, ForkedAt: "2026-01-01T00:00:00Z"},
		},
	}
	root := withUnforkProjectRoot(t, cs)
	seedDisownFile(t, root, rel, "package app // legacy fork\n")

	if err := runDisown([]string{rel}, "settling a legacy fork", false); err != nil {
		t.Fatalf("runDisown: %v", err)
	}
	got, _ := checksums.Load(root)
	e := got.Files[rel]
	if e.Forked || e.Accepted || e.ForkedAt != "" || !e.Disowned || e.Tier != 2 {
		t.Errorf("entry = %+v, want clean disowned shape with legacy flags cleared", e)
	}
}

// TestDisown_CleansSideRenders: stale parked renders (legacy fork-era
// or --explain-drift leftovers) are removed at disown time.
func TestDisown_CleansSideRenders(t *testing.T) {
	const rel = "pkg/app/wire_gen.go"
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			rel: {Hash: "abc", Tier: 1},
		},
	}
	root := withUnforkProjectRoot(t, cs)
	seedDisownFile(t, root, rel, "package app\n")
	if err := checksums.WriteSideRenderNoBase(root, rel, []byte("stale render\n")); err != nil {
		t.Fatal(err)
	}

	if err := runDisown([]string{rel}, "why", false); err != nil {
		t.Fatalf("runDisown: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, checksums.RenderDir, rel)); !os.IsNotExist(err) {
		t.Errorf("side render not cleaned on disown")
	}
}

// TestDisownCmd_FlagsAndHelp pins the cobra surface: the command
// exists at top level, requires <path> args, exposes --reason and
// --dry-run, and the help text teaches the one-way contract and the
// delete + generate re-adoption path.
func TestDisownCmd_FlagsAndHelp(t *testing.T) {
	cmd := newDisownCmd()
	if cmd == nil {
		t.Fatal("newDisownCmd returned nil")
	}
	for _, flag := range []string{"reason", "dry-run"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("forge disown is missing --%s flag", flag)
		}
	}
	long := strings.ToLower(cmd.Long)
	for _, kw := range []string{"one-way", "forge generate", "friction", "extension point"} {
		if !strings.Contains(long, kw) {
			t.Errorf("forge disown --help long text should mention %q", kw)
		}
	}
}

// TestAcceptForkCmd_IsDeprecatedDisownAlias: the one-release alias
// forwards to the disown flow (so it requires --reason too) and is
// marked Deprecated on the cobra command.
func TestAcceptForkCmd_IsDeprecatedDisownAlias(t *testing.T) {
	cmd := newAcceptForkCmd()
	if cmd.Deprecated == "" {
		t.Errorf("accept-fork must carry a cobra Deprecated notice")
	}
	if !strings.Contains(strings.ToLower(cmd.Deprecated), "disown") {
		t.Errorf("accept-fork deprecation notice should point at forge disown; got %q", cmd.Deprecated)
	}

	const rel = "pkg/app/bootstrap.go"
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			rel: {Hash: "abc", Tier: 1, Forked: true},
		},
	}
	root := withUnforkProjectRoot(t, cs)
	seedDisownFile(t, root, rel, "package app\n")

	cmd.SetArgs([]string{rel, "--reason", "legacy alias path"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("accept-fork alias execute: %v", err)
	}
	got, _ := checksums.Load(root)
	e := got.Files[rel]
	if !e.Disowned || e.Tier != 2 || e.Forked {
		t.Errorf("accept-fork alias did not disown the path; entry = %+v", e)
	}
}
