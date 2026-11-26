package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sversion "k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"

	"github.com/reliant-labs/forge/internal/exitcode"
	"github.com/reliant-labs/forge/internal/mlcollect"
	"github.com/reliant-labs/forge/internal/schemagen"
	"github.com/reliant-labs/forge/pkg/mldiscover"
	"github.com/reliant-labs/forge/pkg/mlrecs"
)

const mlSnapshotFixture = "testdata/ml/snapshot.json"

// mlRun runs `forge ml discover` with o and returns stdout.
func mlRun(t *testing.T, o mlDiscoverOptions) (string, error) {
	t.Helper()
	var out bytes.Buffer
	o.stdout = &out
	if o.namespace == "" {
		o.namespace = "forge-ml"
	}
	err := runMLDiscover(context.Background(), o)
	return out.String(), err
}

func mlFixedSnapshot(kubeContext string) func(context.Context, string) (*mldiscover.Snapshot, error) {
	return func(context.Context, string) (*mldiscover.Snapshot, error) {
		return &mldiscover.Snapshot{Schema: mldiscover.SnapshotSchema, Context: kubeContext,
			CollectedAt: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)}, nil
	}
}

func mlFixedProposal(body string) func(*mldiscover.Inventory, []mlrecs.Finding, mlrecs.ProposeOptions) string {
	return func(*mldiscover.Inventory, []mlrecs.Finding, mlrecs.ProposeOptions) string { return body }
}

// mlGolden compares got with testdata/ml/<name>; FORGE_UPDATE_GOLDEN=1
// rewrites it.
func mlGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "ml", name)
	if os.Getenv("FORGE_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (FORGE_UPDATE_GOLDEN=1 writes it): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("%s is stale (FORGE_UPDATE_GOLDEN=1 go test ./internal/cli -run 'TestMLDiscover' rewrites it):\n%s",
			path, mlUnifiedDiff(string(want), got, path))
	}
}

// TestMLDiscoverSchemaIsCurrent regenerates the ml-discover schema from
// MLDiscoverDocument and fails when the committed file differs.
func TestMLDiscoverSchemaIsCurrent(t *testing.T) {
	got, err := generateMLDiscoverSchema(newSourceDescriber(t, "../.."))
	if err != nil {
		t.Fatal(err)
	}
	const path = "../../schema/ml-discover.v1.schema.json"
	if os.Getenv("FORGE_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (FORGE_UPDATE_GOLDEN=1 writes it): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("schema/ml-discover.v1.schema.json is stale: MLDiscoverDocument (or mldiscover.Inventory, mlrecs.Finding) changed.\n" +
			"Regenerate it: FORGE_UPDATE_GOLDEN=1 go test ./internal/cli -run TestMLDiscoverSchemaIsCurrent")
	}
	if !bytes.Equal(want, mlDiscoverSchemaJSON()) {
		t.Errorf("the embedded schema (schema.MLDiscoverV1) is not the committed file")
	}
	var listed bool
	for _, s := range buildCapabilitiesSpec().Schemas {
		listed = listed || (s.Name == "ml-discover" && s.APIVersion == MLDiscoverDocumentSchema)
	}
	if !listed {
		t.Error("forge project capabilities does not list the ml-discover schema")
	}
}

