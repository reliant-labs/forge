package mlrecs

import (
	"bytes"
	"encoding/json"
	"flag"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// update rewrites testdata/*.findings.golden.json and *.ml.k.golden:
//
//	go test ./pkg/mlrecs -run TestGolden -update
var update = flag.Bool("update", false, "rewrite testdata/*.golden from Evaluate's and Propose's output")

// The snapshots are mldiscover's recorded fixtures.
var fixtures = []string{
	"gke-gpu-operator",
	"gke-dcgm-duplicate",
	"eks-spot-training",
	"bare-fake-gpu",
	"mixed-vendors",
	"no-accelerators",
	"kubeadm-traced",
	"aks-operator-custom-counters",
	"probed-forge-stack",
}

func loadSnapshot(t testing.TB, name string) *mldiscover.Snapshot {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "mldiscover", "testdata", name+".snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s mldiscover.Snapshot
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return &s
}

// fixtureDecisions are the decisions the golden run uses: the mixed-vendor
// lab dismisses kubelet tracing until well after the snapshot.
func fixtureDecisions(name string) []Decision {
	if name == "mixed-vendors" {
		return []Decision{{ID: "kubelet-tracing-off", Decision: "dismiss", Reason: "lab nodes export kubelet traces already", Expires: "2027-06-01"}}
	}
	return nil
}

type result struct {
	findings []Finding
	mlk      string
}

func run(t testing.TB, s *mldiscover.Snapshot, decisions []Decision) result {
	t.Helper()
	inv := mldiscover.Discover(s)
	popts := ProposeOptions{Decisions: decisions}
	fs := Evaluate(s, inv, Options{Decisions: decisions, Propose: popts})
	return result{fs, Propose(inv, fs, popts)}
}

func marshal(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s (run with -update to create it): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs; run `go test ./pkg/mlrecs -run TestGolden -update` and review the diff.\n--- got ---\n%s", path, got)
	}
}

func TestGolden(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			r := run(t, loadSnapshot(t, name), fixtureDecisions(name))
			golden(t, filepath.Join("testdata", name+".findings.golden.json"), marshal(t, r.findings))
			golden(t, filepath.Join("testdata", name+".ml.k.golden"), []byte(r.mlk))
		})
	}
}

func shuffle(s *mldiscover.Snapshot, seed int64) {
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(s.Nodes), func(i, j int) { s.Nodes[i], s.Nodes[j] = s.Nodes[j], s.Nodes[i] })
	r.Shuffle(len(s.Pods), func(i, j int) { s.Pods[i], s.Pods[j] = s.Pods[j], s.Pods[i] })
	r.Shuffle(len(s.DaemonSets), func(i, j int) { s.DaemonSets[i], s.DaemonSets[j] = s.DaemonSets[j], s.DaemonSets[i] })
	r.Shuffle(len(s.Deployments), func(i, j int) { s.Deployments[i], s.Deployments[j] = s.Deployments[j], s.Deployments[i] })
	r.Shuffle(len(s.Jobs), func(i, j int) { s.Jobs[i], s.Jobs[j] = s.Jobs[j], s.Jobs[i] })
	r.Shuffle(len(s.HPAs), func(i, j int) { s.HPAs[i], s.HPAs[j] = s.HPAs[j], s.HPAs[i] })
	r.Shuffle(len(s.CRDs), func(i, j int) { s.CRDs[i], s.CRDs[j] = s.CRDs[j], s.CRDs[i] })
	r.Shuffle(len(s.ConfigMaps), func(i, j int) { s.ConfigMaps[i], s.ConfigMaps[j] = s.ConfigMaps[j], s.ConfigMaps[i] })
	r.Shuffle(len(s.Probes), func(i, j int) { s.Probes[i], s.Probes[j] = s.Probes[j], s.Probes[i] })
	for i := range s.Nodes {
		img := s.Nodes[i].Status.Images
		r.Shuffle(len(img), func(a, b int) { img[a], img[b] = img[b], img[a] })
	}
}

func TestEvaluateAndProposeAreDeterministic(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			want := run(t, loadSnapshot(t, name), fixtureDecisions(name))
			wantJSON := marshal(t, want.findings)
			for seed := int64(1); seed <= 5; seed++ {
				s := loadSnapshot(t, name)
				shuffle(s, seed)
				got := run(t, s, fixtureDecisions(name))
				if !bytes.Equal(marshal(t, got.findings), wantJSON) {
					t.Fatalf("seed %d: findings changed with input order", seed)
				}
				if got.mlk != want.mlk {
					t.Fatalf("seed %d: ml.k changed with input order", seed)
				}
			}
		})
	}
}

// TestProposeSurvivesJSON: the CLI prints findings as JSON and may propose
// from findings read back; the file must not change.
func TestProposeSurvivesJSON(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			s := loadSnapshot(t, name)
			inv := mldiscover.Discover(s)
			fs := Evaluate(s, inv, Options{})
			var back []Finding
			if err := json.Unmarshal(marshal(t, fs), &back); err != nil {
				t.Fatal(err)
			}
			if a, b := Propose(inv, fs, ProposeOptions{}), Propose(inv, back, ProposeOptions{}); a != b {
				t.Fatalf("Propose differs after a JSON round trip:\n%s\n---\n%s", a, b)
			}
		})
	}
}
