package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
)

// simpleBackendProject writes a one-service KCL project whose only workload
// is a SimpleBackend, and returns its directory. Shared by the tests below so
// every one of them asserts against the SAME rendered bytes — see
// TestSimpleBackend_RoundTripsKCLToProviderSpec for why that matters.
func simpleBackendProject(t *testing.T) string {
	t.Helper()
	moduleRoot := forgeModuleRoot(t)
	dir := t.TempDir()

	kclMod := "[package]\nname = \"simplebackend\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n\n[dependencies]\nforge = { path = \"" + moduleRoot + "\" }\n"
	if err := os.WriteFile(filepath.Join(dir, "kcl.mod"), []byte(kclMod), 0o644); err != nil {
		t.Fatal(err)
	}

	// The env registry deliberately differs from the image's registry: the
	// app owner's pinned reference must survive both it and the image_tag.
	main := `import forge

_api = forge.RenderedWorkload {
    name = "acme-api"
    deploy = forge.SimpleBackend {
        cluster = "gke_acme_us-central1_hosted"
        namespace = "acme-prod"
        image = "ghcr.io/acme/api:v1.4.2"
        ports = [8080]
        network = "public"
        domain = "api-acme.reliantapps.dev"
        storage_gib = 20
        resources = forge.Resources {
            cpu_request_millicores = 500
            cpu_limit_millicores = 1000
            memory_request_bytes = forge.gib(2)
            memory_limit_bytes = forge.gib(2)
        }
        health_check = forge.HealthCheck {
            liveness_path = "/healthz"
            http_port = 8080
            initial_delay = 7
        }
        env_vars = [
            forge.EnvVar {name = "LOG_LEVEL", value = "info"}
            forge.EnvVar {name = "DB_PASSWORD", secret_ref = "acme-prod-db", secret_key = "password"}
        ]
    }
}

_bundle = forge.Bundle {
    project = "hosted"
    cluster_target = forge.ClusterTarget {
        cluster = "gke_acme_us-central1_hosted"
        namespace = "acme-prod"
        registry = "ghcr.io/reliant-labs"
    }
    services = [_api]
}

output = forge.render(_bundle)
manifests = forge.render_manifests(_bundle, "v9-should-not-appear", {}, False)
`
	if err := os.WriteFile(filepath.Join(dir, "main.k"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestSimpleBackend_RoundTripsKCLToProviderSpec is the ANTI-DRIFT test for
// this deploy tier, and it is deliberately one test spanning three layers
// rather than three tests each pinning its own layer.
//
// The failure it exists to prevent has happened twice in this project: a KCL
// fixture and a Go test came to assert OPPOSITE things about the same
// function, and nothing noticed until one of them went red — at which point
// four stale assertions were found together. That is possible whenever the
// two sides each construct their own input. A Go test that hand-builds a
// ServiceEntity literal is asserting about a struct, not about anything KCL
// produces, so it stays green while the schema it claims to mirror moves.
//
// So this test never constructs a ServiceEntity. It renders REAL KCL through
// the real render seam, unmarshals through the real dispatchServiceDeploy,
// and groups through the real buildDeployGroups — the exact path `forge env
// deploy` takes. Every assertion below is therefore a claim about the
// pipeline, and a KCL-side change that breaks the contract fails HERE as well
// as in kcl/tests/positive_simple_backend.k, which is what makes the two
// unable to disagree.
func TestSimpleBackend_RoundTripsKCLToProviderSpec(t *testing.T) {
	dir := simpleBackendProject(t)
	kclplugin.Register()

	out, err := kclrender.Run(dir, dir, nil)
	if err != nil {
		t.Fatalf("render simple-backend project: %v", err)
	}

	// ── layer 1: KCL -> the Go entity contract ────────────────────────
	entities, err := parseKCLEntities(out)
	if err != nil {
		t.Fatalf("parse entities: %v\n%s", err, out)
	}
	if len(entities.Services) != 1 {
		t.Fatalf("want 1 service, got %d", len(entities.Services))
	}
	svc := entities.Services[0]

	if svc.Deploy.Type != "simple-backend" {
		t.Fatalf("deploy.type = %q, want simple-backend — the entity contract must KEEP the discriminator even though manifests take the cluster path, or the tier is invisible to `forge project audit`", svc.Deploy.Type)
	}
	sb := svc.Deploy.SimpleBackend
	if sb == nil {
		t.Fatal("deploy.simple_backend is nil: dispatchServiceDeploy did not populate the variant")
	}
	if sb.Cluster != "gke_acme_us-central1_hosted" || sb.Namespace != "acme-prod" {
		t.Errorf("target coordinates = %q/%q", sb.Cluster, sb.Namespace)
	}
	if sb.Image != "ghcr.io/acme/api:v1.4.2" {
		t.Errorf("image = %q, want the app owner's pinned reference verbatim", sb.Image)
	}
	if sb.Network != "public" || sb.Domain != "api-acme.reliantapps.dev" {
		t.Errorf("network/domain = %q/%q", sb.Network, sb.Domain)
	}
	if sb.StorageGiB != 20 {
		t.Errorf("storage_gib = %d, want 20", sb.StorageGiB)
	}
	// Resources arrive in NEUTRAL units, not k8s quantity strings. This is
	// the assertion that pins the projection choice: they are a metering
	// input, and the platform's shape rule is arithmetic on millicores and
	// bytes. A regression to _render_resources would produce "500m"/"2Gi"
	// and these would come back zero — silently, since the JSON keys differ.
	if sb.Resources.CPURequestMillicores != 500 || sb.Resources.MemoryRequestBytes != 2*1024*1024*1024 {
		t.Errorf("resources = %+v, want neutral 500 millicores / 2 GiB in bytes", sb.Resources)
	}
	if sb.HealthCheck == nil || sb.HealthCheck.LivenessPath != "/healthz" || sb.HealthCheck.InitialDelay != 7 {
		t.Errorf("health_check = %+v", sb.HealthCheck)
	}

	// forge must NOT synthesize a build for an image it did not produce.
	// Without this, `forge build` would try to compile ./cmd/acme-api.
	if b := svc.EffectiveBuild(); b.Type != "" {
		t.Errorf("EffectiveBuild = %q, want empty: forge neither builds nor pushes a SimpleBackend's image", b.Type)
	}

	// ── layer 2: the entity contract -> deploy groups ─────────────────
	groups, err := buildDeployGroups("hosted", entities, "fallback-ns")
	if err != nil {
		t.Fatalf("buildDeployGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %d: %+v", len(groups), groups)
	}
	g := groups[0]
	// Routed to the EXISTING k8s-cluster provider. This is the design: the
	// manifests were projected onto the cluster shape inside KCL, so apply,
	// prune, rollout-wait and the per-group --context discipline are the
	// ones K8sClusterProvider already implements against clusters forge did
	// not create. A second provider here would be a second copy of all four.
	if g.ProviderID != "k8s-cluster" {
		t.Errorf("ProviderID = %q, want k8s-cluster", g.ProviderID)
	}
	if g.Cluster != "gke_acme_us-central1_hosted" || g.Namespace != "acme-prod" {
		t.Errorf("group target = %q/%q", g.Cluster, g.Namespace)
	}
	// The group must NOT fall back to the env namespace: a hosted workload
	// landing in the shared namespace is a cross-customer leak, not a default.
	if g.Namespace == "fallback-ns" {
		t.Error("group took the fallback namespace instead of the app owner's own")
	}
	if len(g.Services) != 1 || g.Services[0].K8sCluster == nil {
		t.Fatalf("group services = %+v", g.Services)
	}
	if got := g.Services[0].K8sCluster.Replicas; got != 1 {
		t.Errorf("replicas = %d, want 1: storage_gib renders a ReadWriteOnce PVC, which a multi-replica Deployment cannot roll over", got)
	}
	// The provider registry must actually resolve it — the registration
	// trap: an unregistered provider is skipped SILENTLY on some paths.
	if p := deploytarget.NewRegistry().Lookup(g.ProviderID); p == nil {
		t.Fatalf("no registered provider for %q", g.ProviderID)
	}

	// ── layer 3: the same render's MANIFESTS ──────────────────────────
	// Asserted off the SAME bytes as layers 1 and 2, which is the point:
	// entity and manifest cannot describe different workloads here.
	var m struct {
		Manifests []map[string]any `json:"manifests"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal manifests: %v", err)
	}
	byKind := map[string][]map[string]any{}
	for _, obj := range m.Manifests {
		kind, _ := obj["kind"].(string)
		byKind[kind] = append(byKind[kind], obj)
	}
	if n := len(byKind["Deployment"]); n != 1 {
		t.Fatalf("want 1 Deployment, got %d", n)
	}
	if n := len(byKind["PersistentVolumeClaim"]); n != 1 {
		t.Fatalf("want 1 PVC (the one manifest kind this tier adds to forge), got %d", n)
	}

	// ── the OBSERVER's half of the PVC ────────────────────────────────
	//
	// forge EMITS this claim, so forge must be able to read it back —
	// otherwise a claim that never binds shows up only as the
	// Deployment's "0/1 ready", which names the symptom while the cause
	// (no default StorageClass, or a size the class refused) sits one
	// object away, unobserved.
	//
	// The claim name is decided by the KCL render and re-derived in Go by
	// simpleBackendOwnedClaims. That duplication is unavoidable — the
	// observer cannot ask KCL at read time — so it is pinned HERE,
	// against the name the real renderer actually emitted in the very
	// same bytes this test already parsed. A drift in either direction
	// fails: rename it in render.k and the Go side no longer matches;
	// change the Go derivation and it no longer matches the manifest.
	pvcMeta, _ := byKind["PersistentVolumeClaim"][0]["metadata"].(map[string]any)
	renderedClaim, _ := pvcMeta["name"].(string)
	if renderedClaim == "" {
		t.Fatal("rendered PVC has no metadata.name")
	}
	observed := g.Services[0].K8sCluster.OwnedClaims
	if len(observed) != 1 || observed[0] != renderedClaim {
		t.Errorf("OwnedClaims = %v, but the render emitted a PVC named %q.\n"+
			"K8sClusterProvider.Observe reads the claims named here; a mismatch means forge "+
			"emits a PVC it cannot observe, and reports the workload green while its storage "+
			"is unbound.", observed, renderedClaim)
	}
	container := firstContainer(t, byKind["Deployment"][0])
	// Neither the env registry (ghcr.io/reliant-labs) nor the env-wide tag
	// ("v9-should-not-appear") may rewrite a reference forge did not build.
	if got, _ := container["image"].(string); got != sb.Image {
		t.Errorf("container image = %q, want the entity's %q — the env registry or image_tag leaked in", got, sb.Image)
	}
}

// TestSimpleBackend_NetworkNoneDropsService pins that `network` is a manifest
// difference rather than a label, from the Go side, against a real render.
//
// The KCL fixture (kcl/tests/positive_simple_backend_network.k) pins the same
// property. Both exist on purpose and both read the same rendered output
// shape, so a change that satisfied one while breaking the other would have
// to make the renderer emit two different things at once.
func TestSimpleBackend_NetworkNoneDropsService(t *testing.T) {
	moduleRoot := forgeModuleRoot(t)
	dir := t.TempDir()

	kclMod := "[package]\nname = \"sbnone\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n\n[dependencies]\nforge = { path = \"" + moduleRoot + "\" }\n"
	if err := os.WriteFile(filepath.Join(dir, "kcl.mod"), []byte(kclMod), 0o644); err != nil {
		t.Fatal(err)
	}
	main := `import forge

_worker = forge.RenderedWorkload {
    name = "worker"
    deploy = forge.SimpleBackend {
        cluster = "hosted"
        namespace = "acme-prod"
        image = "ghcr.io/acme/worker:v2"
        network = "none"
    }
}

_bundle = forge.Bundle {
    project = "hosted"
    cluster_target = forge.ClusterTarget {
        cluster = "hosted"
        namespace = "acme-prod"
        registry = "ghcr.io/reliant-labs"
    }
    services = [_worker]
}

output = forge.render(_bundle)
manifests = forge.render_manifests(_bundle, "latest", {}, False)
`
	if err := os.WriteFile(filepath.Join(dir, "main.k"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}

	kclplugin.Register()
	out, err := kclrender.Run(dir, dir, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	var m struct {
		Manifests []map[string]any `json:"manifests"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var deployments, services, netpols int
	for _, obj := range m.Manifests {
		switch kind, _ := obj["kind"].(string); kind {
		case "Deployment":
			deployments++
		case "Service":
			services++
		case "NetworkPolicy":
			netpols++
		}
	}
	if deployments != 1 {
		t.Errorf("Deployments = %d, want 1: `network` governs reachability, not whether the workload runs", deployments)
	}
	if services != 0 {
		t.Errorf("Services = %d, want 0 for network = \"none\"", services)
	}
	// "none" is ADDRESSABILITY, not containment. Emitting a NetworkPolicy
	// here would let an operator read it as an egress boundary it is not;
	// egress is the namespace's concern (kcl/lib/netpol.k).
	if netpols != 0 {
		t.Errorf("NetworkPolicies = %d, want 0: network = \"none\" makes no containment claim", netpols)
	}
}

// firstContainer digs the primary container out of a rendered Deployment.
func firstContainer(t *testing.T, deployment map[string]any) map[string]any {
	t.Helper()
	spec, _ := deployment["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	containers, _ := podSpec["containers"].([]any)
	if len(containers) == 0 {
		t.Fatal("Deployment has no containers")
	}
	c, _ := containers[0].(map[string]any)
	return c
}
