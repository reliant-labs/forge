package cli

import (
	"fmt"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/templates"
)

// writeCIScaffold writes a generated CI workflow as a scaffold ("yours"):
// write-once when absent, user-owned from birth, NO forge:hash marker,
// and never re-emitted while the file exists. CI workflows are the
// canonical hand-edited policy file (add jobs, secrets, custom steps),
// so certifying them Tier-1 mis-flagged every sanctioned edit as
// `user_edited_gen_files` drift and pushed users toward `forge disown`.
// The derived jobs (frontend lint, KCL-env matrix, verify-generated) are
// a convenience starting point, not a correctness requirement — a stale
// workflow still runs, unlike buf.yaml whose derived dep gates the build
// — so write-once is the right lifecycle. To refresh, delete and re-run.
func writeCIScaffold(root, relPath string, content []byte) error {
	written, err := generator.WriteScaffoldIfMissing(root, relPath, content)
	if err != nil {
		return fmt.Errorf("write %s: %w", relPath, err)
	}
	if written {
		fmt.Printf("  ✅ Generated %s\n", relPath)
	} else {
		fmt.Printf("  ⏭️  %s exists — yours to edit, leaving it untouched\n", relPath)
	}
	return nil
}

// generateCIWorkflows generates GitHub Actions workflow files from the project config.
// They are emitted as write-once scaffolds (see writeCIScaffold): the
// user owns them after the first write and forge never stomps edits.
//
// Kind branching mirrors `forge new`'s project_ci.go: service kinds get
// the full set (build-images + deploy + e2e + proto-breaking), while
// CLI/library kinds only get the buildable subset (ci.yml + dependabot)
// because they have no Docker images, no k8s deploys, no service to
// stand up, and (for CLIs) typically no protos. Without this guard,
// `forge generate` on a CLI project drifts from `forge new`: it would
// emit build-images.yml + deploy.yml that reference services and
// registries the project does not have.
func generateCIWorkflows(root string, cfg *config.ProjectConfig, cs *generator.FileChecksums, force bool) error {
	if cfg.CI.Provider != "" && cfg.CI.Provider != "github" {
		return nil // only github supported for now
	}

	provider := "github"

	kind := config.EffectiveProjectKind(cfg.Kind)
	isService := kind == config.ProjectKindService

	// Build template data from config
	ciData := buildCIWorkflowData(cfg, root)

	// ── ci.yml ──
	ciContent, err := templates.CITemplates(provider).Render("ci.yml.tmpl", ciData)
	if err != nil {
		return fmt.Errorf("render ci.yml: %w", err)
	}
	if err := writeCIScaffold(root, ".github/workflows/ci.yml", ciContent); err != nil {
		return err
	}

	if isService {
		deployData := buildDeployWorkflowData(cfg)
		buildData := buildBuildImagesWorkflowData(cfg)

		// ── build-images.yml ──
		buildContent, err := templates.CITemplates(provider).Render("build-images.yml.tmpl", buildData)
		if err != nil {
			return fmt.Errorf("render build-images.yml: %w", err)
		}
		if err := writeCIScaffold(root, ".github/workflows/build-images.yml", buildContent); err != nil {
			return err
		}

		// ── deploy.yml ──
		deployContent, err := templates.CITemplates(provider).Render("deploy.yml.tmpl", deployData)
		if err != nil {
			return fmt.Errorf("render deploy.yml: %w", err)
		}
		if err := writeCIScaffold(root, ".github/workflows/deploy.yml", deployContent); err != nil {
			return err
		}
	}

	// ── e2e.yml (only if E2E enabled and project is a service) ──
	if isService && cfg.CI.E2E.Enabled {
		e2eData := buildE2EWorkflowData(cfg)
		e2eContent, err := templates.CITemplates(provider).Render("e2e.yml.tmpl", e2eData)
		if err != nil {
			return fmt.Errorf("render e2e.yml: %w", err)
		}
		if err := writeCIScaffold(root, ".github/workflows/e2e.yml", e2eContent); err != nil {
			return err
		}
	}

	// ── proto-breaking.yml ──
	if ciData.LintBufBreaking && ciData.HasServices {
		breakingContent, err := templates.CITemplates(provider).Render("proto-breaking.yml.tmpl", ciData)
		if err != nil {
			return fmt.Errorf("render proto-breaking.yml: %w", err)
		}
		if err := writeCIScaffold(root, ".github/workflows/proto-breaking.yml", breakingContent); err != nil {
			return err
		}
	}

	// ── dependabot.yml ──
	depData := buildDependabotData(cfg)
	depContent, err := templates.CITemplates(provider).Render("dependabot.yml.tmpl", depData)
	if err != nil {
		return fmt.Errorf("render dependabot.yml: %w", err)
	}
	if err := writeCIScaffold(root, ".github/dependabot.yml", depContent); err != nil {
		return err
	}

	return nil
}

