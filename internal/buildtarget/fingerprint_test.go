package buildtarget

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// writeTree creates files (slash paths relative to root) with the given
// contents.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fixture lays out a build cwd ("app") beside a sibling checkout ("lib"),
// the shape a cross-repo ShellBuild has.
func fixture(t *testing.T) (cwd string) {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app/go.mod":                    "module app",
		"app/main.go":                   "package main",
		"app/cmd/tool/main.go":          "package main",
		"app/docs/readme.md":            "docs",
		"app/.git/HEAD":                 "ref: refs/heads/main",
		"app/web/node_modules/x/x.js":   "x",
		"app/web/package.json":          "{}",
		"app/bin/app":                   "binary",
		"lib/go.mod":                    "module lib",
		"lib/pkg/a.go":                  "package pkg",
		"lib/pkg/a_test.go":             "package pkg",
		"lib/.git/config":               "[core]",
		"lib/node_modules/dep/index.js": "dep",
	})
	return filepath.Join(root, "app")
}

func TestExpandInputs_Patterns(t *testing.T) {
	cwd := fixture(t)
	cases := []struct {
		name     string
		patterns []string
		want     []string
	}{
		{"literal file", []string{"go.mod"}, []string{"go.mod"}},
		{"literal dir selects everything beneath it", []string{"cmd"}, []string{"cmd/tool/main.go"}},
		{"single-segment star", []string{"*.go"}, []string{"main.go"}},
		{"doublestar prunes .git and node_modules", []string{"**"}, []string{
			"bin/app", "cmd/tool/main.go", "docs/readme.md", "go.mod", "main.go", "web/package.json",
		}},
		{"doublestar with suffix", []string{"**/*.go"}, []string{"cmd/tool/main.go", "main.go"}},
		{"sibling checkout via ../", []string{"../lib/**/*.go"}, []string{"../lib/pkg/a.go", "../lib/pkg/a_test.go"}},
		{"exclude applies regardless of order", []string{"!**/*_test.go", "../lib/**"}, []string{"../lib/go.mod", "../lib/pkg/a.go"}},
		{"exclude a directory", []string{"**", "!bin", "!docs/**"}, []string{
			"cmd/tool/main.go", "go.mod", "main.go", "web/package.json",
		}},
		{"a literal reaches into a pruned dir", []string{"web/node_modules/x"}, []string{"web/node_modules/x/x.js"}},
		{"dot is the whole cwd", []string{"."}, []string{
			"bin/app", "cmd/tool/main.go", "docs/readme.md", "go.mod", "main.go", "web/package.json",
		}},
		{"overlapping patterns select once", []string{"*.go", "main.go", "./main.go"}, []string{"main.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandInputs(cwd, tc.patterns)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ExpandInputs(%q)\n got  %q\n want %q", tc.patterns, got, tc.want)
			}
		})
	}
}

func TestExpandInputs_Refusals(t *testing.T) {
	cwd := fixture(t)
	cases := []struct {
		name     string
		patterns []string
		want     string
	}{
		{"a pattern matching nothing names itself", []string{"go.mod", "missing/**"}, `"missing/**" matches no file`},
		{"absolute", []string{"/etc/passwd"}, "is absolute"},
		{"only exclusions", []string{"!bin"}, "only exclusions"},
		{"empty", []string{" "}, "is empty"},
		{"partial doublestar", []string{"src**"}, "whole path segment"},
		{"bad class", []string{"[a-"}, "syntax error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExpandInputs(cwd, tc.patterns)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ExpandInputs(%q) error = %v, want one containing %q", tc.patterns, err, tc.want)
			}
		})
	}
}

func mustFingerprint(t *testing.T, cwd string, patterns []string, recipe string) Fingerprint {
	t.Helper()
	fp, err := FingerprintInputs(cwd, patterns, []byte(recipe))
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestFingerprintInputs_StableAndSensitive(t *testing.T) {
	cwd := fixture(t)
	patterns := []string{"**/*.go", "go.mod", "../lib/**"}
	base := mustFingerprint(t, cwd, patterns, "recipe")

	if again := mustFingerprint(t, cwd, patterns, "recipe"); again.Digest != base.Digest {
		t.Fatalf("same inputs, different fingerprints: %s vs %s", base.Digest, again.Digest)
	}
	reordered := []string{"../lib/**", "go.mod", "**/*.go"}
	if got := mustFingerprint(t, cwd, reordered, "recipe"); got.Digest != base.Digest {
		t.Fatal("pattern order changed the fingerprint; selection is order-independent")
	}
	if base.Files != 6 || !strings.HasPrefix(base.Digest, "sha256:") {
		t.Fatalf("fingerprint = %+v, want 6 files and a sha256 digest", base)
	}

	// Touching a file without changing its bytes is not a change: mtimes are
	// rewritten by every checkout.
	past := time.Unix(1_000_000_000, 0)
	if err := os.Chtimes(filepath.Join(cwd, "main.go"), past, past); err != nil {
		t.Fatal(err)
	}
	if got := mustFingerprint(t, cwd, patterns, "recipe"); got.Digest != base.Digest {
		t.Fatal("an mtime-only touch changed the fingerprint")
	}

	if got := mustFingerprint(t, cwd, patterns, "other recipe"); got.Digest == base.Digest {
		t.Fatal("a different recipe (command, env, platform) left the fingerprint unchanged")
	}

	// A file outside every pattern is not an input.
	writeTree(t, cwd, map[string]string{"docs/readme.md": "edited"})
	if got := mustFingerprint(t, cwd, patterns, "recipe"); got.Digest != base.Digest {
		t.Fatal("editing an undeclared file changed the fingerprint")
	}

	writeTree(t, cwd, map[string]string{"../lib/pkg/a.go": "package pkg // edited"})
	edited := mustFingerprint(t, cwd, patterns, "recipe")
	if edited.Digest == base.Digest {
		t.Fatal("editing a sibling-checkout input left the fingerprint unchanged")
	}
	if got := edited.Changed(base); !reflect.DeepEqual(got, []string{"../lib/pkg/a.go"}) {
		t.Fatalf("Changed = %q, want the edited file", got)
	}

	writeTree(t, cwd, map[string]string{"cmd/new.go": "package cmd"})
	added := mustFingerprint(t, cwd, patterns, "recipe")
	if added.Digest == edited.Digest {
		t.Fatal("adding a matching file left the fingerprint unchanged")
	}
	if got := added.Changed(edited); !reflect.DeepEqual(got, []string{"cmd/new.go"}) {
		t.Fatalf("Changed = %q, want the added file", got)
	}

	if err := os.Chmod(filepath.Join(cwd, "main.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := mustFingerprint(t, cwd, patterns, "recipe"); got.Digest == added.Digest {
		t.Fatal("making an input executable left the fingerprint unchanged")
	}
}