// The document a snapshot produces, as JSON and as text, is pinned, and the
// JSON validates against the published schema.
func TestMLDiscoverFromSnapshot(t *testing.T) {
	t.Parallel()
	out, err := mlRun(t, mlDiscoverOptions{fromSnapshot: mlSnapshotFixture, jsonOut: true})
	if err != nil {
		t.Fatal(err)
	}
	var doc MLDiscoverDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	if doc.Schema != MLDiscoverDocumentSchema || doc.Context != "gke_acme_us-central1_gpu" || doc.ServerVersion != "v1.31.2-gke.1000" {
		t.Errorf("header = %q %q %q", doc.Schema, doc.Context, doc.ServerVersion)
	}
	if doc.Proposal.Written || doc.Proposal.Path != "" || doc.Snapshot != "" {
		t.Errorf("a read with no --env, --out or --write placed or wrote something: %+v snapshot=%q", doc.Proposal, doc.Snapshot)
	}
	compiled, err := schemagen.Compile(mlDiscoverSchemaJSON())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := schemagen.DecodeJSON([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if errs := compiled.Validate(decoded); len(errs) > 0 {
		t.Errorf("the document does not validate against its schema: %v", errs)
	}
	mlGolden(t, "discover.golden.json", out)

	text, err := mlRun(t, mlDiscoverOptions{fromSnapshot: mlSnapshotFixture})
	if err != nil {
		t.Fatal(err)
	}
	mlGolden(t, "discover.golden.txt", text)
}

// The human form, over a hand-built document: every section, a CRD-only
// component, a forge-owned one, findings worst first with a dismissed one
// last, and a proposal that differs from the file on disk.
func TestMLDiscoverHumanForm(t *testing.T) {
	t.Parallel()
	doc := MLDiscoverDocument{
		Schema: MLDiscoverDocumentSchema,
		Inventory: mldiscover.Inventory{
			Context: "gke_acme_us-central1_gpu", ServerVersion: "v1.31.2-gke.1000", Cloud: mldiscover.CloudGKE,
			Accelerators: []mldiscover.AcceleratorGroup{
				{Vendor: "nvidia", Resource: "nvidia.com/gpu", Product: "NVIDIA-H100-80GB-HBM3", Pool: "h100", Nodes: 3, Capacity: 24, Allocatable: 23, Allocated: 16, DriverVersions: []string{"550.127.05"}, DetectedBy: "extended-resource"},
				{Vendor: "nvidia", Resource: "nvidia.com/gpu", Product: "NVIDIA-L4", Pool: "l4", Nodes: 1, Capacity: 0, DetectedBy: "label"},
			},
			Components: []mldiscover.Component{
				{Kind: "gpu-operator", Category: "operator", Vendor: "nvidia", Status: "present", Owner: "theirs", Version: "v24.9.0", Namespace: "gpu-operator", Workload: "Deployment/gpu-operator", MatchedBy: []string{"image:*gpu-operator*"}},
				{Kind: "dcgm-exporter", Category: "accel-metrics", Vendor: "nvidia", Status: "present", Owner: "theirs", Version: "3.3.9-3.6.1-ubuntu22.04", Namespace: "gpu-operator", Workload: "DaemonSet/nvidia-dcgm-exporter", Via: "gpu-operator", MetricsPort: 9400, Pods: 3, MatchedBy: []string{"image:*dcgm-exporter*"},
					Config: &mldiscover.ComponentConfig{DeterminedBy: "probe", Source: "probe :9400/metrics", Fields: []string{"DCGM_FI_DEV_FB_FREE", "DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_GPU_UTIL"}, Settings: []string{"kubernetes-attribution=true"}}},
				{Kind: "node-problem-detector", Category: "node-health", Status: "present", Owner: "forge", Version: "v0.8.20", Namespace: "forge-ml", Workload: "DaemonSet/node-problem-detector", Pods: 4, MatchedBy: []string{"image:*node-problem-detector*"}},
				{Kind: "kueue", Category: "queue", Status: "present", Owner: "theirs", Version: "v0.10.1", Namespace: "kueue-system", Workload: "Deployment/kueue-controller-manager", Resources: 2, MatchedBy: []string{"crd:clusterqueues.kueue.x-k8s.io"}},
				{Kind: "kuberay", Category: "training", Status: "crd-only", Owner: "theirs", Resources: 1, MatchedBy: []string{"crd:rayclusters.ray.io"}},
			},
			Coverage: []mldiscover.SignalCoverage{
				{Signal: "accel.utilisation", Category: "accelerator", Vendor: "nvidia", Status: "collected", DeterminedBy: "probe",
					Sources: []string{"dcgm-exporter/gpu-operator/DaemonSet/nvidia-dcgm-exporter"}, Expected: []string{"DCGM_FI_DEV_GPU_UTIL"}, Present: []string{"DCGM_FI_DEV_GPU_UTIL"},
					NodesCovered: 3, NodesTotal: 3, Versions: []string{"dcgm-exporter 3.3.9-3.6.1-ubuntu22.04"}},
				{Signal: "accel.profiling", Category: "accelerator", Vendor: "nvidia", Status: "missing", DeterminedBy: "probe",
					Expected:   []string{"DCGM_FI_PROF_DRAM_ACTIVE", "DCGM_FI_PROF_GR_ENGINE_ACTIVE", "DCGM_FI_PROF_PIPE_TENSOR_ACTIVE", "DCGM_FI_PROF_SM_ACTIVE", "DCGM_FI_PROF_SM_OCCUPANCY"},
					Missing:    []string{"DCGM_FI_PROF_DRAM_ACTIVE", "DCGM_FI_PROF_GR_ENGINE_ACTIVE", "DCGM_FI_PROF_PIPE_TENSOR_ACTIVE", "DCGM_FI_PROF_SM_ACTIVE", "DCGM_FI_PROF_SM_OCCUPANCY"},
					NodesTotal: 3, Findings: []string{"dcgm-profiling-off"}},
				{Signal: "node.health", Category: "node-health", Status: "partial", DeterminedBy: "config", Sources: []string{"node-problem-detector/forge-ml/DaemonSet/node-problem-detector"}, NodesCovered: 3, NodesTotal: 4},
				{Signal: "kubelet.tracing", Category: "tracing", Status: "unknown", DeterminedBy: "unreadable", Note: "GKE manages the kubelet config: discovery cannot read it.", Findings: []string{"kubelet-tracing-off"}},
				{Signal: "metrics.inference", Category: "inference", Status: "not-applicable", DeterminedBy: "presence"},
			},
		},
		Findings: []mlrecs.Finding{
			{ID: "accel-exporter-duplicate", Severity: "high", Confidence: "certain", Title: "2 DCGM exporters run on 3 nodes", Vendor: "nvidia",
				Evidence: []string{"DaemonSet/gpu-operator/nvidia-dcgm-exporter", "DaemonSet/monitoring/dcgm-exporter"},
				Fix:      mlrecs.Fix{Kind: "manual", Summary: "Remove the second exporter; two break each other's profiling counters."}},
			{ID: "kubelet-tracing-off", Severity: "low", Confidence: "unverified", Title: "kubelet tracing may be off",
				Evidence:  []string{"kubelet config is not readable through the API"},
				Fix:       mlrecs.Fix{Kind: "command", Summary: "Enable kubelet tracing on the node pool.", Command: "gcloud container node-pools update h100 --cluster gpu --system-config-from-file=kubelet.yaml"},
				Dismissed: &mlrecs.Dismissal{Reason: "GKE does not expose kubelet tracing config", Expires: "2027-06-01"}},
		},
		Proposal: MLDiscoverProposal{Path: "deploy/kcl/prod/ml.k", Exists: true, MLK: "import forge\n",
			Diff: "--- a/deploy/kcl/prod/ml.k\n+++ b/deploy/kcl/prod/ml.k\n@@ -1 +1 @@\n-import  forge\n+import forge\n"},
		Warnings: []string{"could not read jobs.batch: forbidden"},
	}
	var out bytes.Buffer
	if err := writeMLDiscoverHuman(&out, doc, mlDiscoverOptions{env: "prod"}); err != nil {
		t.Fatal(err)
	}
	mlGolden(t, "human.golden.txt", out.String())
}

// Through the real root command: `forge ml discover --json` prints the
// document and nothing else.
func TestMLDiscoverThroughTheRootCommand(t *testing.T) {
	out, _ := runRoot(t, "ml", "discover", "--from-snapshot", mlSnapshotFixture, "--json")
	var doc map[string]any
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if dec.More() {
		t.Errorf("stdout carries more than the document:\n%s", out)
	}
	if doc["schema"] != MLDiscoverDocumentSchema {
		t.Errorf("schema = %v", doc["schema"])
	}
}

// --write writes the proposal; a re-run writes nothing; an edited file gets
// the diff printed, then the write. Without --write nothing is ever written.
func TestMLDiscoverWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy", "kcl", "prod", "ml.k")
	base := mlDiscoverOptions{projectDir: dir, env: "prod", collect: mlFixedSnapshot("k3d-gpu"),
		render:  func(context.Context, string, string) ([]byte, error) { return nil, errors.New("no main.k") },
		propose: mlFixedProposal("import forge\n\nml_stacks = []\n")}

	// No --write: printed, not written.
	out, err := mlRun(t, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ml.k exists without --write: %v", err)
	}
	if !strings.Contains(out, "not written: run with --write") || !strings.Contains(out, "ml_stacks = []") {
		t.Errorf("the proposal was not printed:\n%s", out)
	}

	w := base
	w.write, w.jsonOut = true, true
	out, err = mlRun(t, w)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "import forge\n\nml_stacks = []\n" {
		t.Fatalf("ml.k = %q", b)
	}
	var doc MLDiscoverDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Proposal.Written || doc.Proposal.Exists {
		t.Errorf("first write: %+v", doc.Proposal)
	}

	// Unchanged: nothing written.
	out, err = mlRun(t, w)
	if err != nil {
		t.Fatal(err)
	}
	doc = MLDiscoverDocument{}
	_ = json.Unmarshal([]byte(out), &doc)
	if doc.Proposal.Written || !doc.Proposal.Exists || doc.Proposal.Diff != "" {
		t.Errorf("an unchanged re-run: %+v", doc.Proposal)
	}

	// The user's edit is shown as a diff, then overwritten (it is in git).
	if err := os.WriteFile(path, []byte("import forge\n\nml_stacks = [] # mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := base
	text.write = true
	out, err = mlRun(t, text)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "over the existing file") || !strings.Contains(out, "-ml_stacks = [] # mine") || !strings.Contains(out, "+ml_stacks = []") {
		t.Errorf("an overwrite did not print the diff:\n%s", out)
	}
	if b, _ := os.ReadFile(path); string(b) != "import forge\n\nml_stacks = []\n" {
		t.Errorf("ml.k after overwrite = %q", b)
	}

	// --out places it anywhere.
	o := base
	o.out, o.write = filepath.Join(dir, "elsewhere", "ml.k"), true
	if _, err := mlRun(t, o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(o.out); err != nil {
		t.Errorf("--out: %v", err)
	}
}

// TestMLDiscoverWriteKeepsDecisionsAndDiffsApply runs the real Evaluate and
// Propose with --env --write --namespace and a connected cluster: the
// written ml.k keeps the env's RecommendationDecisions, and every
// finding's kcl_diff applies to the file written.
func TestMLDiscoverWriteKeepsDecisionsAndDiffsApply(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "deploy", "kcl", "prod"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy", "kcl", "prod", "main.k"), []byte("import forge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	render := `{"output": {"connected_clusters": {"lab-p4d": "lab-prod"}, "ml_stacks": [
		{"cluster": {"cluster": "lab-p4d", "namespace": "gpu-observability"}, "decisions": [
			{"id": "dcgm-nvlink-off", "decision": "dismiss", "reason": "single-node lab", "expires": "2027-06-01"}]}]}}`
	out, err := mlRun(t, mlDiscoverOptions{projectDir: dir, env: "prod", write: true, jsonOut: true, namespace: "gpu-observability",
		fromSnapshot: filepath.Join("..", "..", "pkg", "mldiscover", "testdata", "probed-forge-stack.snapshot.json"),
		render:       func(context.Context, string, string) ([]byte, error) { return []byte(render), nil }})
	if err != nil {
		t.Fatal(err)
	}
	var doc MLDiscoverDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(dir, "deploy", "kcl", "prod", "ml.k"))
	if err != nil {
		t.Fatal(err)
	}
	mlk := string(written)
	for _, want := range []string{`id = "dcgm-nvlink-off"`, `connected_cluster = "lab-prod"`, `namespace = "gpu-observability"`} {
		if !strings.Contains(mlk, want) {
			t.Errorf("the written ml.k lacks %s:\n%s", want, mlk)
		}
	}
	diffs := 0
	for _, f := range doc.Findings {
		if f.ID == "dcgm-nvlink-off" && f.Dismissed == nil {
			t.Errorf("dcgm-nvlink-off is not dismissed by the env's decision")
		}
		if f.Fix.Kind != mlrecs.FixKCL || f.Dismissed != nil {
			continue
		}
		if f.Fix.KCLDiff == "" {
			t.Errorf("%s: fix.kind=kcl and no kcl_diff", f.ID)
			continue
		}
		diffs++
		if _, err := mlApplyUnifiedDiff(mlk, f.Fix.KCLDiff); err != nil {
			t.Errorf("%s: kcl_diff does not apply to the written ml.k: %v", f.ID, err)
		}
	}
	if diffs == 0 {
		t.Fatal("no actionable finding to check: the fixture no longer exercises kcl_diff")
	}
}

// TestMLDiscoverSavedSnapshotHoldsNoCredentials: a live read through the
// real collector, saved inside a project and with --save-snapshot, keeps no
// credential a pod's args or probe headers carried, and both files are
// readable by their owner only, a file an older release left 0644 included.
func TestMLDiscoverSavedSnapshotHoldsNoCredentials(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "serving", Name: "vllm-0"}, Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "vllm", Image: "vllm/vllm-openai:v0.6",
			Args: []string{"--model=meta-llama/Llama-3-8B", "--api-key=cli-arg-secret", "--hf-token", "cli-next-secret"},
			LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/health", HTTPHeaders: []corev1.HTTPHeader{{Name: "Authorization", Value: "Bearer cli-probe-secret"}}}}},
		}}}}
	kube := kubefake.NewSimpleClientset(pod)
	disco := kube.Discovery().(*fakediscovery.FakeDiscovery)
	disco.FakedServerVersion = &k8sversion.Info{GitVersion: "v1.33.1"}
	disco.Resources = []*metav1.APIResourceList{{GroupVersion: "v1"}}
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	clients := mlcollect.Clients{Kube: kube, Metadata: metadatafake.NewSimpleMetadataClient(scheme),
		Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())}
	collect := func(ctx context.Context, kubeContext string) (*mldiscover.Snapshot, error) {
		return mlcollect.Collect(ctx, clients, mlcollect.Options{Context: kubeContext,
			Now: func() time.Time { return time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC) }})
	}

	dir := t.TempDir()
	state := mlStateFile(dir, "k3d-gpu")
	if err := os.MkdirAll(filepath.Dir(state), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(dir, "shared", "snapshot.json")
	if _, err := mlRun(t, mlDiscoverOptions{projectDir: dir, kubeContext: "k3d-gpu", jsonOut: true, saveSnapshot: saved, collect: collect}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{state, saved} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, want 0600", path, fi.Mode().Perm())
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"cli-arg-secret", "cli-next-secret", "cli-probe-secret"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("%s carries %q", path, secret)
			}
		}
		if !strings.Contains(string(b), "--model=meta-llama/Llama-3-8B") {
			t.Errorf("%s lost the args discovery reads", path)
		}
	}
}

