//go:build e2e

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/reliant-labs/forge/internal/k3dkmsg"
	"github.com/reliant-labs/forge/internal/mlcollect"
	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// fakeGPUOperatorChart is run-ai/fake-gpu-operator's chart: it advertises
// nvidia.com/gpu on CPU nodes labelled run.ai/simulated-gpu-node-pool, with
// GFD-style labels, and runs a DCGM-shaped exporter on :9400 (DaemonSet
// gpu-operator/nvidia-dcgm-exporter, image .../status-exporter).
const (
	fakeGPUOperatorChart   = "oci://ghcr.io/run-ai/fake-gpu-operator/fake-gpu-operator"
	fakeGPUOperatorVersion = "0.2.1"
)

// TestE2EMLDiscoverAgainstFakeGPUs runs `forge ml discover` against a REAL
// throwaway k3d cluster whose one node has fake NVIDIA GPUs from
// run-ai/fake-gpu-operator, and proves:
//
//   - the read: an nvidia accelerator group with capacity, and the fake's
//     DCGM-shaped exporter found as dcgm-exporter;
//   - the proposal: an ml.k declaring a forge.MLStack;
//   - the read set is sufficient AND is the whole story: run as a
//     ServiceAccount granted exactly mlcollect.ClusterRole() (list only, no
//     Secrets), discovery reads everything with no refused list;
//   - a second DCGM exporter on the same nodes raises accel-exporter-duplicate.
//
// THE e2e TIER: it needs docker, k3d, kubectl and helm, and pulls the
// fake-gpu-operator chart and images from ghcr.io. A missing tool is a named
// skip on a laptop and a hard failure with FORGE_E2E_REQUIRE_TOOLS=1:
//
//	FORGE_E2E_REQUIRE_TOOLS=1 go test -tags e2e -count=1 -timeout 30m -run TestE2EMLDiscover ./internal/cli/
//
// The cluster is named uniquely and deleted by that exact name; no other
// cluster is read or touched, and the developer's kubeconfig is never
// modified.
func TestE2EMLDiscoverAgainstFakeGPUs(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a real k3d cluster")
	}
	requireTool(t, "k3d", "kubectl", "helm", "docker")
	requireDockerDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	name := fmt.Sprintf("fmld-%d", time.Now().UnixNano()%1_000_000_000)
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	kctx := "k3d-" + name
	node := "k3d-" + name + "-server-0"

	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		lockErr := withKubeconfigLock(func() error {
			out, err := exec.CommandContext(cleanup, "k3d", "cluster", "delete", name).CombinedOutput()
			t.Logf("cleanup: k3d cluster delete %s: err=%v\n%s", name, err, out)
			return err
		})
		if lockErr != nil {
			t.Errorf("cleanup of k3d cluster %s did not complete (delete it by hand: k3d cluster delete %s): %v", name, name, lockErr)
		}
		// k3d reports success even when docker failed to kill a node
		// container; remove what is left of THIS cluster, by its label.
		// docker can fail the first kill ("did not receive an exit event"),
		// so retry a few times.
		for attempt := 0; attempt < 4; attempt++ {
			ids, _ := exec.CommandContext(cleanup, "docker", "ps", "-aq", "--filter", "label=k3d.cluster="+name).Output()
			left := strings.Fields(string(ids))
			if len(left) == 0 {
				break
			}
			out, err := exec.CommandContext(cleanup, "docker", append([]string{"rm", "-fv"}, left...)...).CombinedOutput()
			t.Logf("cleanup: docker rm -fv (k3d left %d containers of %s): err=%v %s", len(left), name, err, out)
			time.Sleep(5 * time.Second)
		}
		_ = exec.CommandContext(cleanup, "docker", "volume", "rm", "k3d-"+name+"-images").Run()
		_ = exec.CommandContext(cleanup, "docker", "network", "rm", "k3d-"+name).Run()
	})

	createCtx, cancelCreate := context.WithTimeout(ctx, 150*time.Second)
	defer cancelCreate()
	createArgs, createErr := k3dkmsg.AddArgs([]string{"cluster", "create", name,
		"--servers", "1", "--agents", "0", "--no-lb",
		"--wait", "--timeout", "120s",
		// A shared dev box's disk is often past kubelet's default eviction
		// threshold, which evicts the test's pods for DiskPressure; this
		// test measures discovery, not the host's disk.
		"--k3s-arg", "--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%@server:*",
		"--k3s-arg", "--kubelet-arg=eviction-minimum-reclaim=imagefs.available=1%,nodefs.available=1%@server:*",
		"--kubeconfig-update-default=false", "--kubeconfig-switch-context=false"})
	if createErr == nil {
		createErr = exec.CommandContext(createCtx, "k3d", createArgs...).Run()
	}
	if createErr == nil {
		createErr = exec.CommandContext(createCtx, "k3d", "kubeconfig", "write", name,
			"--output", kubeconfig, "--overwrite", "--kubeconfig-switch-context=false").Run()
	}
	if createErr == nil {
		createErr = exec.CommandContext(createCtx, "kubectl", "--kubeconfig", kubeconfig, "--context", kctx,
			"--request-timeout=20s", "get", "--raw", "/readyz").Run()
	}
	if createErr != nil {
		if e2eToolsRequired() {
			t.Fatalf("a k3d cluster did not come up with a reachable API server (%v)", createErr)
		}
		t.Skipf("skipped: a k3d cluster did not come up with a reachable API server (%v); see internal/k3dkmsg", createErr)
	}
	// Every kubectl, helm and the discover read below go through this file.
	t.Setenv("KUBECONFIG", kubeconfig)
	kubectl := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "kubectl", append([]string{"--context", kctx}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	// fake-gpu-operator, per its README: label the node into a pool, a
	// privileged namespace, the chart.
	kubectl("label", "node", node, "run.ai/simulated-gpu-node-pool=default")
	kubectl("create", "namespace", "gpu-operator")
	kubectl("label", "namespace", "gpu-operator", "pod-security.kubernetes.io/enforce=privileged")
	runCluster(ctx, t, nil, "helm", "upgrade", "-i", "gpu-operator", fakeGPUOperatorChart,
		"--kube-context", kctx, "--namespace", "gpu-operator", "--version", fakeGPUOperatorVersion,
		"--wait", "--timeout", "8m")
	gpus := mlWaitFor(ctx, t, 5*time.Minute, "nvidia.com/gpu capacity on "+node, func() (string, bool) {
		out, _ := exec.CommandContext(ctx, "kubectl", "--context", kctx, "get", "node", node,
			"-o", `jsonpath={.status.capacity.nvidia\.com/gpu}`).Output()
		n, err := strconv.Atoi(strings.TrimSpace(string(out)))
		return string(out), err == nil && n > 0
	})
	t.Logf("node %s advertises nvidia.com/gpu=%s", node, gpus)
	runCluster(ctx, t, nil, "kubectl", "--context", kctx, "-n", "gpu-operator", "rollout", "status",
		"daemonset/nvidia-dcgm-exporter", "--timeout", "5m")

	// ── the read, as the developer ──
	doc := mlDiscoverLive(ctx, t, kctx)
	var nvidia *mldiscover.AcceleratorGroup
	for i, g := range doc.Accelerators {
		if g.Vendor == mldiscover.VendorNVIDIA && g.Resource == "nvidia.com/gpu" && g.Capacity > 0 {
			nvidia = &doc.Accelerators[i]
		}
	}
	if nvidia == nil {
		t.Errorf("no nvidia accelerator group with capacity: %+v", doc.Accelerators)
	} else {
		t.Logf("accelerators: %+v", *nvidia)
	}
	if !doc.Inventory.Has(mldiscover.KindDCGMExporter) {
		t.Errorf("fake-gpu-operator's exporter (DaemonSet gpu-operator/nvidia-dcgm-exporter, image "+
			"ghcr.io/run-ai/fake-gpu-operator/status-exporter, :9400) was not found as %s; components: %+v",
			mldiscover.KindDCGMExporter, doc.Components)
	}
	if !strings.Contains(doc.Proposal.MLK, "forge.MLStack") {
		t.Errorf("the proposal declares no forge.MLStack:\n%s", doc.Proposal.MLK)
	}
	if len(doc.Warnings) != 0 {
		t.Logf("warnings as cluster-admin: %v", doc.Warnings)
	}

	// ── the read set, as an identity granted exactly mlcollect.ClusterRole() ──
	roCtx := mlReadOnlyContext(ctx, t, kctx, kubeconfig)
	ro := mlDiscoverSnapshot(ctx, t, roCtx)
	if len(ro.ReadErrors) != 0 {
		t.Errorf("as an identity granted mlcollect.ClusterRole(), discovery was refused: %+v", ro.ReadErrors)
	}
	if len(ro.Nodes) == 0 || len(ro.DaemonSets) == 0 || len(ro.Pods) == 0 {
		t.Errorf("the read-only identity read %d nodes, %d daemonsets, %d pods", len(ro.Nodes), len(ro.DaemonSets), len(ro.Pods))
	}
	if err := tryCluster(ctx, "kubectl", "--context", roCtx, "get", "secrets", "-A"); err == nil {
		t.Error("the read-set ClusterRole lets its identity read Secrets")
	}

	// ── a second DCGM exporter on the same nodes ──
	plantSecondDCGMExporter(ctx, t, name, kctx)
	doc = mlDiscoverLive(ctx, t, kctx)
	var dup bool
	for _, f := range doc.Findings {
		dup = dup || f.ID == "accel-exporter-duplicate"
	}
	if !dup {
		ids := make([]string, 0, len(doc.Findings))
		for _, f := range doc.Findings {
			ids = append(ids, f.ID)
		}
		t.Errorf("two DCGM exporters on %s did not raise accel-exporter-duplicate; findings %v; components %+v", node, ids, doc.Components)
	}
}

