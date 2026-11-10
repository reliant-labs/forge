package ciguard

import (
	"bufio"
	"go/build/constraint"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// integrationNotInCI is every package with `integration`-tagged tests that no
// ci.yml step runs, and why. pkg/obsquery's sat on a list like this one without
// anyone writing it down, and one of its tests was red on main for days
// (forge#1870). Adding a package to a lane is a runner-budget call (the org
// runs 20 jobs at once), so a new one either joins a push-to-main step or
// says here why not.
var integrationNotInCI = map[string]string{
	"./internal/cli":         "docker/envtest end-to-end smokes (db squash, OpenBao, reproducible docker build); not yet in a lane (found 2026-11-10)",
	"./internal/templates":   "docker builds and compose smokes of rendered templates; not yet in a lane (found 2026-11-10)",
	"./pkg/alerting/pgstore": "needs a real Postgres; not yet in a lane (found 2026-11-10)",
	"./pkg/blobstore":        "needs a MinIO container; not yet in a lane (found 2026-11-10)",
	"./pkg/migratekit":       "Postgres behind PgBouncer in containers; not yet in a lane (found 2026-11-10)",
	"./pkg/observe":          "opt-in (RUN_HEADERS_FILE_SMOKE=1): boots two collector containers",
	"./pkg/orm":              "SQLite differ; not yet in a lane (found 2026-11-10)",
	"./pkg/testkit":          "proves testkit.Slow skips in the integration lane; meaningful only inside a lane that runs other tests",
}

// integrationTags matches a `go test` invocation's -tags naming integration.
var integrationTags = regexp.MustCompile(`-tags[= ]\S*\bintegration\b`)

type stepsJob struct {
	If    string `yaml:"if"`
	Steps []struct {
		Name string `yaml:"name"`
		Run  string `yaml:"run"`
	} `yaml:"steps"`
}

// TestIntegrationPackagesRunOnMain proves every package with
// `integration`-tagged tests is run by a ci.yml step in the push-to-main tier,
// or is named in integrationNotInCI with a reason; and that no such step runs
// on a pull request or a merge-queue entry, whose five-minute budget a
// container-backed test does not fit.
func TestIntegrationPackagesRunOnMain(t *testing.T) {
	raw, err := os.ReadFile(ciWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", ciWorkflow, err)
	}
	var wf struct {
		Jobs map[string]stepsJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", ciWorkflow, err)
	}

	var patterns []string // the packages integration steps name: ./a/b or ./a/...
	for name, j := range wf.Jobs {
		for _, s := range j.Steps {
			if !integrationTags.MatchString(s.Run) {
				continue
			}
			if !strings.Contains(j.If, "github.event_name == 'push'") ||
				strings.Contains(j.If, "pull_request") || strings.Contains(j.If, "merge_group") {
				t.Errorf("job %s step %q runs integration tests but the job is not push-to-main only (if: %q): they do not fit the pull-request tier's budget", name, s.Name, j.If)
			}
			for _, f := range strings.Fields(s.Run) {
				if strings.HasPrefix(f, "./") {
					patterns = append(patterns, strings.TrimSuffix(f, "/"))
				}
			}
		}
	}
	if len(patterns) == 0 {
		t.Fatalf("no ci.yml step runs `go test -tags=integration ./<pkg>` — this guard would inspect nothing")
	}

	pkgs := integrationPackages(t, filepath.Join("..", ".."))
	if len(pkgs) == 0 {
		t.Fatal("found no integration-tagged test files — this guard would inspect nothing")
	}
	runs := func(pkg string) bool {
		for _, p := range patterns {
			if base, tree := strings.CutSuffix(p, "/..."); tree && (pkg == base || strings.HasPrefix(pkg, base+"/")) {
				return true
			}
			if p == pkg {
				return true
			}
		}
		return false
	}
	for _, pkg := range pkgs {
		_, listed := integrationNotInCI[pkg]
		switch {
		case runs(pkg) && listed:
			t.Errorf("%s is run by a ci.yml integration step and still listed in integrationNotInCI: delete the entry", pkg)
		case !runs(pkg) && !listed:
			t.Errorf("%s has integration-tagged tests that no ci.yml step runs: add it to a push-to-main step (go test -tags=integration %s), or to integrationNotInCI with the reason it stays out", pkg, pkg)
		}
	}
	for pkg := range integrationNotInCI {
		if !slices.Contains(pkgs, pkg) {
			t.Errorf("integrationNotInCI names %s, which has no integration-tagged tests any more: delete the entry", pkg)
		}
	}
}

// integrationPackages lists, as ./dir, every directory under root holding a
// _test.go file that builds only with the integration tag. Read by this
// process, so the test cache sees an added or retagged file.
func integrationPackages(t *testing.T, root string) []string {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "vendor", ".forge", ".tools":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		only, err := integrationOnly(p)
		if err != nil {
			return err
		}
		if only {
			rel, _ := filepath.Rel(root, filepath.Dir(p))
			seen["./"+path.Clean(filepath.ToSlash(rel))] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// integrationOnly reports whether the file's //go:build line (before its
// package clause) is satisfied with the integration tag and not without it.
func integrationOnly(file string) (bool, error) {
	f, err := os.Open(file)
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "package ") {
			return false, nil
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			return false, nil
		}
		with := expr.Eval(func(tag string) bool { return tag == "integration" || tag == "linux" })
		without := expr.Eval(func(tag string) bool { return tag == "linux" })
		return with && !without, nil
	}
	return false, sc.Err()
}
