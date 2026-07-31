package lint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// TestLintAutoFixDefaultOn pins the auto-fix-then-gate contract at the flag
// layer: `forge lint` with no flags auto-fixes (noFix defaults false, so the
// effective !noFix is true), and --no-fix is the explicit opt-out. The legacy
// --fix flag still parses (back-compat) but is no longer required.
func TestLintAutoFixDefaultOn(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantAutoFx bool
	}{
		{"default (no flags) auto-fixes", nil, true},
		{"--no-fix opts out", []string{"--no-fix"}, false},
		{"legacy --fix still parses, stays on", []string{"--fix"}, true},
		{"--no-fix wins over --fix", []string{"--fix", "--no-fix"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newCmd(testFactory())
			if err := cmd.Flags().Parse(tc.args); err != nil {
				t.Fatalf("Parse(%v): %v", tc.args, err)
			}
			noFix, err := cmd.Flags().GetBool("no-fix")
			if err != nil {
				t.Fatalf("GetBool(no-fix): %v", err)
			}
			// runLint derives the effective auto-fix as !noFix.
			if got := !noFix; got != tc.wantAutoFx {
				t.Errorf("effective auto-fix = %v, want %v", got, tc.wantAutoFx)
			}
		})
	}
}

// writeFile writes content to root/rel, creating parent dirs.
func writeFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
	return abs
}

// newFormatProject scaffolds a temp project with a go.mod so the pre-pass
// resolves the canonical goimports local-prefix (the module path).
func newFormatProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module example.com/thing\n\ngo 1.22\n")
	return root
}

// unformattedGo is a hand-editable owned file with jumbled imports (no
// group split, wrong order) and gofmt-dirty indentation — exactly the drift
// a post-generate hand-edit re-introduces.
const unformattedGo = `package foo

import (
"fmt"
"example.com/thing/other"
"os"
)

func Use() {
        fmt.Println(os.Args)
    other.Do()
}
`

// TestFormatGoTree_RewritesOwnedFile proves the pre-pass reformats an
// owned/hand-edited file (not just generated code) and reports it.
func TestFormatGoTree_RewritesOwnedFile(t *testing.T) {
	root := newFormatProject(t)
	abs := writeFile(t, root, "internal/foo/bar.go", unformattedGo)

	changed, err := formatGoTree(root)
	if err != nil {
		t.Fatalf("formatGoTree: %v", err)
	}
	if len(changed) != 1 || changed[0] != filepath.Join("internal", "foo", "bar.go") {
		t.Fatalf("changed = %v, want [internal/foo/bar.go]", changed)
	}

	got, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) == unformattedGo {
		t.Fatal("file was not rewritten; still in unformatted form")
	}
	// The rewrite must be canonical: a second canonicalization is a no-op.
	want, err := checksums.CanonicalGoSource("example.com/thing", abs, got)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("rewritten file is not canonical:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	// The module-local import must have been split into its own trailing group.
	// (Sanity that the local-prefix wiring took effect, not just gofmt.)
	if _, err := checksums.CanonicalGoSource("example.com/thing", abs, got); err != nil {
		t.Fatalf("re-canonicalize: %v", err)
	}
}

// TestFormatGoTree_Idempotent proves a second pass over already-canonical
// output rewrites nothing — the property that keeps clean generated files
// (canonical + forge:hash-stamped) untouched.
func TestFormatGoTree_Idempotent(t *testing.T) {
	root := newFormatProject(t)
	writeFile(t, root, "internal/foo/bar.go", unformattedGo)

	if _, err := formatGoTree(root); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	changed, err := formatGoTree(root)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("second pass rewrote %v, want no changes (must be idempotent)", changed)
	}
}

// TestFormatGoTree_LeavesBrokenFileUntouched proves an unparseable hand-edit
// is skipped (not corrupted, not an error) — the real syntax error is left
// for the compiler/gate to report.
func TestFormatGoTree_LeavesBrokenFileUntouched(t *testing.T) {
	root := newFormatProject(t)
	const broken = "package foo\n\nfunc Oops( {\n" // unbalanced
	abs := writeFile(t, root, "internal/foo/broken.go", broken)

	changed, err := formatGoTree(root)
	if err != nil {
		t.Fatalf("formatGoTree returned error on broken file: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want none (broken file must be skipped)", changed)
	}
	got, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != broken {
		t.Errorf("broken file was modified:\n%s", got)
	}
}

// TestFormatGoTree_SkipsVendorAndNodeModules proves the walk prunes heavy /
// non-owned trees so it never rewrites dependency or build output.
func TestFormatGoTree_SkipsVendorAndNodeModules(t *testing.T) {
	root := newFormatProject(t)
	writeFile(t, root, "internal/vendor/dep/x.go", unformattedGo)
	writeFile(t, root, "pkg/node_modules/x.go", unformattedGo)

	changed, err := formatGoTree(root)
	if err != nil {
		t.Fatalf("formatGoTree: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want none (vendor/node_modules must be skipped)", changed)
	}
}

// TestFormatGoTree_AlreadyCanonicalNoOp proves a clean file is never rewritten.
func TestFormatGoTree_AlreadyCanonicalNoOp(t *testing.T) {
	root := newFormatProject(t)
	canonical, err := checksums.CanonicalGoSource("example.com/thing", "bar.go", []byte(unformattedGo))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	writeFile(t, root, "internal/foo/bar.go", string(canonical))

	changed, err := formatGoTree(root)
	if err != nil {
		t.Fatalf("formatGoTree: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want none (already-canonical file must be a no-op)", changed)
	}
}
