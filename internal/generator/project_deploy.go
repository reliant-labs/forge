package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/templates"
)

func (g *ProjectGenerator) generateKCLDeploy() error {
	deployDir := filepath.Join(g.Path, "deploy", "kcl")

	// Generate kcl.mod at project root so KCL imports like
	// `deploy.kcl.dev.config_gen` resolve. The kcl.mod also declares
	// the `forge` module dependency that the per-env main.k files
	// import — the schemas live upstream in `forge/kcl/`, not in
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
	if err := os.WriteFile(filepath.Join(g.Path, "kcl.mod"), kclModContent, 0644); err != nil {
		return fmt.Errorf("write kcl.mod: %w", err)
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
	// `forge new` so the user has a complete starting point. The
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
	// with optional overrides. Both render once at `forge new`; not
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
	// main.k files load. writeProjectConfig (called just before this in
	// the scaffold sequence) already wrote forge.yaml + components.json, so
	// read the components back through ReadProjectConfig (which now sources
	// them from components.json — the authored per-service SOT). deploy/kcl/
	// components_gen.json stays a lockfile-class PROJECTION of that source
	// (regenerated every run, untracked), distinct from the authored
	// components.json at the project root.
	cfg, err := ReadProjectConfig(filepath.Join(g.Path, "forge.yaml"))
	if err != nil {
		return fmt.Errorf("read project config for components_gen.json: %w", err)
	}
	if err := codegen.GenerateComponentsJSON(g.Path, g.Name, cfg.Components, nil); err != nil {
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

func (g *ProjectGenerator) generateEnvExample() error {
	var sb strings.Builder
	sb.WriteString("# Database\n")
	sb.WriteString(fmt.Sprintf("DATABASE_URL=postgres://user:pass@localhost:5432/%s?sslmode=disable\n", g.Name))
	sb.WriteString("\n# Server\n")
	sb.WriteString(fmt.Sprintf("PORT=%d\n", g.ServicePort))
	if g.FrontendName != "" {
		sb.WriteString(fmt.Sprintf("CORS_ORIGINS=http://localhost:%d\n", g.FrontendPort))
	} else {
		sb.WriteString("CORS_ORIGINS=http://localhost:3000\n")
	}
	sb.WriteString("\n# Environment: \"production\" (fail-closed defaults) or \"development\"\n")
	sb.WriteString("# (permissive defaults like authz allow-all). Never set to development in production.\n")
	sb.WriteString("ENVIRONMENT=development\n")
	sb.WriteString("\n# Run DB migrations on startup (rarely useful in production)\n")
	sb.WriteString("AUTO_MIGRATE=false\n")
	sb.WriteString("\n# OpenTelemetry\n")
	sb.WriteString("# OTEL_EXPORTER_OTLP_ENDPOINT is optional: set it to a running OTLP\n")
	sb.WriteString("# collector (e.g. http://localhost:4317) to enable trace/metric export.\n")
	sb.WriteString("# When unset, OpenTelemetry is a no-op.\n")
	sb.WriteString("# OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317\n")
	sb.WriteString("# OTEL_SERVICE_NAME is the advertised `service.name` resource attribute\n")
	sb.WriteString("# on every exported span/metric. Default matches the project name.\n")
	sb.WriteString("OTEL_SERVICE_NAME=" + g.Name + "\n")
	if g.FrontendName != "" {
		sb.WriteString(fmt.Sprintf("\n# Frontend (set in frontends/%s/.env.local)\n", g.FrontendName))
		sb.WriteString(fmt.Sprintf("# NEXT_PUBLIC_API_URL=http://localhost:%d\n", g.ServicePort))
	}

	destPath := filepath.Join(g.Path, ".env.example")
	return os.WriteFile(destPath, []byte(sb.String()), 0644)
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