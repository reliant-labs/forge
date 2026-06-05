// Tests for the ownership Inspector.
//
// Each table covers one query method against a synthetic manifest +
// (when needed) a synthetic on-disk tree. Keep the cases small and
// focused — the inspector is the single source of truth for ownership
// queries, so a regression here cascades into every downstream check.
package checksums

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestInspector_TierAndForkedClassification(t *testing.T) {
	cs := &FileChecksums{Files: map[string]FileChecksumEntry{
		"pkg/app/bootstrap.go":     {Tier: 1},
		"pkg/app/app_extras.go":    {Tier: 2},
		"db/embed.go":              {Tier: 0}, // legacy → Tier-1
		"internal/svc/contract.go": {Tier: 1, Forked: true},
		"web/hooks.ts":             {Tier: 1},
	}}
	insp := NewInspector("", cs)

	cases := []struct {
		path       string
		wantTier   int
		wantTier1  bool
		wantTier2  bool
		wantForked bool
		wantGo     bool
	}{
		{"pkg/app/bootstrap.go", 1, true, false, false, true},
		{"pkg/app/app_extras.go", 2, false, true, false, true},
		{"db/embed.go", 0, true, false, false, true}, // legacy promotion
		{"internal/svc/contract.go", 1, true, false, true, true},
		{"web/hooks.ts", 1, true, false, false, false},
		{"never/seen.go", 0, false, false, false, true}, // untracked
	}
	for _, tc := range cases {
		if got := insp.Tier(tc.path); got != tc.wantTier {
			t.Errorf("Tier(%q) = %d, want %d", tc.path, got, tc.wantTier)
		}
		if got := insp.IsTier1(tc.path); got != tc.wantTier1 {
			t.Errorf("IsTier1(%q) = %v, want %v", tc.path, got, tc.wantTier1)
		}
		if got := insp.IsTier2(tc.path); got != tc.wantTier2 {
			t.Errorf("IsTier2(%q) = %v, want %v", tc.path, got, tc.wantTier2)
		}
		if got := insp.IsForked(tc.path); got != tc.wantForked {
			t.Errorf("IsForked(%q) = %v, want %v", tc.path, got, tc.wantForked)
		}
		if got := insp.IsGo(tc.path); got != tc.wantGo {
			t.Errorf("IsGo(%q) = %v, want %v", tc.path, got, tc.wantGo)
		}
	}
}

func TestInspector_NilManifestSafe(t *testing.T) {
	// Construction with a nil manifest must produce a non-nil Inspector
	// whose queries return safe zero values. This is the "fresh project
	// with no .forge/checksums.json yet" code path — callers should not
	// have to nil-check before each query.
	insp := NewInspector("", nil)
	if insp.IsTracked("anything.go") {
		t.Errorf("IsTracked on nil manifest should be false")
	}
	if insp.IsForked("anything.go") {
		t.Errorf("IsForked on nil manifest should be false")
	}
	if insp.Tier("anything.go") != 0 {
		t.Errorf("Tier on nil manifest should be 0")
	}
	if got := insp.Tier1GoFiles(); got != nil {
		t.Errorf("Tier1GoFiles on nil manifest = %v, want nil", got)
	}
	if got := insp.ForkedGoFilesByDir(); len(got) != 0 {
		t.Errorf("ForkedGoFilesByDir on nil manifest should be empty, got %v", got)
	}
}

func TestInspector_ForkedGoFilesByDir(t *testing.T) {
	cs := &FileChecksums{Files: map[string]FileChecksumEntry{
		"pkg/app/bootstrap.go": {Tier: 1, Forked: true},
		"pkg/app/wire_gen.go":  {Tier: 1, Forked: true},
		"pkg/app/app_gen.go":   {Tier: 1}, // not forked
		"internal/x/x.go":      {Tier: 1, Forked: true},
		// Non-Go forked entries must NOT appear in the Go grouping —
		// a YAML or TSX file cannot satisfy a Go package-local
		// reference, so the dangling-ref check ignores them.
		".github/workflows/ci.yml": {Tier: 1, Forked: true},
		"web/hooks.ts":             {Tier: 1, Forked: true},
		"pkg/app/scaffold.go":      {Tier: 2, Forked: true}, // Tier-2 forked: still grouped as forked Go (forked-flag wins)
		"untracked/sibling.go":     {Tier: 0},               // legacy entry, not forked
		"unused/never-forked.go":   {Tier: 1},               // not forked
	}}
	insp := NewInspector("", cs)
	got := insp.ForkedGoFilesByDir()
	want := map[string][]string{
		"pkg/app":    {"pkg/app/bootstrap.go", "pkg/app/scaffold.go", "pkg/app/wire_gen.go"},
		"internal/x": {"internal/x/x.go"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ForkedGoFilesByDir mismatch.\ngot:  %v\nwant: %v", got, want)
	}

	// Second call should return the cached map.
	if got2 := insp.ForkedGoFilesByDir(); !reflect.DeepEqual(got, got2) {
		t.Errorf("second call returned a different map; got %v", got2)
	}
}

func TestInspector_Tier1GoFiles(t *testing.T) {
	cs := &FileChecksums{Files: map[string]FileChecksumEntry{
		"db/embed.go":          {Tier: 0}, // legacy → Tier-1
		"pkg/app/bootstrap.go": {Tier: 1},
		"pkg/app/forked.go":    {Tier: 1, Forked: true}, // skip forked
		"scaffold/svc.go":      {Tier: 2},               // skip Tier-2
		"web/hooks.ts":         {Tier: 1},               // skip non-Go
	}}
	insp := NewInspector("", cs)
	got := insp.Tier1GoFiles()
	want := []string{"db/embed.go", "pkg/app/bootstrap.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tier1GoFiles = %v, want %v", got, want)
	}
}

