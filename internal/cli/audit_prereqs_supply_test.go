package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cli/audittype"
	"github.com/reliant-labs/forge/internal/config"
)

// writeAuditDevProject writes a forge project whose ONLY env is dev, with the
// given deploy/kcl/dev/main.k. There is deliberately no secrets/dev.yaml: every
// fixture is a fresh clone, because that store is gitignored and no clone,
// CI runner or teammate has it.
func writeAuditDevProject(t *testing.T, mainK string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"forge.yaml": "name: acme\nmodule_path: github.com/example/acme\nversion: 0.1.0\nfrontends: []\n",
		// No `forge` dependency: the forge KCL module is supplied by the
		// rendering binary (ADR 0003), so a project's kcl.mod never names it.
		"deploy/kcl/kcl.mod":    "[package]\nname = \"acme_deploy\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n",
		"deploy/kcl/dev/main.k": mainK,
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "dev.yaml")); err == nil {
		t.Fatal("fixture must not carry a local secret store")
	}
	return dir
}

// auditDevHeader is the part of every fixture that does not vary: one
// workload that reads a credential from the "acme-secrets" Secret, rendered
// into `manifests` through fw.render_workloads exactly as the scaffolded dev
// env renders it — a Deployment whose env carries a secretKeyRef.
const auditDevHeader = `import forge
import forge.workloads as fw

_ns = "acme-dev"
_registry = "localhost:5050"
_cluster = forge.ClusterTarget {
    cluster = "k3d-acme"
    namespace = _ns
    registry = _registry
}
_env = [forge.EnvVar {name = "STRIPE_SECRET_KEY", secret_ref = "acme-secrets", secret_key = "stripe_secret_key"}]
_workloads = [fw.Workload {
    name = "api"
    kind = "service"
    image = "acme"
    registry = _registry
    namespace = _ns
    ports = [fw.Port {name = "http", port = 8080, expose = True}]
    env_vars = _env
}]
manifests = fw.render_workloads(_workloads, fw.WorkloadEnv {
    namespace = _ns
    project = "acme"
    registry = _registry
    network_policies = False
})
`

// The scaffolded dev shape: the stream renders the workload as a Deployment,
// but the Bundle runs it as a HOST process fed by the FileSecrets store. forge
// never applies that stream (nothing is placed in a cluster), so nothing can
// FailedMount — and the store being absent on a fresh clone is irrelevant.
const auditDevHostPlaced = auditDevHeader + `
_bundle = forge.Bundle {
    cluster_target = _cluster
    project = "acme"
    services = [forge.RenderedWorkload {
        name = "api"
        image = "acme"
        command = ["go", "run", "./cmd/acme", "server"]
        env_vars = _env
        deploy = forge.HostDeploy {
            runner = "go-run"
            env_vars = _env
        }
    }]
    secret_provider = forge.FileSecrets {path = "secrets/dev.yaml"}
}
output = forge.render(_bundle)
`

// The same workload placed IN the cluster, with FileSecrets declared: the
// provider renders "acme-secrets" from its store at deploy time, so the mount
// is declared. That holds whether or not this checkout's store exists — the
// DECLARATION is what the audit checks; a missing value is `forge secret
// ensure`'s job, reported by deploy/up when it actually needs one.
const auditDevClusterPlacedFileSecrets = auditDevHeader + `
_bundle = forge.Bundle {
    cluster_target = _cluster
    project = "acme"
    services = [forge.RenderedWorkload {
        name = "api"
        image = "acme"
        env_vars = _env
        deploy = _cluster.deploy | {ports = [8080]}
    }]
    secret_provider = forge.FileSecrets {path = "secrets/dev.yaml"}
}
output = forge.render(_bundle)
`

// A genuinely wrong DECLARATION: the workload runs in the cluster and reads
// "acme-secrets", but the env's provider (ExternalSecrets) never supplies a
// Secret itself and no forge.ExternalSecret promises this one. Nothing can
// ever provide it; the pod would sit in CreateContainerConfigError.
const auditDevClusterPlacedUndeclared = auditDevHeader + `
_bundle = forge.Bundle {
    cluster_target = _cluster
    project = "acme"
    services = [forge.RenderedWorkload {
        name = "api"
        image = "acme"
        env_vars = _env
        deploy = _cluster.deploy | {ports = [8080]}
    }]
    secret_provider = forge.ExternalSecrets {}
}
output = forge.render(_bundle)
`

func auditPrereqsFor(t *testing.T, mainK string) audittype.Category {
	t.Helper()
	dir := writeAuditDevProject(t, mainK)
	cat := auditPrerequisites(&config.ProjectConfig{Name: "acme"}, dir)
	if strings.Contains(cat.Summary, "could not evaluate dev KCL") {
		t.Fatalf("fixture did not render: %s", cat.Summary)
	}
	return cat
}