// buildCIWorkflowData maps a ProjectConfig to the CI workflow template data.
func buildCIWorkflowData(cfg *config.ProjectConfig, root string) templates.CIWorkflowData {
	goVersion := cfg.CI.EffectiveGoVersion()
	hasFrontends := len(cfg.Frontends) > 0
	// Services are declared either in forge.yaml components or proto-first
	// (proto/ service declarations with no components entry). CI workflows
	// must see the same shape the generate pipeline sees, so consult proto
	// truth too — otherwise proto-first projects scaffold ci.yml without buf
	// steps and never get proto-breaking.yml.
	hasServices := len(cfg.Servers()) > 0 || projectDefinesConnectServices(root)

	var frontends []templates.FrontendCIConfig
	for _, fe := range cfg.Frontends {
		p := fe.Path
		if p == "" {
			p = "frontends/" + fe.Name
		}
		frontends = append(frontends, templates.FrontendCIConfig{Name: fe.Name, Path: p})
	}

	// Zero-value CILintConfig means "all enabled" (sensible default)
	lintCfg := cfg.CI.Lint
	allLintDefault := lintCfg == (config.CILintConfig{})

	vulnCfg := cfg.CI.VulnScan
	allVulnDefault := vulnCfg == (config.CIVulnConfig{})

	testCfg := cfg.CI.Test
	allTestDefault := testCfg == (config.CITestConfig{})

	// Collect environments for KCL validation — source of truth is
	// the filesystem (deploy/kcl/<env>/main.k presence).
	envs, _ := ListEnvs(projectDirForKCL())

	return templates.CIWorkflowData{
		ProjectName:  cfg.Name,
		GoVersion:    goVersion,
		HasFrontends: hasFrontends,
		Frontends:    frontends,
		HasServices:  hasServices,

		LintGolangci:        allLintDefault || lintCfg.Golangci,
		LintBuf:             allLintDefault || lintCfg.Buf,
		LintBufBreaking:     allLintDefault || lintCfg.BufBreaking,
		LintFrontend:        allLintDefault || lintCfg.Frontend,
		LintFrontendStyles:  (allLintDefault || lintCfg.Frontend) && cfg.Lint.Frontend.CSSHealth,
		LintMigrationSafety: allLintDefault || lintCfg.MigrationSafety,

		TestRace:     allTestDefault || testCfg.Race,
		TestCoverage: testCfg.Coverage,

		VulnGo:     allVulnDefault || vulnCfg.Go,
		VulnDocker: allVulnDefault || vulnCfg.Docker,
		VulnNPM:    allVulnDefault || vulnCfg.NPM,

		LicenseCheck: true,

		E2EEnabled: cfg.CI.E2E.Enabled,
		E2ERuntime: effectiveE2ERuntime(cfg),

		PermContents: cfg.CI.EffectivePermContents(),

		HasKCL:       len(envs) > 0,
		Environments: envs,

		// VerifyGenerated runs `forge generate` + `git diff --exit-code`
		// in CI to catch silent codegen-mock drift (a contract.go grows
		// a parameter, the mock_gen.go is not refreshed, tests in an
		// unrelated package fail). On regeneration we want the same
		// answer `forge new` chose at scaffold time — true regardless
		// of project kind. Without this, `forge generate` would
		// overwrite the scaffold-time CI workflow with a flag-stripped
		// version (the bug is silent: ci.yml renders fine, just without
		// the verify job).
		VerifyGenerated: true,

		// Stamp the INSTALLABLE version (release tag or clean pseudo-version)
		// so the CI `go install` ref is resolvable — never a `+dirty` build.
		// A dirty/dev binary yields "" here and the template pins by SHA
		// instead (fr-8c8a24ea97).
		ForgeVersion:   buildinfo.InstallableVersion(),
		ForgeGitCommit: buildinfo.GitCommit(),
	}
}

