package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func writeEnvMain(t *testing.T, dir string, envs ...string) {
	t.Helper()
	for _, env := range envs {
		d := filepath.Join(dir, "deploy", "kcl", env)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "main.k"), []byte("manifests = []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// deploy.yml targets the envs the project DECLARES — never a hard-coded
// staging/prod pair.
//
// houndersclub (dev + a hosted prod, no staging) was scaffolded with a
// deploy-staging job wired to workflow_run after every image build on main: a
// job for an env with no deploy/kcl/staging/main.k, red on every merge.
func TestCIFiles_DeployTargetsDeclaredEnvsOnly(t *testing.T) {
	g, dir := ciGenerator(t, config.FeaturesConfig{})
	writeEnvMain(t, dir, "dev", "prod")
	if err := g.generateCIFiles(); err != nil {
		t.Fatalf("generateCIFiles: %v", err)
	}
	deploy := readCIFile(t, filepath.Join(dir, ".github", "workflows", "deploy.yml"))
	if strings.Contains(deploy, "staging") {
		t.Error("deploy.yml targets staging, which this project does not declare")
	}
	if !strings.Contains(deploy, "- env: prod") {
		t.Error("deploy.yml does not target prod, which this project declares")
	}
	if strings.Contains(deploy, "- env: dev") {
		t.Error("deploy.yml targets dev, which runs locally via `forge env up`")
	}
	// A lone env is production: never auto-deployed on a merge to main.
	if !strings.Contains(deploy, "auto: false") {
		t.Errorf("a lone prod env is auto-deployed on every merge to main:\n%s", deploy)
	}
}

func TestScaffoldDeployEnvs_PromotionOrder(t *testing.T) {
	dir := t.TempDir()
	writeEnvMain(t, dir, "dev", "prod", "staging", "qa")
	envs := scaffoldDeployEnvs(dir)
	var names []string
	for _, e := range envs {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, ","); got != "qa,staging,prod" {
		t.Fatalf("envs = %s, want qa,staging,prod (dev excluded, prod last)", got)
	}
	if !envs[0].Auto || envs[0].Protection {
		t.Errorf("first env must auto-deploy and be unprotected: %+v", envs[0])
	}
	if last := envs[len(envs)-1]; last.Auto || !last.Protection {
		t.Errorf("prod must be protected and never auto-deployed: %+v", last)
	}
	if got := scaffoldDeployEnvs(t.TempDir()); len(got) != 0 {
		t.Errorf("a project with no deploy/kcl has no deploy envs, got %+v", got)
	}
}
