package pkgdirective

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseReasoned(t *testing.T) {
	const d = "forge:exclude-contract"
	tests := []struct {
		name       string
		raw        string
		wantReason string
		wantOK     bool
	}{
		{"reasoned", "//forge:exclude-contract: pure helper", "pure helper", true},
		{"spaced marker", "// forge:exclude-contract: pure helper", "pure helper", true},
		{"extra space after the colon", "//forge:exclude-contract:    spaced out   ", "spaced out", true},
		{"bare", "//forge:exclude-contract", "", true},
		{"bare, spaced", "//  forge:exclude-contract  ", "", true},
		{"empty reason after the colon is a bare marker", "//forge:exclude-contract:", "", true},
		{"block comment", "/* forge:exclude-contract: block form */", "block form", true},
		{"prose that merely mentions it", "// see forge:exclude-contract for details", "", false},
		{"a different separator", "//forge:exclude-contract - because", "", false},
		{"a longer directive that shares the prefix", "//forge:exclude-contract-extra: x", "", false},
		{"another directive", "//forge:not-a-component: x", "", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, ok := ParseReasoned(tt.raw, d)
			if reason != tt.wantReason || ok != tt.wantOK {
				t.Errorf("ParseReasoned(%q) = (%q, %v), want (%q, %v)", tt.raw, reason, ok, tt.wantReason, tt.wantOK)
			}
		})
	}
}

func TestTrimCommentMarkers(t *testing.T) {
	tests := map[string]string{
		"// text":         "text",
		"//text":          "text",
		"//   padded   ":  "padded",
		"/* block */":     "block",
		"/*tight*/":       "tight",
		"/*\n multi\n*/":  "multi",
		"  bare text  ":   "bare text",
		"":                "",
		"/* unterminated": "unterminated",
	}
	for in, want := range tests {
		if got := TrimCommentMarkers(in); got != want {
			t.Errorf("TrimCommentMarkers(%q) = %q, want %q", in, got, want)
		}
	}
}

func writePkg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestFindAll_ReadsOnlyNonTestNonGeneratedSources(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"b.go":      "// Package p.\n//\n//forge:exclude-contract: second file\npackage p\n",
		"a.go":      "//forge:exclude-contract: first file\npackage p\n",
		"a_test.go": "//forge:exclude-contract: a test file never counts\npackage p\n",
		"x_gen.go":  "//forge:exclude-contract: generated never counts\npackage p\n",
		"notes.txt": "//forge:exclude-contract: not go\n",
		"broken.go": "package p\nfunc (",
		"c.go":      "package p\n\n// forge:exclude-contract: see below\n// unrelated prose mentioning forge:exclude-contract\nvar x = 1\n",
	})
	if err := os.Mkdir(filepath.Join(dir, "sub.go"), 0o755); err != nil { // a directory named like a source
		t.Fatal(err)
	}

	got := FindAll(dir, "forge:exclude-contract")
	var seen []string
	for _, m := range got {
		seen = append(seen, filepath.Base(m.File)+":"+m.Reason)
		if !filepath.IsAbs(m.File) && !filepath.IsAbs(dir) {
			continue
		}
		if m.Line < 1 {
			t.Errorf("%s: line %d, want 1-indexed", m.File, m.Line)
		}
	}
	want := []string{"a.go:first file", "b.go:second file", "c.go:see below"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("FindAll = %v, want %v (name order; tests, generated files, broken files and prose skipped)", seen, want)
	}
	if got[1].Line != 3 {
		t.Errorf("b.go marker line = %d, want 3", got[1].Line)
	}
}

func TestFind_ReasonedWinsOverBare(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"a.go": "//forge:exclude-contract\npackage p\n",
		"b.go": "//forge:exclude-contract: the real reason\npackage p\n",
	})
	m, ok := Find(dir, "forge:exclude-contract")
	if !ok || m.Reason != "the real reason" || filepath.Base(m.File) != "b.go" {
		t.Errorf("Find = (%+v, %v), want the reasoned marker even though a bare one sorts first", m, ok)
	}

	bareOnly := writePkg(t, map[string]string{"z.go": "//forge:exclude-contract\npackage p\n", "y.go": "//forge:exclude-contract\npackage p\n"})
	m, ok = Find(bareOnly, "forge:exclude-contract")
	if !ok || m.Reason != "" || filepath.Base(m.File) != "y.go" {
		t.Errorf("Find among bare markers = (%+v, %v), want the first by name", m, ok)
	}

	if m, ok := Find(writePkg(t, map[string]string{"a.go": "package p\n"}), "forge:exclude-contract"); ok || m != (Marker{}) {
		t.Errorf("a package with no marker = (%+v, %v)", m, ok)
	}
	if m, ok := Find(filepath.Join(dir, "no-such-dir"), "forge:exclude-contract"); ok || m != (Marker{}) {
		t.Errorf("a missing directory = (%+v, %v)", m, ok)
	}
}

func TestForEachPackageComment_VisitsEveryCommentInOrder(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"b.go": "package p\n// second\n",
		"a.go": "// first\npackage p\n\n/* block */\nvar x = 1 // trailing\n",
	})
	type visit struct {
		file string
		line int
		raw  string
	}
	var got []visit
	ForEachPackageComment(dir, func(path string, line int, raw string) {
		got = append(got, visit{filepath.Base(path), line, raw})
	})
	want := []visit{
		{"a.go", 1, "// first"},
		{"a.go", 4, "/* block */"},
		{"a.go", 5, "// trailing"},
		{"b.go", 2, "// second"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("visits = %+v, want %+v", got, want)
	}
	ForEachPackageComment(filepath.Join(dir, "absent"), func(string, int, string) { t.Error("visited a comment of a missing directory") })
}
