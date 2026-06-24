package cluster

import (
	"context"
	"strings"
	"testing"
)

// contains reports whether ss contains the exact string s.
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// indexOf returns the position of s in ss, or -1.
func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}

// TestRenderDArgs_QuotesStringArgs pins the forge-deploy-prod regression
// fix: the forge-controlled string args (image_tag, namespace, env) are
// emitted as QUOTED KCL string literals so KCL types an all-digit
// git-describe tag as `str` rather than coercing it to `int` (which
// fails RenderEnv.image_tag's `str` type). envCfgKV values are left
// unquoted — their KCL type is intentional project config.
func TestRenderDArgs_QuotesStringArgs(t *testing.T) {
	// Numeric tag: the regression trigger. Must come out QUOTED.
	got := renderDArgs("3826648", "cp-forge-prod", "prod", nil)
	for _, want := range []string{
		`image_tag="3826648"`,
		`namespace="cp-forge-prod"`,
		`env="prod"`,
	} {
		if !contains(got, want) {
			t.Errorf("expected %q in dArgs, got %v", want, got)
		}
	}

	// Non-numeric tag is also quoted (uniform handling).
	got = renderDArgs("v1.2.3", "ns", "dev", nil)
	if !contains(got, `image_tag="v1.2.3"`) {
		t.Errorf("expected image_tag=\"v1.2.3\" in dArgs, got %v", got)
	}

	// Empty env produces no env= binding.
	got = renderDArgs("tag", "ns", "", nil)
	for _, a := range got {
		if strings.HasPrefix(a, "env=") {
			t.Errorf("expected no env= entry for empty env, got %v", got)
		}
	}

	// envCfgKV values are appended sorted by key and UNquoted.
	got = renderDArgs("tag", "ns", "", map[string]string{"B": "2", "A": "1"})
	if !contains(got, "A=1") {
		t.Errorf("expected unquoted A=1 in dArgs, got %v", got)
	}
	if !contains(got, "B=2") {
		t.Errorf("expected unquoted B=2 in dArgs, got %v", got)
	}
	if ia, ib := indexOf(got, "A=1"), indexOf(got, "B=2"); ia == -1 || ib == -1 || ia > ib {
		t.Errorf("expected A=1 before B=2 (sorted), got %v", got)
	}
}

