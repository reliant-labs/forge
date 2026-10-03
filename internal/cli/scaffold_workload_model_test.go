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
	"github.com/reliant-labs/forge/internal/hostedimage"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
	deployv1alpha1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// ONE declaration, every runtime (ADR 0002), end to end from a real scaffold.
//
// `forge project new` writes deploy/kcl/workloads.k once, and every env binds
// each workload to where it runs, one line per workload. This scaffolds a
// project, then renders that SAME declaration bound three ways: dev (host
// processes), prod (its cluster), and a MIXED env derived from prod with
// `forge env new cloud --from prod --bind item=hosted --bind web=hosted` —
// item on the forge control plane beside migrate on the cluster, and the web
// frontend on the platform's static hosting (ADR 0002 §6). Each render goes through the
// production decoder and the consumer the deploy path runs for that runtime:
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

	// prod's cluster knobs are placeholders a user fills; fill them so the
	// derived env starts from a deployable file.
	prodMain := filepath.Join(dir, "deploy", "kcl", "prod", "main.k")
	b, err := os.ReadFile(prodMain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prodMain, []byte(strings.Replace(string(b), `registry = "ghcr.io/OWNER"`, `registry = "ghcr.io/acme"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	// The MIXED env, through the real command: derived from prod, item
	// rebound to the control plane. Then its --check gate: no placeholder
	// once the knobs are filled, compiles, and admitted by the hosted plan.
	withCwd(t, dir, func() {
		cmd := newEnvNewCmd()
		cmd.SetArgs([]string{"cloud", "--from", "prod", "--bind", "item=hosted", "--bind", "web=hosted"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("forge env new cloud --from prod --bind item=hosted --bind web=hosted: %v", err)
		}
	})
	cloudDir := filepath.Join(dir, "deploy", "kcl", "cloud")
	cloud, err := os.ReadFile(filepath.Join(cloudDir, "main.k"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"_hosted(wl.item)", "_on_cluster(wl.migrate)", "_hosted_frontend(_web_frontend)", "control_plane = forge.ControlPlane {", `organization = "` + hostedimage.OrgPlaceholder + `"`} {
		if !strings.Contains(string(cloud), want) {
			t.Fatalf("derived cloud/main.k lacks %q:\n%s", want, cloud)
		}
	}
	filled := placeholderRe.ReplaceAllStringFunc(string(cloud), func(p string) string {
		return map[string]string{
			"REPLACE_ME_CLUSTER_CONTEXT": "gke_acme_cloud", "REPLACE_ME_PLATFORM": "amd64",
			"REPLACE_ME_NAMESPACE": "acme-cloud", "REPLACE_ME_REGISTRY": "ghcr.io/acme",
			"REPLACE_ME_BUCKET": "acme-cloud-web",
			// The scaffolded organization. It is a REPLACE_ME_* like every
			// other knob, so `forge env new --check` already gates on it
			// with no new rule — which is why the placeholder was spelled
			// to match placeholderRe rather than invented separately.
			hostedimage.OrgPlaceholder: "4f3c2b1a-0000-4000-8000-000000000001",
		}[p]
	})
	writeEnv(t, dir, "cloud", filled, filepath.Join(dir, "deploy", "kcl", "prod"))
	withCwd(t, dir, func() {
		if err := runNewEnv(context.Background(), "cloud", "", true, false); err != nil {
			t.Fatalf("forge env new cloud --check: %v", err)
		}
	})

	render := func(env string) (*KCLEntities, []byte) {
		t.Helper()
		kclplugin.UsePortStoreReadOnly(filepath.Join(dir, ".forge", "ports-"+env+".json"))
		raw, err := kclrender.Run(dir, filepath.Join(dir, "deploy", "kcl", env), []string{"env=" + env})
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

	// ── mixed: item hosted, admitted under Restricted, probes present ────
	mixed, mixedRaw := render("cloud")
	mw := byName(mixed)
	if mw["item"].Runtime.Type != RuntimeHosted || mw["migrate"].Runtime.Type != RuntimeCluster {
		t.Fatalf("cloud runtimes: item=%q migrate=%q, want hosted/cluster", mw["item"].Runtime.Type, mw["migrate"].Runtime.Type)
	}
	group, err := buildHostedGroup("cloud", mixed)
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
	// The web frontend, rebound to hosted, is published as a StaticSite.
	var hostedSite bool
	for _, s := range group.Services {
		if s.Name == "web" && s.Hosted != nil && s.Hosted.Tier == deploytarget.HostedTierStatic {
			hostedSite = true
		}
	}
	if !hostedSite {
		t.Errorf("the rebound web frontend is not published as a hosted StaticSite: %+v", group.Services)
	}

	// ── cluster (prod): RenderWorkloads, probes present ──────────────────
	prod, prodRaw := render("prod")
	if fe := prod.Frontends; len(fe) != 1 || fe[0].Runtime.Type != FrontendRuntimeBucket {
		t.Errorf("prod frontends = %+v, want web on forge.OnBucket", fe)
	}
	assertClusterDeploymentProbed(t, "prod", prodRaw)

	// ── host (dev): argv from build + args ───────────────────────────────
	dev, _ := render("dev")
	if fe := dev.Frontends; len(fe) != 1 || fe[0].Runtime.Type != FrontendRuntimeHost {
		t.Errorf("dev frontends = %+v, want web on forge.OnHost", fe)
	}
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

	// The binding changed nothing about WHAT item is.
	if strings.Join(mw["item"].Spec.Args, " ") != strings.Join(devW["item"].Spec.Args, " ") {
		t.Errorf("rebinding item to hosted changed its args")
	}
	// The cluster half of the mixed env still renders through RenderWorkloads:
	// migrate is a job there, so no Deployment, but no Workload record of
	// the hosted item may reach the cluster.
	if stream, err := cluster.ExtractManifests(mixedRaw); err != nil {
		t.Fatalf("cloud: expand output.manifests: %v", err)
	} else if strings.Contains(stream, "name: item\n") {
		t.Errorf("cloud: the hosted item leaked into the cluster stream:\n%s", stream)
	}
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
