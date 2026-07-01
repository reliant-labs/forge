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

// forgePBGoPackage is the FIXED go_package that scaffolded projects' vendored
// forge.proto must declare. It points at forge's shared, pre-generated
// forgepb package rather than a project-local gen/forge/v1 copy.
//
// "Path A" proto unification: forge.proto registers the descriptor file
// "forge/v1/forge.proto" at init. If a project generated its OWN copy into
// gen/forge/v1 AND linked forge/pkg/forgepb (which generates the identical
// descriptor), both copies register the same file and every binary panics:
//
//	proto: file "forge/v1/forge.proto" is already registered
//
// Pointing go_package at forgepb means the project's buf pipeline emits no
// local copy (the path is excluded from Go output in buf.gen.yaml) and every
// other generated *.pb.go blank-imports the shared forgepb — single
// registration, both binaries boot.
const forgePBGoPackage = `option go_package = "github.com/reliant-labs/forge/pkg/forgepb;forgepb";`

// WriteForgeV1Proto writes the unified forge.proto into destDir/forge.proto,
// rewriting the go_package option to point at forge's shared forgepb package.
//
// Rationale: scaffolded projects vendor forge.proto into proto/forge/v1/ as a
// buf COMPILE input (other protos import its annotations), but they do NOT
// generate a local Go copy — buf.gen.yaml excludes forge/v1 from output and
// the project links forge/pkg/forgepb instead. The fixed go_package below is
// what makes the cross-file blank-imports in the generated *.pb.go resolve to
// that shared package.
func WriteForgeV1Proto(destDir string) error {
	content, err := GetForgeV1Proto()
	if err != nil {
		return err
	}

	adjusted := goPackageOptionRE.ReplaceAllString(string(content), forgePBGoPackage)

	destPath := filepath.Join(destDir, "forge.proto")
	return writeFile(destPath, []byte(adjusted))
}

func writeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o644)
}
