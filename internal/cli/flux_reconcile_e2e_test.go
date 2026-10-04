//go:build e2e

package cli

// END TO END, AGAINST A REAL CLUSTER AND A REAL FLUX: an env with no control
// plane and no declared lifecycle is converged by the reconciler in its own
// cluster, and forge's client-side apply is not what put the workload there.
//
// WHY THIS CANNOT BE A UNIT TEST. The claim the whole path rests on is that
// `Kustomization.status.lastAppliedRevision` equals the bundle's OCI MANIFEST
// digest, even though what was applied came out of ONE LAYER selected by
// `layerSelector`. That is a property of source-controller and
// kustomize-controller, reasoned from their behaviour, and a unit test over
// forge's own structs cannot observe it — it would assert forge's belief about
// Flux rather than Flux. If a future Flux re-keyed the revision off the layer
// digest, every wait on this path would hang on a comparison that can never be
// true, and only a test that reads a REAL Kustomization would notice.
//
// It also proves the negative, which is the other half of the design: the
// workload is Running and `forge` never applied it. A path that wrote a
// pointer AND applied directly would pass every assertion about the workload
// while leaving two authorities over the same objects.
//
// CLUSTER HYGIENE. This creates a cluster under a UNIQUE name, tears it down
// by that exact name, and touches nothing else. A shared dev box runs several
// k3d clusters (`k3d-control-plane-v2`, `k3d-cp-daemon-v2`, …) that other work
// depends on, so there is no `k3d cluster delete --all` here and no
// pattern-matched anything.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"github.com/reliant-labs/forge/internal/bundle"
	"github.com/reliant-labs/forge/internal/flux"
	"io"
)

// TestE2EFluxReconcilesAMachineLedgerEnv is the whole path, once.
func TestE2EFluxReconcilesAMachineLedgerEnv(t *testing.T) {
	requirePublishedForgePkg(t)
	requireTool(t, "k3d", "kubectl", "helm", "docker")
	t.Parallel() // its own uniquely-named cluster, its own t.TempDir

	forgeBin := buildforgeBinary(t)

	// A name nothing else on this machine can be using, so the teardown
	// can be exact. The PID and the clock keep concurrent runs apart.
	clusterName := fmt.Sprintf("forge-flux-e2e-%d-%d", os.Getpid(), time.Now().UnixNano()%100000)
	kctx := "k3d-" + clusterName
	registryName := clusterName + "-registry"
	registryPort := freePortE2E(t)

	// THE LEDGER IS A TEMP DIR. ~/.forge/ledger holds the developer's real
	// promotion history for every project on the machine; a test must never
	// write into it.
	ledgerHome := t.TempDir()

	projectDir := scaffoldFluxE2EProject(t, forgeBin, clusterName, registryName, registryPort)

	// ── The cluster, created by us and deleted by exact name ─────────────
	t.Cleanup(func() {
		// By the exact name, unconditionally, even on failure: a leaked
		// k3d cluster holds a docker network and several containers.
		// Errors are logged rather than failed — the test's verdict is
		// about forge, not about teardown.
		if out, err := exec.Command("k3d", "cluster", "delete", clusterName).CombinedOutput(); err != nil {
			t.Logf("teardown: k3d cluster delete %s: %v\n%s", clusterName, err, out)
		}
		if out, err := exec.Command("k3d", "registry", "delete", "k3d-"+registryName).CombinedOutput(); err != nil {
			t.Logf("teardown: k3d registry delete %s: %v\n%s", registryName, err, out)
		}
	})

	// `forge cluster up` creates the cluster AND installs the reconciler.
	// Both halves are under test: a cluster with no Flux would let the
	// pointer write succeed and the wait time out with nothing to
	// diagnose.
	runForgeFluxE2E(t, projectDir, forgeBin, ledgerHome, "cluster", "up", "dev-k8s", "--wait")

	// ── Flux is actually installed, and only the two controllers ─────────
	assertFluxInstalled(t, kctx)

	// ── Build the bundle, then deploy ────────────────────────────────────
	// The bundle is the artifact the reconciler fetches; the deploy writes
	// a pointer at it rather than applying.
	runForgeFluxE2E(t, projectDir, forgeBin, ledgerHome, "env", "build", "dev-k8s", "--push")

	deployOut := runForgeFluxE2E(t, projectDir, forgeBin, ledgerHome, "env", "deploy", "dev-k8s", "--yes")

	// ── forge's DIRECT APPLY WAS NOT USED ────────────────────────────────
	// The negative half of the design. Two independent signals, because
	// either alone could be coincidence:
	//
	//  1. the deploy said it was recording desired state, not applying;
	//  2. no object in the cluster is owned by forge's apply field
	//     manager — only by Flux's.
	if !strings.Contains(deployOut, "desired state") {
		t.Errorf("deploy output does not say it recorded desired state; did it apply directly?\n%s", deployOut)
	}
	assertWorkloadOwnedByFluxNotForge(t, kctx)

	// ── The workload is Running ──────────────────────────────────────────
	// Flux applied it under `wait: true`, so by the time the deploy
	// returned 0 the rollout had already been health-checked — this
	// re-reads it from the cluster rather than taking forge's word.
	assertDeploymentAvailable(t, kctx, "app", "api")

	// ── lastAppliedRevision == THE BUNDLE MANIFEST DIGEST ────────────────
	// The assertion that cannot be made anywhere else.
	digest := recordedBundleDigest(t, deployOut)
	ksName := flux.KustomizationName("dev-k8s", kctx)
	assertKustomizationConverged(t, kctx, ksName, digest)
	assertLayerSelectorIsSet(t, kctx, flux.SourceName("dev-k8s"), digest)

	// ── A hand-edited Flux-owned field is REVERTED ───────────────────────
	// The self-healing property, which is the reason to have a reconciler
	// at all rather than a one-shot apply.
	assertHandEditIsReverted(t, kctx, ksName, "app", "api")
}

