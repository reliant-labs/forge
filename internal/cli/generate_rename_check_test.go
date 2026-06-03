// Tests for the pre/post rename-detection passes.
//
// FRICTION 2026-06-02: cp-forge `forgedb.Migrations()` →
// `forgedb.MigrationsFS` rename left internal/db/migrations.go
// orphaned. These tests pin the fix: a stale `pkgName.Name` reference
// in a hand-written file produces a warning identifying the dropped
// symbol, the call site, and the package.
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// TestStepSnapshotTier1Exports_RecordsGoExports verifies the
// pre-codegen snapshot pass picks up the public identifiers of every
// tracked Tier-1 Go file and skips Tier-2 / non-Go / forked entries.
func TestStepSnapshotTier1Exports_RecordsGoExports(t *testing.T) {
	dir := t.TempDir()

	// db/embed.go — Tier-1 Go file with two public exports.
	writeUnderDir(t, dir,"db/embed.go", `package forgedb

var Migrations = "v1"

func Hello() {}
`)
	// hooks.ts — Tier-1 non-Go: skipped by the snapshotter.
	writeUnderDir(t, dir,"web/hooks.ts", `export const Foo = 1;`)
	// fork.go — Tier-1 but forked: skipped (forge no longer owns it).
	writeUnderDir(t, dir,"fork/fork.go", `package fork

var ForkedExport = "x"
`)
	// scaffolded.go — Tier-2: skipped (rename detection is Tier-1 only).
	writeUnderDir(t, dir,"scaffold/scaffolded.go", `package scaffold

var ScaffoldExport = "x"
`)

	cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{
		"db/embed.go":            {Hash: "h1", Tier: 1},
		"web/hooks.ts":           {Hash: "h2", Tier: 1},
		"fork/fork.go":           {Hash: "h3", Tier: 1, Forked: true},
		"scaffold/scaffolded.go": {Hash: "h4", Tier: 2},
	}}
	ctx := &pipelineContext{
		ProjectDir: dir,
		AbsPath:    dir,
		Checksums:  cs,
	}
	if err := stepSnapshotTier1Exports(ctx); err != nil {
		t.Fatalf("stepSnapshotTier1Exports: %v", err)
	}
	if got, ok := ctx.PriorExports["db/embed.go"]; !ok {
		t.Errorf("PriorExports missing db/embed.go entry")
	} else {
		if got.PkgName != "forgedb" {
			t.Errorf("db/embed.go pkg = %q, want forgedb", got.PkgName)
		}
		if !sliceContains(got.Names, "Migrations") || !sliceContains(got.Names, "Hello") {
			t.Errorf("db/embed.go exports = %v, want both Migrations + Hello", got.Names)
		}
	}
	if _, ok := ctx.PriorExports["web/hooks.ts"]; ok {
		t.Errorf("PriorExports should skip non-Go Tier-1 files")
	}
	if _, ok := ctx.PriorExports["fork/fork.go"]; ok {
		t.Errorf("PriorExports should skip forked Tier-1 files")
	}
	if _, ok := ctx.PriorExports["scaffold/scaffolded.go"]; ok {
		t.Errorf("PriorExports should skip Tier-2 files")
	}
}

// TestStepDetectRenamedExports_FlagsStaleCaller is the headline
// FRICTION reproduction: snapshot a `forgedb.Migrations` accessor,
// rewrite the file to use `MigrationsFS`, and verify the post-pass
// emits a warning naming the stale caller.
func TestStepDetectRenamedExports_FlagsStaleCaller(t *testing.T) {
	dir := t.TempDir()

	// 1. Hand-written caller — references the old name.
	writeUnderDir(t, dir,"internal/db/migrations.go", `package db

import "example.com/m/db"

func Apply() { _ = db.Migrations() }
`)

	// 2. Generated file (BEFORE the rename) — declares Migrations.
	writeUnderDir(t, dir,"db/embed.go", `package db

func Migrations() string { return "old" }
`)

	cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{
		"db/embed.go": {Hash: "h1", Tier: 1},
	}}
	ctx := &pipelineContext{
		ProjectDir: dir,
		AbsPath:    dir,
		Checksums:  cs,
	}
	if err := stepSnapshotTier1Exports(ctx); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// 3. Simulate codegen rewriting db/embed.go with the new symbol.
	writeUnderDir(t, dir,"db/embed.go", `package db

var MigrationsFS = "new"
`)

	// 4. Detection: capture stderr to verify the warning surfaces.
	stderr, restore := captureStderr(t)
	if err := stepDetectRenamedExports(ctx); err != nil {
		restore()
		t.Fatalf("detect: %v", err)
	}
	restore()
	out := stderr.String()
	if !strings.Contains(out, "Tier-1 rename detection") {
		t.Errorf("missing rename-detection banner; got:\n%s", out)
	}
	if !strings.Contains(out, "db/embed.go") {
		t.Errorf("warning should name the renamed file; got:\n%s", out)
	}
	if !strings.Contains(out, "Migrations") {
		t.Errorf("warning should name the dropped symbol; got:\n%s", out)
	}
	if !strings.Contains(out, "internal/db/migrations.go") {
		t.Errorf("warning should name the stale caller; got:\n%s", out)
	}
}

