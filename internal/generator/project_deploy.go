package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/templates"
)

func (g *ProjectGenerator) generateKCLDeploy() error {
	deployDir := filepath.Join(g.Path, "deploy", "kcl")

	// Generate kcl.mod at project root so KCL imports like deploy.kcl.schema resolve.
	kclModData := struct {
		ProjectName string
	}{
		ProjectName: g.Name,
	}
	kclModContent, err := templates.DeployTemplates.Render("kcl/kcl.mod.tmpl", kclModData)
	if err != nil {
		return fmt.Errorf("render kcl.mod template: %w", err)
	}
	if err := os.WriteFile(filepath.Join(g.Path, "kcl.mod"), kclModContent, 0644); err != nil {
		return fmt.Errorf("write kcl.mod: %w", err)
	}

	// Static files (no templating needed)
	staticFiles := []struct {
		templateName string
		dest         string
	}{
		{"kcl/schema.k", "schema.k"},
		{"kcl/render.k", "render.k"},
		{"kcl/base.k", "base.k"},
	}

	for _, f := range staticFiles {
		content, err := templates.DeployTemplates.Get(f.templateName)
		if err != nil {
			return fmt.Errorf("read deploy template %s: %w", f.templateName, err)
		}
		destPath := filepath.Join(deployDir, f.dest)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write %s: %w", f.dest, err)
		}
	}

	// Templated per-env files
	envTemplates := []struct {
		templateName string
		dest         string
	}{
		{"kcl/dev/main.k.tmpl", "dev/main.k"},
		{"kcl/staging/main.k.tmpl", "staging/main.k"},
		{"kcl/prod/main.k.tmpl", "prod/main.k"},
	}

	templateData := struct {
		ProjectName string
	}{
		ProjectName: g.Name,
	}

	for _, f := range envTemplates {
		content, err := templates.DeployTemplates.Render(f.templateName, templateData)
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

	return nil
}

// generateDevConfig writes the k3d cluster configuration for local development.
func (g *ProjectGenerator) generateDevConfig() error {
	data := struct {
		ProjectName string
	}{
		ProjectName: g.Name,
	}

	content, err := templates.DeployTemplates.Render("k3d.yaml.tmpl", data)
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
	content, err := templates.ProjectTemplates.Render("alloy-config.alloy.tmpl", data)
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
	content, err := templates.ProjectTemplates.Render("docker-compose.yml.tmpl", data)
	if err != nil {
		return fmt.Errorf("render docker-compose.yml: %w", err)
	}
	destPath := filepath.Join(g.Path, "docker-compose.yml")
	return os.WriteFile(destPath, content, 0644)
}