func TestInspector_GoSiblingsIn(t *testing.T) {
	dir := t.TempDir()
	// Materialize a package directory containing a mix of files.
	mustWrite(t, dir, "pkg/app/a.go", "package app\n")
	mustWrite(t, dir, "pkg/app/b.go", "package app\n")
	mustWrite(t, dir, "pkg/app/a_test.go", "package app\n") // excluded
	mustWrite(t, dir, "pkg/app/README.md", "doc\n")         // excluded
	mustWrite(t, dir, "pkg/app/sub/c.go", "package sub\n")  // excluded (subdir)

	insp := NewInspector(dir, &FileChecksums{Files: map[string]FileChecksumEntry{}})
	got, err := insp.GoSiblingsIn("pkg/app")
	if err != nil {
		t.Fatalf("GoSiblingsIn: %v", err)
	}
	want := []string{"pkg/app/a.go", "pkg/app/b.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GoSiblingsIn = %v, want %v", got, want)
	}

	// Cached second read returns the same slice.
	got2, err := insp.GoSiblingsIn("pkg/app")
	if err != nil {
		t.Fatalf("second GoSiblingsIn: %v", err)
	}
	if !reflect.DeepEqual(got, got2) {
		t.Errorf("cached read mismatch: %v vs %v", got, got2)
	}

	// Missing directory returns an error.
	if _, err := insp.GoSiblingsIn("nope"); err == nil {
		t.Errorf("expected error for missing directory")
	}
}

func TestInspector_DeclaredTypesIn(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "pkg/app/types.go", `package app

type Workers struct{}
type Container struct{}
type internalAlias = int
`)
	mustWrite(t, dir, "pkg/app/broken.go", `package app

this is not valid go
`)
	mustWrite(t, dir, "pkg/app/funcs.go", `package app

// Only func declarations — no type decls.
func Run() {}
`)

	insp := NewInspector(dir, &FileChecksums{Files: map[string]FileChecksumEntry{}})

	got := insp.DeclaredTypesIn("pkg/app/types.go")
	want := map[string]bool{"Workers": true, "Container": true, "internalAlias": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DeclaredTypesIn(types.go) = %v, want %v", got, want)
	}

	// Unparseable file → empty non-nil set.
	got = insp.DeclaredTypesIn("pkg/app/broken.go")
	if got == nil {
		t.Errorf("DeclaredTypesIn on broken file should return non-nil empty set")
	}
	if len(got) != 0 {
		t.Errorf("DeclaredTypesIn(broken.go) = %v, want empty", got)
	}

	// File with no type decls → empty non-nil set.
	got = insp.DeclaredTypesIn("pkg/app/funcs.go")
	if len(got) != 0 {
		t.Errorf("DeclaredTypesIn(funcs.go) = %v, want empty", got)
	}

	// Missing file → empty non-nil set, no error path (callers don't
	// need to distinguish "missing" from "no types declared" for the
	// ownership-resolution scan).
	got = insp.DeclaredTypesIn("pkg/app/nope.go")
	if got == nil || len(got) != 0 {
		t.Errorf("DeclaredTypesIn on missing file = %v, want non-nil empty set", got)
	}
}

func TestInspector_DeclaredTypesIn_CachesResult(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "x.go", `package x
type Foo struct{}
`)
	insp := NewInspector(dir, &FileChecksums{Files: map[string]FileChecksumEntry{}})
	first := insp.DeclaredTypesIn("x.go")

	// Overwrite the file with new content; the inspector must return
	// the cached parse so callers that batch queries inside one
	// pipeline run see a consistent view.
	mustWrite(t, dir, "x.go", `package x
type Bar struct{}
`)
	second := insp.DeclaredTypesIn("x.go")
	if !reflect.DeepEqual(first, second) {
		t.Errorf("expected cached result; first=%v second=%v", first, second)
	}
}

// mustWrite writes content under dir/rel, creating parent directories.
// Inlined here to keep the inspector tests self-contained — sibling
// test files in this package use their own helpers with different
// signatures.
func mustWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

// Avoid unused-import in case sort is dropped elsewhere; keep an
// assertion that sort.Strings was the choice for grouping.
var _ = sort.Strings
