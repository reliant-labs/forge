package assets

import (
	"embed"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/templates"
)

//go:embed proto/forge/v1/forge.proto
var EmbeddedFiles embed.FS

// WriteTemplate writes a project template to the specified path.
func WriteTemplate(templateName, destPath string) error {
	content, err := templates.ProjectTemplates().Get(templateName)
	if err != nil {
		return err
	}
	return writeFile(destPath, content)
}

// WriteTemplateWithData writes a project template with data substitution.
func WriteTemplateWithData(templateName, destPath string, data any) error {
	content, err := templates.ProjectTemplates().Render(templateName, data)
	if err != nil {
		return err
	}
	return writeFile(destPath, content)
}

// WriteExampleProto writes an example proto file with the given service name.
func WriteExampleProto(serviceName, destPath string, data any) error {
	return WriteTemplateWithData("user-example.proto.tmpl", destPath, data)
}

// GetForgeV1Proto returns the unified forge/v1/forge.proto file.
func GetForgeV1Proto() ([]byte, error) {
	return EmbeddedFiles.ReadFile("proto/forge/v1/forge.proto")
}

// WriteForgeV1Proto writes the unified forge.proto into destDir/forge.proto,
// rewriting the go_package option to match the target project's module path.
func WriteForgeV1Proto(destDir, modulePath string) error {
	content, err := GetForgeV1Proto()
	if err != nil {
		return err
	}

	adjusted := string(content)
	if modulePath != "" {
		oldPkg := `option go_package = "github.com/reliant-labs/forge/gen/forge/v1;forgev1";`
		newPkg := `option go_package = "` + modulePath + `/gen/forge/v1;forgev1";`
		adjusted = strings.Replace(adjusted, oldPkg, newPkg, 1)
	}

	destPath := filepath.Join(destDir, "forge.proto")
	return writeFile(destPath, []byte(adjusted))
}

func writeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o644)
}
