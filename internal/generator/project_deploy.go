package generator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/kclvendor"
	"github.com/reliant-labs/forge/internal/templates"
)

func (g *ProjectGenerator) generateKCLDeploy() error {
	deployDir := filepath.Join(g.Path, "deploy", "kcl")

	// Generate kcl.mod at deploy/kcl/ — the KCL package root for the
	// deploy manifests, so the env main.k files' package-rooted imports
	// (`import config_projection`, `import .config`, `import ..ingress`)
	// resolve. It also declares the `forge` module dependency the env
	// files import — the schemas live upstream in `forge/kcl/`, not in
	// the project's tree.
	kclModData := struct {
		ProjectName string
	}{
		ProjectName: g.Name,
	}
	kclModContent, err := templates.DeployTemplates().Render("kcl/kcl.mod.tmpl", kclModData)
	if err != nil {
		return fmt.Errorf("render kcl.mod template: %w", err)
	}
	if err := os.MkdirAll(deployDir, 0755); err != nil {
		return err
	}
	kclModPath := filepath.Join(deployDir, "kcl.mod")
	if err := os.WriteFile(kclModPath, kclModContent, 0644); err != nil {
		return fmt.Errorf("write kcl.mod: %w", err)
	}

	// Dev builds of forge (no ldflags-stamped published forge/pkg
	// version — the same release/dev discriminator resolveForgePkgDep
	// uses for go.mod) have no resolvable published `kcl-vX.Y.Z` tag
	// either. Vendor the binary's embedded copy of the forge KCL module
	// into `.forge-kcl/` and point the freshly-rendered kcl.mod at it,
	// so the project is born rendering — the exact mechanism `forge
	// generate` maintains from here on (internal/cli sync step + the
	// shared internal/kclvendor patcher).
	if buildinfo.PkgVersion() == "" {
		if _, err := kclvendor.Materialize(g.Path); err != nil {
			return fmt.Errorf("dev-vendor forge KCL module: %w", err)
		}
		res, err := kclvendor.EnsureVendorDep(kclModPath, g.Path)
		if err != nil {
			return fmt.Errorf("point kcl.mod at %s: %w", kclvendor.VendorDirName, err)
		}
		if res.Warning != "" {
			// Freshly rendered from our own template — a warning here
			// means the template and patcher drifted apart.
			return fmt.Errorf("kcl.mod vendor patch: %s", res.Warning)
		}
	}

	// The legacy in-tree `deploy/kcl/schema.k` + `base.k` + `render.k`
	// files were retired in favor of the upstream `forge` KCL module.
	// Projects now `import forge` from each env's main.k. See the
	// `kcl-schemas-to-module` migration SKILL.md for the upgrade path.

	// Templated per-env files. binary=shared projects emit a parallel
	// set of templates that produce a single MultiServiceApplication
	// (one image, N Deployments) instead of N copies of Application.
	// Both shapes pin to the same schema/render lambdas; only the
	// composition at the env level differs.
	envTemplates := []struct {
		templateName string
		dest         string
	}{
		{"kcl/dev/main.k.tmpl", "dev/main.k"},
		{"kcl/staging/main.k.tmpl", "staging/main.k"},
		{"kcl/prod/main.k.tmpl", "prod/main.k"},
	}
	if g.isBinaryShared() {
		envTemplates = []struct {
			templateName string
			dest         string
		}{
			{"kcl/dev/main-shared.k.tmpl", "dev/main.k"},
			{"kcl/staging/main-shared.k.tmpl", "staging/main.k"},
			{"kcl/prod/main-shared.k.tmpl", "prod/main.k"},
		}
	}

	// DEPLOY-AS-DATA: the per-env main.k no longer hand-writes a
	// `forge.Service` per component (the old `{{range .Services}}` /
	// `{{range .Binaries}}` KCL-text projection is gone). It loads the
	// denormalized component shape from `deploy/kcl/components_gen.json`
	// and lets the forge.components KCL schema hierarchy expand it. The
	// only data the env templates still need is the project name and
	// the ingress toggle.

	// Ingress is experimental but we still scaffold the wiring files at
	// `forge project new` so the user has a complete starting point. The
	// runtime gate (cert-manager install, audit category) lives on the
	// `forge cluster up` / `forge cluster urls` paths and reads
	// IngressEnabled() at call time. Setting IngressEnabled: true here
	// flips the wiring lines in main.k so an opt-in just needs the
	// experimental.ingress: true flag with no rescaffold.
	ingressOn := true
	templateData := struct {
		ProjectName    string
		IngressEnabled bool
	}{
		ProjectName:    g.Name,
		IngressEnabled: ingressOn,
	}

	for _, f := range envTemplates {
		content, err := templates.DeployTemplates().Render(f.templateName, templateData)
		if err != nil {
			return fmt.Errorf("render deploy template %s: %w", f.templateName, err)
		}
		destPath := filepath.Join(deployDir, f.dest)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write %s: %w", f.dest, err)
		}
	}

	// Gateway API ingress scaffolding. The base topology
	// (`deploy/kcl/ingress.k`) is user-owned and shared across envs;
	// each env's `deploy/kcl/<env>/ingress.k` re-exports the base
	// with optional overrides. Both render once at `forge project new`; not
	// regenerated on subsequent `forge generate` runs.
	if ingressOn {
		ingressFiles := []struct {
			templateName string
			dest         string
		}{
			{"kcl/ingress.k.tmpl", "ingress.k"},
			{"kcl/dev/ingress.k.tmpl", "dev/ingress.k"},
			{"kcl/staging/ingress.k.tmpl", "staging/ingress.k"},
			{"kcl/prod/ingress.k.tmpl", "prod/ingress.k"},
		}
		for _, f := range ingressFiles {
			content, err := templates.DeployTemplates().Render(f.templateName, templateData)
			if err != nil {
				return fmt.Errorf("render ingress template %s: %w", f.templateName, err)
			}
			destPath := filepath.Join(deployDir, f.dest)
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(destPath, content, 0644); err != nil {
				return fmt.Errorf("write %s: %w", f.dest, err)
			}
		}
	}

	// DEPLOY-AS-DATA: emit the denormalized component shape the per-env
	// main.k files load. deploy/kcl/components_gen.json is a lockfile-class
	// PROJECTION of what the tree declares — regenerated every run, untracked
	// — so it is written from a fresh read of the tree, never from a config
	// object. At scaffold time the proto descriptor does not exist yet, so
	// this projection is usually empty; the generate pipeline rewrites it
	// once the descriptor is extracted.
	if err := codegen.GenerateComponentsJSON(g.Path, g.Name,
		codegen.DiscoverProjectComponents(g.Path, g.Name), nil); err != nil {
		return fmt.Errorf("write components_gen.json: %w", err)
	}

	return nil
}

