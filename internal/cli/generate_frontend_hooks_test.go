package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteHooksIndex_FlatModeNoCollisions asserts the historic shape is
// preserved when no two hook files re-export the same identifier: a flat
// `export * from "./..."` per file. This is the path nearly every
// single-service project takes; regressing it would break every existing
// `import { useGetUser } from "@/hooks"` site.
func TestWriteHooksIndex_FlatModeNoCollisions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.ts")

	files := []hookFileEntry{
		{fileName: "user-service-hooks.ts", nsAlias: "userService", symbols: []string{"useGetUser", "useListUsers"}},
		{fileName: "org-service-hooks.ts", nsAlias: "orgService", symbols: []string{"useGetOrg", "useListOrgs"}},
	}

	if err := writeHooksIndex(path, files); err != nil {
		t.Fatalf("writeHooksIndex: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read index.ts: %v", err)
	}
	s := string(got)

	if !strings.Contains(s, "Mode: flat wildcard re-exports") {
		t.Errorf("expected flat-mode comment, got:\n%s", s)
	}
	if !strings.Contains(s, `export * from "./user-service-hooks";`) {
		t.Errorf("expected flat wildcard for user-service-hooks, got:\n%s", s)
	}
	if !strings.Contains(s, `export * from "./org-service-hooks";`) {
		t.Errorf("expected flat wildcard for org-service-hooks, got:\n%s", s)
	}
	if strings.Contains(s, "export * as") {
		t.Errorf("did not expect namespace re-exports in flat mode, got:\n%s", s)
	}
}

// TestWriteHooksIndex_NamespacedModeOnCollision asserts that when two hook
// files export the same identifier (e.g. both have a generic `useList`
// because each service has a List RPC), the entire barrel switches to
// `export * as <alias>` form. This is the collision-aware fix that
// unblocks projects the moment they grow past one service with overlapping
// RPC names.
func TestWriteHooksIndex_NamespacedModeOnCollision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.ts")

	// Both services export `useList` — flat wildcard would generate two
	// `export * from "..."` lines re-exporting the same identifier,
	// which tsc rejects as a duplicate-export error.
	files := []hookFileEntry{
		{fileName: "user-service-hooks.ts", nsAlias: "userService", symbols: []string{"useGet", "useList"}},
		{fileName: "org-service-hooks.ts", nsAlias: "orgService", symbols: []string{"useGet", "useList"}},
	}

	if err := writeHooksIndex(path, files); err != nil {
		t.Fatalf("writeHooksIndex: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read index.ts: %v", err)
	}
	s := string(got)

	if !strings.Contains(s, "Mode: namespaced re-exports") {
		t.Errorf("expected namespaced-mode comment, got:\n%s", s)
	}
	// Both collisions should be documented in the comment block so the
	// user knows exactly which symbols forced the switch.
	if !strings.Contains(s, "useGet (from org-service-hooks.ts, user-service-hooks.ts)") {
		t.Errorf("expected useGet collision in comment, got:\n%s", s)
	}
	if !strings.Contains(s, "useList (from org-service-hooks.ts, user-service-hooks.ts)") {
		t.Errorf("expected useList collision in comment, got:\n%s", s)
	}
	if !strings.Contains(s, `export * as userService from "./user-service-hooks";`) {
		t.Errorf("expected namespaced re-export for userService, got:\n%s", s)
	}
	if !strings.Contains(s, `export * as orgService from "./org-service-hooks";`) {
		t.Errorf("expected namespaced re-export for orgService, got:\n%s", s)
	}
	// Confirm the flat wildcard form is NOT present in namespaced mode.
	if strings.Contains(s, `export * from "./user-service-hooks";`) {
		t.Errorf("did not expect flat wildcard in namespaced mode, got:\n%s", s)
	}
}

// TestDetectIndexCollisions_NoOverlap asserts the no-collision path
// returns nil, which is the signal writeHooksIndex uses to pick flat-mode
// emission.
func TestDetectIndexCollisions_NoOverlap(t *testing.T) {
	files := []hookFileEntry{
		{fileName: "a.ts", symbols: []string{"useGetA", "useListA"}},
		{fileName: "b.ts", symbols: []string{"useGetB", "useListB"}},
	}
	if got := detectIndexCollisions(files); len(got) != 0 {
		t.Errorf("expected zero collisions, got %+v", got)
	}
}

// TestDetectIndexCollisions_ListsAllOverlapsSorted asserts ALL colliding
// symbols are reported (not just the first) and the result is sorted so
// the comment block at the top of index.ts is byte-stable across runs.
func TestDetectIndexCollisions_ListsAllOverlapsSorted(t *testing.T) {
	files := []hookFileEntry{
		{fileName: "a.ts", symbols: []string{"useList", "useGet", "useUnique"}},
		{fileName: "b.ts", symbols: []string{"useGet", "useList"}},
	}
	got := detectIndexCollisions(files)
	if len(got) != 2 {
		t.Fatalf("expected 2 collisions, got %d: %+v", len(got), got)
	}
	if got[0].symbol != "useGet" || got[1].symbol != "useList" {
		t.Errorf("expected sorted symbols [useGet, useList], got [%s, %s]", got[0].symbol, got[1].symbol)
	}
	// File slices inside each collision must also be sorted.
	want := []string{"a.ts", "b.ts"}
	for _, c := range got {
		if len(c.files) != 2 || c.files[0] != want[0] || c.files[1] != want[1] {
			t.Errorf("expected files %v for %s, got %v", want, c.symbol, c.files)
		}
	}
}
