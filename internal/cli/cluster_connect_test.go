package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/deploytarget"
)

// The addresses that fail AGAINST A HEALTHY CLUSTER. The hub dials from a pod,
// where a loopback host is the pod — so these produce "connection refused"
// against a target that is fine, which is why they are refused up front.
func TestRefuseUnusableAddress(t *testing.T) {
	for _, tc := range []struct {
		address string
		wantErr string
	}{
		{address: "https://34.1.2.3"},
		{address: "https://my-cluster.example.com:6443"},

		{address: "http://34.1.2.3", wantErr: "https only"},
		{address: "https://127.0.0.1:6443", wantErr: "loopback or bind address"},
		{address: "https://0.0.0.0:6443", wantErr: "loopback or bind address"},
		{address: "https://[::1]:6443", wantErr: "loopback or bind address"},
		{address: "https://localhost:6443", wantErr: "localhost"},
		{address: "", wantErr: "not a URL"},
	} {
		t.Run(tc.address, func(t *testing.T) {
			err := refuseUnusableAddress(tc.address)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refuseUnusableAddress(%q) = %v, want nil", tc.address, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("refuseUnusableAddress(%q) = %v, want one containing %q", tc.address, err, tc.wantErr)
			}
		})
	}
}

func TestReadConnectTarget_RefusesAMissingCA(t *testing.T) {
	restore := kubeconfigClusterOf
	defer func() { kubeconfigClusterOf = restore }()
	kubeconfigClusterOf = func(string) (string, string, error) {
		return "https://34.1.2.3", "", nil
	}
	_, err := readConnectTarget("gke_acme_us-central1_prod")
	if err == nil || !strings.Contains(err.Error(), "cannot verify") {
		t.Fatalf("error = %v, want one about not being able to verify the API server", err)
	}
}

// The address registered is the context's own server, for every kind of
// cluster: the operator chooses what the hub dials by choosing the context.
func TestReadConnectTarget_RegistersTheContextsOwnServer(t *testing.T) {
	restore := kubeconfigClusterOf
	defer func() { kubeconfigClusterOf = restore }()
	kubeconfigClusterOf = func(string) (string, string, error) {
		return "https://172.16.0.34", "ca", nil
	}
	got, err := readConnectTarget("gke_acme_us-central1_prod")
	if err != nil {
		t.Fatalf("readConnectTarget: %v", err)
	}
	if got.Address != "https://172.16.0.34" || got.CAPEM != "ca" || got.Context != "gke_acme_us-central1_prod" {
		t.Errorf("target = %+v, want the context's server and CA", got)
	}
}

// ── The request against a recorded fixture of cp's wire ─────────────────────
//
// The field NAMES are the contract, and protojson's lowerCamelCase is what
// the control plane reads. A typo here is a field the server ignores, so the
// connect appears to succeed and stores nothing — which is why this asserts
// on the exact keys rather than on a round trip.

func TestConnectRequestWire_TokenIsWriteOnlyAndNeverPrinted(t *testing.T) {
	var out strings.Builder
	calls := recordConnectTo(t, &out, "vke-prod")
	got := calls[procConnectCluster]
	if got["auth"] != "CLUSTER_AUTH_SERVICE_ACCOUNT_TOKEN" {
		t.Errorf("auth = %v, want the ServiceAccount token auth", got["auth"])
	}
	if got["token"] != fakeMintedToken {
		t.Errorf("token = %v, want the minted token to be sent", got["token"])
	}
	if _, ok := got["cloudCluster"]; ok {
		t.Error("the token request carries cloudCluster; the control plane refuses it there")
	}
	// THE TOKEN IS NEVER PRINTED — not in the summary, not in a grant, not
	// anywhere. A credential in a terminal is a credential in a scrollback
	// buffer and a CI log.
	if strings.Contains(out.String(), fakeMintedToken) {
		t.Fatalf("the minted token appears in the command's output:\n%s", out.String())
	}
}

