package generator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// E2EMethodInfo holds method metadata for E2E test template rendering.
type E2EMethodInfo struct {
	Name       string
	InputType  string
	OutputType string
}

// E2ETemplateData holds all data needed to render E2E test templates.
type E2ETemplateData struct {
	Module           string
	ServiceName      string
	ProtoServiceName string // PascalCase service name for Connect client types
	ProtoPackage     string
	ProjectName      string
	Port             int
	Methods          []E2EMethodInfo
	FirstRequestType string // Used to anchor the pb import in helpers
}

// GenerateE2ETests renders E2E test templates into e2e/<serviceName>/ under projectDir.
// It does not overwrite existing files — only creates new ones.
func GenerateE2ETests(projectDir, serviceName, modulePath, projectName string, methods []E2EMethodInfo) error {
	protoServiceName := naming.ToPascalCase(serviceName)

	destDir := filepath.Join(projectDir, "e2e", serviceName)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create e2e directory: %w", err)
	}

	data := E2ETemplateData{
		Module:           modulePath,
		ServiceName:      serviceName,
		ProtoServiceName: protoServiceName,
		ProtoPackage:     serviceName + "v1",
		ProjectName:      projectName,
		Port:             0, // E2E uses freePort(); this is available as a template var
		Methods:          methods,
		FirstRequestType: "",
	}

	templateFiles := []struct {
		tmplName string // relative to test/ dir
		destName string // filename in e2e/<service>/
	}{
		{"e2e/main_test.go.tmpl", "main_test.go"},
		{"e2e/helpers_test.go.tmpl", "helpers_test.go"},
		{"e2e/service_test.go.tmpl", "service_test.go"},
		{"e2e/docker-compose.e2e.yml.tmpl", "docker-compose.e2e.yml"},
	}

	for _, tf := range templateFiles {
		destPath := filepath.Join(destDir, tf.destName)

		// Don't overwrite existing files
		if _, err := os.Stat(destPath); err == nil {
			fmt.Printf("  e2e: skipping %s (already exists)\n", tf.destName)
			continue
		}

		content, err := templates.RenderTestTemplate(tf.tmplName, data)
		if err != nil {
			return fmt.Errorf("render e2e template %s: %w", tf.tmplName, err)
		}

		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write e2e file %s: %w", tf.destName, err)
		}
	}

	return nil
}

// MethodsFromProtoStub returns placeholder E2E method info when no proto methods
// are available yet (e.g., when creating a brand-new service).
func MethodsFromProtoStub(serviceName string) []E2EMethodInfo {
	_ = serviceName
	// New services start with no RPCs; return empty slice.
	// Users add RPCs to the proto and re-generate.
	return nil
}