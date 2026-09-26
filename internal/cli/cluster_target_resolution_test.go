package cli

import (
	"testing"
)

// clusterTargetJSON is the render shape that exposed the defect: the env's
// WORKLOADS are agnostic forge.Services (folded into `services` AFTER
// `bundle.services`), and `bundle.services` holds an image-less infra service
// that pins its manifests to a SECOND cluster. forge.render emits
// `services = bundle.services + projected(bundle.workloads)`, so the infra
// service comes first in the list even though it is the exception, not the
// env.
//
// The env's declared target — the Bundle's `cluster_target` — names the
// control-plane cluster and namespace. That declaration is what every
// env-wide question must be answered from.
const clusterTargetJSON = `{
  "output": {
    "cluster_target": {
      "cluster": "gke_proj_us-central1_prod",
      "namespace": "control-plane-prod",
      "registry": "us-central1-docker.pkg.dev/proj/reliant-prod",
      "platform": "amd64",
      "image_tag": "stable"
    },
    "services": [
      {"name": "workspace-base", "deploy": {"type": "build-only"}},
      {"name": "kata-prepull", "deploy": {"type": "cluster", "cluster": "gke_proj_us-central1-a_prod-daemon-v2", "namespace": "kata-prepull", "registry": "us-central1-docker.pkg.dev/proj/other", "platform": "arm64"}},
      {"name": "admin-server", "image": "control-plane", "deploy": {"type": "cluster", "cluster": "gke_proj_us-central1_prod", "namespace": "control-plane-prod", "registry": "us-central1-docker.pkg.dev/proj/reliant-prod", "platform": "amd64"}}
    ]
  }
}`

func parseClusterTargetFixture(t *testing.T) *KCLEntities {
	t.Helper()
	entities, err := parseKCLEntities([]byte(clusterTargetJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	return entities
}

// TestParseKCLEntities_ClusterTarget pins that the declared env target
// survives the parse. Without it every consumer below is left to guess.
func TestParseKCLEntities_ClusterTarget(t *testing.T) {
	e := parseClusterTargetFixture(t)
	if e.ClusterTarget == nil {
		t.Fatal("cluster_target was dropped during parse")
	}
	if e.ClusterTarget.Namespace != "control-plane-prod" || e.ClusterTarget.Cluster != "gke_proj_us-central1_prod" {
		t.Errorf("cluster_target parsed as %+v", *e.ClusterTarget)
	}
}

// TestK8sClusterField_PrefersDeclaredClusterTarget is the regression. Before
// the fix, the env namespace resolved to the FIRST cluster-shaped service's —
// the infra service's "kata-prepull" — and `forge env render/deploy prod`
// silently re-rendered every prod object into that namespace. The cluster,
// registry and domain fields go through the same resolver and had the same
// defect.
func TestK8sClusterField_PrefersDeclaredClusterTarget(t *testing.T) {
	e := parseClusterTargetFixture(t)
	for field, want := range map[string]string{
		"namespace": "control-plane-prod",
		"cluster":   "gke_proj_us-central1_prod",
		"registry":  "us-central1-docker.pkg.dev/proj/reliant-prod",
	} {
		if got := k8sClusterFieldFromEntities(e, field); got != want {
			t.Errorf("%s: got %q, want the declared cluster_target's %q", field, got, want)
		}
	}
}

// TestMainClusterForEntities_PrefersDeclaredClusterTarget: operators and
// cronjobs carry no deploy block and are attributed to the env's main
// cluster. That must be the declared target, not whichever cross-cluster
// service happens to render first — otherwise the workspace operator is
// applied to the daemon cluster and dropped from its own.
func TestMainClusterForEntities_PrefersDeclaredClusterTarget(t *testing.T) {
	e := parseClusterTargetFixture(t)
	if got := mainClusterForEntities(e, nil); got != "gke_proj_us-central1_prod" {
		t.Errorf("main cluster: got %q, want the declared cluster_target's", got)
	}
}

// TestKCLFirstClusterPlatform_PrefersDeclaredClusterTarget: the build arch for
// the env's project image is the env's declared platform, not a secondary
// cluster's.
func TestKCLFirstClusterPlatform_PrefersDeclaredClusterTarget(t *testing.T) {
	e := parseClusterTargetFixture(t)
	if got := kclFirstClusterPlatform(e); got != "amd64" {
		t.Errorf("platform: got %q, want the declared cluster_target's amd64", got)
	}
}

// TestK8sClusterField_NoClusterTargetKeepsServiceFallback: an env that
// declares no cluster_target (a hand-rolled contract, the in-tree test
// fixtures) still resolves from its services exactly as before.
func TestK8sClusterField_NoClusterTargetKeepsServiceFallback(t *testing.T) {
	e, err := parseKCLEntities([]byte(`{"services": [
	  {"name": "a", "deploy": {"type": "cluster", "cluster": "k3d-x", "namespace": "ns-x", "registry": "r", "platform": "arm64"}}
	]}`))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	if e.ClusterTarget != nil {
		t.Fatalf("no cluster_target declared, parsed %+v", *e.ClusterTarget)
	}
	if got := k8sClusterFieldFromEntities(e, "namespace"); got != "ns-x" {
		t.Errorf("namespace fallback: got %q", got)
	}
	if got := mainClusterForEntities(e, nil); got != "k3d-x" {
		t.Errorf("main cluster fallback: got %q", got)
	}
	if got := kclFirstClusterPlatform(e); got != "arm64" {
		t.Errorf("platform fallback: got %q", got)
	}
}