// mlDiscoverLive runs `forge ml discover --context kctx --json` outside any
// project and returns the document.
func mlDiscoverLive(ctx context.Context, t *testing.T, kctx string) MLDiscoverDocument {
	t.Helper()
	var out bytes.Buffer
	if err := runMLDiscover(ctx, mlDiscoverOptions{kubeContext: kctx, jsonOut: true, namespace: "forge-ml", stdout: &out}); err != nil {
		t.Fatalf("forge ml discover --context %s: %v", kctx, err)
	}
	var doc MLDiscoverDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("not a document: %v\n%s", err, out.String())
	}
	return doc
}

func mlDiscoverSnapshot(ctx context.Context, t *testing.T, kctx string) *mldiscover.Snapshot {
	t.Helper()
	s, err := mlCollect(ctx, kctx)
	if err != nil {
		t.Fatalf("collect through %s: %v", kctx, err)
	}
	return s
}

// mlReadOnlyContext creates a ServiceAccount bound to exactly
// mlcollect.ClusterRole(), and a kube context in kubeconfig that
// authenticates as it.
func mlReadOnlyContext(ctx context.Context, t *testing.T, kctx, kubeconfig string) string {
	t.Helper()
	const ns, sa = "default", "forge-ml-discover"
	role := rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: sa},
		Rules:      mlcollect.ClusterRole(),
	}
	binding := rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: sa},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: sa},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa, Namespace: ns}},
	}
	var manifest bytes.Buffer
	for _, obj := range []any{role, binding} {
		b, err := yaml.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Write(b)
		manifest.WriteString("---\n")
	}
	runCluster(ctx, t, nil, "kubectl", "--context", kctx, "-n", ns, "create", "serviceaccount", sa)
	apply := exec.CommandContext(ctx, "kubectl", "--context", kctx, "apply", "-f", "-")
	apply.Stdin = &manifest
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("apply the read-set ClusterRole: %v\n%s", err, out)
	}
	token, err := exec.CommandContext(ctx, "kubectl", "--context", kctx, "-n", ns, "create", "token", sa, "--duration", "1h").Output()
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	cluster := strings.TrimSpace(mlMustOutput(ctx, t, "kubectl", "config", "view", "--kubeconfig", kubeconfig,
		"-o", `jsonpath={.contexts[?(@.name=="`+kctx+`")].context.cluster}`))
	roCtx := kctx + "-ml-readonly"
	runCluster(ctx, t, nil, "kubectl", "config", "--kubeconfig", kubeconfig, "set-credentials", sa, "--token", strings.TrimSpace(string(token)))
	runCluster(ctx, t, nil, "kubectl", "config", "--kubeconfig", kubeconfig, "set-context", roCtx, "--cluster", cluster, "--user", sa)
	return roCtx
}