// scaffoldFluxE2EProject creates a project with ONE env, `dev-k8s`, shaped
// like the envs this path serves: a k3d cluster, a registry, no control plane,
// and — the load-bearing part — NO `lifecycle` declaration.
func scaffoldFluxE2EProject(t *testing.T, forgeBin, clusterName, registryName string, registryPort int) string {
	t.Helper()
	dir := t.TempDir()
	runCmd(t, dir, forgeBin, "project", "new", "fluxapp", "--mod", "example.com/fluxapp", "--service", "api")
	projectDir := filepath.Join(dir, "fluxapp")

	// The k3d config: our own cluster name, our own registry, our own
	// ports. Nothing here can collide with another cluster on the box.
	k3dConfig := fmt.Sprintf(`apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: %s
servers: 1
agents: 0
registries:
  use:
    - k3d-%s:5000
  config: |
    mirrors:
      "localhost:%d":
        endpoint:
          - http://k3d-%s:5000
      "host.k3d.internal:%d":
        endpoint:
          - http://k3d-%s:5000
`, clusterName, registryName, registryPort, registryName, registryPort, registryName)
	writeFluxE2EFile(t, filepath.Join(projectDir, "deploy", "k3d-"+clusterName+".yaml"), k3dConfig)

	// The env. NO `lifecycle`, no `forge.ControlPlane` — which is exactly
	// what routes it through the reconciler.
	mainK := fmt.Sprintf(`import forge
import forge.workloads as fw

_cluster = forge.Cluster {
    name = %q
    config = "deploy/k3d-%s.yaml"
}

_target = forge.ClusterTarget {
    cluster = _cluster.context
    namespace = "app"
}

# A three-line busybox httpd answering /readyz and /healthz (the default probes), so forge builds and pushes a real image
# in seconds: the subject is the reconcile path, and a Go build would add
# minutes and a second failure mode.
_api = fw.Workload {
    name = "api"
    kind = "service"
    image = "localhost:%d/fluxapp/api"
    build = forge.DockerBuild {dockerfile = "Dockerfile.pause"}
}

# DELIBERATELY NO lifecycle declaration: an env that declares none and targets
# a cluster is a REAL environment, and forge reconciles it from its bundle.
output = forge.render(forge.Bundle {
    project = "fluxapp"
    env = "dev-k8s"
    clusters = [_cluster]
    cluster_target = _target
    workloads = [_api | {runtime = forge.OnCluster {target = _target}}]
})
`, clusterName, clusterName, registryPort)
	writeFluxE2EFile(t, filepath.Join(projectDir, "deploy", "kcl", "dev-k8s", "main.k"), mainK)

	writeFluxE2EFile(t, filepath.Join(projectDir, "Dockerfile.pause"), "FROM busybox:1.36\nRUN mkdir /www && echo ok > /www/readyz && echo ok > /www/healthz\nCMD [\"httpd\", \"-f\", \"-p\", \"8080\", \"-h\", \"/www\"]\n")

	return projectDir
}

