package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFile writes content at dir/rel, creating parent dirs.
func writeFileAt(t *testing.T, dir, rel, content string) string {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return full
}

func TestMissingGoWorkModule(t *testing.T) {
	t.Run("no go.work returns empty", func(t *testing.T) {
		dir := t.TempDir()
		if got := missingGoWorkModule(dir); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})

	t.Run("use'd module without go.mod is reported", func(t *testing.T) {
		dir := t.TempDir()
		writeFileAt(t, dir, "go.work", "go 1.26.2\n\nuse (\n\t.\n\tgen\n)\n")
		// main module go.mod present; gen/ has none.
		writeFileAt(t, dir, "go.mod", "module example.com/x\n\ngo 1.26.2\n")
		if got := missingGoWorkModule(dir); got != "gen" {
			t.Fatalf("want \"gen\", got %q", got)
		}
	})

	t.Run("all use'd modules present returns empty", func(t *testing.T) {
		dir := t.TempDir()
		writeFileAt(t, dir, "go.work", "go 1.26.2\n\nuse (\n\t.\n\tgen\n)\n")
		writeFileAt(t, dir, "go.mod", "module example.com/x\n\ngo 1.26.2\n")
		writeFileAt(t, dir, "gen/go.mod", "module example.com/x/gen\n\ngo 1.26.2\n")
		if got := missingGoWorkModule(dir); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})

	t.Run("single-line use directive", func(t *testing.T) {
		dir := t.TempDir()
		writeFileAt(t, dir, "go.work", "go 1.26.2\n\nuse ./gen\n")
		if got := missingGoWorkModule(dir); got != "gen" {
			t.Fatalf("want \"gen\", got %q", got)
		}
	})
}

func TestGeneratedCodeNeedsRefresh(t *testing.T) {
	t.Run("missing module trumps staleness", func(t *testing.T) {
		dir := t.TempDir()
		writeFileAt(t, dir, "go.work", "go 1.26.2\n\nuse (\n\t.\n\tgen\n)\n")
		writeFileAt(t, dir, "go.mod", "module example.com/x\n\ngo 1.26.2\n")
		reason, needs := generatedCodeNeedsRefresh(dir)
		if !needs {
			t.Fatal("want needs=true for missing gen module")
		}
		if reason == "" {
			t.Fatal("want a non-empty reason")
		}
	})

	t.Run("fresh tree needs nothing", func(t *testing.T) {
		dir := t.TempDir()
		writeFileAt(t, dir, "go.work", "go 1.26.2\n\nuse (\n\t.\n\tgen\n)\n")
		writeFileAt(t, dir, "go.mod", "module example.com/x\n\ngo 1.26.2\n")
		writeFileAt(t, dir, "gen/go.mod", "module example.com/x/gen\n\ngo 1.26.2\n")
		// proto older than gen: write proto, then bump gen mtime forward.
		proto := writeFileAt(t, dir, "proto/v1/a.proto", "syntax = \"proto3\";\n")
		gen := filepath.Join(dir, "gen/go.mod")
		past := time.Now().Add(-time.Hour)
		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(proto, past, past); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(gen, future, future); err != nil {
			t.Fatal(err)
		}
		if _, needs := generatedCodeNeedsRefresh(dir); needs {
			t.Fatal("want needs=false for fresh tree")
		}
	})

	t.Run("proto newer than gen is stale", func(t *testing.T) {
		dir := t.TempDir()
		writeFileAt(t, dir, "go.work", "go 1.26.2\n\nuse (\n\t.\n\tgen\n)\n")
		writeFileAt(t, dir, "go.mod", "module example.com/x\n\ngo 1.26.2\n")
		gen := writeFileAt(t, dir, "gen/go.mod", "module example.com/x/gen\n\ngo 1.26.2\n")
		proto := writeFileAt(t, dir, "proto/v1/a.proto", "syntax = \"proto3\";\n")
		past := time.Now().Add(-time.Hour)
		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(gen, past, past); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(proto, future, future); err != nil {
			t.Fatal(err)
		}
		reason, needs := generatedCodeNeedsRefresh(dir)
		if !needs {
			t.Fatal("want needs=true when proto newer than gen")
		}
		if reason == "" {
			t.Fatal("want a non-empty reason")
		}
	})

	t.Run("no codegen surface is a no-op", func(t *testing.T) {
		dir := t.TempDir()
		writeFileAt(t, dir, "go.mod", "module example.com/x\n\ngo 1.26.2\n")
		if _, needs := generatedCodeNeedsRefresh(dir); needs {
			t.Fatal("want needs=false when project has no go.work/proto")
		}
	})
}
