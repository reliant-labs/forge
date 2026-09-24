package cli

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/deploytarget"
)

const hostedStaticDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

// TestHostedStaticSiteCLIEndToEnd drives a hosted StaticSite through the real
// commands against an httptest control plane:
//
//	forge build hosted --push <push base>   (site → OCI artifact, stubbed push)
//	forge release cut v1 --env hosted       (records web@<digest>)
//	forge env promote v1 --to hosted
//	forge env deploy hosted                 (ensure → STATIC deployment pinned
//	                                         to that digest → publish → ready)
//
// MUTATIONS VERIFIED RED (see reports/STATIC.md):
//   - the by-name refusal restored in buildHostedGroups → deploy errors;
//   - buildHostedStaticSites not called from runBuild → the cut is refused as
//     missing "web (hosted static site…)";
//   - Registry recorded without the static.v1 segment → wrong repository URI.
func TestHostedStaticSiteCLIEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL and runs the whole CLI; skipped in -short")
	}
	fake := newFakeDeployService(map[string]string{})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	dir := writeHostedStaticProject(t, srv.URL)
	t.Chdir(dir)
	t.Setenv("ACME_CP_TOKEN", "rlat_e2e")
	t.Setenv("FORGE_HOME", t.TempDir())
	prevPoll := hostedPollInterval
	hostedPollInterval = time.Millisecond
	t.Cleanup(func() { hostedPollInterval = prevPoll })

	var pushedTo string
	prevPush := hostedStaticPusher
	hostedStaticPusher = func(_ context.Context, _, repository string, fe deploytarget.StaticSiteFrontend) (string, error) {
		pushedTo = repository
		if fe.Name != "web" || fe.Spec.PublicDir != "dist" {
			t.Errorf("pushed frontend = %+v", fe)
		}
		return hostedStaticDigest, nil
	}
	t.Cleanup(func() { hostedStaticPusher = prevPush })

	if out, err := runForge(t, "build", "hosted", "--push", "localhost:5051/org1", "--tag", "t1"); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if pushedTo != "localhost:5051/org1/static.v1/web" {
		t.Fatalf("site pushed to %q, want the org's static.v1 subtree", pushedTo)
	}
	if out, err := runForge(t, "release", "cut", "v1", "--env", "hosted"); err != nil {
		t.Fatalf("release cut: %v\n%s", err, out)
	}
	rel := fake.releases["v1"]
	if len(rel.Artifacts) != 1 || rel.Artifacts[0].Name != "web" || rel.Artifacts[0].Digest != hostedStaticDigest ||
		rel.Artifacts[0].Kind != "oci" || rel.Artifacts[0].URI != "localhost:5051/org1/static.v1" {
		t.Fatalf("cut release = %+v, want one oci artifact web@%s under localhost:5051/org1/static.v1", rel, hostedStaticDigest)
	}
	if out, err := runForge(t, "env", "promote", "v1", "--to", "hosted"); err != nil {
		t.Fatalf("promote: %v\n%s", err, out)
	}
	envID := fake.envs["hosted"]
	fake.bodies = nil
	if out, err := runForge(t, "env", "deploy", "hosted", "--rollout-timeout", "2s"); err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	var paths []string
	for _, b := range fake.bodies {
		paths = append(paths, b.Path[strings.LastIndex(b.Path, "/")+1:])
	}
	if joined := strings.Join(paths, ","); !strings.Contains(joined, "EnsureEnvironment,EnsureDeployment,PublishDeploymentConfig,GetStatus") {
		t.Fatalf("deploy call sequence = %s", joined)
	}
	d := fake.deployments[envID]["web"]
	if d == nil || !d.Published || d.Tier != "DEPLOY_TIER_STATIC" {
		t.Fatalf("deployment = %+v", d)
	}
	if d.Spec["liveDigest"] != hostedStaticDigest || d.Spec["basePath"] != "/app" {
		t.Errorf("published spec = %v", d.Spec)
	}
	if _, ok := d.Spec["bucket"]; ok {
		t.Errorf("hosted spec names a bucket: %v", d.Spec)
	}
}

// writeHostedStaticProject is a hosted env with ONE StaticSite frontend and
// no backend.
func writeHostedStaticProject(t *testing.T, endpoint string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"forge.yaml": "name: acme\nmodule_path: github.com/example/acme\nversion: 0.1.0\nfrontends: []\n",
		"deploy/kcl/kcl.mod": "[package]\nname = \"acme_deploy\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n\n[dependencies]\n" +
			"forge = { path = \"" + forgeModuleRoot(t) + "\" }\n",
		"deploy/kcl/hosted/main.k": `import forge

_bundle = forge.Bundle {
    project = "acme"
    env = "hosted"
    control_plane = forge.ControlPlane {
        endpoint = "` + endpoint + `"
        token_env = "ACME_CP_TOKEN"
    }
    services = []
    frontends = [forge.Frontend {
        name = "web"
        path = "frontends/web"
        type = "vite"
        deploy = forge.StaticSite { public_dir = "dist", base_path = "/app" }
    }]
}

output = forge.render(_bundle)
manifests = forge.render_manifests(_bundle, forge.image_tag("hosted"), forge.image_digests(), False)
`,
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
	markServiceProject(t, dir)
	return dir
}