// runForgeFluxE2E runs forge with the ledger pointed at a temp dir.
//
// FORGE_LEDGER_HOME is the whole reason this wrapper exists: without it every
// build and deploy here would write into the developer's real
// `~/.forge/ledger`, mixing a test project's promotion history into the
// machine's.
func runForgeFluxE2E(t *testing.T, projectDir, forgeBin, ledgerHome string, args ...string) string {
	t.Helper()
	cmd := exec.Command(forgeBin, args...)
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(),
		"FORGE_LEDGER_HOME="+ledgerHome,
		"GOFLAGS=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("forge %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	t.Logf("forge %s:\n%s", strings.Join(args, " "), out)
	return string(out)
}

// assertFluxInstalled checks that `forge cluster up` put the reconciler in,
// and put in ONLY the two controllers this path uses.
//
// The negative half matters as much as the positive: helm-controller and
// notification-controller being absent is a security posture, and a chart
// default change upstream must not quietly turn them on.
func assertFluxInstalled(t *testing.T, kctx string) {
	t.Helper()
	deployments := kubectlJSONFluxE2E(t, kctx, "get", "deployments", "-n", "flux-system", "-o", "json")
	running := map[string]bool{}
	for _, item := range deployments["items"].([]any) {
		name := item.(map[string]any)["metadata"].(map[string]any)["name"].(string)
		running[name] = true
	}
	for _, want := range []string{"source-controller", "kustomize-controller"} {
		if !running[want] {
			t.Errorf("flux-system has no %s Deployment; `forge cluster up` must install the reconciler, "+
				"or a pointer written at it converges nothing. Present: %v", want, running)
		}
	}
	for _, unwanted := range []string{"helm-controller", "notification-controller",
		"image-automation-controller", "image-reflector-controller"} {
		if running[unwanted] {
			t.Errorf("flux-system runs %s; only source- and kustomize-controller are on this path, and "+
				"every extra controller is attack surface that has to be patched", unwanted)
		}
	}
	// The CRDs the pointer is written against must be Established, or the
	// write fails on a kind the apiserver has never registered.
	for _, crd := range []string{"ocirepositories.source.toolkit.fluxcd.io", "kustomizations.kustomize.toolkit.fluxcd.io"} {
		out := kubectlFluxE2E(t, kctx, "get", "crd", crd, "-o", "jsonpath={.status.conditions[?(@.type=='Established')].status}")
		if strings.TrimSpace(out) != "True" {
			t.Errorf("CRD %s is not Established (%q); forge's CRD-first apply is what the pointer write depends on", crd, out)
		}
	}
}

