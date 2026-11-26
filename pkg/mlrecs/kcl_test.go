package mlrecs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	kcl "kcl-lang.io/kcl-go"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// runKCL evaluates an ml.k against the contract stub in testdata/kclstub,
// laid out as a project whose `forge` package is the stub.
func runKCL(t *testing.T, stub []byte, mlk string) error {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "forge"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"kcl.mod":    []byte("[package]\nname = \"mlproposal\"\n"),
		"forge/ml.k": stub,
		"ml.k":       []byte(mlk),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := kcl.Run(filepath.Join(dir, "ml.k"), kcl.WithWorkDir(dir))
	return err
}

// TestProposalIsValidKCL (in-process, well under a second) evaluates every fixture's proposal as written,
// with each FixKCL finding's install applied alone, and with all applied
// at once, against the KCL contract.
func TestProposalIsValidKCL(t *testing.T) {
	stub, err := os.ReadFile(filepath.Join("testdata", "kclstub", "ml.k"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			s := loadSnapshot(t, name)
			inv := mldiscover.Discover(s)
			opts := ProposeOptions{Decisions: fixtureDecisions(name), ConnectedCluster: "acme-gpu"}
			fs := Evaluate(s, inv, Options{Decisions: opts.Decisions, Propose: opts})
			variants := map[string]map[string]bool{"as-written": nil}
			all := map[string]bool{}
			for _, f := range fs {
				if f.Fix.Kind == FixKCL && f.Dismissed == nil {
					variants[f.ID] = map[string]bool{f.ID: true}
					all[f.ID] = true
				}
			}
			if len(all) > 1 {
				variants["all-applied"] = all
			}
			for vname, active := range variants {
				mlk := propose(inv, fs, opts, active)
				if err := runKCL(t, stub, mlk); err != nil {
					t.Errorf("%s: invalid KCL: %v\n%s", vname, err, mlk)
				}
			}
		})
	}
}

// TestStubRefusesInstallingAnObservedKind proves the stub enforces the
// invariant the proposal leans on, so the test above can fail.
func TestStubRefusesInstallingAnObservedKind(t *testing.T) {
	stub, err := os.ReadFile(filepath.Join("testdata", "kclstub", "ml.k"))
	if err != nil {
		t.Fatal(err)
	}
	mlk := `import forge

ml_stacks = [forge.MLStack {
    cluster = forge.ClusterTarget {cluster = "c", namespace = "forge-ml"}
    observed = ["kueue"]
    queue = forge.Install {kind = "kueue", chart = forge.HelmChart {name = "kueue", oci = "oci://x", version = "1", namespace = "n"}}
}]
`
	err = runKCL(t, stub, mlk)
	if err == nil || !strings.Contains(err.Error(), "observed") {
		t.Fatalf("want the observed-kind check to fail, got %v", err)
	}
}
