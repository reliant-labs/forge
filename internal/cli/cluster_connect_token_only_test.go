package cli

import (
	"context"
	"strings"
	"testing"
)

// ONE AUTH. A GKE context connects exactly like any other cluster: forge mints
// the scoped ServiceAccount token and uploads it write-only. Before, `--auth
// auto` turned every gke_ context into GKE workload identity, which needed
// gcloud on the operator's machine and an IAM grant in the owner's project.
func TestClusterConnect_GKEContextUploadsAToken(t *testing.T) {
	restoreKube, restoreClient := kubeconfigClusterOf, clusterConnectClient
	restoreApply, restoreToken := connectApply, connectReadToken
	t.Cleanup(func() {
		kubeconfigClusterOf, clusterConnectClient = restoreKube, restoreClient
		connectApply, connectReadToken = restoreApply, restoreToken
	})
	kubeconfigClusterOf = func(string) (string, string, error) {
		return "https://172.16.0.34", "ca-bundle", nil
	}
	caller := &fakeConnectCaller{}
	clusterConnectClient = func(context.Context, string, string) (cloudCaller, string, error) {
		return caller, "acme", nil
	}
	var applied []string
	connectApply = func(_ context.Context, kctx, manifests string) error {
		applied = append(applied, manifests)
		return nil
	}
	connectReadToken = func(context.Context, string, connectRBAC) (string, error) {
		return fakeMintedToken, nil
	}

	var out strings.Builder
	if err := runClusterConnect(context.Background(), clusterConnectOptions{
		Name: "prod-control-plane", KubeContext: "gke_acme_us-central1_prod", Env: "prod", Out: &out,
	}); err != nil {
		t.Fatalf("runClusterConnect: %v\n%s", err, out.String())
	}

	req := caller.requests[procConnectCluster]
	if req["auth"] != authToken {
		t.Errorf("auth = %v, want %s: a GKE context connects by token like any other", req["auth"], authToken)
	}
	if req["token"] != fakeMintedToken {
		t.Errorf("token = %v, want the minted ServiceAccount token", req["token"])
	}
	if req["address"] != "https://172.16.0.34" {
		t.Errorf("address = %v, want the context's own server (the operator picks the address the hub dials)", req["address"])
	}
	if _, ok := req["cloudCluster"]; ok {
		t.Error("the request carries cloudCluster; a token connection has none")
	}
	if len(applied) == 0 || !strings.Contains(applied[0], "kind: ServiceAccount") {
		t.Errorf("connect did not mint the ServiceAccount before uploading its token:\n%v", applied)
	}
	if strings.Contains(out.String(), fakeMintedToken) {
		t.Error("the minted token appears in the command's output")
	}
	if strings.Contains(out.String(), "gcloud") {
		t.Errorf("the output still prints a gcloud grant:\n%s", out.String())
	}
}

// `--auth` is gone: with one mode, a flag that selects it is noise, and a
// script still passing `--auth gcp` must fail loudly rather than connect a
// cluster some other way than its author expected.
func TestClusterConnect_HasNoAuthFlag(t *testing.T) {
	cmd := newClusterConnectCmd()
	if f := cmd.Flags().Lookup("auth"); f != nil {
		t.Fatalf("forge cluster connect still has --auth (default %q)", f.DefValue)
	}
	cmd.SetArgs([]string{"prod-us", "--context", "gke_acme_us-central1_prod", "--env", "prod", "--auth", "gcp"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --auth") {
		t.Fatalf("`--auth gcp` = %v, want an unknown-flag error", err)
	}
}

// THE WHOLE CLUSTER, TO ONE ORG'S PINNED IDENTITY — and the connect identity
// itself reduced to becoming it. Before, the connect role carried namespace,
// ServiceAccount, Role and RoleBinding writes (with bind and escalate), and the
// identity Flux actually applies as was bound to nothing, so every apply of a
// cluster-scoped object was Forbidden.
func TestConnectRBAC_GrantsTheWholeClusterToTheOrgsIdentityOnly(t *testing.T) {
	got := connectRBAC{
		ClusterName:         "prod-us",
		KubeContext:         "gke_acme_us-central1_prod",
		Org:                 "acme",
		TokenNamespace:      connectTokenNamespace,
		TokenServiceAccount: connectTokenServiceAccount,
	}.bootstrapManifests()
	docs := strings.Split(got, "\n---\n")

	var deploy, role string
	for _, d := range docs {
		switch {
		case strings.Contains(d, "kind: ClusterRoleBinding") && strings.Contains(d, "name: forge-connect-prod-us-deploy"):
			deploy = d
		case strings.HasPrefix(d, "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\n"):
			role = d
		}
	}
	if deploy == "" {
		t.Fatalf("no ClusterRoleBinding forge-connect-prod-us-deploy grants the org's deploy identity anything:\n%s", got)
	}
	for _, want := range []string{
		"name: cluster-admin",
		"kind: ServiceAccount\n  name: reliant-deploy-tenant\n  namespace: flux-acme\n",
	} {
		if !strings.Contains(deploy, want) {
			t.Errorf("the deploy binding omits %q:\n%s", want, deploy)
		}
	}
	if strings.Count(deploy, "- kind:") != 1 {
		t.Errorf("the deploy binding names more than one subject:\n%s", deploy)
	}

	// The connect identity: impersonate the deploy name, nothing else.
	if strings.Count(role, "- apiGroups:") != 1 {
		t.Errorf("the connect ClusterRole carries more than the one impersonate rule:\n%s", role)
	}
	for _, want := range []string{`resources: ["serviceaccounts"]`, `verbs: ["impersonate"]`,
		`resourceNames: ["reliant-deploy-tenant"]`} {
		if !strings.Contains(role, want) {
			t.Errorf("the connect ClusterRole omits %q:\n%s", want, role)
		}
	}
	for _, gone := range []string{"namespaces", "rolebindings", "escalate", "bind", `"users"`} {
		if strings.Contains(role, gone) {
			t.Errorf("the connect ClusterRole still carries %q:\n%s", gone, role)
		}
	}
}

// Disconnect removes the grant it made: the org's whole-cluster binding goes
// with the rest, by exact name.
func TestDisconnect_RemovesTheDeployGrant(t *testing.T) {
	var out strings.Builder
	caller := &fakeConnectCaller{clusters: []wireConnectedCluster{{ID: "cl_1", Name: "prod-us"}}}
	restore := clusterConnectClient
	t.Cleanup(func() { clusterConnectClient = restore })
	clusterConnectClient = func(context.Context, string, string) (cloudCaller, string, error) {
		return caller, "acme", nil
	}
	if err := runClusterDisconnect(context.Background(), "prod-us", "", "prod", "", &out); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if !strings.Contains(out.String(), "forge-connect-prod-us-deploy") {
		t.Errorf("the manual cleanup omits the deploy grant:\n%s", out.String())
	}
}