// mlApplyUnifiedDiff applies a unified diff to base, failing on any hunk
// whose context or removed lines are not in base where the hunk says.
func mlApplyUnifiedDiff(base, diff string) (string, error) {
	src := strings.SplitAfter(base, "\n")
	if src[len(src)-1] == "" {
		src = src[:len(src)-1]
	}
	lines := strings.SplitAfter(diff, "\n")
	var out []string
	next := 0
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") || l == "" {
			continue
		}
		var start, n int
		if _, err := fmt.Sscanf(l, "@@ -%d,%d", &start, &n); err != nil {
			return "", fmt.Errorf("line %d: expected a hunk header, got %q", i, l)
		}
		at := start - 1
		if n == 0 {
			at = start
		}
		if at < next {
			return "", fmt.Errorf("hunk %q overlaps the previous one", l)
		}
		out = append(out, src[next:at]...)
		next = at
		for i+1 < len(lines) && lines[i+1] != "" && !strings.HasPrefix(lines[i+1], "@@ ") {
			i++
			h, body := lines[i][0], lines[i][1:]
			switch h {
			case ' ', '-':
				if next >= len(src) || src[next] != body {
					return "", fmt.Errorf("hunk %q: base line %d does not match %q", l, next+1, body)
				}
				if h == ' ' {
					out = append(out, body)
				}
				next++
			case '+':
				out = append(out, body)
			default:
				return "", fmt.Errorf("hunk %q: unexpected line %q", l, lines[i])
			}
		}
	}
	return strings.Join(append(out, src[next:]...), ""), nil
}