// assertWorkloadOwnedByFluxNotForge is the NEGATIVE: forge's client-side apply
// did not put this object here.
//
// Server-side apply records a field manager per object, so ownership is the
// evidence. forge's own apply uses the manager `forge`
// (internal/flux.FieldManager, and cluster.KubectlApply's SSA); Flux's
// kustomize-controller uses `kustomize-controller`. The workload being owned by
// the latter and not the former is what proves the deploy wrote a pointer
// instead of applying.
func assertWorkloadOwnedByFluxNotForge(t *testing.T, kctx string) {
	t.Helper()
	obj := kubectlJSONFluxE2E(t, kctx, "get", "deployment", "api", "-n", "app", "-o", "json", "--show-managed-fields")
	meta := obj["metadata"].(map[string]any)
	raw, _ := meta["managedFields"].([]any)
	var managers []string
	for _, f := range raw {
		managers = append(managers, f.(map[string]any)["manager"].(string))
	}
	joined := strings.Join(managers, ",")
	if !strings.Contains(joined, "kustomize-controller") {
		t.Errorf("deployment/api is not owned by kustomize-controller (managers: %v) — Flux is supposed to "+
			"be what applied it", managers)
	}
	// The manager forge's own apply would have used. Its presence would
	// mean forge applied the env as well as pointing at it, which leaves
	// two authorities over one object.
	for _, m := range managers {
		if m == "forge" || strings.HasPrefix(m, "kubectl") {
			t.Errorf("deployment/api carries field manager %q — forge (or a kubectl) applied this object "+
				"directly, and the reconciled path must write only the pointer", m)
		}
	}
}

// assertDeploymentAvailable re-reads the rollout from the cluster rather than
// trusting the deploy's exit code.
func assertDeploymentAvailable(t *testing.T, kctx, namespace, name string) {
	t.Helper()
	// Flux applied under `wait: true`, so this should already be true; the
	// short retry covers the gap between the Kustomization reporting Ready
	// and this read.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out := kubectlFluxE2E(t, kctx, "get", "deployment", name, "-n", namespace,
			"-o", "jsonpath={.status.availableReplicas}")
		if strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "0" {
			return
		}
		if !time.Now().Before(deadline) {
			describe, _ := exec.Command("kubectl", "--context", kctx, "describe",
				"deployment", name, "-n", namespace).CombinedOutput()
			t.Fatalf("deployment %s/%s never became available (availableReplicas=%q)\n%s",
				namespace, name, out, describe)
		}
		time.Sleep(3 * time.Second)
	}
}

// recordedBundleDigest is the digest the LEDGER recorded for the bundle this
// deploy pinned: the LAST `oci@sha256:<digest>` the deploy reported.
//
// The deploy builds (and so records) its own bundle after our explicit build,
// so the newest line is the one the pointer names. It comes from forge's own
// report of what it wrote to the ledger, never from the Kustomization, because
// taking it from the cluster would compare the cluster to itself.
func recordedBundleDigest(t *testing.T, deployOut string) string {
	t.Helper()
	const marker = "oci@"
	i := strings.LastIndex(deployOut, marker)
	if i < 0 {
		t.Fatalf("the deploy reported no recorded bundle (no %q line):\n%s", marker, deployOut)
	}
	digest := firstSHA256InFluxE2E(deployOut[i:])
	if digest == "" {
		t.Fatalf("no canonical digest after %q:\n%s", marker, deployOut[i:])
	}
	return digest
}

// firstSHA256InFluxE2E pulls the first canonical digest out of some text.
func firstSHA256InFluxE2E(s string) string {
	const prefix = "sha256:"
	for i := 0; i+len(prefix)+64 <= len(s); i++ {
		if s[i:i+len(prefix)] != prefix {
			continue
		}
		candidate := s[i : i+len(prefix)+64]
		hex := candidate[len(prefix):]
		ok := true
		for _, r := range hex {
			if !strings.ContainsRune("0123456789abcdef", r) {
				ok = false
				break
			}
		}
		if ok {
			return candidate
		}
	}
	return ""
}