// TestStepDetectRenamedExports_NoFalsePositive verifies the prefix-
// guard: `pkg.Migrations` must NOT match against `pkg.MigrationsFS`.
// Without isIdentChar(), the textual scan would over-flag MigrationsFS
// when scanning for a deleted "Migrations".
func TestStepDetectRenamedExports_NoFalsePositive(t *testing.T) {
	dir := t.TempDir()

	// Caller only references the NEW name, not the dropped one.
	writeUnderDir(t, dir,"callers/x.go", `package callers

import "example.com/m/db"

func Use() { _ = db.MigrationsFS }
`)
	// Old: declared both.
	writeUnderDir(t, dir,"db/embed.go", `package db

var Migrations = "old"
var MigrationsFS = "x"
`)
	cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{
		"db/embed.go": {Tier: 1},
	}}
	ctx := &pipelineContext{ProjectDir: dir, AbsPath: dir, Checksums: cs}
	if err := stepSnapshotTier1Exports(ctx); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Drop Migrations from the new render — but the only call site
	// references the still-present MigrationsFS, NOT Migrations.
	writeUnderDir(t, dir,"db/embed.go", `package db

var MigrationsFS = "x"
`)
	stderr, restore := captureStderr(t)
	if err := stepDetectRenamedExports(ctx); err != nil {
		restore()
		t.Fatalf("detect: %v", err)
	}
	restore()
	out := stderr.String()
	if strings.Contains(out, "Tier-1 rename detection") {
		t.Errorf("got false-positive rename warning when only the new symbol was referenced:\n%s", out)
	}
}

// TestStepDetectRenamedExports_SkipsGenFiles verifies the scanner
// skips _gen.go files. Forge owns those files; their stale references
// will be cleaned up the moment the codegen rewrites them.
func TestStepDetectRenamedExports_SkipsGenFiles(t *testing.T) {
	dir := t.TempDir()
	// The stale reference is in a generated file — should be ignored.
	writeUnderDir(t, dir,"handlers/billing/handlers_crud_gen.go", `package billing

import "example.com/m/db"

func init() { _ = db.Migrations() }
`)
	writeUnderDir(t, dir,"db/embed.go", `package db

func Migrations() string { return "old" }
`)
	cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{
		"db/embed.go": {Tier: 1},
	}}
	ctx := &pipelineContext{ProjectDir: dir, AbsPath: dir, Checksums: cs}
	if err := stepSnapshotTier1Exports(ctx); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	writeUnderDir(t, dir,"db/embed.go", `package db

var MigrationsFS = "new"
`)
	stderr, restore := captureStderr(t)
	if err := stepDetectRenamedExports(ctx); err != nil {
		restore()
		t.Fatalf("detect: %v", err)
	}
	restore()
	if strings.Contains(stderr.String(), "Tier-1 rename detection") {
		t.Errorf("scanner should skip _gen.go files; got false-positive warning:\n%s", stderr.String())
	}
}

// writeUnderDir writes content under dir/rel, creating parent
// directories. Test helper — fails the test on I/O errors. Local to
// this file to avoid colliding with mustWrite in generate_cleanup_test.go
// (different signature).
func writeUnderDir(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

// sliceContains is the tiny string-set predicate used by the snapshot
// test. Local to this file to avoid colliding with the `contains`
// substring predicate in generate_test.go.
func sliceContains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// captureStderr is reused from lint_buf_test.go (same package). It
// redirects os.Stderr to a pipe and returns a Builder populated when
// the returned restore() is called.
