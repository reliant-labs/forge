package skills

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const forgeModule = "github.com/reliant-labs/forge/"

// forgeImportsAllowed are the forge packages this package may reach, directly
// or through one another. Each is light: none reaches internal/cli,
// internal/codegen, pkg/deploy or a cloud SDK.
var forgeImportsAllowed = []string{
	"internal/benefits",
	"internal/buildinfo",
	"internal/claimablecaps",
	"internal/cli/cmdutil",
	"internal/cliutil",
	"internal/naming",
	"internal/procguard",
	"internal/skillcatalog",
	"internal/templates",
	"internal/webruntimepeers",
	"pkg/contractkit",
	"pkg/hostslot",
	"pkg/toolpins",
}

// thirdPartyImportsAllowed are the non-forge, non-standard packages those
// forge packages may import directly. Each is small and has a small closure.
var thirdPartyImportsAllowed = []string{
	"github.com/jinzhu/inflection",
	"github.com/spf13/cobra",
	"github.com/spf13/pflag",
	"go.opentelemetry.io/otel/attribute",
	"go.opentelemetry.io/otel/codes",
	"go.opentelemetry.io/otel/metric",
	"go.opentelemetry.io/otel/trace",
	"go.yaml.in/yaml/v3",
	"golang.org/x/mod/modfile",
	"golang.org/x/mod/module",
	"golang.org/x/mod/semver",
	"golang.org/x/sys/unix",
	"golang.org/x/sys/windows",
	"golang.org/x/term",
	"golang.org/x/text/cases",
	"golang.org/x/text/language",
	"gopkg.in/yaml.v3",
}

// This package exists so a host can list and render forge's skills without
// linking the forge CLI: reliant's skill catalog imported forge/cli for
// ListSkills and RenderSkill, which put ~2,150 packages (KCL, docker, the
// cloud SDKs, k8s, envoy) into 36 of its test binaries. So the forge packages
// it reaches are a fixed, light set, and they import only the standard
// library, one another and a short list of small libraries. A new import that
// fails here puts that weight back: keep what the skill renderer needs out of
// heavy packages instead (pkg/deploy's claimable caps live in
// internal/claimablecaps for this reason).
//
// The walk parses every non-test file, for every platform, in-process, so the
// test cache sees each input.
func TestImportsStayLight(t *testing.T) {
	forgeAllowed := map[string]bool{"pkg/skills": true}
	for _, p := range forgeImportsAllowed {
		forgeAllowed[p] = true
	}
	thirdAllowed := map[string]bool{}
	for _, p := range thirdPartyImportsAllowed {
		thirdAllowed[p] = true
	}
	seen := map[string]bool{}
	var walk func(rel string)
	walk = func(rel string) {
		if seen[rel] {
			return
		}
		seen[rel] = true
		dir := filepath.Join("..", "..", filepath.FromSlash(rel))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		files := 0
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			files++
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
			}
			for _, spec := range f.Imports {
				path, _ := strconv.Unquote(spec.Path.Value)
				switch {
				case !strings.Contains(strings.SplitN(path, "/", 2)[0], "."):
					// the standard library
				case strings.HasPrefix(path, forgeModule):
					next := strings.TrimPrefix(path, forgeModule)
					if !forgeAllowed[next] {
						t.Errorf("%s (%s) imports %s, which is not in forgeImportsAllowed: pkg/skills must stay light", rel, name, path)
						continue
					}
					walk(next)
				case !thirdAllowed[path]:
					t.Errorf("%s (%s) imports %s, which is not in thirdPartyImportsAllowed: pkg/skills must stay light", rel, name, path)
				}
			}
		}
		if files == 0 {
			t.Fatalf("%s: no Go files in %s", rel, dir)
		}
	}
	walk("pkg/skills")
	for p := range forgeAllowed {
		if !seen[p] {
			t.Errorf("forgeImportsAllowed lists %s, which pkg/skills no longer reaches: drop it", p)
		}
	}
}

// TestClosureStaysSmall asks the go command for the real closure: the parse
// above sees only direct imports of third-party packages. Today it is about
// 180 packages, of which ~135 are the standard library (forge/cli: ~2,150).
func TestClosureStaysSmall(t *testing.T) {
	if testing.Short() {
		t.Skip("runs `go list -deps`; runs in task test")
	}
	out, err := exec.Command("go", "list", "-deps", "-json=ImportPath", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	var deps []string
	for dec.More() {
		var p struct{ ImportPath string }
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		deps = append(deps, p.ImportPath)
	}
	sort.Strings(deps)
	const ceiling = 250
	if len(deps) > ceiling {
		t.Errorf("pkg/skills links %d packages, over the ceiling of %d:\n%s", len(deps), ceiling, strings.Join(deps, "\n"))
	}
	for _, heavy := range []string{"internal/cli", "internal/codegen", "pkg/deploy", "pkg/observe"} {
		for _, d := range deps {
			if d == forgeModule+heavy {
				t.Errorf("pkg/skills links %s", d)
			}
		}
	}
}
