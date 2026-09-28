package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
	deployv1alpha1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// ONE declaration, every runtime (ADR 0002), end to end from a real scaffold.
//
// `forge project new` writes deploy/kcl/workloads.k once. This scaffolds a
// project, then renders that SAME file under each runtime the scaffold and
// `forge env new --runtime` offer — host (dev), cluster (prod), hosted — plus
// a MIXED env that binds one workload to a cluster and leaves the rest on the
// host. Each render goes through the production decoder and the consumer the
// deploy path runs for that runtime:
//
//   - hosted: the spec is admitted under ProfileRestricted (Workload.Validate
//     plus a restricted render of the set), exactly as the control plane
//     would admit it — the original hounders defect was a hosted service with
//     no probes, so the admitted spec must carry /readyz + /healthz;
//   - cluster: the Workload records are expanded through
//     pkg/deploy.RenderWorkloads (cluster.ExtractManifests), and the
//     Deployment it produces carries the same probes;
//   - host: the argv is derived from build + args, never re-stated.
func TestScaffold_OneWorkloadDeclarationRendersOnEveryRuntime(t *testing.T) {
	kclplugin.Register()
	if !kclplugin.Available() {
		t.Skip("kcl_plugin.forge unavailable (CGO_ENABLED=0 build); forge cannot render KCL")
	}
	dir := scaffoldWorkloadModelProject(t)

	// The hosted env, through the real command (`forge env new --runtime
	// hosted`), and its --check gate: no placeholder, compiles, admitted.
	withCwd(t, dir, func() {
		if err := runNewEnvForRuntime(context.Background(), "cloud", "hosted", false); err != nil {
			t.Fatalf("forge env new cloud --runtime hosted: %v", err)
		}
		if err := runNewEnv(context.Background(), "cloud", "", true, false); err != nil {
			t.Fatalf("forge env new cloud --check: %v", err)
		}
	})

	// The MIXED env: dev's file with the primary service rebound to the local
	// cluster. The declaration is untouched; only the binding changes.
	devMain, err := os.ReadFile(filepath.Join(dir, "deploy", "kcl", "dev", "main.k"))
	if err != nil {
		t.Fatal(err)
	}
	mixedMain := strings.Replace(string(devMain), "_workloads = wl.ALL\n",
		`_workloads = [w | {runtime = forge.OnCluster {target = _k3d}} if w.name == "item" else w for w in wl.ALL]`+"\n", 1)
	if mixedMain == string(devMain) {
		t.Fatal("dev/main.k no longer has the `_workloads = wl.ALL` line the mixed binding rewrites")
	}
	writeEnv(t, dir, "mixed", mixedMain, filepath.Join(dir, "deploy", "kcl", "dev"))

	// The mixed env IS a dev env (a local loop with one workload in k3d), so
	// it renders under the dev binding: FileSecrets is dev/e2e-only.
	renderAs := map[string]string{"mixed": "dev"}
	render := func(env string) (*KCLEntities, []byte) {
		t.Helper()
		as := env
		if a, ok := renderAs[env]; ok {
			as = a
		}
		kclplugin.UsePortStoreReadOnly(filepath.Join(dir, ".forge", "ports-"+as+".json"))
		raw, err := kclrender.Run(dir, filepath.Join(dir, "deploy", "kcl", env), []string{"env=" + as})
		if err != nil {
			t.Fatalf("render %s: %v", env, err)
		}
		e, err := parseKCLEntities(raw)
		if err != nil {
			t.Fatalf("decode %s: %v", env, err)
		}
		return e, raw
	}
	byName := func(e *KCLEntities) map[string]WorkloadEntity {
		out := map[string]WorkloadEntity{}
		for _, w := range e.Workloads {
			out[w.Name] = w
		}
		return out
	}

	// ── hosted: admitted under Restricted, probes present ────────────────
	hosted, _ := render("cloud")
	group, err := buildHostedGroup("cloud", hosted)
	if err != nil || group == nil {
		t.Fatalf("buildHostedGroup: %v", err)
	}
	items, err := deploytarget.PreflightHosted(*group)
	if err != nil {
		t.Fatalf("the scaffolded hosted env is refused under the Restricted profile: %v", err)
	}
	var admittedService bool
	for _, it := range items {
		if it.Workload == nil || it.Name != "item" {
			continue
		}
		admittedService = true
		cr := deployv1alpha1.Workload{Spec: *it.Workload}
		cr.Name = it.Name
		if err := cr.Validate(deployv1alpha1.ProfileRestricted); err != nil {
			t.Errorf("hosted item spec does not validate under ProfileRestricted: %v", err)
		}
		assertServeProbes(t, "hosted item", it.Workload.Probes)
	}
	if !admittedService {
		t.Fatalf("the hosted env admitted no `item` Workload (items: %+v)", items)
	}

	// ── cluster (prod): RenderWorkloads, probes present ──────────────────
	_, prodRaw := render("prod")
	assertClusterDeploymentProbed(t, "prod", prodRaw)

	// ── host (dev): argv from build + args ───────────────────────────────
	dev, _ := render("dev")
	devW := byName(dev)
	if rt := devW["item"].Runtime; rt.Type != RuntimeHost {
		t.Fatalf("dev item runtime = %q, want host", rt.Type)
	}
	assertServeProbes(t, "dev item", devW["item"].Spec.Probes)
	if got := strings.Join(devW["item"].Spec.Args, " "); got != "item" {
		t.Errorf("dev item args = %q, want the component subcommand", got)
	}
	if got := strings.Join(devW["migrate"].Spec.Args, " "); got != "db migrate up" {
		t.Errorf("dev migrate args = %q", got)
	}

	// ── mixed: one workload on the cluster, the rest on the host ─────────
	mixed, mixedRaw := render("mixed")
	mw := byName(mixed)
	if mw["item"].Runtime.Type != RuntimeCluster || mw["migrate"].Runtime.Type != RuntimeHost {
		t.Fatalf("mixed runtimes: item=%q migrate=%q, want cluster/host", mw["item"].Runtime.Type, mw["migrate"].Runtime.Type)
	}
	// The binding changed nothing about WHAT item is.
	if strings.Join(mw["item"].Spec.Args, " ") != strings.Join(devW["item"].Spec.Args, " ") {
		t.Errorf("rebinding item to a cluster changed its args")
	}
	assertClusterDeploymentProbed(t, "mixed", mixedRaw)
}

