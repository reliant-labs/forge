package pkgguard

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// TestLibraryReadsNoAmbientEnvironment is the guard on forge/pkg: no package
// in the library that ships into a user's binary reads the process
// environment directly, except the packages pkg/.golangci.yml allowlists.
//
// Both halves of the rule are DERIVED from that config — the forbidden calls
// from its `forbid` patterns, the exemptions from its `exclusions.rules` —
// so this test and the lint that gates CI cannot drift apart, and neither
// names a file it does not own. Loosening the rule means editing the config,
// where the loosening is visible.
//
// Why it matters, stated once: a library changes behaviour only through the
// arguments its caller passed. A value read out of the ambient environment
// appears in no config proto, no KCL env block and no typed config object,
// so nothing the application can read explains why the same binary behaves
// differently on two machines — and in the worst case (an authentication
// mode) a shell variable decides whether the server checks credentials.
func TestLibraryReadsNoAmbientEnvironment(t *testing.T) {
	t.Parallel()
	pkgRoot := filepath.Join(repoRoot(t), "pkg")

	policy, err := LoadForbidigoPolicy(filepath.Join(pkgRoot, ".golangci.yml"))
	if err != nil {
		t.Fatalf("load the library's lint policy: %v", err)
	}
	if !policy.Enabled {
		t.Fatal("forbidigo is not in linters.enable in pkg/.golangci.yml — a settings block alone " +
			"configures a linter that never runs, which is exactly how the root config reported " +
			"green over 12 environment reads")
	}
	if len(policy.Forbid) == 0 {
		t.Fatal("pkg/.golangci.yml forbids nothing — this guard would then certify every call in the module")
	}

	findings, files, err := Scan(pkgRoot, policy)
	if err != nil {
		t.Fatalf("scan the library module: %v", err)
	}
	if files == 0 {
		t.Fatal("the walk parsed no Go files under pkg/ — the guard is broken, and a guard that " +
			"inspects nothing certifies everything")
	}
	for _, f := range findings {
		t.Errorf("%s — forge/pkg compiles into every generated binary and must take this value from "+
			"its caller (a field on the options/config struct the package already has), not from the "+
			"ambient environment. The same rule fails a generated project's build: see "+
			"internal/templates/project/golangci.yml.tmpl. If this package is genuinely a sanctioned "+
			"reader, allowlist it in pkg/.golangci.yml with the reason written down.", f)
	}
	t.Logf("scanned %d Go files under pkg/", files)
}

// TestLibraryModuleIsGatedInCI closes the failure mode the guard above cannot
// see: a lint config that nothing runs, and a test suite nothing executes.
//
// forge/pkg is a separate module. `golangci-lint run` and `go test ./...`
// both act on the module of the directory they are invoked in, so the repo's
// root jobs — which is all CI had — read none of it. Every assertion in
// pkg/**/*_test.go, including the ones pinning that no ambient credential can
// re-authenticate a request, ran on developer machines only.
//
// The set is derived by walking the workflows and matching the COMMANDS, not
// by naming a job or a file, and it fails loudly when the walk finds no
// golangci-lint or `go test` step at all.
func TestLibraryModuleIsGatedInCI(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}

	var lintSteps, testSteps []step
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // repo-relative workflow path
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		var wf workflow
		if uerr := yaml.Unmarshal(raw, &wf); uerr != nil {
			t.Fatalf("parse %s: %v", e.Name(), uerr)
		}
		for _, job := range wf.Jobs {
			for _, s := range job.Steps {
				switch {
				case strings.Contains(s.Uses, "golangci-lint-action") || strings.Contains(s.Run, "golangci-lint run"):
					lintSteps = append(lintSteps, s)
				case strings.Contains(s.Run, "go test"):
					testSteps = append(testSteps, s)
				}
			}
		}
	}

	if len(lintSteps) == 0 {
		t.Fatal("no workflow runs golangci-lint at all — this assertion inspected nothing")
	}
	if len(testSteps) == 0 {
		t.Fatal("no workflow runs `go test` at all — this assertion inspected nothing")
	}

	if !coversPkgModule(lintSteps) {
		t.Error("no CI step lints the forge/pkg module: golangci-lint reads the module of the " +
			"directory it runs in, so a job without `working-directory: pkg` (or a `cd pkg`) leaves " +
			"the library that ships into every user binary unlinted — which is how 12 forbidden " +
			"environment reads accumulated under a config that already forbade them")
	}
	if !coversPkgModule(testSteps) {
		t.Error("no CI step runs `go test` in the forge/pkg module: `./...` at the repo root does not " +
			"match a separate module even inside a go.work workspace, so every test in pkg/**, " +
			"including the ones pinning that no ambient credential authenticates a request, runs " +
			"nowhere but a developer's laptop")
	}
}

// coversPkgModule reports whether any step is invoked with the pkg module as
// its working directory — declared via working-directory, or entered by the
// script itself.
func coversPkgModule(steps []step) bool {
	for _, s := range steps {
		if strings.Trim(s.workingDir(), "./") == "pkg" {
			return true
		}
		if strings.Contains(s.Run, "cd pkg") {
			return true
		}
	}
	return false
}

type workflow struct {
	Jobs map[string]struct {
		Steps []step `yaml:"steps"`
	} `yaml:"jobs"`
}

type step struct {
	Uses             string `yaml:"uses"`
	Run              string `yaml:"run"`
	WorkingDirectory string `yaml:"working-directory"`
	// With carries an action's inputs. golangci-lint-action takes its
	// working directory there rather than as a step key, so a guard that
	// only read the step key would report "unlinted" for a correctly wired
	// job — and, worse, could be "fixed" by moving the key where the action
	// ignores it.
	With map[string]any `yaml:"with"`
}

// workingDir is where this step actually runs, from either spelling.
func (s step) workingDir() string {
	if s.WorkingDirectory != "" {
		return s.WorkingDirectory
	}
	wd, _ := s.With["working-directory"].(string)
	return wd
}

// repoRoot finds the repository root from this test's own compiled-in source
// path, so it is correct under `go test ./...` from any directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate the repository root")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(self)))
}
