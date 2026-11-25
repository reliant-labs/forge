package cigate

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/frontenddeps"
	"github.com/reliant-labs/forge/internal/templates"
)

// RelatedScript is the package.json script a frontend declares to run only
// the tests related to some files: the gate passes the changed files under
// the frontend, relative to its directory, as arguments. A vitest frontend
// declares `"test:related": "vitest related --run --passWithNoTests"`. forge
// reads no test runner's config: the script is the frontend's word for which
// tests a file reaches.
const RelatedScript = "test:related"

// FrontendTestName is the affected-mode row of frontend fe's tests.
func FrontendTestName(fe string) string { return "test-" + fe }

// WithFrontendTests adds a test-<name> row after unit-tests for each frontend
// an affected rule selected (a.Frontends). In full mode, and when no rule
// selected a frontend, checks come back unchanged: `task test` runs every
// frontend's suite itself, and a narrowed `task test -- <packages>` runs
// none, which is what these rows are for.
//
// The row runs the frontend's test:related script with the changed files
// under the frontend when it declares one; otherwise — or when no changed
// file is under it (a rule claimed a file elsewhere for it) — the whole
// `test` script, as `task test` runs it.
func WithFrontendTests(checks []Check, a Affected, root string, frontends []templates.FrontendCIConfig) []Check {
	if a.Full || len(a.Frontends) == 0 {
		return checks
	}
	at := -1
	for i, c := range checks {
		if c.Name == UnitTests {
			at = i
		}
	}
	if at < 0 || checks[at].Deferred != "" {
		return checks
	}
	byName := map[string]templates.FrontendCIConfig{}
	for _, fe := range frontends {
		byName[fe.Name] = fe
	}
	taken := map[string]bool{}
	for _, c := range checks {
		taken[c.Name] = true
	}
	var rows []Check
	for _, name := range a.Frontends {
		fe, ok := byName[name]
		if !ok || taken[FrontendTestName(name)] {
			continue // Plan rejects an unknown frontend; a project row of this name wins
		}
		rows = append(rows, frontendTest(root, fe, changedUnder(root, a.Changed, fe.Path)))
	}
	out := make([]Check, 0, len(checks)+len(rows))
	out = append(out, checks[:at+1]...)
	out = append(out, rows...)
	return append(out, checks[at+1:]...)
}

func frontendTest(root string, fe templates.FrontendCIConfig, files []string) Check {
	c := Check{
		Name:   FrontendTestName(fe.Name),
		Lane:   LaneTest,
		Group:  GroupTest,
		Heavy:  true,
		After:  []string{depsName(fe)},
		Prereq: needsDepsAt(root, fe),
		Dir:    fe.Path,
	}
	scripts, err := packageScripts(filepath.Join(root, fe.Path))
	if err != nil {
		c.Prereq = func() string { return "cannot read the frontend's scripts: " + err.Error() }
		return c
	}
	// npm hands a script its arguments only after `--`; pnpm and yarn pass
	// them as they are (the Taskfile's test:frontend does the same).
	sep := []string(nil)
	if frontenddeps.PackageManager(filepath.Join(root, fe.Path), "") == "npm" {
		sep = []string{"--"}
	}
	switch {
	case scripts[RelatedScript] != "" && len(files) > 0:
		c.Argv = feRunAt(root, fe, append(append([]string{RelatedScript}, sep...), files...)...)
		c.Note = fmt.Sprintf("affected mode: tests related to %d changed file(s)", len(files))
	case scripts["test"] != "":
		// The same command `task test` runs for this frontend.
		c.Argv = feRunAt(root, fe, append(append([]string{"test"}, sep...), "--passWithNoTests")...)
		switch {
		case len(files) == 0:
			c.Note = "affected mode: selected by ci.gate.affected_rules with no changed file under " + fe.Path + " — whole suite"
		default:
			c.Note = "affected mode: whole suite (package.json declares no " + RelatedScript + " script)"
		}
	default:
		c.NotApplicable = "package.json declares no test or " + RelatedScript + " script"
	}
	return c
}

// changedUnder returns the changed files under dir that still exist,
// relative to dir. A deleted file has no tests of its own to relate; the
// tests that imported it fail to compile in the whole suite instead.
func changedUnder(root string, changed []string, dir string) []string {
	prefix := strings.TrimSuffix(path.Clean(filepath.ToSlash(dir)), "/") + "/"
	if prefix == "./" {
		prefix = "" // a frontend at the project root
	}
	var out []string
	for _, f := range changed {
		f = filepath.ToSlash(f)
		if !strings.HasPrefix(f, prefix) {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(f))); err != nil {
			continue
		}
		out = append(out, strings.TrimPrefix(f, prefix))
	}
	return out
}

// packageScripts reads the scripts of dir's package.json.
func packageScripts(dir string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	return manifest.Scripts, nil
}
