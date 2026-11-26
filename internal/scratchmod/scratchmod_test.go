package scratchmod

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

// The scratch module's build list is forge's: every requirement forge
// declares, at forge's version, plus forge itself replaced to the checkout,
// and forge's go.sum beside it. That is the whole offline guarantee, so it is
// pinned here without running the go command.
func TestWriteCopiesForgesBuildList(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := Write(root, dir, "example.com/scratch"); err != nil {
		t.Fatal(err)
	}
	forge := parse(t, filepath.Join(root, "go.mod"))
	got := parse(t, filepath.Join(dir, "go.mod"))

	if got.Module.Mod.Path != "example.com/scratch" || got.Go.Version != forge.Go.Version {
		t.Errorf("module %s go %s, want example.com/scratch go %s", got.Module.Mod.Path, got.Go.Version, forge.Go.Version)
	}
	required := map[string]string{}
	for _, r := range got.Require {
		required[r.Mod.Path] = r.Mod.Version
	}
	if _, ok := required[forge.Module.Mod.Path]; !ok {
		t.Errorf("the scratch module does not require %s", forge.Module.Mod.Path)
	}
	for _, r := range forge.Require {
		if v := required[r.Mod.Path]; v != r.Mod.Version {
			t.Errorf("requires %s at %q, want forge's %s", r.Mod.Path, v, r.Mod.Version)
		}
	}
	if len(got.Require) != len(forge.Require)+1 {
		t.Errorf("%d requirements, want forge's %d plus forge", len(got.Require), len(forge.Require))
	}
	replaced := false
	for _, r := range got.Replace {
		if r.Old.Path == forge.Module.Mod.Path {
			replaced = r.New.Path == root
		}
	}
	if !replaced {
		t.Errorf("forge is not replaced to the checkout %s: %+v", root, got.Replace)
	}

	want, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if sum, err := os.ReadFile(filepath.Join(dir, "go.sum")); err != nil || string(sum) != string(want) {
		t.Errorf("go.sum is not forge's (err %v)", err)
	}
}

func parse(t *testing.T, path string) *modfile.File {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// writeForge lays out a fake forge checkout and returns its root.
func writeForge(t *testing.T, goMod string, goSum *string) string {
	t.Helper()
	root := t.TempDir()
	if goMod != "" {
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(goMod), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if goSum != nil {
		if err := os.WriteFile(filepath.Join(root, "go.sum"), []byte(*goSum), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// forge's relative directory replaces are made absolute against the checkout,
// because the scratch module lives somewhere else; remote replaces and
// versioned ones are copied as they are.
func TestWriteMakesRelativeReplacesAbsolute(t *testing.T) {
	sum := "example.com/dep v1.0.0 h1:abc=\n"
	abs := filepath.Join(t.TempDir(), "abs") // absolute on every OS, unlike "/opt/abs"
	root := writeForge(t, `module example.com/forge

go 1.23

require example.com/dep v1.0.0

replace example.com/local => ./vendor/local

replace example.com/abs => `+strconv.Quote(abs)+`

replace example.com/remote v1.0.0 => example.com/fork v1.0.1
`, &sum)
	dir := t.TempDir()
	if err := Write(root, dir, "example.com/scratch"); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range parse(t, filepath.Join(dir, "go.mod")).Replace {
		got[r.Old.Path] = r.New.Path + "@" + r.New.Version
	}
	want := map[string]string{
		"example.com/forge":  root + "@",
		"example.com/local":  filepath.Join(root, "vendor", "local") + "@",
		"example.com/abs":    abs + "@",
		"example.com/remote": "example.com/fork@v1.0.1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("replace %s => %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("replaces = %v, want exactly %v", got, want)
	}
}

// Every way the forge checkout can be unusable is an error that writes
// nothing into dir: a half-written scratch module would build against the
// wrong requirement list.
func TestWriteRefusesAnUnusableForgeCheckout(t *testing.T) {
	sum := "x\n"
	for _, tc := range []struct {
		name string
		root func(t *testing.T) string
		want string
	}{
		{"no go.mod", func(t *testing.T) string { return writeForge(t, "", &sum) }, "go.mod"},
		{"go.mod does not parse", func(t *testing.T) string { return writeForge(t, "module (\n", &sum) }, "go.mod"},
		{"no go directive", func(t *testing.T) string { return writeForge(t, "module example.com/forge\n", &sum) }, "declares no module or go version"},
		{"no module directive", func(t *testing.T) string { return writeForge(t, "go 1.23\n", &sum) }, "declares no module or go version"},
		{"no go.sum", func(t *testing.T) string { return writeForge(t, "module example.com/forge\n\ngo 1.23\n", nil) }, "go.sum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			err := Write(tc.root(t), dir, "example.com/scratch")
			if err == nil {
				t.Fatal("Write succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q lacks %q", err, tc.want)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("a failed Write left %d file(s) in the scratch dir", len(entries))
			}
		})
	}
}

// A scratch directory that does not exist cannot be written to; Write says so
// rather than creating it, since the caller owns the directory's lifetime.
func TestWriteFailsWhenTheScratchDirectoryIsMissing(t *testing.T) {
	sum := "x\n"
	root := writeForge(t, "module example.com/forge\n\ngo 1.23\n", &sum)
	if err := Write(root, filepath.Join(t.TempDir(), "absent"), "example.com/scratch"); err == nil {
		t.Fatal("Write into a missing directory succeeded")
	}
}
