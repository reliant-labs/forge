package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/deploy"
	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// TestRenderMatchesKCLProjection is the proof that the Go deploy.Render is a
// FAITHFUL replacement for forge's retired KCL SimpleBackend projection
// (_project_simple_backend + the shared k8s builders in kcl/render.k).
//
// The KCL projection no longer exists: self-hosted SimpleBackends now render
// through deploy.Render. What it produced is preserved as GOLDEN files in
// testdata/simplebackend_kcl_projection/. They were rendered from forge commit
// ac5f6da0b338, the last one carrying the projection, for exactly the inputs
// below. So the comparison that justified the swap is still checked on every
// run, instead of being deleted along with the thing it compared against.
//
// For each case this test renders the SAME backend through Go, applies
// EXACTLY the documented deltas (render.go, RenderSimpleBackend's doc,
// numbered 1-6) to the golden, and requires deep equality. Any undocumented
// difference in either direction fails. Comparison is on decoded object
// trees, so key order and quoting do not matter.
func TestRenderMatchesKCLProjection(t *testing.T) {
	cases := []struct {
		name string
		kcl  string // the forge.SimpleBackend { ... } body
		spec v1alpha1.SimpleBackendSpec
	}{
		{
			name: "acme-api",
			kcl: `image = "ghcr.io/acme/api:v1.4.2"
        ports = [8080, 9090]
        network = "public"
        domain = "api.example.com"
        storage_gib = 20
        resources = forge.Resources { cpu_request_millicores = 500, cpu_limit_millicores = 1000, memory_request_bytes = forge.gib(2), memory_limit_bytes = forge.gib(2) }
        health_check = forge.HealthCheck { liveness_path = "/healthz", http_port = 8080, initial_delay = 7 }
        env_vars = [
            forge.EnvVar {name = "LOG_LEVEL", value = "info"}
            forge.EnvVar {name = "DB_PASSWORD", secret_ref = "acme-prod-db", secret_key = "password"}
        ]`,
			spec: v1alpha1.SimpleBackendSpec{
				Image: "ghcr.io/acme/api:v1.4.2", Ports: []int32{8080, 9090}, Network: v1alpha1.NetworkPublic,
				Domains: []string{"api.example.com"}, StorageGiB: 20,
				Resources:   v1alpha1.Resources{CPURequestMillicores: 500, CPULimitMillicores: 1000, MemoryRequestBytes: 2 << 30, MemoryLimitBytes: 2 << 30},
				HealthCheck: &v1alpha1.HealthCheck{Path: "/healthz", Port: 8080, InitialDelaySeconds: 7},
				Env: []v1alpha1.EnvVar{
					{Name: "LOG_LEVEL", Value: "info"},
					{Name: "DB_PASSWORD", SecretRef: &v1alpha1.SecretKeyRef{Name: "acme-prod-db", Key: "password"}},
				},
			},
		},
		{
			name: "priv",
			kcl: `image = "ghcr.io/acme/p:v1"
        ports = [3000]`,
			spec: v1alpha1.SimpleBackendSpec{Image: "ghcr.io/acme/p:v1", Ports: []int32{3000}},
		},
		{
			name: "worker",
			kcl: `image = "ghcr.io/acme/w@sha256:` + strings.Repeat("a", 64) + `"
        network = "none"`,
			spec: v1alpha1.SimpleBackendSpec{Image: "ghcr.io/acme/w@sha256:" + strings.Repeat("a", 64), Network: v1alpha1.NetworkNone},
		},
	}

	const ns, project = "acme-prod", "proj"
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kclObjs := loadKCLProjectionGolden(t, c.name)

			goObjs, err := deploy.RenderSimpleBackend(c.name, c.spec, deploy.Context{Namespace: ns, PartOf: project})
			if err != nil {
				t.Fatalf("deploy.RenderSimpleBackend: %v", err)
			}
			got := map[string]map[string]any{}
			for _, o := range goObjs {
				b, _ := json.Marshal(o.Object)
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				got[o.GetKind()+"/"+o.GetName()] = m
			}

			want := applyDocumentedDeltas(t, kclObjs, c.name, c.spec)

			if !reflect.DeepEqual(keys(want), keys(got)) {
				t.Fatalf("object sets differ:\n KCL(+deltas) %v\n Go           %v", keys(want), keys(got))
			}
			for k := range want {
				if want[k] == nil {
					continue // delta 5: presence asserted above, shape pinned in pkg/deploy
				}
				if !reflect.DeepEqual(want[k], got[k]) {
					wb, _ := json.MarshalIndent(want[k], "", "  ")
					gb, _ := json.MarshalIndent(got[k], "", "  ")
					t.Errorf("%s differs\n--- KCL(+deltas)\n%s\n--- Go\n%s", k, wb, gb)
				}
			}
		})
	}
}

