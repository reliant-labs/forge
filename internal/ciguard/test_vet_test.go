package ciguard

import (
	"os"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// TestVetOffOnlyWhereGoVetRuns holds GO_TEST_VET=-vet=off (scripts/ci-go-test.sh)
// to the steps whose vet another job of the same run already does. go test's
// built-in vet is a subset of `go vet`'s analyzers, so turning it off loses
// nothing only when `go vet ./...` covers the same packages and test files on
// every event the step runs on. So a step that sets it must:
//
//   - be in a job that runs on exactly the events Lint (whose `go vet ./...`
//     it relies on) runs on, never in the merge tier, whose golangci job
//     reports only the changed files and never vets pkg/;
//   - not test under build tags, which `go vet ./...` does not set;
//   - not be test-short-warm, whose vet results seed the cache the merge tier
//     restores.
func TestVetOffOnlyWhereGoVetRuns(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(ciWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			If    string `yaml:"if"`
			Steps []struct {
				Name string            `yaml:"name"`
				Run  string            `yaml:"run"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
			Env map[string]string `yaml:"env"`
		} `yaml:"jobs"`
		Env map[string]string `yaml:"env"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	if _, set := wf.Env["GO_TEST_VET"]; set {
		t.Error("ci.yml sets GO_TEST_VET workflow-wide: set it on the steps whose vet Lint's `go vet ./...` already does")
	}
	lint, ok := wf.Jobs["lint"]
	if !ok {
		t.Fatal("ci.yml has no lint job")
	}
	vets := false
	for _, s := range lint.Steps {
		if strings.TrimSpace(s.Run) == "go vet ./..." {
			vets = true
		}
	}
	if !vets {
		t.Fatal("the lint job no longer runs `go vet ./...`: every GO_TEST_VET=-vet=off step relies on it")
	}
	var off []string
	for name, j := range wf.Jobs {
		if _, set := j.Env["GO_TEST_VET"]; set {
			t.Errorf("job %s sets GO_TEST_VET job-wide: set it on the step whose vet Lint already does", name)
		}
		for _, s := range j.Steps {
			v, set := s.Env["GO_TEST_VET"]
			if !set {
				continue
			}
			off = append(off, name+"/"+s.Name)
			if v != "-vet=off" {
				t.Errorf("%s/%s sets GO_TEST_VET=%q, want -vet=off or nothing", name, s.Name, v)
			}
			if name == "test-short-warm" {
				t.Errorf("%s/%s turns vet off in the cache warmer, whose vet results the merge tier restores", name, s.Name)
			}
			if j.If != lint.If {
				t.Errorf("%s/%s turns vet off in a job that runs on %q, but Lint's go vet runs on %q", name, s.Name, j.If, lint.If)
			}
			if strings.Contains(s.Run, "-tags") {
				t.Errorf("%s/%s turns vet off for tagged tests, which `go vet ./...` does not vet", name, s.Name)
			}
		}
	}
	if len(off) == 0 {
		t.Error("no ci.yml step sets GO_TEST_VET=-vet=off: update this guard if that is on purpose")
	}
}
