package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
)

// writeSimpleBackendProject writes a one-service KCL project whose only
// workload is the given SimpleBackend deploy block, and returns its directory.
func writeSimpleBackendProject(t *testing.T, name, deployBlock string) string {
	t.Helper()
	dir := t.TempDir()
	kclMod := "[package]\nname = \"simplebackend\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n\n[dependencies]\nforge = { path = \"" + forgeModuleRoot(t) + "\" }\n"
	// The env registry deliberately differs from the image's registry: the
	// app owner's pinned reference must survive both it and the image_tag.
	main := `import forge
import forge.tiers

_api = forge.RenderedWorkload {
    name = "` + name + `"
    deploy = ` + deployBlock + `
}

_bundle = forge.Bundle {
    project = "hosted"
    env = "prod"
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
	for f, c := range map[string]string{"kcl.mod": kclMod, "main.k": main} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const acmeAPIBlock = `forge.SimpleBackend {
        cluster = "gke_acme_us-central1_hosted"
        namespace = "acme-prod"
        spec = tiers.SimpleBackend {
            image = "ghcr.io/acme/api:v1.4.2"
            ports = [8080]
            network = "public"
            domains = ["api-acme.reliantapps.dev"]
            storageGiB = 20
            resources = tiers.Resources { cpuRequestMillicores = 500, cpuLimitMillicores = 1000, memoryRequestBytes = 2147483648, memoryLimitBytes = 2147483648 }
            healthCheck = tiers.HealthCheck { port = 8080, path = "/healthz", initialDelaySeconds = 7 }
            env = [
                tiers.EnvVar { name = "LOG_LEVEL", value = "info" }
                tiers.EnvVar { name = "DB_PASSWORD", secretRef = tiers.SecretKeyRef { name = "acme-prod-db", key = "password" } }
                tiers.EnvVar { name = "DATABASE_URL", databaseRef = tiers.DatabaseRef { name = "orders" } }
            ]
        }
    }`

// appliedObjects runs KCL output through cluster.ExtractManifests (exactly the
// stream kubectl receives, with forge.dev declarations expanded by
// pkg/deploy.Render) and returns the objects grouped by kind.
func appliedObjects(t *testing.T, kclOut []byte) map[string][]map[string]any {
	t.Helper()
	stream, err := cluster.ExtractManifests(kclOut)
	if err != nil {
		t.Fatalf("ExtractManifests: %v", err)
	}
	byKind := map[string][]map[string]any{}
	for _, doc := range strings.Split(stream, "\n---\n") {
		var obj map[string]any
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("parse applied object: %v\n%s", err, doc)
		}
		kind, _ := obj["kind"].(string)
		byKind[kind] = append(byKind[kind], obj)
	}
	return byKind
}

// TestSimpleBackend_RoundTripsKCLToProviderSpec is the ANTI-DRIFT test for
// this deploy tier. It is deliberately one test spanning every layer, rather
// than one test per layer each constructing its own input: a Go test that
// hand-builds a ServiceEntity asserts about a struct, not about anything KCL
// produces, and stays green while the schema moves.
//
// So it renders REAL KCL through the real render seam, unmarshals the entity
// contract through the real dispatchServiceDeploy, groups through the real
// buildDeployGroups, and expands the manifest stream through the real
// cluster.ExtractManifests. That last step is the path `forge env deploy`
// takes to kubectl, and it is where pkg/deploy.Render turns the declaration
// into objects.
func TestSimpleBackend_RoundTripsKCLToProviderSpec(t *testing.T) {
	dir := writeSimpleBackendProject(t, "acme-api", acmeAPIBlock)
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
		t.Fatalf("deploy.type = %q, want simple-backend: the entity contract must KEEP the discriminator, or the tier is invisible to `forge project audit`", svc.Deploy.Type)
	}
	sb := svc.Deploy.SimpleBackend
	if sb == nil {
		t.Fatal("deploy.simple_backend is nil: dispatchServiceDeploy did not populate the variant")
	}
	if sb.Cluster != "gke_acme_us-central1_hosted" || sb.Namespace != "acme-prod" {
		t.Errorf("target coordinates = %q/%q", sb.Cluster, sb.Namespace)
	}
	// The spec decoded into forge's Go tier type and is VALID. What an
	// author writes in KCL is a valid CR spec.
	if err := sb.Spec.Validate(); err != nil {
		t.Errorf("entity spec fails the tier's own Validate: %v", err)
	}
	if sb.Spec.Image != "ghcr.io/acme/api:v1.4.2" || sb.Spec.StorageGiB != 20 || sb.Spec.Resources.MemoryRequestBytes != 2<<30 {
		t.Errorf("entity spec = %+v", sb.Spec)
	}
	if b := svc.EffectiveBuild(); b.Type != "" {
		t.Errorf("EffectiveBuild = %q, want empty: forge neither builds nor pushes a SimpleBackend's image", b.Type)
	}
	// The secret pre-flight sees the secretRef (and only channels it can
	// check). databaseRef reads CNPG's own Secret, which no store renders.
	envs := sb.EnvVars()
	if len(envs) != 2 || envs[1].SecretRef != "acme-prod-db" || envs[1].SecretKey != "password" {
		t.Errorf("EnvVars() = %+v, want LOG_LEVEL and the DB_PASSWORD secret_ref only", envs)
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
	if g.ProviderID != "k8s-cluster" {
		t.Errorf("ProviderID = %q, want k8s-cluster", g.ProviderID)
	}
	if g.Cluster != "gke_acme_us-central1_hosted" || g.Namespace != "acme-prod" {
		t.Errorf("group target = %q/%q (a hosted workload in the fallback namespace is a cross-customer leak)", g.Cluster, g.Namespace)
	}
	if len(g.Services) != 1 || g.Services[0].K8sCluster == nil || g.Services[0].K8sCluster.Replicas != 1 {
		t.Fatalf("group services = %+v", g.Services)
	}
	if p := deploytarget.NewRegistry().Lookup(g.ProviderID); p == nil {
		t.Fatalf("no registered provider for %q", g.ProviderID)
	}

	// ── layer 3: what kubectl actually receives ───────────────────────
	byKind := appliedObjects(t, out)
	if n := len(byKind["SimpleBackend"]); n != 0 {
		t.Fatalf("%d forge.dev declaration(s) reached the applied stream: a self-hosted cluster has no CRD for them", n)
	}
	for kind, want := range map[string]int{"Deployment": 1, "Service": 1, "ServiceAccount": 1, "PersistentVolumeClaim": 1, "NetworkPolicy": 1} {
		if got := len(byKind[kind]); got != want {
			t.Errorf("%s objects = %d, want %d", kind, got, want)
		}
	}
	// The env gate stamped the declaration record, and the expansion must
	// carry that stamp onto every object, or env-scoped prune misses them.
	for kind, objs := range byKind {
		for _, o := range objs {
			md, _ := o["metadata"].(map[string]any)
			labels, _ := md["labels"].(map[string]any)
			if kind != "Namespace" && labels["forge.dev/env"] != "prod" {
				t.Errorf("%s %v lost the forge.dev/env stamp: %v", kind, md["name"], labels)
			}
		}
	}
	// The OBSERVER's half of the PVC: OwnedClaims must name the claim the
	// render actually emitted, or forge reports the workload green while its
	// storage is unbound.
	pvcMeta, _ := byKind["PersistentVolumeClaim"][0]["metadata"].(map[string]any)
	if observed := g.Services[0].K8sCluster.OwnedClaims; len(observed) != 1 || observed[0] != pvcMeta["name"] {
		t.Errorf("OwnedClaims = %v, but the render emitted a PVC named %v", observed, pvcMeta["name"])
	}
	container := firstContainer(t, byKind["Deployment"][0])
	// Neither the env registry nor the env-wide tag may rewrite a
	// reference forge did not build.
	if got, _ := container["image"].(string); got != sb.Spec.Image {
		t.Errorf("container image = %q, want the declared %q (the env registry or image_tag leaked in)", got, sb.Spec.Image)
	}
	env, _ := container["env"].([]any)
	dbURL, _ := env[2].(map[string]any)
	if ref, _ := json.Marshal(dbURL["valueFrom"]); !strings.Contains(string(ref), `"name":"orders-app"`) || !strings.Contains(string(ref), `"key":"uri"`) {
		t.Errorf("DATABASE_URL = %s, want CNPG's orders-app/uri", ref)
	}
}

// TestSimpleBackend_RequestOnlyLimitsEqualRequests: "set a request, leave the
// limit" through the real KCL render and expansion. The tier schema leaves the
// limits unset (kcl/tests/positive_simple_backend_request_only.k), so the
// declaration must reach Validate and the rendered Deployment with each limit
// equal to its request. It must never carry a static 250m / 1 GiB limit that
// sits below the request and gets the spec refused.
func TestSimpleBackend_RequestOnlyLimitsEqualRequests(t *testing.T) {
	dir := writeSimpleBackendProject(t, "api", `forge.SimpleBackend {
        cluster = "hosted"
        namespace = "acme-prod"
        spec = tiers.SimpleBackend {
            image = "ghcr.io/acme/api:v2"
            ports = [8080]
            resources = tiers.Resources { cpuRequestMillicores = 500, memoryRequestBytes = 2147483648 }
        }
    }`)
	kclplugin.Register()
	out, err := kclrender.Run(dir, dir, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	entities, err := parseKCLEntities(out)
	if err != nil {
		t.Fatalf("parse entities: %v\n%s", err, out)
	}
	if err := entities.Services[0].Deploy.SimpleBackend.Spec.Validate(); err != nil {
		t.Fatalf("request-only spec refused: %v", err)
	}
	res, _ := json.Marshal(firstContainer(t, appliedObjects(t, out)["Deployment"][0])["resources"])
	want := `{"limits":{"cpu":"500m","memory":"2Gi"},"requests":{"cpu":"500m","memory":"2Gi"}}`
	if string(res) != want {
		t.Errorf("container resources = %s, want %s", res, want)
	}
}

// TestSimpleBackend_NetworkNoneDropsService pins that `network` is a manifest
// difference rather than a label, against the real render and expansion.
//
// A "none" backend gets NO Service, and exactly one NetworkPolicy that is
// INGRESS-ONLY with no rules. That policy makes a true claim ("nothing may
// connect to this pod") and deliberately no egress claim. Egress is the
// namespace's control, so "none" is still addressability and not
// containment. The KCL-era version rendered no policy at all, and it gave
// the pod containerPort 8080 on a workload that listens on nothing.
func TestSimpleBackend_NetworkNoneDropsService(t *testing.T) {
	dir := writeSimpleBackendProject(t, "worker", `forge.SimpleBackend {
        cluster = "hosted"
        namespace = "acme-prod"
        spec = tiers.SimpleBackend { image = "ghcr.io/acme/worker:v2", network = "none" }
    }`)
	kclplugin.Register()
	out, err := kclrender.Run(dir, dir, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	byKind := appliedObjects(t, out)
	if n := len(byKind["Deployment"]); n != 1 {
		t.Errorf("Deployments = %d, want 1: `network` governs reachability, not whether the workload runs", n)
	}
	if n := len(byKind["Service"]); n != 0 {
		t.Errorf("Services = %d, want 0 for network = \"none\"", n)
	}
	if n := len(byKind["NetworkPolicy"]); n != 1 {
		t.Fatalf("NetworkPolicies = %d, want 1 (ingress default-deny)", n)
	}
	np, _ := json.Marshal(byKind["NetworkPolicy"][0]["spec"])
	if !strings.Contains(string(np), `"policyTypes":["Ingress"]`) || strings.Contains(string(np), "Egress") || strings.Contains(string(np), `"ingress"`) {
		t.Errorf("network none policy = %s, want Ingress-only with no rules and no egress claim", np)
	}
	if ports, ok := firstContainer(t, byKind["Deployment"][0])["ports"]; ok {
		t.Errorf("a network none worker declares container ports %v; it listens on nothing", ports)
	}
}

// TestManagedDatabase_BackendReachesDatabase is the three-tier shape in one
// declaration: a ManagedDatabase plus a SimpleBackend whose DATABASE_URL is a
// databaseRef to it, rendered through real KCL and expanded by
// cluster.ExtractManifests exactly as `forge env deploy` applies it.
//
// It pins the CONTRACT between the two tiers, which the two renderers must
// agree on without either being told: the Cluster is named for the
// database, it lands in the backend's namespace, and CloudNativePG will
// publish "<cluster>-app" there. The backend's secretKeyRef names exactly
// that Secret, with key uri.
func TestManagedDatabase_BackendReachesDatabase(t *testing.T) {
	dir := t.TempDir()
	kclMod := "[package]\nname = \"threetier\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n\n[dependencies]\nforge = { path = \"" + forgeModuleRoot(t) + "\" }\n"
	main := `import forge
import forge.tiers

_bundle = forge.Bundle {
    project = "shop"
    env = "prod"
    cluster_target = forge.ClusterTarget { cluster = "c", namespace = "shop-prod", registry = "ghcr.io/acme" }
    databases = [forge.ManagedDatabase { name = "orders", namespace = "shop-prod" }]
    services = [forge.RenderedWorkload {
        name = "api"
        deploy = forge.SimpleBackend {
            cluster = "c"
            namespace = "shop-prod"
            spec = tiers.SimpleBackend {
                image = "ghcr.io/acme/api:v1"
                ports = [8080]
                env = [tiers.EnvVar { name = "DATABASE_URL", databaseRef = tiers.DatabaseRef { name = "orders" } }]
            }
        }
    }]
}
output = forge.render(_bundle)
manifests = forge.render_manifests(_bundle, "v1", {}, False)
`
	for f, c := range map[string]string{"kcl.mod": kclMod, "main.k": main} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	kclplugin.Register()
	out, err := kclrender.Run(dir, dir, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	byKind := appliedObjects(t, out)
	if n := len(byKind["ManagedDatabase"]); n != 0 {
		t.Fatalf("%d forge.dev declaration(s) reached the applied stream", n)
	}
	if n := len(byKind["Cluster"]); n != 1 {
		t.Fatalf("CNPG Clusters = %d, want 1", n)
	}
	cl := byKind["Cluster"][0]
	md, _ := cl["metadata"].(map[string]any)
	if cl["apiVersion"] != "postgresql.cnpg.io/v1" || md["name"] != "orders" || md["namespace"] != "shop-prod" {
		t.Fatalf("Cluster = %v %v/%v", cl["apiVersion"], md["namespace"], md["name"])
	}
	annotations, _ := md["annotations"].(map[string]any)
	if annotations["forge.dev/deletion-policy"] != "retain" {
		t.Errorf("deletion-policy annotation = %v, want retain (the default)", annotations["forge.dev/deletion-policy"])
	}
	spec, _ := json.Marshal(cl["spec"])
	for _, want := range []string{`"enableSuperuserAccess":false`, `"database":"orders"`, `"owner":"orders_app"`, `"size":"10Gi"`} {
		if !strings.Contains(string(spec), want) {
			t.Errorf("Cluster spec %s missing %s", spec, want)
		}
	}

	env, _ := firstContainer(t, byKind["Deployment"][0])["env"].([]any)
	ref, _ := json.Marshal(env[0].(map[string]any)["valueFrom"])
	if !strings.Contains(string(ref), `"name":"orders-app"`) || !strings.Contains(string(ref), `"key":"uri"`) {
		t.Errorf("DATABASE_URL valueFrom = %s, want the Secret CNPG publishes for the Cluster (orders-app, key uri)", ref)
	}

	entities, err := parseKCLEntities(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entities.Databases) != 1 || entities.Databases[0].Name != "orders" || entities.Databases[0].Spec.WithDefaults().DeletionPolicy != "retain" {
		t.Errorf("entity databases = %+v", entities.Databases)
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
