package generator

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// CIWorkflowFile is one scaffolded GitHub Actions file: the template that
// renders it, where it lands, and the data it renders with.
type CIWorkflowFile struct {
	Template string
	Dest     string
	Data     any
}

// CIWorkflows is THE mapping from a project to its scaffolded GitHub Actions
// files. `forge project new` (generateCIFiles) and `forge generate`
// (internal/cli's generateCIWorkflows) both call it; neither builds template
// data of its own.
//
// There used to be two mappers, one per command, and they disagreed on a
// project seconds old. houndersclub deleted ci.yml to pick up forge v0.1.18's
// template, re-ran `forge generate`, and lost the whole Docker Build job: the
// generate-side mapper never set HasDocker. It also dropped e2e.yml, the
// reconcile workflow, and — on a project without a service proto yet — every
// buf step, even though `forge generate` runs `buf generate` on that very
// project. TestCIWorkflows_NewAndGenerateRenderIdentically pins the two
// commands to identical bytes so a second mapper cannot grow back.
//
// root is the project directory, read for the facts that live on disk: the
// declared deploy envs (deploy/kcl/<env>/main.k) and the e2e harness (e2e/).
// cfg is the project config with derived defaults applied — what the loader
// returns, or what `forge project new` is about to write. frontends are the
// repo-relative frontend directories CI drives a Node toolchain for; the
// caller supplies them because only it knows the authoritative source (the
// KCL topology on a live project, the requested frontend at scaffold time).
func CIWorkflows(root string, cfg *config.ProjectConfig, frontends []templates.FrontendCIConfig) []CIWorkflowFile {
	isService := cfg.IsServiceKind()
	hasFrontends := isService && len(frontends) > 0
	if !hasFrontends {
		frontends = nil
	}
	// Services — and so buf, proto-breaking and the Docker image — are what
	// a service-kind project with codegen on is made of. Deliberately not
	// "a proto declares a service": the default scaffold has none yet, and
	// `forge generate` still runs `buf generate` over proto/config, so a
	// verify-generated job without buf could not reproduce the tree.
	hasServices := isService && cfg.Features.CodegenEnabled()

	lint := cfg.CI.Lint
	lintDefault := lint == (config.CILintConfig{})
	vuln := cfg.CI.VulnScan
	vulnDefault := vuln.UsesDefaultScanners()
	test := cfg.CI.Test
	testDefault := test == (config.CITestConfig{})

	kclEnvs := declaredKCLEnvs(root)
	deployEnvs := ciDeployEnvs(cfg, kclEnvs)
	// The once-per-commit image is built for the first deploy env in
	// promotion order — the one deploy.yml auto-deploys — and pushed to the
	// registry THAT env's KCL declares. With no deploy env there is no
	// registry to push to, so no build-images workflow is emitted.
	var buildEnv string
	if len(deployEnvs) > 0 {
		buildEnv = deployEnvs[0].Name
	}
	e2eRuntime := cfg.CI.E2E.Runtime
	if e2eRuntime == "" {
		e2eRuntime = "docker-compose"
	}
	// The e2e workflow runs `task test:e2e` over ./e2e/..., so it exists
	// only where there is a suite to run: a service project that either
	// opted in (ci.e2e.enabled) or carries the generated e2e/ harness. A
	// project with neither would get a job that fails on `lstat ./e2e/`.
	hasE2E := isService && (cfg.CI.E2E.Enabled || isDir(filepath.Join(root, "e2e")))

	var firstFrontendPath, firstFrontendName string
	if hasFrontends {
		firstFrontendPath, firstFrontendName = frontends[0].Path, frontends[0].Name
	}

	ci := templates.CIWorkflowData{
		ProjectName:  cfg.Name,
		HasFrontends: hasFrontends,
		Frontends:    frontends,
		HasServices:  hasServices,

		LintGolangci:        lintDefault || lint.Golangci,
		LintBuf:             hasServices && (lintDefault || lint.Buf),
		LintBufBreaking:     hasServices && (lintDefault || lint.BufBreaking),
		LintFrontend:        hasFrontends && (lintDefault || lint.Frontend),
		LintFrontendStyles:  hasFrontends && (lintDefault || lint.Frontend) && cfg.Lint.Frontend.CSSHealth,
		LintMigrationSafety: isService && (lintDefault || lint.MigrationSafety),

		TestRace:     testDefault || test.Race,
		TestCoverage: test.Coverage,

		VulnGo:     vulnDefault || vuln.Go,
		VulnDocker: isService && (vulnDefault || vuln.Docker),
		VulnNPM:    hasFrontends && (vulnDefault || vuln.NPM),

		LicenseCheck: true,

		E2EEnabled: isService && cfg.CI.E2E.Enabled,
		E2ERuntime: e2eRuntime,

		PermContents: cfg.CI.EffectivePermContents(),

		HasKCL:       isService && len(kclEnvs) > 0,
		Environments: kclEnvs,
		// Every service project ships a Dockerfile, so every one gets the
		// PR-time image build. This is the flag the generate-side mapper
		// never set.
		HasDocker: isService,
		// verify-generated applies to every kind: contract-driven mocks
		// (mock_gen.go) drift silently in CLI and library projects too.
		VerifyGenerated: true,

		Module:       cfg.ModulePath,
		FrontendName: firstFrontendName,
		GitHubOwner:  githubOwnerFromModulePath(cfg.ModulePath),
	}

	files := []CIWorkflowFile{{"ci.yml.tmpl", ".github/workflows/ci.yml", ci}}
	if isService {
		if buildEnv != "" {
			files = append(files, CIWorkflowFile{"build-images.yml.tmpl", ".github/workflows/build-images.yml", templates.BuildImagesWorkflowData{
				ProjectName:  cfg.Name,
				BuildEnv:     buildEnv,
				HasFrontends: hasFrontends,
				FrontendPath: firstFrontendPath,
				VulnDocker:   ci.VulnDocker,
				// Cut-release + promote talks to a control plane, the same
				// server the reconcile workflow does, so it rides the same
				// opt-in gate: a project without one must not get a job
				// that fails on every push to main.
				CutRelease: cfg.Features.ReconcileEnabled(),
			}})
		}
		files = append(files,
			CIWorkflowFile{"deploy.yml.tmpl", ".github/workflows/deploy.yml", templates.DeployWorkflowData{
				ProjectName:      cfg.Name,
				Environments:     deployEnvs,
				HasFrontends:     hasFrontends,
				FrontendPath:     firstFrontendPath,
				FrontendDeploy:   cfg.Deploy.FrontendDeploy,
				MigrationTest:    cfg.Deploy.MigrationTest,
				Concurrency:      cfg.Deploy.IsConcurrencyEnabled(),
				CancelInProgress: cfg.Deploy.Concurrency.CancelInProgress,
			}},
		)
		if hasE2E {
			files = append(files, CIWorkflowFile{"e2e.yml.tmpl", ".github/workflows/e2e.yml", templates.E2EWorkflowData{
				ProjectName:  cfg.Name,
				Runtime:      e2eRuntime,
				HasFrontends: hasFrontends,
				FrontendPath: firstFrontendPath,
			}})
		}
	}
	if ci.LintBufBreaking {
		files = append(files, CIWorkflowFile{"proto-breaking.yml.tmpl", ".github/workflows/proto-breaking.yml", ci})
	}
	// OPT-IN, OFF BY DEFAULT: `forge reconcile` only exists for a project
	// whose reconcile loop is wired, so an unconditional scheduled workflow
	// would hand every project an hourly failing job.
	if isService && cfg.Features.ReconcileEnabled() {
		files = append(files, CIWorkflowFile{"reconcile.yml.tmpl", ".github/workflows/reconcile.yml", templates.ReconcileWorkflowData{
			ProjectName:  cfg.Name,
			Environments: deployEnvs,
		}})
	}
	files = append(files, CIWorkflowFile{"dependabot.yml.tmpl", ".github/dependabot.yml", struct{ FrontendName string }{firstFrontendName}})
	return files
}

