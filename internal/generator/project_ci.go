package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/templates"
)

// githubOwnerFromModulePath extracts the GitHub owner segment from a Go
// module path like `github.com/example/demo` (-> "example"). Returns ""
// for non-github hosts or any path that doesn't have an owner segment.
// Used to seed the default `.github/CODEOWNERS` entry; when inference
// fails the generator skips emitting the file rather than shipping a
// review-free stub.
func githubOwnerFromModulePath(modulePath string) string {
	const host = "github.com/"
	if !strings.HasPrefix(modulePath, host) {
		return ""
	}
	rest := modulePath[len(host):]
	slash := strings.Index(rest, "/")
	if slash <= 0 {
		return ""
	}
	return rest[:slash]
}

func (g *ProjectGenerator) generateCIFiles() error { //nolint:funlen // length is embedded GitHub Actions YAML, not control flow.
	provider := "github"

	hasFrontends := g.FrontendName != ""
	var frontends []templates.FrontendCIConfig
	if hasFrontends {
		frontends = []templates.FrontendCIConfig{
			{Name: g.FrontendName, Path: fmt.Sprintf("frontends/%s", g.FrontendName)},
		}
	}

	githubOwner := githubOwnerFromModulePath(g.ModulePath)

	data := templates.CIWorkflowData{
		ProjectName:  g.Name,
		HasFrontends: hasFrontends,
		Frontends:    frontends,
		HasServices:  true,

		LintGolangci:        true,
		LintBuf:             true,
		LintBufBreaking:     true,
		LintFrontend:        hasFrontends,
		LintFrontendStyles:  hasFrontends,
		LintMigrationSafety: true,

		TestRace:     true,
		TestCoverage: false,

		VulnGo:     true,
		VulnDocker: true,
		VulnNPM:    hasFrontends,

		LicenseCheck: true,

		E2EEnabled: false,

		PermContents: "read",

		HasKCL:          true,
		HasDocker:       true,
		VerifyGenerated: true,
		Environments:    []string{"dev", "staging", "prod"},

		// Legacy fields for other CI templates
		Module:       g.ModulePath,
		Registry:     "ghcr",
		FrontendName: g.FrontendName,
		GitHubOwner:  githubOwner,
	}

	// The deploy environments, shared by the deploy workflow and the
	// reconcile matrix. ONE list rather than two literals: the two workflows
	// must agree about which environments exist, and two lists would be free
	// to drift into reconciling an environment nothing deploys.
	//
	// Read from disk, never hard-coded: the KCL envs are written before the
	// workflows, and a workflow job for an env with no deploy/kcl/<env>/main.k
	// fails on every run. A project converted in place keeps its own env set
	// (houndersclub had dev + prod and got a deploy-staging job wired to every
	// image build on main).
	deployEnvs := scaffoldDeployEnvs(g.Path)

	// Deploy and build-images use their own spec-driven data types
	var frontendPath string
	if hasFrontends {
		frontendPath = fmt.Sprintf("frontends/%s", g.FrontendName)
	}
	deployData := templates.DeployWorkflowData{
		ProjectName:      g.Name,
		Environments:     deployEnvs,
		Registry:         "ghcr",
		HasFrontends:     hasFrontends,
		FrontendPath:     frontendPath,
		FrontendDeploy:   "none",
		MigrationTest:    false,
		Concurrency:      true,
		CancelInProgress: false,
	}

	buildImagesData := templates.BuildImagesWorkflowData{
		ProjectName: g.Name,
		Registry:    "ghcr",
		VulnDocker:  true,
		// The cut-release + promote job rides the same gate as the reconcile
		// workflow. Both talk to a control plane, and a project that has not
		// opted into that machinery must not get CI steps that fail on every
		// push to main against a server it does not have.
		CutRelease: g.Features.ReconcileEnabled(),
	}

	reconcileData := templates.ReconcileWorkflowData{
		ProjectName:  g.Name,
		Environments: deployEnvs,
	}

	var e2eFrontendPath string
	if hasFrontends {
		e2eFrontendPath = fmt.Sprintf("frontends/%s", g.FrontendName)
	}
	e2eData := templates.E2EWorkflowData{
		ProjectName:  g.Name,
		Runtime:      "docker-compose",
		HasFrontends: hasFrontends,
		FrontendPath: e2eFrontendPath,
	}

	// Templated files — each with its own data type. The full set is
	// emitted for service kinds; CLI/library kinds get just ci.yml +
	// dependabot since they have no Docker images, no deploys, and (for
	// CLIs) typically no protos.
	var templatedFiles []struct {
		templateName string
		dest         string
		data         interface{}
	}
	if g.isService() {
		templatedFiles = []struct {
			templateName string
			dest         string
			data         interface{}
		}{
			{"ci.yml.tmpl", ".github/workflows/ci.yml", data},
			{"build-images.yml.tmpl", ".github/workflows/build-images.yml", buildImagesData},
			{"deploy.yml.tmpl", ".github/workflows/deploy.yml", deployData},
			{"e2e.yml.tmpl", ".github/workflows/e2e.yml", e2eData},
			{"proto-breaking.yml.tmpl", ".github/workflows/proto-breaking.yml", data},
			{"dependabot.yml.tmpl", ".github/dependabot.yml", data},
		}
		// OPT-IN, OFF BY DEFAULT — the same à la carte rule every other
		// forge feature follows. `forge reconcile` only exists for a project
		// whose reconcile loop is wired, so scaffolding a scheduled workflow
		// that calls it unconditionally would hand every project an hourly
		// failing job for a feature it never enabled.
		if g.Features.ReconcileEnabled() {
			templatedFiles = append(templatedFiles, struct {
				templateName string
				dest         string
				data         interface{}
			}{"reconcile.yml.tmpl", ".github/workflows/reconcile.yml", reconcileData})
		}
	} else {
		// CLI/library: lint + test + vuln scan still apply, but skip
		// proto-breaking (no proto/services in CLI), build-images
		// (no Dockerfile), deploy (no k8s), and e2e (no service to
		// stand up).
		// The ci.yml template branches on HasFrontends + LintBuf etc;
		// for CLI mode we strip frontend hooks and proto-related lints
		// so the rendered workflow is buildable.
		data.LintBuf = false
		data.LintBufBreaking = false
		data.LintFrontend = false
		data.LintFrontendStyles = false
		data.LintMigrationSafety = false
		data.VulnDocker = false
		data.VulnNPM = false
		data.HasFrontends = false
		data.HasServices = false
		data.HasKCL = false
		data.HasDocker = false
		// VerifyGenerated stays true for CLI/library kinds: even without
		// proto codegen, contract-driven mock generation (`forge generate`
		// emits `mock_gen.go` for any package with a contract.go) drifts
		// silently when a contract method gains a parameter and the mock
		// is not refreshed. The drift surfaces only at test time, often
		// in unrelated packages. CI's verify-generate gate catches it
		// pre-merge regardless of project kind.
		data.VerifyGenerated = true
		data.Frontends = nil
		templatedFiles = []struct {
			templateName string
			dest         string
			data         interface{}
		}{
			{"ci.yml.tmpl", ".github/workflows/ci.yml", data},
			{"dependabot.yml.tmpl", ".github/dependabot.yml", data},
		}
	}

	// Load checksums to record initial CI file hashes
	cs, err := LoadChecksums(g.Path)
	if err != nil {
		return fmt.Errorf("load checksums: %w", err)
	}

	// Record the binary that produced the CI files in forge's ownership
	// state. (The workflows themselves pin nothing: they install the forge
	// go.mod resolves at run time.)
	cs.ForgeVersion = buildinfo.Version()

	for _, f := range templatedFiles {
		content, err := templates.CITemplates(provider).Render(f.templateName, f.data)
		if err != nil {
			return fmt.Errorf("render CI template %s: %w", f.templateName, err)
		}
		// Scaffold-once ("yours"): CI workflows are the canonical
		// hand-edited policy file (add jobs, secrets, custom steps), so
		// they are user-owned from birth — NO forge:hash marker, never
		// re-emitted while present. Certifying them Tier-1 mis-flagged
		// every sanctioned edit as `user_edited_gen_files` drift and
		// pushed users to `forge project disown`. This mirrors the PR template /
		// CODEOWNERS starters written just below.
		if _, err := checksums.WriteScaffoldIfMissing(g.Path, f.dest, content); err != nil {
			return fmt.Errorf("write %s: %w", f.dest, err)
		}
	}

	// Static files
	staticFiles := []struct {
		templateName string
		dest         string
	}{
		{"pull_request_template.md", ".github/pull_request_template.md"},
	}

	for _, f := range staticFiles {
		content, err := templates.CITemplates(provider).Get(f.templateName)
		if err != nil {
			return fmt.Errorf("read CI template %s: %w", f.templateName, err)
		}
		// Scaffold-once ("yours"): the PR template is a one-shot starter
		// the user owns after creation — `forge generate` never re-emits
		// it, so a Tier-1/legacy record would put the user's own edits
		// under the stomp guard (FRICTION 2026-06-05, cp-forge: users
		// hand-flipped `forked: true` to escape exactly that
		// misclassification).
		if _, err := checksums.WriteScaffoldIfMissing(g.Path, f.dest, content); err != nil {
			return fmt.Errorf("write %s: %w", f.dest, err)
		}
	}

	// CODEOWNERS is only emitted when we can confidently infer a GitHub
	// owner from the module path. For non-github module paths (e.g.
	// `example.com/team/proj`) we skip the file entirely — shipping a
	// review-free stub that silently bypasses branch protection is worse
	// than having no file at all. Users can add `.github/CODEOWNERS`
	// manually when they're ready.
	if githubOwner != "" {
		content, err := templates.CITemplates(provider).Render("CODEOWNERS.tmpl", data)
		if err != nil {
			return fmt.Errorf("render CODEOWNERS: %w", err)
		}
		// Scaffold-once ("yours"): CODEOWNERS carries the `yours:
		// scaffolded once ... — starter` banner — review policy is the
		// user's to evolve, and edits must not trip the Tier-1 stomp
		// guard.
		if _, err := checksums.WriteScaffoldIfMissing(g.Path, ".github/CODEOWNERS", content); err != nil {
			return fmt.Errorf("write CODEOWNERS: %w", err)
		}
	}

	// Save checksums so forge generate knows what was initially generated
	if err := SaveChecksums(g.Path, cs); err != nil {
		return fmt.Errorf("save checksums: %w", err)
	}

	return nil
}

// scaffoldDeployEnvs lists the environments a deploy workflow should target:
// every deploy/kcl/<env>/main.k under projectDir except dev (which runs
// locally via `forge env up`), ordered along the promotion path. The first
// auto-deploys after a green image build on main; the last is protected.
func scaffoldDeployEnvs(projectDir string) []templates.DeployEnv {
	kclDir := filepath.Join(projectDir, "deploy", "kcl")
	entries, err := os.ReadDir(kclDir)
	if err != nil {
		return nil
	}
	var envs []templates.DeployEnv
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "dev" {
			continue
		}
		if _, err := os.Stat(filepath.Join(kclDir, e.Name(), "main.k")); err == nil {
			envs = append(envs, templates.DeployEnv{Name: e.Name()})
		}
	}
	// prod ships last; everything else precedes it alphabetically.
	sort.SliceStable(envs, func(i, j int) bool {
		pi, pj := envs[i].Name == "prod", envs[j].Name == "prod"
		if pi != pj {
			return pj
		}
		return envs[i].Name < envs[j].Name
	})
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
