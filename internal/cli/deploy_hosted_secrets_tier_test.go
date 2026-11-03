package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/deploytarget"
	deployv1alpha1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// prodShapeStream is control-plane's prod in miniature (forge bug 382bc13a):
// three Secrets on the clusters this machine applies (two on the hub, one on
// the daemon cluster) beside a hosted StaticSite that reads none of them.
const prodShapeStream = `# cluster: hub
apiVersion: v1
kind: Secret
metadata:
  name: control-plane-operator-sign-in-codes
  namespace: control-plane-prod
---
# cluster: hub
apiVersion: v1
kind: Secret
metadata:
  name: control-plane-ops-automation
  namespace: control-plane-prod
---
# cluster: daemon
apiVersion: v1
kind: Secret
metadata:
  name: alertmanager-config
  namespace: observability-data
---
# cluster: hosted
apiVersion: forge.dev/v1alpha1
kind: StaticSite
metadata:
  name: reliant-web
`

func stubShapeManifests(t *testing.T, stream string) {
	t.Helper()
	prev := projectEnvShapeFn
	projectEnvShapeFn = func(_ context.Context, _ io.Writer, env string) (envShapeDoc, error) {
		return envShapeDoc{Env: env, manifests: stream}, nil
	}
	t.Cleanup(func() { projectEnvShapeFn = prev })
}

func mixedProdEntities(hosted ...WorkloadEntity) *KCLEntities {
	return &KCLEntities{
		ManifestClusters: []ManifestClusterEntity{
			{Cluster: "daemon", Namespace: "observability-data"},
			{Cluster: "hub", Namespace: "control-plane-prod"},
			{Cluster: "hosted"},
		},
		Workloads: hosted,
	}
}

func hostedGroupOf(names ...string) []deploytarget.ServiceGroup {
	g := deploytarget.ServiceGroup{Env: "prod", ProviderID: deploytarget.HostedProviderID}
	for _, n := range names {
		g.Services = append(g.Services, deploytarget.ResolvedService{Name: n})
	}
	return []deploytarget.ServiceGroup{g}
}

// A mixed env's hosted tier is judged on what IT consumes: the cluster tiers'
// Secrets, synced by this machine's own apply, do not block a hosted
// StaticSite that reads none of them.
func TestHostedTierIsNotRefusedForClusterTierSecrets(t *testing.T) {
	stubShapeManifests(t, prodShapeStream)
	if err := refuseUnsyncableHostedSecretsFor(context.Background(), "prod", mixedProdEntities(), hostedGroupOf()); err != nil {
		t.Fatalf("a hosted StaticSite beside three cluster-only Secrets was refused: %v", err)
	}
}

// A hosted workload that names a cluster tier's Secret still needs it supplied
// where it runs, which a cluster-tier copy is not: that stays a refusal.
func TestHostedTierIsRefusedForASecretItConsumes(t *testing.T) {
	stubShapeManifests(t, prodShapeStream)
	api := WorkloadEntity{Name: "api", Runtime: RuntimeEntity{Type: RuntimeHosted}}
	api.Spec.Env = []deployv1alpha1.EnvVar{{
		Name:      "OPS_TOKEN",
		SecretRef: &deployv1alpha1.SecretKeyRef{Name: "control-plane-ops-automation", Key: "token"},
	}}
	err := refuseUnsyncableHostedSecretsFor(context.Background(), "prod", mixedProdEntities(api), hostedGroupOf("api"))
	if err == nil || !strings.Contains(err.Error(), "control-plane-prod/control-plane-ops-automation") {
		t.Fatalf("want a refusal naming the Secret the hosted api consumes, got %v", err)
	}
	if strings.Contains(err.Error(), "alertmanager-config") || strings.Contains(err.Error(), "sign-in-codes") {
		t.Fatalf("the refusal names cluster-only Secrets the hosted tier does not consume: %v", err)
	}
}

// A pure-hosted env applies nothing from this machine, so every Secret its
// render needs is one forge cannot deliver: unchanged behaviour.
func TestPureHostedEnvKeepsEverySecretRefusal(t *testing.T) {
	stubShapeManifests(t, prodShapeStream)
	err := refuseUnsyncableHostedSecretsFor(context.Background(), "prod", &KCLEntities{}, hostedGroupOf())
	if err == nil || !strings.Contains(err.Error(), "observability-data/alertmanager-config") {
		t.Fatalf("a pure-hosted env must still be refused for its Secrets, got %v", err)
	}
}

// A hosted object that names a cluster tier's Secret in a field the workload
// scan never sees (here a rendered volume's secretName, and a CRD field) still
// needs it supplied where it runs: that stays a refusal, and only for the
// Secrets it names.
func TestHostedTierIsRefusedForASecretItsRenderedObjectsName(t *testing.T) {
	stubShapeManifests(t, prodShapeStream+`---
# cluster: hosted
apiVersion: apps/v1
kind: Deployment
metadata:
  name: hosted-api
spec:
  template:
    spec:
      volumes:
        - name: codes
          secret:
            secretName: control-plane-operator-sign-in-codes
---
# cluster: hosted
apiVersion: forge.dev/v1alpha1
kind: HostedProvider
metadata:
  name: alerts
spec:
  secret_refs: [alertmanager-config]
`)
	err := refuseUnsyncableHostedSecretsFor(context.Background(), "prod", mixedProdEntities(), hostedGroupOf())
	if err == nil {
		t.Fatal("a hosted object naming a cluster tier's Secret was not refused")
	}
	for _, want := range []string{"control-plane-prod/control-plane-operator-sign-in-codes", "observability-data/alertmanager-config"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %s, which a hosted object consumes: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "ops-automation") {
		t.Fatalf("the refusal names a cluster-only Secret no hosted object consumes: %v", err)
	}
}