// scaffoldWorkloadModelProject scaffolds a service project with one service
// and a frontend, and produces the config modules `forge generate` would.
func scaffoldWorkloadModelProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	g := generator.NewProjectGenerator("acme", dir, "example.com/acme")
	g.Kind = config.ProjectKindService
	g.ApplyKindFeatureDefaults(config.ProjectKindService)
	g.ServiceName = "item"
	g.FrontendName = "web"
	if err := g.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cfg, err := loadProjectConfigFrom(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("load forge.yaml: %v", err)
	}
	cs, err := generator.LoadChecksums(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := generatePerEnvDeployConfig(dir, cfg, cs); err != nil {
		t.Fatalf("generate per-env config: %v", err)
	}
	return dir
}

// writeEnv creates deploy/kcl/<env>/ with main.k and a copy of from's
// other files (config.k, ingress.k, the dev identity stub).
func writeEnv(t *testing.T, dir, env, mainK, from string) {
	t.Helper()
	dst := filepath.Join(dir, "deploy", "kcl", env)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "main.k" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dst, "main.k"), []byte(mainK), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertServeProbes(t *testing.T, where string, p *deployv1alpha1.Probes) {
	t.Helper()
	if p == nil {
		t.Fatalf("%s: no probes in the spec — the lowering must write /readyz + /healthz for a service forge builds", where)
	}
	if p.ReadinessPath != "/readyz" || p.LivenessPath != "/healthz" || p.Port != 8080 {
		t.Errorf("%s: probes = %+v, want /readyz + /healthz on :8080", where, *p)
	}
}

// assertClusterDeploymentProbed expands a render's output.manifests through
// the ONE Go renderer and requires the item Deployment to carry the split
// probes.
func assertClusterDeploymentProbed(t *testing.T, env string, raw []byte) {
	t.Helper()
	stream, err := cluster.ExtractManifests(raw)
	if err != nil {
		t.Fatalf("%s: expand output.manifests through RenderWorkloads: %v", env, err)
	}
	var deployment string
	for _, doc := range strings.Split(stream, "\n---\n") {
		if strings.Contains(doc, "kind: Deployment") && strings.Contains(doc, "name: item\n") {
			deployment = doc
		}
	}
	if deployment == "" {
		t.Fatalf("%s: RenderWorkloads produced no item Deployment:\n%s", env, stream)
	}
	for _, want := range []string{"readinessProbe:", "path: /readyz", "livenessProbe:", "path: /healthz"} {
		if !strings.Contains(deployment, want) {
			t.Errorf("%s: item Deployment lacks %q:\n%s", env, want, deployment)
		}
	}
}