// plantSecondDCGMExporter runs a second DaemonSet whose image is named
// *dcgm-exporter*, on the fake's nodes and port. The image is the node's own
// pause image re-tagged inside the node's containerd, so nothing is pulled
// (and pause runs forever with no command).
func plantSecondDCGMExporter(ctx context.Context, t *testing.T, cluster, kctx string) {
	t.Helper()
	const image = "forge-e2e.invalid/monitoring/dcgm-exporter:e2e"
	node := "k3d-" + cluster + "-server-0"
	pause := ""
	for _, ref := range strings.Fields(mlMustOutput(ctx, t, "docker", "exec", node, "ctr", "-n", "k8s.io", "images", "ls", "-q")) {
		if strings.Contains(ref, "pause") && !strings.HasPrefix(ref, "sha256:") {
			pause = ref
			break
		}
	}
	if pause == "" {
		t.Fatalf("node %s has no pause image to re-tag", node)
	}
	runCluster(ctx, t, nil, "docker", "exec", node, "ctr", "-n", "k8s.io", "images", "tag", "--force", pause, image)
	ds := `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: dcgm-exporter
  namespace: monitoring
spec:
  selector:
    matchLabels: {app: dcgm-exporter}
  template:
    metadata:
      labels: {app: dcgm-exporter}
    spec:
      nodeSelector: {nvidia.com/gpu.deploy.dcgm-exporter: "true"}
      containers:
      - name: dcgm-exporter
        image: ` + image + `
        imagePullPolicy: Never
        ports: [{name: metrics, containerPort: 9400}]
`
	runCluster(ctx, t, nil, "kubectl", "--context", kctx, "create", "namespace", "monitoring")
	apply := exec.CommandContext(ctx, "kubectl", "--context", kctx, "apply", "-f", "-")
	apply.Stdin = strings.NewReader(ds)
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("apply the second exporter: %v\n%s", err, out)
	}
	runCluster(ctx, t, nil, "kubectl", "--context", kctx, "-n", "monitoring", "rollout", "status",
		"daemonset/dcgm-exporter", "--timeout", "3m")
}

func mlMustOutput(ctx context.Context, t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		t.Fatalf("%s %s: %v", name, strings.Join(args, " "), err)
	}
	return string(out)
}

// mlWaitFor polls cond every 3s until it holds or budget passes.
func mlWaitFor(ctx context.Context, t *testing.T, budget time.Duration, what string, cond func() (string, bool)) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		last, ok := cond()
		if ok {
			return last
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("timed out after %s waiting for %s (last: %q)", budget, what, last)
		}
		time.Sleep(3 * time.Second)
	}
}