// loadKCLProjectionGolden returns the retired KCL projection's manifests for
// one case (minus the env-level Namespace, which belongs to the environment,
// not the tier), keyed by kind/name.
func loadKCLProjectionGolden(t *testing.T, name string) map[string]map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "simplebackend_kcl_projection", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifests []map[string]any
	if err := json.Unmarshal(b, &manifests); err != nil {
		t.Fatal(err)
	}
	objs := map[string]map[string]any{}
	for _, m := range manifests {
		kind, _ := m["kind"].(string)
		if kind == "Namespace" {
			continue
		}
		md, _ := m["metadata"].(map[string]any)
		objs[kind+"/"+fmt.Sprint(md["name"])] = m
	}
	return objs
}

// applyDocumentedDeltas turns the KCL projection's output into what Render is
// documented to emit. Each numbered step here is the numbered delta in
// RenderSimpleBackend's doc comment. Nothing else is allowed to differ.
func applyDocumentedDeltas(t *testing.T, objs map[string]map[string]any, name string, spec v1alpha1.SimpleBackendSpec) map[string]map[string]any {
	t.Helper()
	dep := objs["Deployment/"+name]
	if dep == nil {
		t.Fatalf("KCL rendered no Deployment/%s (have %v)", name, keys(objs))
	}
	pod := dig(dep, "spec", "template", "spec")
	container := dig(pod, "containers").([]any)[0].(map[string]any)

	// (1) no Role/RoleBinding, and the ServiceAccount carries no token.
	delete(objs, "Role/"+name+"-role")
	delete(objs, "RoleBinding/"+name+"-rolebinding")
	pod.(map[string]any)["automountServiceAccountToken"] = false
	objs["ServiceAccount/"+name]["automountServiceAccountToken"] = false

	// (2) fsGroup so a non-root container can write its PVC.
	dig(pod, "securityContext").(map[string]any)["fsGroup"] = float64(deploy.RunAsUser)

	// (3) writable /tmp under the read-only root filesystem, mounted first.
	mounts, _ := container["volumeMounts"].([]any)
	container["volumeMounts"] = append([]any{map[string]any{"name": "tmp", "mountPath": "/tmp"}}, mounts...)
	vols, _ := pod.(map[string]any)["volumes"].([]any)
	pod.(map[string]any)["volumes"] = append([]any{map[string]any{"name": "tmp", "emptyDir": map[string]any{}}}, vols...)

	// (4) network none declares no container port (KCL inherited 8080).
	if !spec.ServesTraffic() {
		delete(container, "ports")
	}

	// (5) the per-backend ingress NetworkPolicy. Its SHAPE is pinned
	// separately (TestRenderNetworkPolicy in pkg/deploy); here it is only
	// admitted into the set, taken from the Go side verbatim.
	objs["NetworkPolicy/"+name+"-ingress"] = nil

	// (6) TCP probes are new capability; every case here uses an HTTP path,
	// so the KCL side needs no change for it.
	return objs
}

func dig(m any, path ...string) any {
	for _, p := range path {
		m = m.(map[string]any)[p]
	}
	return m
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
