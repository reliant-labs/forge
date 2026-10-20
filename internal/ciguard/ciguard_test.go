package ciguard

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// The file under guard, relative to this package's directory — the working
// directory `go test` runs it in. Read by this process, so the test cache sees
// an edit to it.
var ciWorkflow = filepath.Join("..", "..", ".github", "workflows", "ci.yml")

type workflow struct {
	Jobs map[string]job `yaml:"jobs"`
}

type job struct {
	If       string   `yaml:"if"`
	Needs    []string `yaml:"needs"`
	Strategy struct {
		Matrix struct {
			Include []map[string]string `yaml:"include"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
}

func readWorkflow(t *testing.T) workflow {
	t.Helper()
	raw, err := os.ReadFile(ciWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", ciWorkflow, err)
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", ciWorkflow, err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatalf("%s declares no jobs — this guard would inspect nothing", ciWorkflow)
	}
	return wf
}

// TestShortTierShardsCoverEveryIndex proves the test-short matrix runs every
// shard of the partition it declares. scripts/ci-test-packages.sh deals every
// package onto `short I N`, which is total only if all N indices run. A matrix
// entry deleted, mistyped, or left at an old N after the others moved would
// drop a slice of the module's packages while every remaining shard stays
// green.
func TestShortTierShardsCoverEveryIndex(t *testing.T) {
	wf := readWorkflow(t)
	short, ok := wf.Jobs["test-short"]
	if !ok {
		t.Fatalf("%s has no test-short job", ciWorkflow)
	}
	entries := short.Strategy.Matrix.Include
	if len(entries) == 0 {
		t.Fatal("test-short has no matrix.include entries — this guard would inspect nothing")
	}

	shards := -1
	var indices []int
	for _, e := range entries {
		args := strings.Fields(e["args"])
		if len(args) != 2 {
			t.Errorf("test-short entry %q: args %q are not `I N`", e["shard"], e["args"])
			continue
		}
		i, err1 := strconv.Atoi(args[0])
		n, err2 := strconv.Atoi(args[1])
		if err1 != nil || err2 != nil || n < 1 || i < 0 || i >= n {
			t.Errorf("test-short entry %q: %q is not a valid shard I of N (0 <= I < N)", e["shard"], e["args"])
			continue
		}
		if shards == -1 {
			shards = n
		}
		if n != shards {
			t.Errorf("test-short entries disagree on the shard count (%d and %d) — every entry must name the same N", shards, n)
		}
		indices = append(indices, i)
	}
	if shards < 1 {
		return
	}
	sort.Ints(indices)
	want := make([]int, shards)
	for i := range want {
		want[i] = i
	}
	if !slices.Equal(indices, want) {
		t.Errorf("test-short runs shards %v of %d, want exactly %v — a missing shard is a slice of the module that runs nowhere on a pull request", indices, shards, want)
	}
}

// TestGateNeedsEveryJob proves `CI gate` waits on, and so reports, every other
// job in ci.yml. It is the one check a ruleset would require; a job missing
// from its needs could fail while the gate stays green.
func TestGateNeedsEveryJob(t *testing.T) {
	wf := readWorkflow(t)
	gate, ok := wf.Jobs["gate"]
	if !ok {
		t.Fatalf("%s has no gate job", ciWorkflow)
	}
	if !strings.Contains(gate.If, "always()") {
		t.Errorf("gate's if is %q: without always() it is skipped when a job it needs fails, and a skipped gate reads as passing", gate.If)
	}
	for name := range wf.Jobs {
		if name != "gate" && !slices.Contains(gate.Needs, name) {
			t.Errorf("gate does not need %q: that job could fail while CI gate stays green", name)
		}
	}
	for _, name := range gate.Needs {
		if _, ok := wf.Jobs[name]; !ok {
			t.Errorf("gate needs %q, which is not a job in %s", name, ciWorkflow)
		}
	}
}