func TestMLDiscoverRefusals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"schema":"forge.dev/other/v1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		o    mlDiscoverOptions
		want string
	}{
		{"write outside a project", mlDiscoverOptions{write: true, collect: mlFixedSnapshot("c")}, "--write outside a forge project needs --out"},
		{"write without a path", mlDiscoverOptions{write: true, projectDir: dir, collect: mlFixedSnapshot("c")}, "--write needs --env"},
		{"env outside a project", mlDiscoverOptions{env: "prod", collect: mlFixedSnapshot("c")}, "needs a forge project"},
		{"bad env name", mlDiscoverOptions{env: "../x", projectDir: dir}, "not an env name"},
		{"snapshot and context", mlDiscoverOptions{fromSnapshot: mlSnapshotFixture, kubeContext: "x"}, "exclusive"},
		{"not a snapshot", mlDiscoverOptions{fromSnapshot: bad}, `want "forge.dev/ml-snapshot/v1"`},
		{"missing snapshot", mlDiscoverOptions{fromSnapshot: filepath.Join(dir, "nope.json")}, "no such file"},
	} {
		_, err := mlRun(t, tc.o)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
		if err != nil && exitcode.Of(err) != exitWrong {
			t.Errorf("%s: exit %d, want %d (bad input)", tc.name, exitcode.Of(err), exitWrong)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("a refused run wrote into the project: %v", entries)
	}
}