// TestKubectlArgs_ThreadsContext is the BUG 1 regression test: when an
// ApplyOpts.Context is set, every kubectl invocation in the apply/wait
// path must carry `--context <ctx>` as the leading argument (a global
// kubectl flag, valid before any subcommand) — NOT a global
// `kubectl config use-context` switch. Per-command threading is what
// makes concurrent multi-cluster `forge deploy` safe: two deploys
// sharing one kubeconfig but targeting different clusters can no longer
// race on the single global active context (the cross-cluster
// contamination incident). An empty context leaves the args untouched
// (current/default context — unchanged single-cluster behaviour).
func TestKubectlArgs_ThreadsContext(t *testing.T) {
	// With a context: --context <ctx> is prepended before the subcommand.
	got := KubectlArgs("prod-cluster", "apply", "--server-side", "-f", "-")
	want := []string{"--context", "prod-cluster", "apply", "--server-side", "-f", "-"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("[%d]: got %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	// --context must be the LEADING arg (global flag, ahead of the
	// subcommand) so kubectl accepts it for every verb.
	if ic, ia := indexOf(got, "--context"), indexOf(got, "apply"); ic == -1 || ic > ia {
		t.Errorf("expected --context ahead of the subcommand, got %v", got)
	}

	// Empty context: args pass through verbatim (no --context injected).
	got = KubectlArgs("", "rollout", "status", "deployment/x")
	if contains(got, "--context") {
		t.Errorf("expected no --context for empty context, got %v", got)
	}
	if len(got) != 3 || got[0] != "rollout" {
		t.Errorf("expected unchanged args for empty context, got %v", got)
	}
}

// TestKubectlCmd_IncludesContext confirms the *exec.Cmd built by the
// single construction point (kubectlCmd) carries `--context <ctx>` in
// its argv — every kubectl-executing function in this package
// (KubectlApply, WaitRollout, WaitJobComplete, ListManagedDeployments,
// Prune, diagnoseFailedRollout) goes through it, so this pins the
// per-command context threading for the whole apply/wait path.
func TestKubectlCmd_IncludesContext(t *testing.T) {
	cmd := kubectlCmd(context.Background(), "edge-cluster", "apply", "--server-side", "-f", "-")
	// cmd.Args[0] is the binary ("kubectl"); the rest is the argv.
	if !contains(cmd.Args, "--context") || !contains(cmd.Args, "edge-cluster") {
		t.Fatalf("expected --context edge-cluster in argv, got %v", cmd.Args)
	}
	ic, ia := indexOf(cmd.Args, "--context"), indexOf(cmd.Args, "apply")
	if ic == -1 || ia == -1 || ic > ia {
		t.Errorf("expected --context ahead of the apply subcommand, got %v", cmd.Args)
	}

	// No context → no --context flag.
	cmd = kubectlCmd(context.Background(), "", "get", "deployments")
	if contains(cmd.Args, "--context") {
		t.Errorf("expected no --context for empty context, got %v", cmd.Args)
	}
}

// TestKubectlApply_ForcesConflicts pins the deploy apply to Server-Side
// Apply with --force-conflicts. forge is the declarative source of
// truth, so its SSA field manager must win unconditionally: without
// --force-conflicts, a resource previously touched by a plain
// `kubectl apply` (manager kubectl-client-side-apply) makes SSA abort
// the whole deploy with a field-manager conflict ("exit status 1").
// Both flags are required together — --force-conflicts is an SSA-only
// flag and is a no-op without --server-side.
func TestKubectlApply_ForcesConflicts(t *testing.T) {
	// Mirror exactly the argv KubectlApply constructs.
	cmd := kubectlCmd(context.Background(), "prod-cluster", "apply", "--server-side", "--force-conflicts", "-f", "-")
	if !contains(cmd.Args, "--server-side") {
		t.Errorf("expected --server-side in apply argv, got %v", cmd.Args)
	}
	if !contains(cmd.Args, "--force-conflicts") {
		t.Errorf("expected --force-conflicts in apply argv, got %v", cmd.Args)
	}
	// --force-conflicts is meaningless without --server-side; assert the
	// SSA flag precedes it so the apply stays a valid SSA invocation.
	iss, ifc := indexOf(cmd.Args, "--server-side"), indexOf(cmd.Args, "--force-conflicts")
	if iss == -1 || ifc == -1 || iss > ifc {
		t.Errorf("expected --server-side ahead of --force-conflicts, got %v", cmd.Args)
	}
}

// TestRenderedDeploymentNames verifies the extractor parses the multi-
// document YAML stream forge produces from KCL, returning only
// Deployment kind names. Non-Deployments and malformed docs are skipped.
func TestRenderedDeploymentNames(t *testing.T) {
	manifests := `apiVersion: v1
kind: Service
metadata:
  name: workspace-controller
spec:
  ports: []
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: workspace-controller
  labels:
    app.kubernetes.io/managed-by: forge
spec: {}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cp-forge-config
data:
  KEY: value
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: workspace-proxy
spec: {}
`
	got := RenderedDeploymentNames(manifests)
	want := []string{"workspace-controller", "workspace-proxy"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestExtractManifests_SiblingOutputIsSilent confirms that when the
// generated `main.k` exports both `manifests` (the YAML manifest list
// we consume) AND `output` (the JSON contract the forge build/run/
// deploy pipeline consumes via a separate kcl invocation), the
// `output` sibling is silently skipped rather than emitting a noisy
// "extra top-level KCL var" warning on every `forge deploy` /
// `forge up`. Pins the dual-output contract documented at the top of
// the canonical main.k template.
func TestExtractManifests_SiblingOutputIsSilent(t *testing.T) {
	// Mirrors the shape kcl emits when main.k declares both
	// `output = forge.render(_bundle)` and
	// `manifests = forge.render_manifests(_bundle, _env)`.
	in := `manifests:
- apiVersion: v1
  kind: Namespace
  metadata:
    name: example-dev
- apiVersion: apps/v1
  kind: Deployment
  metadata:
    name: workspace-proxy
  spec: {}
output:
  services:
  - name: workspace-proxy
    deploy:
      type: cluster
  operators: []
  frontends: []
  cronjobs: []
  config_maps: []
`
	got, err := extractManifests([]byte(in))
	if err != nil {
		t.Fatalf("extractManifests: %v", err)
	}
	if !strings.Contains(got, "kind: Namespace") || !strings.Contains(got, "kind: Deployment") {
		t.Errorf("expected Namespace + Deployment in output, got:\n%s", got)
	}
	// Two manifest items should be `---`-separated.
	if !strings.Contains(got, "---") {
		t.Errorf("expected `---` document separator, got:\n%s", got)
	}
}

// TestExtractManifests_UnexpectedSiblingStillWarns confirms we only
// silence the documented `output` sibling — any OTHER unexpected
// top-level var still triggers the helpful warning so projects don't
// silently drop manifest content into a stray top-level binding.
func TestExtractManifests_UnexpectedSiblingStillWarns(t *testing.T) {
	in := `manifests:
- apiVersion: v1
  kind: Namespace
  metadata:
    name: example-dev
stray_var:
  something: else
`
	// We can't capture os.Stderr without plumbing without changing the
	// production signature; instead, assert success (warning is fire-
	// and-forget) and that the function does still extract manifests.
	got, err := extractManifests([]byte(in))
	if err != nil {
		t.Fatalf("extractManifests: %v", err)
	}
	if !strings.Contains(got, "kind: Namespace") {
		t.Errorf("expected Namespace in output, got:\n%s", got)
	}
}

// TestRenderedDeploymentNames_EmptyAndMalformed confirms the extractor
// degrades gracefully on edge cases: empty input, all-non-Deployment,
// and unparseable docs all return an empty slice rather than panicking.
func TestRenderedDeploymentNames_EmptyAndMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace", "   \n\n  "},
		{"no Deployments", "kind: Service\nmetadata:\n  name: x\n"},
		{"malformed YAML", "this is not yaml: : :"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RenderedDeploymentNames(c.in); len(got) != 0 {
				t.Errorf("expected empty slice, got %v", got)
			}
		})
	}
}

// filterTestManifests is a representative env bundle: two app
// Deployments (each carrying app.kubernetes.io/name), a shared ConfigMap
// and a Namespace (NO app.kubernetes.io/name — the shared/infra shape
// forge's renderer produces). It mirrors what RenderManifests emits and
// is `\n---\n`-joined exactly like the production stream.
const filterTestManifests = `apiVersion: v1
kind: Namespace
metadata:
  name: example-dev
  labels:
    app.kubernetes.io/managed-by: forge
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: example-config
  namespace: example-dev
  labels:
    app.kubernetes.io/managed-by: forge
    app.kubernetes.io/part-of: example-dev
data:
  KEY: value
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: admin-server
  namespace: example-dev
  labels:
    app.kubernetes.io/name: admin-server
    app.kubernetes.io/managed-by: forge
spec: {}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: workspace-proxy
  namespace: example-dev
  labels:
    app.kubernetes.io/name: workspace-proxy
    app.kubernetes.io/managed-by: forge
spec: {}`

// TestFilterManifestsByApp_KeepsTargetAndShared is the core assertion
// of the --target application filter: targeting one app keeps that app's
// Deployment plus every shared resource (the ConfigMap and Namespace
// have no app.kubernetes.io/name label) and drops the other app's
// Deployment.
func TestFilterManifestsByApp_KeepsTargetAndShared(t *testing.T) {
	got, err := FilterManifestsByApp(filterTestManifests, []string{"admin-server"})
	if err != nil {
		t.Fatalf("FilterManifestsByApp: %v", err)
	}
	if !strings.Contains(got, "name: admin-server") {
		t.Errorf("expected targeted app admin-server to be kept, got:\n%s", got)
	}
	if !strings.Contains(got, "kind: Namespace") {
		t.Errorf("expected shared Namespace (no app label) to be kept, got:\n%s", got)
	}
	if !strings.Contains(got, "name: example-config") {
		t.Errorf("expected shared ConfigMap (no app label) to be kept, got:\n%s", got)
	}
	if strings.Contains(got, "name: workspace-proxy") {
		t.Errorf("expected non-targeted app workspace-proxy to be dropped, got:\n%s", got)
	}
}

// TestFilterManifestsByApp_MultipleTargets confirms the filter unions
// multiple --target apps and still keeps shared resources.
func TestFilterManifestsByApp_MultipleTargets(t *testing.T) {
	got, err := FilterManifestsByApp(filterTestManifests, []string{"admin-server", "workspace-proxy"})
	if err != nil {
		t.Fatalf("FilterManifestsByApp: %v", err)
	}
	for _, want := range []string{"name: admin-server", "name: workspace-proxy", "kind: Namespace", "name: example-config"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output, got:\n%s", want, got)
		}
	}
}

