// project_config_nested_test.go — the second caller of the whole-struct
// write, `forge scaffold frontend`, carries the same silent-damage bug as
// `forge project upgrade`.
//
// That command runs against a forge.yaml the user has been living in for the
// life of the project and changes three things: it appends a frontends entry,
// flips features.frontend on, and sets stack.frontend.framework. Three fields
// — and the write-back marshalled the whole struct, so the file came back
// stripped of every comment and every block NormalizeForWrite considers
// derivable.
package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// richManifest is a forge.yaml with the shapes a real one has: a comment
// header, comments inside blocks, a nested block the scaffold has to reach
// into (features:), and one it has to create (stack:).
const richManifest = `# My project. Please do not eat the comments.
name: app
module_path: example.com/app
forge_version: v0.0.1

# The database block is hand-tuned; the defaults were wrong for us.
database:
    driver: postgres
    migrations_dir: db/migrations

ci:
    provider: github
    lint:
        golangci: true   # we care about this one
        buf: false

features:
    ci: true
    codegen: true
    docs: true
    frontend: false
    observability: false

# A trailing comment with nothing after it. Also must survive.
`

// TestSetProjectConfigScalarPath_PreservesUserContent is the gate on the
// nested, in-place setter the scaffold-frontend lane needs: reaching into an
// existing block, and creating one that is absent, must both leave every
// other byte of the document alone.
func TestSetProjectConfigScalarPath_PreservesUserContent(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "forge.yaml")
	if err := os.WriteFile(p, []byte(richManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Reach INTO an existing block…
	if err := SetProjectConfigScalarPath(p, []string{"features", "frontend"}, true); err != nil {
		t.Fatalf("set features.frontend: %v", err)
	}
	// …and CREATE a nested one that does not exist yet.
	if err := SetProjectConfigScalarPath(p, []string{"stack", "frontend", "framework"}, "nextjs"); err != nil {
		t.Fatalf("set stack.frontend.framework: %v", err)
	}

	gotRaw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(gotRaw)

	// ── The three things that must be true of the new values. ──
	if !strings.Contains(got, "frontend: true") {
		t.Errorf("features.frontend was not flipped on:\n%s", got)
	}
	if strings.Contains(got, "frontend: false") {
		t.Errorf("the old features.frontend: false is still present:\n%s", got)
	}
	if !strings.Contains(got, "framework: nextjs") {
		t.Errorf("stack.frontend.framework was not created:\n%s", got)
	}

	// ── Everything the user wrote must still be there, verbatim. ──
	for _, survivor := range []string{
		"# My project. Please do not eat the comments.",
		"# The database block is hand-tuned; the defaults were wrong for us.",
		"golangci: true   # we care about this one",
		"# A trailing comment with nothing after it. Also must survive.",
		"\nci:\n",
		"    provider: github",
		"    migrations_dir: db/migrations",
		"    ci: true",
		"    codegen: true",
		"    docs: true",
		"    observability: false",
	} {
		if !strings.Contains(got, survivor) {
			t.Errorf("lost user content %q from the document:\n%s", survivor, got)
		}
	}

	// ── And the result must still load. ──
	cfg, err := ReadProjectConfig(p)
	if err != nil {
		t.Fatalf("result does not load: %v\n%s", err, got)
	}
	if cfg.Features.Frontend == nil || !*cfg.Features.Frontend {
		t.Errorf("features.frontend did not read back as true")
	}
	if cfg.Stack.Frontend.Framework != "nextjs" {
		t.Errorf("stack.frontend.framework = %q, want nextjs", cfg.Stack.Frontend.Framework)
	}
}

// TestScaffoldFrontendWriteBackKeepsComments is the end-to-end shape of the
// bug: the exact sequence `forge scaffold frontend` performs against a
// user-authored manifest.
func TestScaffoldFrontendWriteBackKeepsComments(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "forge.yaml")
	if err := os.WriteFile(p, []byte(richManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	before := richManifest

	// The three mutations, in place.
	if err := appendToProjectConfigSequence(p, "frontends", map[string]any{
		"name": "web",
		"type": "nextjs",
		"path": "frontends/web",
	}); err != nil {
		t.Fatalf("append frontend: %v", err)
	}
	if err := SetProjectConfigScalarPath(p, []string{"features", "frontend"}, true); err != nil {
		t.Fatalf("flip feature: %v", err)
	}
	if err := SetProjectConfigScalarPath(p, []string{"stack", "frontend", "framework"}, "nextjs"); err != nil {
		t.Fatalf("set framework: %v", err)
	}

	gotRaw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(gotRaw)

	// Comment count must not drop. This is the assertion the old
	// whole-struct write could never pass — it took 40 comment lines on
	// forge's own manifest to zero.
	countComments := func(s string) int {
		n := 0
		for _, l := range strings.Split(s, "\n") {
			if strings.Contains(l, "#") {
				n++
			}
		}
		return n
	}
	if b, a := countComments(before), countComments(got); a < b {
		t.Errorf("comment lines dropped from %d to %d:\n%s", b, a, got)
	}

	if !strings.Contains(got, "\nci:\n") {
		t.Errorf("the ci: block was dropped:\n%s", got)
	}
	if _, err := ReadProjectConfig(p); err != nil {
		t.Fatalf("result does not load: %v\n%s", err, got)
	}
}