func TestMLDiscoverUnreachableClusterExitsUndetermined(t *testing.T) {
	t.Parallel()
	_, err := mlRun(t, mlDiscoverOptions{collect: func(context.Context, string) (*mldiscover.Snapshot, error) {
		return nil, fmt.Errorf("%w: dial tcp 10.0.0.1:443: i/o timeout", mlcollect.ErrUnreachable)
	}})
	if exitcode.Of(err) != exitUndetermined || !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("err = %v (exit %d), want exit %d", err, exitcode.Of(err), exitUndetermined)
	}
}

// Inside a project every live read is saved for the lint that refuses an
// Install of a component the inventory shows present.
func TestMLDiscoverSavesTheSnapshotInsideAProject(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	extra := filepath.Join(dir, "out", "snap.json")
	out, err := mlRun(t, mlDiscoverOptions{projectDir: dir, jsonOut: true, saveSnapshot: extra,
		collect: mlFixedSnapshot("arn:aws:eks:us-east-1:1:cluster/gpu")})
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, ".forge", "state", "ml-inventory-arn_aws_eks_us-east-1_1_cluster_gpu.json")
	for _, p := range []string{state, extra} {
		s, err := readMLSnapshot(p)
		if err != nil || s.Context != "arn:aws:eks:us-east-1:1:cluster/gpu" {
			t.Errorf("%s: %v %+v", p, err, s)
		}
	}
	var doc MLDiscoverDocument
	_ = json.Unmarshal([]byte(out), &doc)
	if !strings.HasSuffix(doc.Snapshot, "ml-inventory-arn_aws_eks_us-east-1_1_cluster_gpu.json") {
		t.Errorf("snapshot = %q", doc.Snapshot)
	}
	// Outside a project a live read is saved only where --save-snapshot says.
	if _, err := mlRun(t, mlDiscoverOptions{collect: mlFixedSnapshot("c")}); err != nil {
		t.Fatal(err)
	}
}