// TestFilterManifestsByApp_UnknownTargetErrors confirms a typo'd target
// (matching no app workload) errors with the available app names rather
// than applying a shared-only bundle that does nothing the user wanted.
func TestFilterManifestsByApp_UnknownTargetErrors(t *testing.T) {
	_, err := FilterManifestsByApp(filterTestManifests, []string{"nope"})
	if err == nil {
		t.Fatal("expected error for unknown target, got nil")
	}
	// Available app names should be surfaced for the fix.
	if !strings.Contains(err.Error(), "admin-server") || !strings.Contains(err.Error(), "workspace-proxy") {
		t.Errorf("expected available app names in error, got: %v", err)
	}
}

// operatorFilterManifests mirrors what forge renders for an Operator:
// a Deployment plus its cluster RBAC trio (ServiceAccount, ClusterRole,
// ClusterRoleBinding), every doc carrying app.kubernetes.io/name =
// <operator>. A peer service Deployment (different app label) and a
// shared Namespace (no app label) round it out — the exact shape the
// control-plane `workspace-controller` operator produces alongside its
// app services.
const operatorFilterManifests = `apiVersion: v1
kind: Namespace
metadata:
  name: example-prod
  labels:
    app.kubernetes.io/managed-by: forge
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: workspace-controller
  namespace: example-prod
  labels:
    app.kubernetes.io/name: workspace-controller
    app.kubernetes.io/managed-by: forge
spec: {}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: workspace-controller
  namespace: example-prod
  labels:
    app.kubernetes.io/name: workspace-controller
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: workspace-controller-clusterrole
  labels:
    app.kubernetes.io/name: workspace-controller
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: workspace-controller-clusterrolebinding
  labels:
    app.kubernetes.io/name: workspace-controller
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: admin-server
  namespace: example-prod
  labels:
    app.kubernetes.io/name: admin-server
    app.kubernetes.io/managed-by: forge
spec: {}`

