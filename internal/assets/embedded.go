package assets

import (
	"embed"
	"os"
	"path/filepath"
	"regexp"

	"github.com/reliant-labs/forge/internal/templates"
)

// EmbeddedFiles bundles the proto annotation definitions shipped
// with the forge binary so they can be vendored into scaffolded
// projects without a network fetch.
//
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

// goPackageOptionRE matches the file-level go_package option line of the
// embedded forge.proto regardless of what path it currently declares.
// WriteForgeV1Proto must not depend on the exact embedded value: a stale
// literal-match here is exactly how scaffolds historically shipped a
// forge.proto pointing at `github.com/reliant-labs/forge/gen/...` — a
// module that does not exist — leaving every generated *.pb.go in the
// project with an unresolvable import.
var goPackageOptionRE = regexp.MustCompile(`(?m)^option go_package = "[^"]*";$`)

// WriteForgeV1Proto writes the unified forge.proto into destDir/forge.proto,
// rewriting the go_package option to `<modulePath>/gen/forge/v1`.
//
// Rationale: scaffolded projects vendor forge.proto into proto/forge/v1/
// and run buf generate over it with `paths=source_relative`, so the
// generated forge.pb.go lands at gen/forge/v1/ inside the project's own
// `<module>/gen` submodule. Pointing go_package at the project module
// makes every other generated *.pb.go import the project-local copy —
// no external "forge/gen" module is required (none is published).
func WriteForgeV1Proto(destDir, modulePath string) error {
	content, err := GetForgeV1Proto()
	if err != nil {
		return err
	}

	adjusted := string(content)
	if modulePath != "" {
		newPkg := `option go_package = "` + modulePath + `/gen/forge/v1;forgev1";`
		adjusted = goPackageOptionRE.ReplaceAllString(adjusted, newPkg)
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
