package generator

import (
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/internal/buildinfo"
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

func (g *ProjectGenerator) generateCIFiles() error {
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
		GoVersion:    goVersionMinor(g.resolveGoVersion()),
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
		GithubOrg:    g.Name,
		FrontendName: g.FrontendName,
		GitHubOwner:  githubOwner,

		// Stamp forge's version so `verify-generated` installs exactly the
		// same version that produced the scaffold. Git SHA is a fallback when
		// the binary was built without a version tag (local `dev` builds).
		ForgeVersion:   buildinfo.Version(),
		ForgeGitCommit: buildinfo.GitCommit(),
	}

	// Deploy and build-images use their own spec-driven data types
	deployData := templates.DeployWorkflowData{
		ProjectName: g.Name,
		Environments: []templates.DeployEnv{
			{Name: "staging", Auto: true, Protection: false},
			{Name: "prod", Auto: false, Protection: true},
		},
		Registry:         "ghcr",
		HasFrontends:     hasFrontends,
		FrontendDeploy:   "none",
		MigrationTest:    false,
		Concurrency:      true,
		CancelInProgress: false,
	}

	buildImagesData := templates.BuildImagesWorkflowData{
		ProjectName:  g.Name,
		Registry:     "ghcr",
		HasFrontends: hasFrontends,
		VulnDocker:   true,
	}

	var e2eFrontendPath string
	if hasFrontends {
		e2eFrontendPath = fmt.Sprintf("frontends/%s", g.FrontendName)
	}
	e2eData := templates.E2EWorkflowData{
		ProjectName:  g.Name,
		GoVersion:    goVersionMinor(g.resolveGoVersion()),
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

	// Keep the stamped forge version in sync with the binary that produced
	// the CI files. This allows CI `verify-generated` to pin the exact forge
	// version via install.
	cs.ForgeVersion = buildinfo.Version()

	for _, f := range templatedFiles {
		content, err := templates.CITemplates(provider).Render(f.templateName, f.data)
		if err != nil {
			return fmt.Errorf("render CI template %s: %w", f.templateName, err)
		}
		// Use WriteGeneratedFile to record the checksum. force=true since
		// this is initial project creation — there's nothing to preserve.
		if _, err := WriteGeneratedFile(g.Path, f.dest, content, cs, true); err != nil {
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
		if _, err := WriteGeneratedFile(g.Path, f.dest, content, cs, true); err != nil {
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
		if _, err := WriteGeneratedFile(g.Path, ".github/CODEOWNERS", content, cs, true); err != nil {
			return fmt.Errorf("write CODEOWNERS: %w", err)
		}
	}

	// Save checksums so forge generate knows what was initially generated
	if err := SaveChecksums(g.Path, cs); err != nil {
		return fmt.Errorf("save checksums: %w", err)
	}

	return nil
}