// assertKustomizationConverged IS THE CONTRACT: `lastAppliedRevision` equals
// the bundle's OCI MANIFEST digest.
//
// It is reasoned (the `layerSelector` selects a layer; it does not re-key the
// revision) and measured HERE. If a future Flux changed it, this is the only
// test that would notice, and everything else on the path would merely hang.
func assertKustomizationConverged(t *testing.T, kctx, name, digest string) {
	t.Helper()
	obj := kubectlJSONFluxE2E(t, kctx, "get", "kustomization", name, "-n", "flux-system", "-o", "json")
	status, _ := obj["status"].(map[string]any)
	if status == nil {
		t.Fatalf("kustomization %s has no status; it has never reconciled", name)
	}
	revision, _ := status["lastAppliedRevision"].(string)
	if revision != digest {
		t.Fatalf("lastAppliedRevision = %q, want the bundle's OCI MANIFEST digest %q.\n"+
			"  This is the comparison forge's wait is built on: if Flux now keys the revision off "+
			"something else (the layer digest, a tag), every wait on this path hangs on a test that "+
			"can never be true.", revision, digest)
	}
	// Ready=True as well: the revision alone is an apply that was admitted
	// and may then have failed its health check.
	ready := ""
	for _, c := range status["conditions"].([]any) {
		cond := c.(map[string]any)
		if cond["type"] == "Ready" {
			ready = cond["status"].(string)
		}
	}
	if ready != "True" {
		t.Errorf("kustomization %s Ready = %q, want True", name, ready)
	}
}

// assertLayerSelectorIsSet pins that the revision above was produced WITH the
// layerSelector in place.
//
// Without this, the previous assertion would still pass for a bundle whose
// layer happened to be handed over unselected — and the brief's requirement is
// specifically that the equality holds WHILE `layerSelector` is set, because
// that combination is the one that was reasoned rather than measured.
func assertLayerSelectorIsSet(t *testing.T, kctx, name, digest string) {
	t.Helper()
	obj := kubectlJSONFluxE2E(t, kctx, "get", "ocirepository", name, "-n", "flux-system", "-o", "json")
	spec := obj["spec"].(map[string]any)
	sel, ok := spec["layerSelector"].(map[string]any)
	if !ok {
		t.Fatalf("ocirepository %s has no layerSelector; the manifest layer would not be extracted and the "+
			"Kustomization's path would not exist inside the artifact", name)
	}
	if sel["operation"] != "extract" {
		t.Errorf("layerSelector.operation = %v, want extract", sel["operation"])
	}
	if got, _ := sel["mediaType"].(string); !strings.Contains(got, "forge.bundle.manifests") {
		t.Errorf("layerSelector.mediaType = %q, want forge's manifest layer type", got)
	}
	// And the source pins the same digest the Kustomization reported.
	ref, _ := spec["ref"].(map[string]any)
	if ref == nil || ref["digest"] != digest {
		t.Errorf("ocirepository ref = %v, want digest %q", ref, digest)
	}
}

// assertHandEditIsReverted is the self-healing property: the cluster matches
// what was RECORDED, not what somebody last typed.
//
// This is the reason to run a reconciler rather than a one-shot apply, and it
// is also the clearest demonstration that Flux — not forge — owns these
// objects: nothing is running forge while this happens.
func assertHandEditIsReverted(t *testing.T, kctx, ksName, namespace, name string) {
	t.Helper()
	const edited = "7"
	original := strings.TrimSpace(kubectlFluxE2E(t, kctx, "get", "deployment", name, "-n", namespace,
		"-o", "jsonpath={.spec.replicas}"))
	if original == edited {
		t.Fatalf("the fixture already runs %s replicas; the test needs a value Flux will revert TO", edited)
	}

	// A Flux-owned field, changed out from under it.
	kubectlFluxE2E(t, kctx, "scale", "deployment", name, "-n", namespace, "--replicas="+edited)
	if got := strings.TrimSpace(kubectlFluxE2E(t, kctx, "get", "deployment", name, "-n", namespace,
		"-o", "jsonpath={.spec.replicas}")); got != edited {
		t.Fatalf("the hand edit did not take (replicas=%q); there is nothing to revert", got)
	}

	// Ask Flux to reconcile now rather than waiting out its interval — the
	// same annotation forge's pointer carries, for the same reason.
	kubectlFluxE2E(t, kctx, "annotate", "--overwrite", "kustomization", ksName, "-n", "flux-system",
		"reconcile.fluxcd.io/requestedAt="+time.Now().Format(time.RFC3339Nano))

	deadline := time.Now().Add(3 * time.Minute)
	for {
		got := strings.TrimSpace(kubectlFluxE2E(t, kctx, "get", "deployment", name, "-n", namespace,
			"-o", "jsonpath={.spec.replicas}"))
		if got == original {
			return
		}
		if !time.Now().Before(deadline) {
			describe, _ := exec.Command("kubectl", "--context", kctx, "describe",
				"kustomization", ksName, "-n", "flux-system").CombinedOutput()
			t.Fatalf("a hand-edited Flux-owned field was NOT reverted: replicas is %q, the bundle says %q.\n"+
				"  Self-healing is the reason to run a reconciler at all.\n%s", got, original, describe)
		}
		time.Sleep(5 * time.Second)
	}
}