// --probe: the first inventory names the targets, the probe's results land
// in the snapshot, and discovery runs again over them. It needs a live read.
func TestMLDiscoverProbe(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var gotTargets []mldiscover.ProbeTarget
	target := mldiscover.ProbeTarget{Kind: "dcgm-exporter", Namespace: "gpu-operator", Pod: "nvidia-dcgm-exporter-x", Port: 9400, Path: "/metrics"}
	out, err := mlRun(t, mlDiscoverOptions{projectDir: dir, jsonOut: true, probe: true, collect: mlFixedSnapshot("k3d-gpu"),
		probeTargets: func(s *mldiscover.Snapshot, inv *mldiscover.Inventory) []mldiscover.ProbeTarget {
			if s.Context != "k3d-gpu" || inv == nil {
				t.Errorf("probe targets got %q %v", s.Context, inv)
			}
			return []mldiscover.ProbeTarget{target}
		},
		runProbe: func(_ context.Context, kctx string, ts []mldiscover.ProbeTarget) ([]mldiscover.MetricsProbe, error) {
			if kctx != "k3d-gpu" {
				t.Errorf("probe context %q", kctx)
			}
			gotTargets = ts
			return []mldiscover.MetricsProbe{{Kind: target.Kind, Namespace: target.Namespace, Pod: target.Pod, Port: 9400, Path: "/metrics",
				Families: []string{"DCGM_FI_DEV_GPU_UTIL"}, LabelKeys: []string{"gpu", "pod"}}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotTargets) != 1 || gotTargets[0] != target {
		t.Errorf("probed %v", gotTargets)
	}
	saved, err := readMLSnapshot(filepath.Join(dir, ".forge", "state", "ml-inventory-k3d-gpu.json"))
	if err != nil || len(saved.Probes) != 1 || saved.Probes[0].Families[0] != "DCGM_FI_DEV_GPU_UTIL" {
		t.Errorf("the saved snapshot does not hold the probe: %v %+v", err, saved)
	}
	var doc MLDiscoverDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	// Without --probe nothing is probed.
	if _, err := mlRun(t, mlDiscoverOptions{collect: mlFixedSnapshot("c"),
		runProbe: func(context.Context, string, []mldiscover.ProbeTarget) ([]mldiscover.MetricsProbe, error) {
			t.Error("probed without --probe")
			return nil, nil
		}}); err != nil {
		t.Fatal(err)
	}
	// No targets: a warning, not an error.
	out, err = mlRun(t, mlDiscoverOptions{jsonOut: true, probe: true, collect: mlFixedSnapshot("c"),
		probeTargets: func(*mldiscover.Snapshot, *mldiscover.Inventory) []mldiscover.ProbeTarget { return nil },
		runProbe: func(context.Context, string, []mldiscover.ProbeTarget) ([]mldiscover.MetricsProbe, error) {
			return nil, nil
		}})
	if err != nil || !strings.Contains(out, "no metrics endpoint to probe") {
		t.Errorf("no targets: %v\n%s", err, out)
	}
	if _, err := mlRun(t, mlDiscoverOptions{probe: true, fromSnapshot: mlSnapshotFixture}); err == nil || !strings.Contains(err.Error(), "--probe reads a live cluster") {
		t.Errorf("--probe --from-snapshot: %v", err)
	}
}

func TestSanitizeMLContext(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"gke_acme_us-central1_gpu":            "gke_acme_us-central1_gpu",
		"k3d-dev":                             "k3d-dev",
		"arn:aws:eks:us-east-1:1:cluster/gpu": "arn_aws_eks_us-east-1_1_cluster_gpu",
		"../../etc":                           "etc",
		"":                                    "default",
	} {
		if got := sanitizeMLContext(in); got != want {
			t.Errorf("sanitizeMLContext(%q) = %q, want %q", in, got, want)
		}
	}
}

// With --env, the decisions of the env's ml_stack for this context reach
// Evaluate and its connected cluster reaches Propose; a render that fails, or
// a render with no ml_stacks, degrades to none with a note at most.
func TestMLDiscoverAppliesTheEnvsDecisions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "deploy", "kcl", "prod"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy", "kcl", "prod", "main.k"), []byte("import forge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	render := `{"output": {"connected_clusters": {"gke_acme_gpu": "gpu-prod"}, "ml_stacks": [
		{"cluster": {"cluster": "other"}, "decisions": [{"id": "wrong-stack", "decision": "dismiss", "reason": "r", "expires": "2027-01-01"}]},
		{"cluster": {"cluster": "gke_acme_gpu", "namespace": "forge-ml"}, "decisions": [
			{"id": "kubelet-tracing-off", "decision": "dismiss", "reason": "GKE does not expose it", "expires": "2027-06-01"}]}]}}`
	run := func(render func(context.Context, string, string) ([]byte, error)) ([]mlrecs.Decision, string, []string) {
		var got []mlrecs.Decision
		var cc string
		out, err := mlRun(t, mlDiscoverOptions{projectDir: dir, env: "prod", jsonOut: true, collect: mlFixedSnapshot("gke_acme_gpu"), render: render,
			evaluate: func(_ *mldiscover.Snapshot, _ *mldiscover.Inventory, o mlrecs.Options) []mlrecs.Finding {
				got = o.Decisions
				return nil
			},
			propose: func(_ *mldiscover.Inventory, _ []mlrecs.Finding, o mlrecs.ProposeOptions) string {
				cc = o.ConnectedCluster
				return ""
			}})
		if err != nil {
			t.Fatal(err)
		}
		var doc MLDiscoverDocument
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Findings == nil {
			t.Error("findings is null, want []")
		}
		return got, cc, doc.Warnings
	}

	got, cc, warnings := run(func(context.Context, string, string) ([]byte, error) { return []byte(render), nil })
	if len(got) != 1 || got[0].ID != "kubelet-tracing-off" || got[0].Expires != "2027-06-01" || cc != "gpu-prod" || len(warnings) != 0 {
		t.Errorf("decisions %+v, connected %q, warnings %v", got, cc, warnings)
	}
	got, _, warnings = run(func(context.Context, string, string) ([]byte, error) {
		return []byte(`{"output": {"workloads": []}}`), nil
	})
	if len(got) != 0 || len(warnings) != 0 {
		t.Errorf("an env with no ml_stacks: decisions %v, warnings %v", got, warnings)
	}
	got, _, warnings = run(func(context.Context, string, string) ([]byte, error) { return nil, errors.New("kcl: boom") })
	if len(got) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "did not render") || !strings.Contains(warnings[0], "boom") {
		t.Errorf("a failed render: decisions %v, warnings %v", got, warnings)
	}
}

func TestMLDiscoverRules(t *testing.T) {
	t.Parallel()
	out, err := mlRun(t, mlDiscoverOptions{rules: true, jsonOut: true})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Rules []mlrecs.Rule `json:"rules"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Rules == nil {
		t.Fatalf("--rules --json = %q (%v)", out, err)
	}
	if len(doc.Rules) != len(mlrecs.Rules()) {
		t.Errorf("--rules lists %d rules, mlrecs has %d", len(doc.Rules), len(mlrecs.Rules()))
	}
}

// --help prints the read set from mlcollect's one table.
func TestMLDiscoverHelpPrintsTheReadSet(t *testing.T) {
	t.Parallel()
	help := newMLDiscoverCmd().Long
	for _, r := range mlcollect.ReadSet() {
		if !strings.Contains(help, r.Name()) {
			t.Errorf("--help omits the read of %s", r.Name())
		}
	}
	if !strings.Contains(help, "secrets") {
		t.Error("--help does not say Secrets are never read")
	}
}
