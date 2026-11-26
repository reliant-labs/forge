package kcloptions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/kclvendor"
	"github.com/reliant-labs/forge/pkg/testkit"
)

// writeFiles lays out files (slash-separated relative paths) under a fresh
// root and returns it.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const emptyKclMod = "[package]\nname = \"fixture-deploy\"\nversion = \"0.1.0\"\n\n[dependencies]\n"

// A kcl.mod that still declares the forge module as a dependency is refused
// before any KCL is read: the render would take the module from that
// declaration instead of from the binary.
func TestDiscoverRefusesAnUnmigratedKclMod(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"deploy/kcl/kcl.mod":    emptyKclMod + "forge = { path = \"../../.forge-kcl\" }\n",
		"deploy/kcl/dev/main.k": "x = option(\"x\")\n",
	})
	opts, discoverable, err := Discover(root, "dev")
	var unmigrated *kclvendor.UnmigratedError
	if !errors.As(err, &unmigrated) {
		t.Fatalf("Discover error = %v, want *kclvendor.UnmigratedError", err)
	}
	if len(unmigrated.Paths) != 1 || unmigrated.Paths[0] != "deploy/kcl/kcl.mod" {
		t.Errorf("UnmigratedError.Paths = %v, want [deploy/kcl/kcl.mod]", unmigrated.Paths)
	}
	if opts != nil || discoverable {
		t.Errorf("Discover = %+v, %v on error; want nil, false", opts, discoverable)
	}
}

// A kcl.mod kpm cannot parse is an error that names the module, not a project
// that quietly declares no options.
func TestDiscoverReportsAnUnloadableKclMod(t *testing.T) {
	testkit.Slow(t, "starts the native KCL client")
	t.Cleanup(kclvendor.SetCacheDirForTest(t.TempDir()))
	root := writeFiles(t, map[string]string{
		"deploy/kcl/kcl.mod":    "[package\nname = \n",
		"deploy/kcl/dev/main.k": "x = option(\"x\")\n",
	})
	_, discoverable, err := Discover(root, "dev")
	if err == nil {
		t.Fatal("Discover accepted a kcl.mod that is not TOML")
	}
	if !strings.Contains(err.Error(), "load kcl package") {
		t.Errorf("error = %q, want it to say the kcl package failed to load", err)
	}
	if discoverable {
		t.Error("discoverable = true on error")
	}
}

// A [forge_projects] import that cannot be resolved fails discovery with the
// resolver's message rather than reporting "undiscoverable".
func TestDiscoverReportsAnUnresolvableForgeProjectImport(t *testing.T) {
	testkit.Slow(t, "starts the native KCL client")
	t.Cleanup(kclvendor.SetCacheDirForTest(t.TempDir()))
	root := writeFiles(t, map[string]string{
		"deploy/kcl/kcl.mod":    emptyKclMod + "\n[forge_projects]\nghost = { module = \"example.com/not-in-go-mod\" }\n",
		"deploy/kcl/dev/main.k": "x = option(\"x\")\n",
	})
	_, discoverable, err := Discover(root, "dev")
	if err == nil {
		t.Fatal("Discover resolved a forge project that no go.mod requires")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %q, want it to name the import `ghost`", err)
	}
	if discoverable {
		t.Error("discoverable = true on error")
	}
}

// An env whose KCL reads no option at all — not even forge's own — is the
// "the walk bailed" signal: discoverable=false with no error, so callers
// degrade permissively instead of rejecting a name they merely failed to see.
func TestDiscoverWithNoOptionsAtAllIsUndiscoverable(t *testing.T) {
	testkit.Slow(t, "evaluates KCL")
	root := writeEnvProject(t, "dev", "name = \"api\"\n")
	opts, discoverable, err := Discover(root, "dev")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if discoverable || opts != nil {
		t.Errorf("Discover = %+v, %v; want nil, false", opts, discoverable)
	}
}

// A project with no kcl.mod anywhere resolves no external packages and is
// still discoverable.
func TestDiscoverWithoutAKclModStillFindsOptions(t *testing.T) {
	testkit.Slow(t, "evaluates KCL")
	t.Cleanup(kclvendor.SetCacheDirForTest(t.TempDir()))
	root := writeFiles(t, map[string]string{
		"deploy/kcl/dev/main.k": "region = option(\"region\", type=\"str\", default=\"us\", help=\"Where\")\n",
	})
	opts, discoverable, err := Discover(root, "dev")
	if err != nil || !discoverable {
		t.Fatalf("Discover = %+v, %v, %v", opts, discoverable, err)
	}
	if len(opts) != 1 || opts[0].Name != "region" || opts[0].Default != "us" || opts[0].Help != "Where" {
		t.Fatalf("Discover() = %+v, want the region option with its metadata", opts)
	}
}

// A path dependency in kcl.mod is resolved through kpm and handed to the
// walk, so an env importing it is discoverable.
func TestDiscoverResolvesAPathDependency(t *testing.T) {
	testkit.Slow(t, "evaluates KCL")
	t.Cleanup(kclvendor.SetCacheDirForTest(t.TempDir()))
	root := writeFiles(t, map[string]string{
		"shared/kcl.mod":        "[package]\nname = \"shared\"\nversion = \"0.1.0\"\n",
		"shared/shared.k":       "greeting = \"hi\"\n",
		"deploy/kcl/kcl.mod":    emptyKclMod + "shared = { path = \"../../shared\" }\n",
		"deploy/kcl/dev/main.k": "import shared\n\nzone = option(\"zone\", default=\"a\")\ng = shared.greeting\n",
	})
	opts, discoverable, err := Discover(root, "dev")
	if err != nil || !discoverable {
		t.Fatalf("Discover = %+v, %v, %v", opts, discoverable, err)
	}
	if len(opts) != 1 || opts[0].Name != "zone" {
		t.Fatalf("Discover() = %+v, want only the env's zone option", opts)
	}
}

func TestFindModuleRoot(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"deploy/kcl/kcl.mod":        emptyKclMod,
		"deploy/kcl/dev/main.k":     "",
		"deploy/kcl/inner/kcl.mod":  emptyKclMod,
		"deploy/kcl/inner/env/a.k":  "",
		"unrelated/leaf/.gitignore": "",
	})
	for _, tc := range []struct{ name, dir, want string }{
		{"env without its own kcl.mod walks up", "deploy/kcl/dev", "deploy/kcl"},
		{"the nearest kcl.mod wins", "deploy/kcl/inner/env", "deploy/kcl/inner"},
		{"a dir holding the kcl.mod is its own root", "deploy/kcl", "deploy/kcl"},
	} {
		got := findModuleRoot(filepath.Join(root, filepath.FromSlash(tc.dir)))
		if want := filepath.Join(root, filepath.FromSlash(tc.want)); got != want {
			t.Errorf("%s: findModuleRoot = %q, want %q", tc.name, got, want)
		}
	}
	// With no kcl.mod above, the walk ends at the filesystem root and says "".
	// Only checkable when no ancestor of the temp dir happens to hold one.
	for dir := root; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "kcl.mod")); err == nil && dir != root {
			t.Skipf("%s holds a kcl.mod above the temp dir", dir)
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if got := findModuleRoot(filepath.Join(root, "unrelated", "leaf")); got != "" {
		t.Errorf("findModuleRoot with no kcl.mod above = %q, want \"\"", got)
	}
}
