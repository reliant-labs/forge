package assets

import (
	"embed"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/templates"
)

//go:embed proto/forge/options/v1/service.proto
//go:embed proto/forge/options/v1/method.proto
//go:embed proto/forge/options/v1/entity.proto
//go:embed proto/forge/options/v1/field.proto
//go:embed proto/forge/options/v1/config.proto
var EmbeddedFiles embed.FS

// Versioned options proto file names under proto/forge/options/v1/.
var optionsV1Files = []string{
	"service.proto",
	"method.proto",
	"entity.proto",
	"field.proto",
	"config.proto",
}

// GetForgeOptionsV1Proto returns a single versioned options proto by name.
// Valid names: service.proto, method.proto, entity.proto, field.proto, config.proto.
func GetForgeOptionsV1Proto(name string) ([]byte, error) {
	return EmbeddedFiles.ReadFile(filepath.Join("proto/forge/options/v1", name))
}

// GetForgeOptionsV1Protos returns all versioned options proto files as a
// map of filename to content.
func GetForgeOptionsV1Protos() (map[string][]byte, error) {
	result := make(map[string][]byte, len(optionsV1Files))
	for _, name := range optionsV1Files {
		content, err := GetForgeOptionsV1Proto(name)
		if err != nil {
			return nil, err
		}
		result[name] = content
	}
	return result, nil
}

// WriteForgeOptionsV1Protos writes all versioned options proto files into destDir.
// Creates files like destDir/service.proto, destDir/method.proto, etc.
func WriteForgeOptionsV1Protos(destDir string) error {
	protos, err := GetForgeOptionsV1Protos()
	if err != nil {
		return err
	}

	for name, content := range protos {
		adjustedContent := rewriteOptionsGoPackage(string(content), destDir)
		destPath := filepath.Join(destDir, name)
		if err := writeFile(destPath, []byte(adjustedContent)); err != nil {
			return err
		}
	}

	return nil
}

// WriteTemplate writes a project template to the specified path.
func WriteTemplate(templateName, destPath string) error {
	content, err := templates.GetProjectTemplate(templateName)
	if err != nil {
		return err
	}

	return writeFile(destPath, content)
}

// WriteTemplateWithData writes a project template with data substitution.
// Uses the consolidated template engine with the full funcMap (case converters,
// type converters, comment formatter, etc.).
func WriteTemplateWithData(templateName, destPath string, data interface{}) error {
	content, err := templates.RenderProjectTemplate(templateName, data)
	if err != nil {
		return err
	}

	return writeFile(destPath, content)
}

// WriteExampleProto writes an example proto file with the given service name.
func WriteExampleProto(serviceName, destPath string, data interface{}) error {
	return WriteTemplateWithData("user-example.proto.tmpl", destPath, data)
}

func rewriteOptionsGoPackage(content, destDir string) string {
	cleanDest := filepath.ToSlash(destDir)
	marker := "/proto/forge/options/v1"
	index := strings.LastIndex(cleanDest, marker)
	if index == -1 {
		return content
	}

	moduleRoot := cleanDest[:index]
	goModPath := filepath.Join(filepath.FromSlash(moduleRoot), "go.mod")
	goModContents, err := os.ReadFile(goModPath)
	if err != nil {
		return content
	}

	modulePath := parseModulePath(string(goModContents))
	if modulePath == "" {
		return content
	}

	oldValue := `option go_package = "github.com/reliant-labs/forge/gen/forge/options/v1;optionsv1";`
	newValue := `option go_package = "` + modulePath + `/gen/forge/options/v1;optionsv1";`
	return strings.Replace(content, oldValue, newValue, 1)
}

func parseModulePath(goModContents string) string {
	for _, line := range strings.Split(goModContents, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "module "))
		}
	}
	return ""
}

func writeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o644)
}