func auditFindings(cat audittype.Category) string {
	findings, _ := cat.Details["findings"].([]string)
	return strings.Join(findings, "\n")
}

// TestAuditPrerequisites_HostPlacedWorkloadOnFreshClone is the reported bug:
// `forge project audit` failed with `undeclared-mount: Secret "<project>-secrets"`
// on every checkout of a scaffolded project whose workload declares a
// config_secrets credential, which made the scaffolded pre-push hook block
// every push. The mount was in a Deployment nothing applies.
func TestAuditPrerequisites_HostPlacedWorkloadOnFreshClone(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	cat := auditPrereqsFor(t, auditDevHostPlaced)
	if cat.Status == audittype.StatusError {
		t.Fatalf("status = error, want ok: a host-placed workload's Secret is never mounted by a pod.\nsummary: %s\nfindings:\n%s",
			cat.Summary, auditFindings(cat))
	}
	if got := cat.Details["undeclared_secret_mounts"]; got != 0 {
		t.Errorf("undeclared_secret_mounts = %v, want 0", got)
	}
	if _, ok := cat.Details["secret_supply_check"].(string); !ok {
		t.Errorf("the skipped supply check must say why it is n/a; details = %v", cat.Details)
	}
}

// TestAuditPrerequisites_FileSecretsDeclaredOnFreshClone: a Secret the
// FileSecrets provider is DECLARED to supply is declared, whether or not this
// machine's store has been populated.
func TestAuditPrerequisites_FileSecretsDeclaredOnFreshClone(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	cat := auditPrereqsFor(t, auditDevClusterPlacedFileSecrets)
	if cat.Status == audittype.StatusError {
		t.Fatalf("status = error, want ok: FileSecrets declares %q for the cluster workload.\nfindings:\n%s",
			"acme-secrets", auditFindings(cat))
	}
	if _, skipped := cat.Details["secret_supply_check"]; skipped {
		t.Error("a cluster-placed workload must keep the supply check")
	}
}

// TestAuditPrerequisites_UndeclaredClusterMountStillFails pins the other half:
// scoping the gate to cluster-placed workloads must not let a real
// declaration defect through.
func TestAuditPrerequisites_UndeclaredClusterMountStillFails(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	cat := auditPrereqsFor(t, auditDevClusterPlacedUndeclared)
	if cat.Status != audittype.StatusError {
		t.Fatalf("status = %q, want error for a cluster Secret nothing supplies.\nsummary: %s", cat.Status, cat.Summary)
	}
	if got := auditFindings(cat); !strings.Contains(got, `undeclared-mount: Secret "acme-secrets"`) {
		t.Errorf("findings should name the undeclared Secret:\n%s", got)
	}
}

// TestEnvAppliesManifestsToCluster pins the placement rule the audit shares
// with the deploy preflight's gate.
func TestEnvAppliesManifestsToCluster(t *testing.T) {
	cases := []struct {
		name string
		json string
		want bool
	}{
		{"nil", "", false},
		{
			"host only",
			`{"services":[{"name":"api","deploy":{"type":"host"}}]}`,
			false,
		},
		{
			"host process plus host-infra postgres",
			`{"services":[{"name":"api","deploy":{"type":"host"}},{"name":"postgres","deploy":{"type":"host-infra","port":5432}}]}`,
			false,
		},
		{
			"cluster",
			`{"services":[{"name":"api","deploy":{"type":"cluster","cluster":"c","namespace":"n","registry":"r"}}]}`,
			true,
		},
		{
			"mixed host and cluster",
			`{"services":[{"name":"a","deploy":{"type":"host"}},{"name":"b","deploy":{"type":"cluster","cluster":"c","namespace":"n","registry":"r"}}]}`,
			true,
		},
		{
			"self-hosted simple backend",
			`{"services":[{"name":"api","deploy":{"type":"simple-backend","cluster":"c","namespace":"n","spec":{"image":"i"}}}]}`,
			true,
		},
		{
			"hosted simple backend (control plane applies)",
			`{"control_plane":{"type":"reliant","endpoint":"https://cp"},"services":[{"name":"api","deploy":{"type":"simple-backend","spec":{"image":"i"}}}]}`,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e *KCLEntities
			if tc.json != "" {
				var err error
				if e, err = parseKCLEntities([]byte(tc.json)); err != nil {
					t.Fatalf("parseKCLEntities: %v", err)
				}
			}
			if got := envAppliesManifestsToCluster(e); got != tc.want {
				t.Errorf("envAppliesManifestsToCluster = %v, want %v", got, tc.want)
			}
		})
	}
}