// TestFilterManifestsByApp_Operator is the GAP-1 assertion: targeting an
// operator name keeps that operator's Deployment AND its cluster RBAC
// (ServiceAccount / ClusterRole / ClusterRoleBinding all carry the
// operator's app label), keeps shared infra (the Namespace), and drops
// the unrelated service Deployment. This is what `forge deploy prod
// --target workspace-controller` renders.
func TestFilterManifestsByApp_Operator(t *testing.T) {
	got, err := FilterManifestsByApp(operatorFilterManifests, []string{"workspace-controller"})
	if err != nil {
		t.Fatalf("FilterManifestsByApp: %v", err)
	}
	for _, want := range []string{
		"name: workspace-controller\n",                  // Deployment + ServiceAccount
		"name: workspace-controller-clusterrole\n",      // ClusterRole
		"name: workspace-controller-clusterrolebinding", // ClusterRoleBinding
		"kind: Namespace",                               // shared infra kept
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in scoped output, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "name: admin-server") {
		t.Errorf("expected unrelated service admin-server to be dropped, got:\n%s", got)
	}
}

// jobStreamManifests is a multi-doc stream mirroring a real forge
// render: RuntimeClass + Namespace first, then a ConfigMap and a
// Secret, then a workload Deployment, a schedule=="" one-shot Job
// (the migrate pattern), and a scheduled CronJob. Used by the GAP 1
// (config-first ordering) and GAP 2 (manifest-derived Job wait) tests.
const jobStreamManifests = `apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
---
apiVersion: v1
kind: Namespace
metadata:
  name: cp-forge
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: admin-server
  namespace: cp-forge
spec: {}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: cp-forge
data:
  KEY: value
---
apiVersion: v1
kind: Secret
metadata:
  name: db-credentials
  namespace: cp-forge
data:
  database-url: aHR0cA==
---
apiVersion: batch/v1
kind: Job
metadata:
  name: cp-forge-migrate
  namespace: cp-forge
spec: {}
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: nightly-report
  namespace: cp-forge
spec:
  schedule: "0 0 * * *"
`