// ── kubectl helpers, scoped to OUR context only ─────────────────────────────

// kubectlFluxE2E runs kubectl against the test's own cluster.
//
// The context is ALWAYS passed explicitly. A kubectl with no `--context` uses
// whatever is current, and `k3d cluster create` silently flips that — so an
// unscoped command here could read, or worse write, a cluster another agent's
// work depends on.
func kubectlFluxE2E(t *testing.T, kctx string, args ...string) string {
	t.Helper()
	full := append([]string{"--context", kctx}, args...)
	out, err := exec.Command("kubectl", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s failed: %v\n%s", strings.Join(full, " "), err, out)
	}
	return string(out)
}

func kubectlJSONFluxE2E(t *testing.T, kctx string, args ...string) map[string]any {
	t.Helper()
	raw := kubectlFluxE2E(t, kctx, args...)
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("parse kubectl JSON: %v\n%s", err, raw)
	}
	return out
}

func writeFluxE2EFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestE2EClusterUpInstallsADeclaredFluxChart: an env that DECLARES
// `forge.flux_chart()` gets that Flux from `forge cluster up`. forge's own
// install steps aside for a declared one, so before the platform phase nothing
// installed it and the first pointer write failed on `no matches for kind
// OCIRepository`.
func TestE2EClusterUpInstallsADeclaredFluxChart(t *testing.T) {
	requirePublishedForgePkg(t)
	requireTool(t, "k3d", "kubectl", "helm", "docker")
	t.Parallel()

	forgeBin := buildforgeBinary(t)
	clusterName := fmt.Sprintf("forge-flux-decl-%d-%d", os.Getpid(), time.Now().UnixNano()%100000)
	kctx := "k3d-" + clusterName
	registryName := clusterName + "-registry"
	registryPort := freePortE2E(t)
	ledgerHome := t.TempDir()

	projectDir := scaffoldFluxE2EProject(t, forgeBin, clusterName, registryName, registryPort)
	mainK := filepath.Join(projectDir, "deploy", "kcl", "dev-k8s", "main.k")
	raw, err := os.ReadFile(mainK)
	if err != nil {
		t.Fatal(err)
	}
	declared := strings.Replace(string(raw), "    clusters = [_cluster]\n",
		"    clusters = [_cluster]\n    helm_charts = [forge.flux_chart()]\n", 1)
	if declared == string(raw) {
		t.Fatal("could not declare flux_chart in the scaffolded env")
	}
	writeFluxE2EFile(t, mainK, declared)

	t.Cleanup(func() {
		if out, err := exec.Command("k3d", "cluster", "delete", clusterName).CombinedOutput(); err != nil {
			t.Logf("teardown: k3d cluster delete %s: %v\n%s", clusterName, err, out)
		}
		if out, err := exec.Command("k3d", "registry", "delete", "k3d-"+registryName).CombinedOutput(); err != nil {
			t.Logf("teardown: k3d registry delete %s: %v\n%s", registryName, err, out)
		}
	})

	runForgeFluxE2E(t, projectDir, forgeBin, ledgerHome, "cluster", "up", "dev-k8s", "--wait")
	assertFluxInstalled(t, kctx)
}

