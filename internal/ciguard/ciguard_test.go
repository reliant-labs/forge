package ciguard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// The files under guard, relative to this package's directory — the working
// directory `go test` runs it in. Read by this process, so the test cache sees
// an edit to either.
var (
	ciWorkflow     = filepath.Join("..", "..", ".github", "workflows", "ci.yml")
	packagesScript = filepath.Join("..", "..", "scripts", "ci-test-packages.sh")
)

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

// splitPackages reads the `split=( ... )` array from
// scripts/ci-test-packages.sh: the packages whose tests the pull-request tier
// divides across shards, and which its package shards therefore leave out.
func splitPackages(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(packagesScript)
	if err != nil {
		t.Fatalf("read %s: %v", packagesScript, err)
	}
	m := regexp.MustCompile(`(?ms)^split=\(\n(.*?)^\)`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s: no `split=( ... )` array", packagesScript)
	}
	var pkgs []string
	for _, line := range strings.Split(string(m[1]), "\n") {
		if f := strings.TrimSpace(line); f != "" && !strings.HasPrefix(f, "#") {
			pkgs = append(pkgs, f)
		}
	}
	if len(pkgs) == 0 {
		t.Fatalf("%s: the split array is empty", packagesScript)
	}
	sort.Strings(pkgs)
	return pkgs
}

// TestShortTierShardsCoverEveryIndex proves the test-short matrix runs every
// shard of every partition it declares. scripts/ci-test-packages.sh deals
// packages onto `short I N` and scripts/ci-test-shard.sh deals a split
// package's tests onto chunk I of N; both partitions are total only if all N
// indices run. A matrix entry deleted, mistyped, or left at an old N after the
// others moved would drop a slice of the suite while every remaining shard
// stays green. And a split package the matrix never names would run nowhere:
// the package shards exclude it.
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

	type partition struct {
		n       int
		indices []int
	}
	partitions := map[string]*partition{}
	for _, e := range entries {
		args := strings.Fields(e["args"])
		var key, idx, cnt string
		switch {
		case len(args) == 3 && args[0] == "pkgs":
			key, idx, cnt = "pkgs", args[1], args[2]
		case len(args) == 4 && args[0] == "split":
			key, idx, cnt = "split "+args[1], args[2], args[3]
		default:
			t.Errorf("test-short entry %q: args %q are neither `pkgs I N` nor `split PKG I N`", e["shard"], e["args"])
			continue
		}
		i, err1 := strconv.Atoi(idx)
		n, err2 := strconv.Atoi(cnt)
		if err1 != nil || err2 != nil || n < 1 || i < 0 || i >= n {
			t.Errorf("test-short entry %q: %q is not a valid index I of N (0 <= I < N)", e["shard"], e["args"])
			continue
		}
		p := partitions[key]
		if p == nil {
			p = &partition{n: n}
			partitions[key] = p
		}
		if p.n != n {
			t.Errorf("%s: entries disagree on the shard count (%d and %d) — every entry of one partition must name the same N", key, p.n, n)
		}
		p.indices = append(p.indices, i)
	}

	if _, ok := partitions["pkgs"]; !ok {
		t.Error("test-short runs no `pkgs` shard: every package outside the split set would go untested on pull requests")
	}
	for key, p := range partitions {
		sort.Ints(p.indices)
		want := make([]int, p.n)
		for i := range want {
			want[i] = i
		}
		if !slices.Equal(p.indices, want) {
			t.Errorf("%s: matrix runs indices %v of %d, want exactly %v — a missing index is a slice of the suite that runs nowhere", key, p.indices, p.n, want)
		}
	}

	var matrixSplit []string
	for key := range partitions {
		if pkg, ok := strings.CutPrefix(key, "split "); ok {
			matrixSplit = append(matrixSplit, pkg)
		}
	}
	sort.Strings(matrixSplit)
	if script := splitPackages(t); !slices.Equal(matrixSplit, script) {
		t.Errorf("test-short splits %v, scripts/ci-test-packages.sh splits %v: a package split by the script but not "+
			"the matrix runs in no shard, and one split by the matrix alone runs twice", matrixSplit, script)
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