func TestBootstrapManifests_TokenMintsTheCredential(t *testing.T) {
	got := connectRBAC{
		ClusterName:         "vke-prod",
		KubeContext:         "vke-prod",
		Org:                 "acme",
		TokenNamespace:      connectTokenNamespace,
		TokenServiceAccount: connectTokenServiceAccount,
	}.bootstrapManifests()

	for _, want := range []string{
		"kind: Namespace",
		"name: forge-system",
		"kind: ServiceAccount",
		"name: forge-connect",
		// A long-lived Secret token, not a TokenRequest: nothing rotates a
		// bounded one, so a cluster nobody re-connects would stop deploying.
		"type: kubernetes.io/service-account-token",
		"kubernetes.io/service-account.name: forge-connect",
		// The binding's subject is the minted SA, not a User.
		"kind: ServiceAccount\n  name: forge-connect\n  namespace: forge-system",
		`resourceNames: ["system:serviceaccount:flux-acme:reliant-deploy-tenant"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the token bootstrap omits %q:\n%s", want, got)
		}
	}
}

// Every object forge creates carries the managed-by label, which is what makes
// disconnect's cleanup safe to run by name.
func TestBootstrapManifests_LabelsEverythingForgeCreates(t *testing.T) {
	got := connectRBAC{
		ClusterName: "c", Org: "o",
		TokenNamespace: connectTokenNamespace, TokenServiceAccount: connectTokenServiceAccount,
	}.bootstrapManifests()
	docs := strings.Split(got, "\n---\n")
	if len(docs) != 5 {
		t.Fatalf("got %d objects, want 5 (Namespace, SA, Secret, ClusterRole, ClusterRoleBinding)", len(docs))
	}
	for _, doc := range docs {
		if !strings.Contains(doc, forgeManagedLabelKey+": "+forgeManagedLabelValue) {
			t.Errorf("an object carries no managed-by label:\n%s", doc)
		}
		if !strings.Contains(doc, "forge.dev/connected-cluster: c") {
			t.Errorf("an object is not attributed to its connected cluster:\n%s", doc)
		}
	}
}

// ── The KCL binding and its refusal ─────────────────────────────────────────

func TestClusterBindingsOf(t *testing.T) {
	e := &KCLEntities{
		ClusterTarget: &ClusterTargetEntity{Cluster: "gke_a_b_prod", ConnectedCluster: "prod-us"},
		Workloads: []WorkloadEntity{
			{Runtime: RuntimeEntity{Type: RuntimeCluster,
				Cluster: &ClusterRuntime{Cluster: "gke_a_b_prod", ConnectedCluster: "prod-us"}}},
			{Runtime: RuntimeEntity{Type: RuntimeCluster,
				Cluster: &ClusterRuntime{Cluster: "vke-eu", ConnectedCluster: "eu-west"}}},
			// A host workload contributes no binding.
			{Runtime: RuntimeEntity{Type: RuntimeHost, Host: &HostRuntime{}}},
		},
	}
	got := clusterBindingsOf(e)
	want := []clusterBinding{
		{Cluster: "gke_a_b_prod", ConnectedCluster: "prod-us"},
		{Cluster: "vke-eu", ConnectedCluster: "eu-west"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d bindings, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("binding %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRefuseUnboundClusterTargets(t *testing.T) {
	cp := &ControlPlaneEntity{Endpoint: "https://cp.example"}

	// A cloud cluster with no connected name: refused, because the control
	// plane would record the deploy with nowhere to send it.
	err := refuseUnboundClusterTargets("prod", &KCLEntities{
		ControlPlane:  cp,
		ClusterTarget: &ClusterTargetEntity{Cluster: "gke_a_b_prod"},
	})
	if err == nil {
		t.Fatal("a control-plane env targeting an unbound cloud cluster was accepted")
	}
	for _, want := range []string{"forge cluster connect", "connected_cluster", `"gke_a_b_prod"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal omits %q: %v", want, err)
		}
	}

	// Bound: fine.
	if err := refuseUnboundClusterTargets("prod", &KCLEntities{
		ControlPlane:  cp,
		ClusterTarget: &ClusterTargetEntity{Cluster: "gke_a_b_prod", ConnectedCluster: "prod-us"},
	}); err != nil {
		t.Errorf("a bound target was refused: %v", err)
	}

	// A k3d cluster needs no registration — forge applies to it directly,
	// and requiring one would mean connecting a cluster that exists on one
	// laptop.
	if err := refuseUnboundClusterTargets("dev", &KCLEntities{
		ControlPlane:  cp,
		ClusterTarget: &ClusterTargetEntity{Cluster: "k3d-demo"},
	}); err != nil {
		t.Errorf("a k3d target was refused: %v", err)
	}

	// An env with no control plane declares nothing to ship, so there is
	// nothing to bind.
	if err := refuseUnboundClusterTargets("local", &KCLEntities{
		ClusterTarget: &ClusterTargetEntity{Cluster: "gke_a_b_prod"},
	}); err != nil {
		t.Errorf("an env with no control plane was refused: %v", err)
	}
}