// TestE2EFluxSyncsSecretsAndKeepsThemOutOfTheBundle is the F-13/Flux contract
// against a real cluster and a real Flux:
//
//   - a Secret's REAL value ends up in the cluster (Flux used to apply the
//     redaction marker over it, so it never did);
//   - the bundle's layer holds no Secret object at all;
//   - a second deploy is idempotent (the Secret is not rewritten);
//   - dropping the Secrets from the KCL neither makes Flux prune them nor makes
//     forge delete them — removal is the user's call.
func TestE2EFluxSyncsSecretsAndKeepsThemOutOfTheBundle(t *testing.T) {
	requirePublishedForgePkg(t)
	requireTool(t, "k3d", "kubectl", "helm", "docker")
	t.Parallel()

	forgeBin := buildforgeBinary(t)
	clusterName := fmt.Sprintf("forge-flux-sec-%d-%d", os.Getpid(), time.Now().UnixNano()%100000)
	kctx := "k3d-" + clusterName
	registryName := clusterName + "-registry"
	registryPort := freePortE2E(t)
	ledgerHome := t.TempDir()

	projectDir := scaffoldFluxE2EProject(t, forgeBin, clusterName, registryName, registryPort)
	t.Cleanup(func() {
		if out, err := exec.Command("k3d", "cluster", "delete", clusterName).CombinedOutput(); err != nil {
			t.Logf("teardown: k3d cluster delete %s: %v\n%s", clusterName, err, out)
		}
		if out, err := exec.Command("k3d", "registry", "delete", "k3d-"+registryName).CombinedOutput(); err != nil {
			t.Logf("teardown: k3d registry delete %s: %v\n%s", registryName, err, out)
		}
	})

	const realValue = "real-secret-value-9f3a"
	mainK := filepath.Join(projectDir, "deploy", "kcl", "dev-k8s", "main.k")
	base, err := os.ReadFile(mainK)
	if err != nil {
		t.Fatal(err)
	}
	withSecrets := strings.Replace(string(base), "    workloads = [_api",
		`    rendered_secrets = [forge.RenderedSecret {
        name = "store-creds"
        keys = {"token" = forge.RenderedSecretKey {key = "STORE_TOKEN"}}
    }]
    manifests = [forge.Manifests {objects = [{
        apiVersion = "v1"
        kind = "Secret"
        metadata = {name = "raw-creds", namespace = "app"}
        type = "Opaque"
        stringData = {token = "`+realValue+`"}
    }]}]
    workloads = [_api`, 1)
	if withSecrets == string(base) {
		t.Fatal("the fixture's Bundle shape changed; the Secrets were not injected")
	}
	writeFluxE2EFile(t, mainK, withSecrets)
	writeFluxE2EFile(t, filepath.Join(projectDir, "secrets", "dev-k8s.yaml"), "STORE_TOKEN: "+realValue+"\n")

	runForgeFluxE2E(t, projectDir, forgeBin, ledgerHome, "cluster", "up", "dev-k8s", "--wait")
	runForgeFluxE2E(t, projectDir, forgeBin, ledgerHome, "env", "build", "dev-k8s", "--push")

	// ── --explain / plan: names only ─────────────────────────────────────
	explain := runForgeFluxE2E(t, projectDir, forgeBin, ledgerHome, "env", "deploy", "dev-k8s", "--explain")
	for _, name := range []string{"store-creds", "raw-creds"} {
		if !strings.Contains(explain, name) {
			t.Errorf("--explain does not list Secret %s to be synced:\n%s", name, explain)
		}
	}
	for _, out := range []string{explain, runForgeFluxE2E(t, projectDir, forgeBin, ledgerHome, "env", "deploy", "dev-k8s", "--dry-run")} {
		if strings.Contains(out, realValue) {
			t.Fatalf("a preview printed a Secret value:\n%s", out)
		}
	}

	deployOut := runForgeFluxE2E(t, projectDir, forgeBin, ledgerHome, "env", "deploy", "dev-k8s", "--yes")
	if strings.Contains(deployOut, realValue) {
		t.Fatalf("the deploy printed a Secret value:\n%s", deployOut)
	}
	if !strings.Contains(deployOut, "synced 2 secret(s)") {
		t.Errorf("the deploy did not report syncing its two Secrets:\n%s", deployOut)
	}

	// ── The REAL value is in the cluster, written by forge-secrets ───────
	for _, name := range []string{"store-creds", "raw-creds"} {
		assertSecretValue(t, kctx, name, "token", realValue)
		assertSecretManagedBy(t, kctx, name, "forge-secrets")
	}

	// ── The bundle holds no Secret object ────────────────────────────────
	assertBundleHasNoSecret(t, ledgerHome, recordedBundleDigest(t, deployOut))

	// ── Re-deploy is idempotent ──────────────────────────────────────────
	before := kubectlFluxE2E(t, kctx, "get", "secret", "store-creds", "-n", "app", "-o", "jsonpath={.metadata.resourceVersion}")
	runForgeFluxE2E(t, projectDir, forgeBin, ledgerHome, "env", "deploy", "dev-k8s", "--yes")
	after := kubectlFluxE2E(t, kctx, "get", "secret", "store-creds", "-n", "app", "-o", "jsonpath={.metadata.resourceVersion}")
	if before != after {
		t.Errorf("a re-deploy rewrote an unchanged Secret (resourceVersion %s -> %s)", before, after)
	}

	// ── Removing the Secrets from the KCL deletes nothing ────────────────
	writeFluxE2EFile(t, mainK, string(base))
	runForgeFluxE2E(t, projectDir, forgeBin, ledgerHome, "env", "build", "dev-k8s", "--push")
	runForgeFluxE2E(t, projectDir, forgeBin, ledgerHome, "env", "deploy", "dev-k8s", "--yes")
	for _, name := range []string{"store-creds", "raw-creds"} {
		assertSecretValue(t, kctx, name, "token", realValue)
	}
}