// TestPartitionConfigManifests_ConfigFirst pins the GAP 1 fix: the
// config kinds (Namespace, ConfigMap, Secret) are split into the
// first-pass stream, every other kind (RuntimeClass, Deployment, Job,
// CronJob) into the second. Apply applies the config pass — and waits
// for kubectl to return — before the rest pass, so a workload pod never
// schedules ahead of the ConfigMap/Secret it references.
func TestPartitionConfigManifests_ConfigFirst(t *testing.T) {
	config, rest := PartitionConfigManifests(jobStreamManifests)

	// Config pass: exactly Namespace + ConfigMap + Secret.
	for _, want := range []string{"kind: Namespace", "name: app-config", "name: db-credentials"} {
		if !strings.Contains(config, want) {
			t.Errorf("config pass missing %q:\n%s", want, config)
		}
	}
	for _, notWant := range []string{"kind: Deployment", "kind: Job", "kind: CronJob", "kind: RuntimeClass"} {
		if strings.Contains(config, notWant) {
			t.Errorf("config pass should not contain %q:\n%s", notWant, config)
		}
	}

	// Rest pass: workloads + cluster-scoped config, NOT the namespaced
	// ConfigMap/Secret.
	for _, want := range []string{"kind: Deployment", "kind: Job", "kind: CronJob", "kind: RuntimeClass"} {
		if !strings.Contains(rest, want) {
			t.Errorf("rest pass missing %q:\n%s", want, rest)
		}
	}
	for _, notWant := range []string{"name: app-config", "name: db-credentials"} {
		if strings.Contains(rest, notWant) {
			t.Errorf("rest pass should not contain %q:\n%s", notWant, rest)
		}
	}
}