// declaredKCLEnvs lists every env with a deploy/kcl/<env>/main.k, sorted.
func declaredKCLEnvs(root string) []string {
	kclDir := filepath.Join(root, "deploy", "kcl")
	entries, err := os.ReadDir(kclDir)
	if err != nil {
		return nil
	}
	var envs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(kclDir, e.Name(), "main.k")); err == nil {
			envs = append(envs, e.Name())
		}
	}
	sort.Strings(envs)
	return envs
}

// ciDeployEnvs is the environment list the deploy and reconcile workflows
// share. ONE list rather than two: the workflows must agree about which
// environments exist.
//
// forge.yaml's deploy.environments wins when present. Otherwise the list is
// every declared KCL env except dev (which runs locally via `forge env up`),
// never a hard-coded one: a job for an env with no main.k fails on every run
// (houndersclub had dev + prod and was scaffolded a deploy-staging job).
//
// Position carries meaning — envs[0] auto-deploys after every green image
// build on main, envs[len-1] is the protected env a `v*` tag ships to — so
// the discovered list is ordered along the promotion path, never lexically.
func ciDeployEnvs(cfg *config.ProjectConfig, kclEnvs []string) []templates.DeployEnv {
	var envs []templates.DeployEnv
	for _, e := range cfg.Deploy.Environments {
		envs = append(envs, templates.DeployEnv{Name: e.Name, Auto: e.Auto, Protection: e.Protection, URL: e.URL})
	}
	if len(envs) > 0 {
		return envs
	}
	for _, name := range kclEnvs {
		if name != "dev" {
			envs = append(envs, templates.DeployEnv{Name: name})
		}
	}
	sortByPromotionOrder(envs)
	// A lone env is the one users reach — protected, and never
	// auto-deployed: auto-promoting it would ship every merge to main
	// straight to production.
	if len(envs) > 0 {
		envs[len(envs)-1].Protection = true
	}
	if len(envs) > 1 {
		envs[0].Auto = true
	}
	return envs
}

// promotionRank orders an environment along the promotion path. Unknown
// names sort between staging and prod: a bespoke env is a mid-pipeline
// stage, and prod must stay last so the release tag and the protection gate
// land on it.
func promotionRank(name string) int {
	switch strings.ToLower(name) {
	case "staging", "stage", "stg":
		return 10
	case "preprod", "pre-prod", "preproduction", "uat", "qa", "canary":
		return 20
	case "prod", "production":
		return 30
	default:
		return 15
	}
}

// sortByPromotionOrder orders envs in place along the promotion path,
// breaking rank ties by name so the output is deterministic.
func sortByPromotionOrder(envs []templates.DeployEnv) {
	sort.SliceStable(envs, func(i, j int) bool {
		ri, rj := promotionRank(envs[i].Name), promotionRank(envs[j].Name)
		if ri != rj {
			return ri < rj
		}
		return envs[i].Name < envs[j].Name
	})
}