// The refusal fires at the RENDER, which is the one place every path that
// could ship the env passes through. A version of this that only tested the
// predicate passed with the call site deleted from RenderKCLWith — exactly the
// regression it is meant to catch.
func TestRenderRefusesAnUnboundCloudTarget(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "entities.json")
	if err := os.WriteFile(fixture, []byte(`{
      "control_plane": {"type": "control_plane", "endpoint": "https://cp.example"},
      "cluster_target": {"cluster": "gke_acme_us-central1_prod", "namespace": "acme-prod"},
      "workloads": []
    }`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", fixture)

	_, err := RenderKCLWith(context.Background(), t.TempDir(), "prod", nil)
	if err == nil {
		t.Fatal("the render accepted a control-plane env whose cloud target names no connected cluster")
	}
	if !strings.Contains(err.Error(), "forge cluster connect") {
		t.Errorf("the refusal does not name the fix: %v", err)
	}
}

// The same render, with the binding declared, renders.
func TestRenderAcceptsABoundCloudTarget(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "entities.json")
	if err := os.WriteFile(fixture, []byte(`{
      "control_plane": {"type": "control_plane", "endpoint": "https://cp.example"},
      "cluster_target": {"cluster": "gke_acme_us-central1_prod", "namespace": "acme-prod",
                         "connected_cluster": "prod-us"},
      "workloads": []
    }`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", fixture)

	got, err := RenderKCLWith(context.Background(), t.TempDir(), "prod", nil)
	if err != nil {
		t.Fatalf("RenderKCLWith: %v", err)
	}
	if got.ClusterTarget.ConnectedCluster != "prod-us" {
		t.Errorf("connected_cluster = %q, want prod-us (the field must survive the render)",
			got.ClusterTarget.ConnectedCluster)
	}
}

func TestResolveClusterBindings_ResolvesNamesToIDs(t *testing.T) {
	c := &fakeConnectCaller{clusters: []wireConnectedCluster{
		{ID: "cl_1", Name: "prod-us"}, {ID: "cl_2", Name: "eu-west"},
	}}
	got, err := resolveClusterBindings(context.Background(), c, &KCLEntities{
		ClusterTarget: &ClusterTargetEntity{Cluster: "gke_a_b_prod", ConnectedCluster: "prod-us"},
	})
	if err != nil {
		t.Fatalf("resolveClusterBindings: %v", err)
	}
	want := []deploytarget.ClusterBinding{{Cluster: "gke_a_b_prod", ClusterID: "cl_1"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("bindings = %+v, want %+v", got, want)
	}
}

// A name the org has not connected is an error, never a dropped binding:
// omitting it would record the env with no target and report success.
func TestResolveClusterBindings_RefusesAnUnknownName(t *testing.T) {
	c := &fakeConnectCaller{clusters: []wireConnectedCluster{{ID: "cl_1", Name: "prod-us"}}}
	_, err := resolveClusterBindings(context.Background(), c, &KCLEntities{
		ClusterTarget: &ClusterTargetEntity{Cluster: "gke_a_b_prod", ConnectedCluster: "typo"},
	})
	if err == nil || !strings.Contains(err.Error(), "has not connected") {
		t.Fatalf("error = %v, want one naming the unconnected cluster", err)
	}
}

// The EnsureEnvironment request carries the bindings under the key the control
// plane reads (DeployEnvironmentSpec.clusterBindings, tag 14).
func TestEnsureEnvironmentCarriesClusterBindings(t *testing.T) {
	c := &fakeConnectCaller{}
	_, _, err := deploytarget.EnsureHostedEnvironment(context.Background(), c, deploytarget.HostedEnvRef{
		Name: "prod", Kind: deploytarget.HostedEnvSelfManaged,
		ClusterBindings: []deploytarget.ClusterBinding{{Cluster: "gke_a_b_prod", ClusterID: "cl_1"}},
	})
	if err != nil {
		t.Fatalf("EnsureHostedEnvironment: %v", err)
	}
	spec, _ := c.requests["controlplane.v1.DeployService/EnsureEnvironment"]["spec"].(map[string]any)
	bindings, ok := spec["clusterBindings"].([]map[string]any)
	if !ok || len(bindings) != 1 {
		t.Fatalf("spec.clusterBindings = %#v, want one binding", spec["clusterBindings"])
	}
	if bindings[0]["cluster"] != "gke_a_b_prod" || bindings[0]["clusterId"] != "cl_1" {
		t.Errorf("binding = %#v, want {cluster: gke_a_b_prod, clusterId: cl_1}", bindings[0])
	}
}

// ── Disconnect ──────────────────────────────────────────────────────────────

func TestLookupConnectedCluster(t *testing.T) {
	c := &fakeConnectCaller{clusters: []wireConnectedCluster{
		{ID: "cl_1", Name: "prod-us"}, {ID: "cl_2", Name: "prod-us-2"},
	}}
	// EXACT name, never a prefix: a fuzzy match here is a deletion that hits
	// the wrong cluster.
	got, err := lookupConnectedCluster(context.Background(), c, "prod-us")
	if err != nil {
		t.Fatalf("lookupConnectedCluster: %v", err)
	}
	if got != "cl_1" {
		t.Errorf("id = %q, want cl_1", got)
	}
	if _, err := lookupConnectedCluster(context.Background(), c, "prod"); err == nil {
		t.Error("a partial name resolved; it must not")
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

const fakeMintedToken = "eyJhbGciOi-FAKE-TOKEN"

// fakeConnectCaller records every request and answers the two reads the
// connect/disconnect paths make.
type fakeConnectCaller struct {
	clusters []wireConnectedCluster
	requests map[string]map[string]any
}

func (f *fakeConnectCaller) Call(_ context.Context, procedure string, req, out any) error {
	if f.requests == nil {
		f.requests = map[string]map[string]any{}
	}
	if m, ok := req.(map[string]any); ok {
		f.requests[procedure] = m
	}
	switch procedure {
	case procListClusters:
		if r, ok := out.(*struct {
			Clusters []wireConnectedCluster `json:"clusters"`
		}); ok {
			r.Clusters = f.clusters
		}
	case procConnectCluster:
		if r, ok := out.(*wireConnectClusterResponse); ok {
			name, _ := f.requests[procedure]["name"].(string)
			r.Cluster = wireConnectedCluster{ID: "cl_new", Name: name}
		}
	default:
		// Every other procedure decodes an empty object, which is what the
		// ensure and remove paths expect.
		b, _ := json.Marshal(map[string]any{"environment": map[string]any{"id": "env_1"}})
		_ = json.Unmarshal(b, out)
	}
	return nil
}

// recordConnectTo drives the real runClusterConnect against a fake control
// plane and a fake cluster, and returns what it sent.
func recordConnectTo(t *testing.T, out *strings.Builder, kctx string) map[string]map[string]any {
	t.Helper()

	restoreKube := kubeconfigClusterOf
	restoreClient := clusterConnectClient
	restoreApply := connectApply
	restoreToken := connectReadToken
	t.Cleanup(func() {
		kubeconfigClusterOf = restoreKube
		clusterConnectClient = restoreClient
		connectApply = restoreApply
		connectReadToken = restoreToken
	})

	kubeconfigClusterOf = func(string) (string, string, error) {
		return "https://34.1.2.3", "ca-bundle", nil
	}
	caller := &fakeConnectCaller{}
	clusterConnectClient = func(context.Context, string, string) (cloudCaller, string, error) {
		return caller, "acme", nil
	}
	connectApply = func(context.Context, string, string) error { return nil }
	connectReadToken = func(context.Context, string, connectRBAC) (string, error) {
		return fakeMintedToken, nil
	}

	if err := runClusterConnect(context.Background(), clusterConnectOptions{
		Name: "prod-us", KubeContext: kctx, Env: "prod", Out: out,
	}); err != nil {
		t.Fatalf("runClusterConnect: %v", err)
	}
	return caller.requests
}

// A cluster placed only by a forge.Manifests group (no ClusterTarget names it)
// still gets a bundle tree, so it must be bound too.
func TestRefuseUnboundClusterTargets_EveryRenderedCluster(t *testing.T) {
	cp := &ControlPlaneEntity{Endpoint: "https://cp.example"}
	e := &KCLEntities{
		ControlPlane:     cp,
		ClusterTarget:    &ClusterTargetEntity{Cluster: "gke_a_b_prod", ConnectedCluster: "prod-us"},
		ManifestClusters: []ManifestClusterEntity{{Cluster: "gke_a_b_prod"}, {Cluster: "gke_a_b_daemon"}},
	}
	err := refuseUnboundClusterTargets("prod", e)
	if err == nil {
		t.Fatal("an unbound manifests-only cluster was accepted")
	}
	if !strings.Contains(err.Error(), `"gke_a_b_daemon"`) || strings.Contains(err.Error(), `"gke_a_b_prod"`) {
		t.Errorf("refusal must name only the unbound cluster: %v", err)
	}
	e.HelmCharts = []HelmChartEntity{{Cluster: "gke_a_b_helm"}}
	if err := refuseUnboundClusterTargets("prod", e); err == nil || !strings.Contains(err.Error(), `"gke_a_b_helm"`) {
		t.Errorf("helm-placed cluster not named: %v", err)
	}
}

// prod-daemon's shape: a second cluster placed ONLY by several forge.Manifests
// groups (no workload, no ClusterTarget). One cluster-level binding in
// ConnectedClusters must bind every group on it; without it the render is
// refused, and the binding list the control plane receives must carry it.
func TestConnectedClustersBindEveryGroupOnACluster(t *testing.T) {
	cp := &ControlPlaneEntity{Endpoint: "https://cp.example"}
	e := &KCLEntities{
		ControlPlane:  cp,
		ClusterTarget: &ClusterTargetEntity{Cluster: "gke_a_b_prod", ConnectedCluster: "prod-control-plane"},
		ManifestClusters: []ManifestClusterEntity{
			{Cluster: "gke_a_b_prod"}, {Cluster: "gke_a_b_daemon"}, {Cluster: "gke_a_b_daemon"}, {Cluster: "gke_a_b_daemon"},
		},
		HelmCharts: []HelmChartEntity{{Cluster: "gke_a_b_daemon"}},
	}
	if err := refuseUnboundClusterTargets("prod", e); err == nil {
		t.Fatal("precondition: an unbound manifests-only cluster must be refused")
	}

	e.ConnectedClusters = map[string]string{"gke_a_b_daemon": "prod-daemon"}
	if err := refuseUnboundClusterTargets("prod", e); err != nil {
		t.Fatalf("a cluster bound once at cluster level was refused: %v", err)
	}
	got := clusterBindingsOf(e)
	want := []clusterBinding{
		{Cluster: "gke_a_b_daemon", ConnectedCluster: "prod-daemon"},
		{Cluster: "gke_a_b_prod", ConnectedCluster: "prod-control-plane"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("bindings = %+v, want %+v", got, want)
	}

	// Two declarations that disagree for one context are drift, not a merge.
	e.ConnectedClusters["gke_a_b_prod"] = "someone-else"
	if err := refuseUnboundClusterTargets("prod", e); err == nil || !strings.Contains(err.Error(), "someone-else") {
		t.Fatalf("conflicting bindings accepted: %v", err)
	}
}