// buildDeployWorkflowData maps a ProjectConfig to the deploy workflow template data.
func buildDeployWorkflowData(cfg *config.ProjectConfig) templates.DeployWorkflowData {
	var envs []templates.DeployEnv
	for _, e := range cfg.Deploy.Environments {
		envs = append(envs, templates.DeployEnv{
			Name:       e.Name,
			Auto:       e.Auto,
			Protection: e.Protection,
			URL:        e.URL,
		})
	}
	// If no deploy environments configured, derive defaults from the
	// envs declared on the filesystem (deploy/kcl/<env>/main.k). The
	// "dev" env is treated as local-only and skipped; every non-dev env
	// is treated as cloud.
	// Convention (matches the hardcoded defaults in new-project scaffolding):
	//   * the first cloud env auto-deploys after a successful image build
	//     (workflow_run trigger) — this is typically "staging"
	//   * the last cloud env is gated behind environment protection — this
	//     is typically "prod"
	// Without these defaults the deploy.yml template's `{{- if $env.Auto}}`
	// branch never fires, leaving the workflow_run trigger at the top of the
	// file unreachable from any job `if:` (H-5).
	if len(envs) == 0 {
		discovered, _ := ListEnvs(projectDirForKCL())
		for _, name := range discovered {
			if name == "dev" {
				continue
			}
			envs = append(envs, templates.DeployEnv{Name: name})
		}
		if len(envs) > 0 {
			envs[0].Auto = true
			envs[len(envs)-1].Protection = true
		}
	}

	return templates.DeployWorkflowData{
		ProjectName:      cfg.Name,
		Environments:     envs,
		Registry:         cfg.Deploy.EffectiveRegistry(),
		HasFrontends:     len(cfg.Frontends) > 0,
		FrontendDeploy:   cfg.Deploy.FrontendDeploy,
		MigrationTest:    cfg.Deploy.MigrationTest,
		Concurrency:      cfg.Deploy.IsConcurrencyEnabled(),
		CancelInProgress: cfg.Deploy.Concurrency.CancelInProgress,
	}
}

// buildBuildImagesWorkflowData maps a ProjectConfig to the build-images workflow template data.
func buildBuildImagesWorkflowData(cfg *config.ProjectConfig) templates.BuildImagesWorkflowData {
	vulnCfg := cfg.CI.VulnScan
	allVulnDefault := vulnCfg == (config.CIVulnConfig{})

	return templates.BuildImagesWorkflowData{
		ProjectName:  cfg.Name,
		Registry:     cfg.Deploy.EffectiveRegistry(),
		HasFrontends: len(cfg.Frontends) > 0,
		VulnDocker:   allVulnDefault || vulnCfg.Docker,
	}
}

// buildE2EWorkflowData maps a ProjectConfig to the E2E workflow template data.
func buildE2EWorkflowData(cfg *config.ProjectConfig) templates.E2EWorkflowData {
	var fePath string
	if len(cfg.Frontends) > 0 {
		fePath = cfg.Frontends[0].Path
	}
	return templates.E2EWorkflowData{
		ProjectName:  cfg.Name,
		GoVersion:    cfg.CI.EffectiveGoVersion(),
		Runtime:      effectiveE2ERuntime(cfg),
		HasFrontends: len(cfg.Frontends) > 0,
		FrontendPath: fePath,
	}
}

// buildDependabotData builds template data for the dependabot config.
// The dependabot template uses FrontendName (singular) for the npm directory.
func buildDependabotData(cfg *config.ProjectConfig) struct{ FrontendName string } {
	var feName string
	if len(cfg.Frontends) > 0 {
		feName = cfg.Frontends[0].Name
	}
	return struct{ FrontendName string }{FrontendName: feName}
}

// effectiveE2ERuntime returns the E2E runtime, defaulting to "docker-compose".
func effectiveE2ERuntime(cfg *config.ProjectConfig) string {
	if cfg.CI.E2E.Runtime != "" {
		return cfg.CI.E2E.Runtime
	}
	return "docker-compose"
}