func assertSecretValue(t *testing.T, kctx, name, key, want string) {
	t.Helper()
	got := kubectlFluxE2E(t, kctx, "get", "secret", name, "-n", "app", "-o", "go-template={{index .data \""+key+"\" | base64decode}}")
	if strings.TrimSpace(got) != want {
		t.Errorf("secret %s key %s = %q in the cluster, want the real value %q", name, key, got, want)
	}
}

func assertSecretManagedBy(t *testing.T, kctx, name, manager string) {
	t.Helper()
	managers := kubectlFluxE2E(t, kctx, "get", "secret", name, "-n", "app", "--show-managed-fields",
		"-o", "jsonpath={.metadata.managedFields[*].manager}")
	if !strings.Contains(managers, manager) {
		t.Errorf("secret %s is managed by %q, want %q among them", name, managers, manager)
	}
	if strings.Contains(managers, "kustomize-controller") {
		t.Errorf("secret %s is managed by kustomize-controller: a Secret must not ride the bundle", name)
	}
}

// assertBundleHasNoSecret reads the recorded bundle back out of the ledger's
// OCI layout and scans its manifest layer, so the check is on the bytes Flux
// would fetch rather than on forge's belief about them.
func assertBundleHasNoSecret(t *testing.T, ledgerHome, digest string) {
	t.Helper()
	var ociDir string
	_ = filepath.WalkDir(ledgerHome, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == "oci" {
			ociDir = path
			return filepath.SkipAll
		}
		return nil
	})
	if ociDir == "" {
		t.Fatalf("no OCI layout under %s", ledgerHome)
	}
	layout, err := bundle.NewLocalLayout(ociDir)
	if err != nil {
		t.Fatal(err)
	}
	fetched, err := bundle.Fetch(context.Background(), layout, digest)
	if err != nil {
		t.Fatalf("read bundle %s: %v", digest, err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(fetched.Manifests))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		data, _ := io.ReadAll(tr)
		if strings.Contains(string(data), "kind: Secret") {
			t.Errorf("the bundle layer carries a Secret object at %s:\n%s", hdr.Name, data)
		}
	}
	if len(fetched.Doc.Secrets) != 2 {
		t.Errorf("the bundle document names %d Secrets, want 2: %+v", len(fetched.Doc.Secrets), fetched.Doc.Secrets)
	}
}
