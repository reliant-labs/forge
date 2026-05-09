package generator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/templates"
)

// GenerateBinaryFiles emits the four files that make up a non-server
// long-running binary scaffold:
//
//   - cmd/<package>.go               cobra subcommand + Run wiring
//   - internal/<package>/<package>.go main loop with Start/Stop/Run lifecycle
//   - internal/<package>/contract.go  Deps, Service interface, New(deps)
//   - internal/<package>/<package>_test.go basic lifecycle test
//
// The shape is deliberately opinionated: the scaffold solves the
// boilerplate (signal handling, graceful shutdown, metrics-server-on-
// separate-port) so the user can hand-write only the binary's actual
// business logic. Kind today is always "long-running" — see
// config.BinaryConfig for the forward-compat reservation of cron/
// oneshot kinds.
//
// CLI/display name (which may contain hyphens) is translated to a Go-
// package-safe form for the directory and `package` declaration; the
// hyphenated form is preserved on the cobra `Use:` field so users can
// invoke `./<bin> workspace-proxy`.
func GenerateBinaryFiles(root, modulePath, binaryName, kind string) error {
	binaryPackage := ServicePackageName(binaryName)

	// internal/<package>/ — main loop, contract, test.
	binaryDir := filepath.Join(root, "internal", binaryPackage)
	if err := os.MkdirAll(binaryDir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", binaryDir, err)
	}

	// cmd/<package>.go — cobra subcommand registered against the shared
	// rootCmd. We don't pre-create cmd/ because every service-shaped
	// project already has it.
	cmdDir := filepath.Join(root, "cmd")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", cmdDir, err)
	}

	if kind == "" {
		kind = "long-running"
	}
	data := struct {
		Name    string // display form, may contain hyphens
		Package string // Go-package-safe form
		Module  string
		Kind    string
	}{
		Name:    binaryName,
		Package: binaryPackage,
		Module:  modulePath,
		Kind:    kind,
	}

	files := []struct {
		template string
		dest     string
	}{
		{"binary/cmd-binary.go.tmpl", filepath.Join(cmdDir, binaryPackage+".go")},
		{"binary/binary.go.tmpl", filepath.Join(binaryDir, binaryPackage+".go")},
		{"binary/contract.go.tmpl", filepath.Join(binaryDir, "contract.go")},
		{"binary/binary_test.go.tmpl", filepath.Join(binaryDir, binaryPackage+"_test.go")},
		// Pre-emit an empty contract_test.go marker so the
		// `forge generate` internal-package contract-test scaffolder
		// sees it exists and skips the canonical scaffold (which
		// renders `pkg.New(pkg.Deps{})` and doesn't account for the
		// two-result `(Service, error)` shape the binary template
		// uses). The file's `_test` package + leading comment make
		// it clear the user owns the body — they can either grow
		// the binary's contract tests here or stick with the
		// lifecycle test in <name>_test.go.
		{"binary/contract_test.go.tmpl", filepath.Join(binaryDir, "contract_test.go")},
	}
	for _, f := range files {
		content, err := renderBinaryTemplate(f.template, data)
		if err != nil {
			return fmt.Errorf("render %s: %w", f.template, err)
		}
		if err := os.WriteFile(f.dest, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", f.dest, err)
		}
	}
	return nil
}

// renderBinaryTemplate renders one of the binary-scaffold templates from
// the embedded template FS. Mirrors renderWorkerTemplate shape.
func renderBinaryTemplate(name string, data interface{}) ([]byte, error) {
	tmpl, err := templates.ProjectTemplates().Render(name, data)
	if err != nil {
		return nil, err
	}
	return tmpl, nil
}
