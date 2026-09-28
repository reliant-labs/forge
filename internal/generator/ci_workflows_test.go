package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// ciFile returns the data CIWorkflows planned for dest, or nil when the file
// is not part of the set.
func ciFile(files []CIWorkflowFile, dest string) any {
	for _, f := range files {
		if f.Dest == dest {
			return f.Data
		}
	}
	return nil
}

// serviceCfg is a service-kind config with derived defaults applied — the
// shape the loader hands both CI callers.
func serviceCfg() *config.ProjectConfig {
	cfg := &config.ProjectConfig{Name: "demo", ModulePath: "github.com/example/demo", Kind: config.ProjectKindService}
	config.ApplyDerivedDefaults(cfg)
	return cfg
}

// The Docker Build job is part of every service project's ci.yml. It is the
// job houndersclub lost when `forge generate` re-scaffolded ci.yml: the
// generate-side mapper never set HasDocker.
func TestCIWorkflows_ServiceGetsDockerBuild(t *testing.T) {
	files := CIWorkflows(t.TempDir(), serviceCfg(), nil)
	ci, ok := ciFile(files, ".github/workflows/ci.yml").(templates.CIWorkflowData)
	if !ok {
		t.Fatalf("no ci.yml in %+v", files)
	}
	if !ci.HasDocker {
		t.Fatal("ci.yml data for a service project has HasDocker=false — the docker-build job would vanish")
	}
	out, err := templates.CITemplates("github").Render("ci.yml.tmpl", ci)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "  docker-build:\n") {
		t.Fatalf("rendered ci.yml has no docker-build job:\n%s", out)
	}
}

// A service project gets buf lint and proto-breaking.yml even before it
// declares its first service proto: `forge generate` runs `buf generate`
// over proto/config on that project, so a verify-generated job without buf
// could not reproduce the committed tree.
func TestCIWorkflows_ServiceWithoutServiceProtoStillGetsBuf(t *testing.T) {
	files := CIWorkflows(t.TempDir(), serviceCfg(), nil)
	ci := ciFile(files, ".github/workflows/ci.yml").(templates.CIWorkflowData)
	if !ci.HasServices || !ci.LintBuf {
		t.Fatalf("service project must lint protos with buf: HasServices=%v LintBuf=%v", ci.HasServices, ci.LintBuf)
	}
	if ciFile(files, ".github/workflows/proto-breaking.yml") == nil {
		t.Fatal("service project must get proto-breaking.yml")
	}
}

// CLI and library projects get only what they can run: no Docker image, no
// deploy, no proto-breaking, and no job that `needs:` one of those.
func TestCIWorkflows_CLIGetsTheBuildableSubset(t *testing.T) {
	cfg := &config.ProjectConfig{Name: "tool", ModulePath: "github.com/example/tool", Kind: config.ProjectKindCLI}
	config.ApplyDerivedDefaults(cfg)
	files := CIWorkflows(t.TempDir(), cfg, []templates.FrontendCIConfig{{Name: "web", Path: "frontends/web"}})

	var dests []string
	for _, f := range files {
		dests = append(dests, f.Dest)
	}
	if got := strings.Join(dests, ","); got != ".github/workflows/ci.yml,.github/dependabot.yml" {
		t.Fatalf("CLI workflow set = %s", got)
	}
	ci := ciFile(files, ".github/workflows/ci.yml").(templates.CIWorkflowData)
	if ci.HasDocker || ci.HasServices || ci.HasFrontends || ci.LintBuf || ci.VulnDocker || ci.E2EEnabled {
		t.Fatalf("CLI ci.yml carries service-only jobs: %+v", ci)
	}
	if !ci.VerifyGenerated {
		t.Fatal("verify-generated applies to every kind: contract mocks drift in CLIs too")
	}
}

// A CLI never gets the reconcile workflow or the cut-release job, even with
// the flag set: there is no image and nothing deployed to reconcile.
func TestCIWorkflows_CLIIgnoresReconcileFlag(t *testing.T) {
	cfg := &config.ProjectConfig{Name: "tool", Kind: config.ProjectKindCLI}
	cfg.Features.Experimental.Reconcile = true
	config.ApplyDerivedDefaults(cfg)
	for _, f := range CIWorkflows(t.TempDir(), cfg, nil) {
		switch f.Dest {
		case ".github/workflows/reconcile.yml", ".github/workflows/build-images.yml", ".github/workflows/deploy.yml":
			t.Errorf("%s planned for a CLI project", f.Dest)
		}
	}
}

// e2e.yml runs `task test:e2e` over ./e2e/..., so it is emitted only where a
// suite exists: the generated harness is on disk, or ci.e2e.enabled opts in.
// Otherwise the job fails on `lstat ./e2e/`.
func TestCIWorkflows_E2EWorkflowNeedsASuite(t *testing.T) {
	root := t.TempDir()
	if ciFile(CIWorkflows(root, serviceCfg(), nil), ".github/workflows/e2e.yml") != nil {
		t.Error("e2e.yml emitted for a project with no e2e/ harness and ci.e2e off")
	}

	if err := os.MkdirAll(filepath.Join(root, "e2e", "item"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ciFile(CIWorkflows(root, serviceCfg(), nil), ".github/workflows/e2e.yml") == nil {
		t.Error("e2e.yml missing for a project that carries the generated e2e/ harness")
	}

	optedIn := serviceCfg()
	optedIn.CI.E2E.Enabled = true
	if ciFile(CIWorkflows(t.TempDir(), optedIn, nil), ".github/workflows/e2e.yml") == nil {
		t.Error("e2e.yml missing with ci.e2e.enabled")
	}
}

// The deploy workflow assigns meaning by POSITION: envs[0] auto-deploys
// on every successful image build on main, envs[len-1] carries the
// environment-protection gate and is where a `v*` release tag ships.
// Filesystem discovery returns envs alphabetically, so an unsorted list
// put PROD first: prod auto-deployed on every merge to main with no
// protection, and the release tag shipped to staging forever.
func TestSortByPromotionOrder_ProdIsLast(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"default scaffold, alphabetical", []string{"prod", "staging"}, []string{"staging", "prod"}},
		{"three stages", []string{"preprod", "prod", "staging"}, []string{"staging", "preprod", "prod"}},
		{"bespoke env sorts mid-pipeline", []string{"prod", "sandbox", "staging"}, []string{"staging", "sandbox", "prod"}},
		{"production spelled out", []string{"production", "staging"}, []string{"staging", "production"}},
		{"single env", []string{"prod"}, []string{"prod"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envs := make([]templates.DeployEnv, 0, len(tc.in))
			for _, n := range tc.in {
				envs = append(envs, templates.DeployEnv{Name: n})
			}
			sortByPromotionOrder(envs)
			got := make([]string, 0, len(envs))
			for _, e := range envs {
				got = append(got, e.Name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("promotion order = %v, want %v", got, tc.want)
			}
		})
	}
}

// forge.yaml's deploy.environments, when declared, is used verbatim —
// including its auto/protection choices.
func TestCIDeployEnvs_ConfigWins(t *testing.T) {
	cfg := serviceCfg()
	cfg.Deploy.Environments = []config.DeployEnvConfig{{Name: "prod", Protection: true, URL: "https://example.com"}}
	envs := ciDeployEnvs(cfg, []string{"dev", "staging", "prod"})
	if len(envs) != 1 || envs[0].Name != "prod" || !envs[0].Protection || envs[0].URL != "https://example.com" {
		t.Fatalf("declared deploy.environments must be used verbatim, got %+v", envs)
	}
}
