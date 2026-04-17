package cli

import (
	"fmt"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/templates"
)

// generateCIWorkflows generates GitHub Actions workflow files from the project config.
// It uses the checksum system to detect user modifications and skip overwriting them.
func generateCIWorkflows(root string, cfg *config.ProjectConfig, cs *generator.FileChecksums, force bool) error {
	if cfg.CI.Provider != "" && cfg.CI.Provider != "github" {
		return nil // only github supported for now
	}

	provider := "github"

	// Build template data from config
	ciData := buildCIWorkflowData(cfg)
	deployData := buildDeployWorkflowData(cfg)
	buildData := buildBuildImagesWorkflowData(cfg)

	// ── ci.yml ──
	ciContent, err := templates.CITemplates(provider).Render("ci.yml.tmpl", ciData)
	if err != nil {
		return fmt.Errorf("render ci.yml: %w", err)
	}
	written, err := generator.WriteGeneratedFile(root, ".github/workflows/ci.yml", ciContent, cs, force)
	if err != nil {
		return fmt.Errorf("write ci.yml: %w", err)
	}
	if written {
		fmt.Println("  ✅ Generated .github/workflows/ci.yml")
	} else {
		fmt.Println("  ⚠️  .github/workflows/ci.yml has local modifications, skipping (use --force to overwrite)")
	}

	// ── build-images.yml ──
	buildContent, err := templates.CITemplates(provider).Render("build-images.yml.tmpl", buildData)
	if err != nil {
		return fmt.Errorf("render build-images.yml: %w", err)
	}
	written, err = generator.WriteGeneratedFile(root, ".github/workflows/build-images.yml", buildContent, cs, force)
	if err != nil {
		return fmt.Errorf("write build-images.yml: %w", err)
	}
	if written {
		fmt.Println("  ✅ Generated .github/workflows/build-images.yml")
	} else {
		fmt.Println("  ⚠️  .github/workflows/build-images.yml has local modifications, skipping (use --force to overwrite)")
	}

	// ── deploy.yml ──
	deployContent, err := templates.CITemplates(provider).Render("deploy.yml.tmpl", deployData)
	if err != nil {
		return fmt.Errorf("render deploy.yml: %w", err)
	}
	written, err = generator.WriteGeneratedFile(root, ".github/workflows/deploy.yml", deployContent, cs, force)
	if err != nil {
		return fmt.Errorf("write deploy.yml: %w", err)
	}
	if written {
		fmt.Println("  ✅ Generated .github/workflows/deploy.yml")
	} else {
		fmt.Println("  ⚠️  .github/workflows/deploy.yml has local modifications, skipping (use --force to overwrite)")
	}

	// ── e2e.yml (only if E2E enabled) ──
	if cfg.CI.E2E.Enabled {
		e2eData := buildE2EWorkflowData(cfg)
		e2eContent, err := templates.CITemplates(provider).Render("e2e.yml.tmpl", e2eData)
		if err != nil {
			return fmt.Errorf("render e2e.yml: %w", err)
		}
		written, err = generator.WriteGeneratedFile(root, ".github/workflows/e2e.yml", e2eContent, cs, force)
		if err != nil {
			return fmt.Errorf("write e2e.yml: %w", err)
		}
		if written {
			fmt.Println("  ✅ Generated .github/workflows/e2e.yml")
		} else {
			fmt.Println("  ⚠️  .github/workflows/e2e.yml has local modifications, skipping (use --force to overwrite)")
		}
	}

	// ── dependabot.yml ──
	depData := buildDependabotData(cfg)
	depContent, err := templates.CITemplates(provider).Render("dependabot.yml.tmpl", depData)
	if err != nil {
		return fmt.Errorf("render dependabot.yml: %w", err)
	}
	written, err = generator.WriteGeneratedFile(root, ".github/dependabot.yml", depContent, cs, force)
	if err != nil {
		return fmt.Errorf("write dependabot.yml: %w", err)
	}
	if written {
		fmt.Println("  ✅ Generated .github/dependabot.yml")
	} else {
		fmt.Println("  ⚠️  .github/dependabot.yml has local modifications, skipping (use --force to overwrite)")
	}

	return nil
}

// buildCIWorkflowData maps a ProjectConfig to the CI workflow template data.
func buildCIWorkflowData(cfg *config.ProjectConfig) templates.CIWorkflowData {
	goVersion := cfg.CI.EffectiveGoVersion()
	hasFrontends := len(cfg.Frontends) > 0
	hasServices := len(cfg.Services) > 0

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

	// Collect environments for KCL validation
	var envs []string
	for _, e := range cfg.Envs {
		envs = append(envs, e.Name)
	}

	return templates.CIWorkflowData{
		ProjectName:  cfg.Name,
		GoVersion:    goVersion,
		HasFrontends: hasFrontends,
		Frontends:    frontends,
		HasServices:  hasServices,

		LintGolangci: allLintDefault || lintCfg.Golangci,
		LintBuf:      allLintDefault || lintCfg.Buf,
		LintFrontend: allLintDefault || lintCfg.Frontend,

		TestRace:     allTestDefault || testCfg.Race,
		TestCoverage: testCfg.Coverage,

		VulnGo:     allVulnDefault || vulnCfg.Go,
		VulnDocker:  allVulnDefault || vulnCfg.Docker,
		VulnNPM:     allVulnDefault || vulnCfg.NPM,

		LicenseCheck: true,

		E2EEnabled:  cfg.CI.E2E.Enabled,
		E2ERuntime:  effectiveE2ERuntime(cfg),

		PermContents: cfg.CI.EffectivePermContents(),

		HasKCL:       len(envs) > 0,
		Environments: envs,

		ForgeVersion:   buildinfo.Version(),
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
	// If no deploy environments configured, derive defaults from project envs.
	// Convention (matches the hardcoded defaults in new-project scaffolding):
	//   * the first cloud env auto-deploys after a successful image build
	//     (workflow_run trigger) — this is typically "staging"
	//   * the last cloud env is gated behind environment protection — this
	//     is typically "prod"
	// Without these defaults the deploy.yml template's `{{- if $env.Auto}}`
	// branch never fires, leaving the workflow_run trigger at the top of the
	// file unreachable from any job `if:` (H-5).
	if len(envs) == 0 {
		for _, e := range cfg.Envs {
			if e.Type == "cloud" {
				envs = append(envs, templates.DeployEnv{
					Name: e.Name,
				})
			}
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