// generateDevConfig writes the k3d cluster configuration for local development.
func (g *ProjectGenerator) generateDevConfig() error {
	data := struct {
		ProjectName string
	}{
		ProjectName: g.Name,
	}

	content, err := templates.DeployTemplates().Render("k3d.yaml.tmpl", data)
	if err != nil {
		return fmt.Errorf("render k3d.yaml: %w", err)
	}

	destPath := filepath.Join(g.Path, "deploy", "k3d.yaml")
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(destPath, content, 0644)
}

func (g *ProjectGenerator) generateAlloyConfig() error {
	port := g.ServicePort
	if port == 0 {
		port = 8080
	}
	data := struct {
		ProjectName string
		Services    []ServiceInfo
	}{
		ProjectName: g.Name,
		Services:    []ServiceInfo{{Name: "app", Port: port}},
	}
	content, err := templates.ProjectTemplates().Render("alloy-config.alloy.tmpl", data)
	if err != nil {
		return fmt.Errorf("render alloy-config.alloy: %w", err)
	}
	destPath := filepath.Join(g.Path, "deploy", "alloy-config.alloy")
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(destPath, content, 0644)
}

func (g *ProjectGenerator) generateDockerCompose() error {
	data := struct {
		ProjectName string
	}{
		ProjectName: g.Name,
	}
	content, err := templates.ProjectTemplates().Render("docker-compose.yml.tmpl", data)
	if err != nil {
		return fmt.Errorf("render docker-compose.yml: %w", err)
	}
	destPath := filepath.Join(g.Path, "docker-compose.yml")
	return os.WriteFile(destPath, content, 0644)
}