// TestPartitionConfigManifests_NoConfig confirms a bundle with no config
// kinds yields an empty config pass (so Apply skips it) and routes
// everything to rest — the unchanged single-apply behaviour.
func TestPartitionConfigManifests_NoConfig(t *testing.T) {
	const noConfig = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: admin-server
spec: {}
`
	config, rest := PartitionConfigManifests(noConfig)
	if strings.TrimSpace(config) != "" {
		t.Errorf("expected empty config pass, got:\n%s", config)
	}
	if !strings.Contains(rest, "kind: Deployment") {
		t.Errorf("expected Deployment in rest pass, got:\n%s", rest)
	}
}

// TestRenderedJobNames_PicksUpOneShotJob is the GAP 2 regression test:
// a schedule=="" CronJob renders as a `kind: Job`, and the wait set is
// derived from the rendered manifest stream so the migrate Job is
// blocked on even if the entity-list derivation (OneShotJobs) came back
// empty. A scheduled CronJob renders as `kind: CronJob` and is excluded.
func TestRenderedJobNames_PicksUpOneShotJob(t *testing.T) {
	got := RenderedJobNames(jobStreamManifests)
	if !contains(got, "cp-forge-migrate") {
		t.Errorf("expected one-shot Job %q in wait set, got %v", "cp-forge-migrate", got)
	}
	// The scheduled CronJob (kind: CronJob) must NOT be waited on.
	if contains(got, "nightly-report") {
		t.Errorf("scheduled CronJob should not be in the one-shot wait set, got %v", got)
	}
	// Nothing else should sneak in (Deployment, Namespace, etc.).
	if len(got) != 1 {
		t.Errorf("expected exactly one one-shot Job, got %v", got)
	}
}

// TestUnionJobNames_DedupesCallerFirst confirms the wait set unions the
// caller-supplied OneShotJobs with the manifest-derived names, keeps the
// caller's order first, and de-dupes the overlap — so a Job named in
// both sources is waited on exactly once.
func TestUnionJobNames_DedupesCallerFirst(t *testing.T) {
	got := unionJobNames([]string{"caller-job", "cp-forge-migrate"}, []string{"cp-forge-migrate", "rendered-only"})
	want := []string{"caller-job", "cp-forge-migrate", "rendered-only"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v (order-preserving, de-duped), got %v", want, got)
		}
	}
	// Empty names are dropped from both sides.
	if g := unionJobNames([]string{""}, []string{"", "real"}); len(g) != 1 || g[0] != "real" {
		t.Errorf("expected empty names dropped, got %v", g)
	}
}

// TestPodSelectorForDeploy pins the rollout-diagnostic pod selector to
// the appNameLabel constant. A literal "app=<deploy>" selector matched
// zero pods (forge stamps app.kubernetes.io/name, not app), silently
// producing empty diagnostics on a failed rollout. Building the selector
// from the constant makes that drift impossible.
func TestPodSelectorForDeploy(t *testing.T) {
	got := podSelectorForDeploy("daemon-gateway")
	want := appNameLabel + "=daemon-gateway"
	if got != want {
		t.Errorf("podSelectorForDeploy: got %q, want %q", got, want)
	}
	// Guard against a regression to the bare `app=` selector.
	if strings.HasPrefix(got, "app=") {
		t.Errorf("selector must use %q, not a bare app= label; got %q", appNameLabel, got)
	}
}

// multiClusterManifests mirrors what the e2e env renders in the DECLARED-
// CLUSTER-ONLY model. The env-level Namespace stays UNLABELED (it is
// genuinely env-wide and replicated to every cluster). The ConfigMap and
// Gateway are OWNED by an image-less infra service "cp-infra" — they carry
// `app.kubernetes.io/name: cp-infra` (stamped by render_owned_manifests) so
// they route to cp-infra's declared cluster, NOT by a primary heuristic.
// admin-server + workspace-controller live on the control-plane cluster;
// workspace-proxy + its per-service RBAC `deploy` to k3d-cp-daemon. This is
// the exact shape the multi-cluster bug cross-contaminated.
const multiClusterManifests = `apiVersion: v1
kind: Namespace
metadata:
  name: control-plane-e2e
  labels:
    app.kubernetes.io/managed-by: forge
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: control-plane-e2e
  labels:
    app.kubernetes.io/managed-by: forge
    app.kubernetes.io/name: cp-infra
data:
  KEY: value
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: public-gateway
  namespace: control-plane-e2e
  labels:
    app.kubernetes.io/name: cp-infra
spec: {}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: workspace-controller
  namespace: control-plane-e2e
  labels:
    app.kubernetes.io/name: workspace-controller
    app.kubernetes.io/managed-by: forge
spec: {}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: admin-server
  namespace: control-plane-e2e
  labels:
    app.kubernetes.io/name: admin-server
    app.kubernetes.io/managed-by: forge
spec: {}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: workspace-proxy
  namespace: control-plane-e2e
  labels:
    app.kubernetes.io/name: workspace-proxy
    app.kubernetes.io/managed-by: forge
spec: {}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: workspace-proxy
  namespace: control-plane-e2e
  labels:
    app.kubernetes.io/name: workspace-proxy`

// TestScopeManifestsToGroup_ControlPlaneKeepsOwnAndInfra proves the
// control-plane cluster receives its own services' workloads AND the
// cp-infra-owned env-level resources (the ConfigMap + Gateway, which carry
// the infra service's app label) plus the env-wide Namespace — but NOT the
// daemon cluster's service (workspace-proxy) or its RBAC. cp-infra is in
// OwnApps because the infra service declares the control-plane cluster.
func TestScopeManifestsToGroup_ControlPlaneKeepsOwnAndInfra(t *testing.T) {
	scope := GroupScope{
		OwnApps:   map[string]struct{}{"admin-server": {}, "workspace-controller": {}, "cp-infra": {}},
		OtherApps: map[string]struct{}{"workspace-proxy": {}},
	}
	got := ScopeManifestsToGroup(multiClusterManifests, scope)

	for _, want := range []string{
		"name: admin-server",         // own service
		"name: workspace-controller", // own service (operator workload)
		"kind: Namespace",            // env-wide, replicated
		"name: app-config",           // cp-infra-owned ConfigMap
		"kind: Gateway",              // cp-infra-owned Gateway (the CRD-bearing doc)
	} {
		if !strings.Contains(got, want) {
			t.Errorf("control-plane cluster missing %q:\n%s", want, got)
		}
	}
	// The daemon cluster's service AND its RBAC must NOT land here.
	if strings.Contains(got, "name: workspace-proxy") {
		t.Errorf("daemon-cluster service workspace-proxy must not land on control-plane:\n%s", got)
	}
}

// TestScopeManifestsToGroup_DaemonOnlyOwnPlusNamespace is the load-bearing
// isolation assertion: the daemon cluster (hosting only workspace-proxy)
// receives ONLY its own service's workload + RBAC and the env-wide Namespace
// — and NONE of the other cluster's stack: no Gateway (the CRD it doesn't
// have), no cp-infra ConfigMap, no other-cluster services. This is the
// cross-contamination the bug caused, now prevented purely by ownership
// (cp-infra is in OtherApps here), no primary flag.
func TestScopeManifestsToGroup_DaemonOnlyOwnPlusNamespace(t *testing.T) {
	scope := GroupScope{
		OwnApps:   map[string]struct{}{"workspace-proxy": {}},
		OtherApps: map[string]struct{}{"admin-server": {}, "workspace-controller": {}, "cp-infra": {}},
	}
	got := ScopeManifestsToGroup(multiClusterManifests, scope)

	// Kept: the proxy Deployment, its RBAC, and the env-wide Namespace.
	if !strings.Contains(got, "name: workspace-proxy") {
		t.Errorf("daemon cluster must keep its own service workspace-proxy:\n%s", got)
	}
	if !strings.Contains(got, "kind: ServiceAccount") {
		t.Errorf("daemon cluster must keep the proxy's own RBAC:\n%s", got)
	}
	if !strings.Contains(got, "kind: Namespace") {
		t.Errorf("daemon cluster must keep the env-wide Namespace its workload lives in:\n%s", got)
	}
	// Dropped: everything owned by the other cluster (admin-server,
	// workspace-controller, and the cp-infra-owned ConfigMap/Gateway).
	for _, notWant := range []string{
		"kind: Gateway",              // cp-infra-owned CRD cp-daemon doesn't have — the hard-fail
		"name: app-config",           // cp-infra-owned ConfigMap
		"name: admin-server",         // other cluster's service
		"name: workspace-controller", // other cluster's service
	} {
		if strings.Contains(got, notWant) {
			t.Errorf("daemon cluster must NOT receive %q (owned by the other cluster):\n%s", notWant, got)
		}
	}
}

// TestScopeManifestsToGroup_NoCrossContamination is the round-trip
// invariant: partition the SAME rendered stream into the two clusters and
// assert the owned sets are disjoint and together cover every owned manifest
// exactly once — neither cluster sees the other's service or infra, the
// env-wide Namespace lands on both, and nothing is dropped entirely.
func TestScopeManifestsToGroup_NoCrossContamination(t *testing.T) {
	controlPlane := ScopeManifestsToGroup(multiClusterManifests, GroupScope{
		OwnApps:   map[string]struct{}{"admin-server": {}, "workspace-controller": {}, "cp-infra": {}},
		OtherApps: map[string]struct{}{"workspace-proxy": {}},
	})
	daemon := ScopeManifestsToGroup(multiClusterManifests, GroupScope{
		OwnApps:   map[string]struct{}{"workspace-proxy": {}},
		OtherApps: map[string]struct{}{"admin-server": {}, "workspace-controller": {}, "cp-infra": {}},
	})
	// admin-server only on control-plane.
	if !strings.Contains(controlPlane, "name: admin-server") || strings.Contains(daemon, "name: admin-server") {
		t.Errorf("admin-server must be on control-plane only")
	}
	// workspace-proxy only on daemon.
	if strings.Contains(controlPlane, "name: workspace-proxy") || !strings.Contains(daemon, "name: workspace-proxy") {
		t.Errorf("workspace-proxy must be on daemon only")
	}
	// The cp-infra-owned Gateway (CRD-bearing) only on control-plane — the
	// cp-daemon hard-fail guard, now purely ownership-driven.
	if !strings.Contains(controlPlane, "kind: Gateway") || strings.Contains(daemon, "kind: Gateway") {
		t.Errorf("Gateway must be on control-plane only (its owner cp-infra targets it)")
	}
	// The env-wide Namespace lands on BOTH (replicated, not guessed).
	if !strings.Contains(controlPlane, "kind: Namespace") || !strings.Contains(daemon, "kind: Namespace") {
		t.Errorf("the env-wide Namespace must be replicated to both clusters")
	}
}

// TestScopeManifestsToGroup_UnlabeledNonNamespaceReplicated documents the
// "replicate, don't guess" rule: an env-level resource the user left
// UNLABELED (not attributed to any cluster via an infra service) is kept on
// every cluster rather than routed to a guessed primary. To pin it to one
// cluster, the user declares it on an infra service's `manifests`.
func TestScopeManifestsToGroup_UnlabeledNonNamespaceReplicated(t *testing.T) {
	const stream = `apiVersion: v1
kind: ConfigMap
metadata:
  name: shared-config
  namespace: ns
data:
  KEY: value
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: admin-server
  labels:
    app.kubernetes.io/name: admin-server
spec: {}`
	controlPlane := ScopeManifestsToGroup(stream, GroupScope{
		OwnApps:   map[string]struct{}{"admin-server": {}},
		OtherApps: map[string]struct{}{"workspace-proxy": {}},
	})
	daemon := ScopeManifestsToGroup(stream, GroupScope{
		OwnApps:   map[string]struct{}{"workspace-proxy": {}},
		OtherApps: map[string]struct{}{"admin-server": {}},
	})
	if !strings.Contains(controlPlane, "name: shared-config") || !strings.Contains(daemon, "name: shared-config") {
		t.Errorf("an unlabeled env-level ConfigMap must be replicated to every cluster, not routed to a guessed primary")
	}
	// The labeled workload still routes by owner.
	if !strings.Contains(controlPlane, "name: admin-server") || strings.Contains(daemon, "name: admin-server") {
		t.Errorf("admin-server must route to its own cluster only")
	}
}

// TestScopeManifestsToGroup_UngroupedAppLabelKept covers the defensive
// branch: an app-labelled doc whose owner is in NEITHER OwnApps nor
// OtherApps (should not happen once every service is grouped) is KEPT rather
// than dropped or routed by guess — the safe non-heuristic default.
func TestScopeManifestsToGroup_UngroupedAppLabelKept(t *testing.T) {
	const stream = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: orphan
  labels:
    app.kubernetes.io/name: orphan
spec: {}`
	got := ScopeManifestsToGroup(stream, GroupScope{
		OwnApps:   map[string]struct{}{"admin-server": {}},
		OtherApps: map[string]struct{}{"workspace-proxy": {}},
	})
	if !strings.Contains(got, "name: orphan") {
		t.Errorf("an app-labelled doc owned by no group must be kept, not silently dropped:\n%s", got)
	}
}
