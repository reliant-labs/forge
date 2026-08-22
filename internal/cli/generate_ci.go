package cli

import (
	"fmt"
	"sort"
	"strings"

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
// `user_edited_gen_files` drift and pushed users toward `forge project disown`.
// The derived jobs (frontend lint, KCL-env matrix, verify-generated) are
// a convenience starting point, not a correctness requirement — a stale
// workflow still runs, unlike buf.yaml whose derived dep gates the build
// — so write-once is the right lifecycle. Write-once covers deletion too:
// a repo that manages its own CI removes these and they stay removed. To
// re-scaffold, drop the path's entry from .forge/scaffolded.json.
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
// Kind branching mirrors `forge project new`'s project_ci.go: service kinds get
// the full set (build-images + deploy + e2e + proto-breaking), while
// CLI/library kinds only get the buildable subset (ci.yml + dependabot)
// because they have no Docker images, no k8s deploys, no service to
// stand up, and (for CLIs) typically no protos. Without this guard,
// `forge generate` on a CLI project drifts from `forge project new`: it would
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
		deployData := buildDeployWorkflowData(cfg, root)
		buildData := buildBuildImagesWorkflowData(cfg, root)

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
		e2eData := buildE2EWorkflowData(cfg, root)
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
	depData := buildDependabotData(cfg, root)
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
	// Frontend topology comes from deploy/kcl/<env>/main.k, not from the
	// retired forge.yaml `frontends:` key — see generate_ci_frontends.go for
	// why reading the key froze every project's workflows at whatever the
	// frontend happened to be called when the key still existed.
	declared := discoverCIFrontends(root, cfg)
	hasFrontends := len(declared) > 0
	// Services are declared by their protos. CI workflows must see the same
	// shape the generate pipeline sees, so ask the same source it does —
	// otherwise a project scaffolds ci.yml without buf steps and never gets
	// proto-breaking.yml.
	hasServices := projectDefinesConnectServices(root)

	var frontends []templates.FrontendCIConfig
	for _, fe := range declared {
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
	// the filesystem (deploy/kcl/<env>/main.k presence). Read from the
	// project root we were handed, not from the process cwd: the two agree
	// for a real `forge generate` and only the explicit root is testable.
	envs, _ := ListEnvs(root)

	return templates.CIWorkflowData{
		ProjectName:  cfg.Name,
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
		// answer `forge project new` chose at scaffold time — true regardless
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

// promotionRank orders an environment along the promotion path. The
// deploy workflow assigns meaning by POSITION — envs[0] auto-deploys on
// every successful image build on main, envs[len-1] is the protected
// env the `v*` release tag ships to — so the list must be ordered by
// promotion, never lexically.
//
// `ListEnvs` sorts alphabetically, which for the default scaffold
// (dev/prod/staging) yields [prod, staging]: PROD auto-deployed on
// every merge to main with no environment protection, staging carried
// the protection gate, and a `v*` release tag shipped to staging and
// could never reach prod. Ranking by name is what keeps position and
// meaning aligned.
//
// Unknown env names sort between staging and prod (alphabetically among
// themselves): a bespoke env is a mid-pipeline stage, and "prod" must
// stay last so the release tag and the protection gate land on it.
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

// buildDeployWorkflowData maps a ProjectConfig to the deploy workflow template data.
func buildDeployWorkflowData(cfg *config.ProjectConfig, root string) templates.DeployWorkflowData {
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
		discovered, _ := ListEnvs(root)
		for _, name := range discovered {
			if name == "dev" {
				continue
			}
			envs = append(envs, templates.DeployEnv{Name: name})
		}
		sortByPromotionOrder(envs)
		if len(envs) > 0 {
			envs[0].Auto = true
			envs[len(envs)-1].Protection = true
		}
	}

	return templates.DeployWorkflowData{
		ProjectName:      cfg.Name,
		Environments:     envs,
		Registry:         cfg.Deploy.EffectiveRegistry(),
		HasFrontends:     len(discoverCIFrontends(root, cfg)) > 0,
		FrontendDeploy:   cfg.Deploy.FrontendDeploy,
		MigrationTest:    cfg.Deploy.MigrationTest,
		Concurrency:      cfg.Deploy.IsConcurrencyEnabled(),
		CancelInProgress: cfg.Deploy.Concurrency.CancelInProgress,
	}
}

// buildBuildImagesWorkflowData maps a ProjectConfig to the build-images workflow template data.
func buildBuildImagesWorkflowData(cfg *config.ProjectConfig, root string) templates.BuildImagesWorkflowData {
	vulnCfg := cfg.CI.VulnScan
	allVulnDefault := vulnCfg == (config.CIVulnConfig{})

	return templates.BuildImagesWorkflowData{
		ProjectName:  cfg.Name,
		Registry:     cfg.Deploy.EffectiveRegistry(),
		HasFrontends: len(discoverCIFrontends(root, cfg)) > 0,
		VulnDocker:   allVulnDefault || vulnCfg.Docker,
	}
}

// buildE2EWorkflowData maps a ProjectConfig to the E2E workflow template data.
// The frontend path comes from the KCL topology (discoverCIFrontends), so a
// frontend rename re-derives here instead of freezing the old directory name
// into a workflow forge would never rewrite.
func buildE2EWorkflowData(cfg *config.ProjectConfig, root string) templates.E2EWorkflowData {
	declared := discoverCIFrontends(root, cfg)
	var fePath string
	if len(declared) > 0 {
		fePath = declared[0].Path
	}
	return templates.E2EWorkflowData{
		ProjectName:  cfg.Name,
		Runtime:      effectiveE2ERuntime(cfg),
		HasFrontends: len(declared) > 0,
		FrontendPath: fePath,
	}
}

// buildDependabotData builds template data for the dependabot config.
// The dependabot template uses FrontendName (singular) for the npm directory.
func buildDependabotData(cfg *config.ProjectConfig, root string) struct{ FrontendName string } {
	declared := discoverCIFrontends(root, cfg)
	var feName string
	if len(declared) > 0 {
		feName = declared[0].Name
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
