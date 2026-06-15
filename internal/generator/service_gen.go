package generator

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/naming"
)

// ScaffoldMode controls how scaffolding functions handle pre-existing output
// files. It exists so `forge add service --resume / --force` can re-run a
// partial scaffold without forcing the user to rm -rf and start over.
type ScaffoldMode int

const (
	// ScaffoldFail is the default: writes unconditionally. Callers are
	// expected to have done their own up-front "does this name conflict"
	// check (forge.yaml services: list). Equivalent to pre-resume behavior.
	ScaffoldFail ScaffoldMode = iota
	// ScaffoldResume skips any output file that already exists on disk.
	// Use this to recover from a partial scaffold where buf generate (or a
	// later pipeline step) failed mid-flight and left some files written.
	ScaffoldResume
	// ScaffoldForce overwrites every output file, even when present. The
	// proto file is included — the only file the default codepath
	// historically preserved — so users can fully re-stamp a scaffold.
	ScaffoldForce
)

// GenerateServiceFiles generates all files for a single Go service:
//   - handlers/<servicePackage>/service.go (from service/service.go.tmpl)
//   - proto/services/<servicePackage>/v1/<servicePackage>.proto (inline stub, skipped if exists)
//
// The display/CLI form of the service name (which may contain hyphens) is
// translated to a Go-package-safe form ("admin-server" -> "admin_server") for
// filesystem directories and Go/proto identifiers. Display strings keep the
// original spelling.
//
// Both the "new project" and "add service" flows delegate here so the
// generated output is always identical.
//
// handlers.go is intentionally not emitted at scaffold time: with zero RPC
// methods there is nothing for it to contain. Once RPCs are added to the
// proto file, `forge generate` produces handlers_gen.go; the user then moves
// those stubs to handlers.go (or any other file) as they implement them.
func GenerateServiceFiles(root, modulePath, serviceName, projectName string, port int) error {
	return GenerateServiceFilesWithMode(root, modulePath, serviceName, projectName, port, ScaffoldFail, nil)
}

// GenerateServiceFilesWithMode is the mode-aware form of GenerateServiceFiles.
// `mode` controls per-file overwrite/skip behavior so callers can support
// --resume (recover a partially-applied scaffold) and --force (re-stamp
// every output file). When non-nil, `progress` receives one human-readable
// line per scaffolding step so the CLI can render "skipped" / "overwriting"
// notices without re-implementing the existence check.
func GenerateServiceFilesWithMode(root, modulePath, serviceName, projectName string, port int, mode ScaffoldMode, progress io.Writer) error {
	servicePackage := naming.ServicePackage(serviceName)
	svcDir := filepath.Join(root, "handlers", servicePackage)

	// Create directories
	dirs := []string{
		svcDir,
		filepath.Join(root, "proto", "services", servicePackage, "v1"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("create directory %s: %w", d, err)
		}
	}

	handlerName := naming.ToPascalCase(serviceName)

	// -- service.go (via service/service.go.tmpl) --
	svcData := struct {
		ServiceName         string
		ServicePackage      string
		Module              string
		ProtoImportPath     string
		ProtoConnectPackage string
		ProtoFileSymbol     string
		HandlerName         string
		Port                string
		Methods             []string
	}{
		ServiceName:         serviceName,
		ServicePackage:      servicePackage,
		Module:              modulePath,
		ProtoImportPath:     fmt.Sprintf("services/%s", servicePackage),
		ProtoConnectPackage: fmt.Sprintf("%sv1connect", servicePackage),
		ProtoFileSymbol:     fmt.Sprintf("File_services_%s_v1_%s_proto", servicePackage, servicePackage),
		HandlerName:         fmt.Sprintf("%sService", handlerName),
		Port:                fmt.Sprintf("%d", port),
		Methods:             []string{},
	}

	svcPath := filepath.Join(svcDir, "service.go")
	if err := renderAndWriteWithMode(svcPath, "service/service.go.tmpl", svcData, mode, progress); err != nil {
		return err
	}

	// handlers.go is intentionally not emitted at scaffold (zero RPC methods).
	// See function docstring for details.

	// -- authorizer.go (via service/authorizer.go.tmpl) --
	authzData := struct {
		Package     string
		ServiceName string
		Module      string
	}{
		Package:     servicePackage,
		ServiceName: handlerName,
		Module:      modulePath,
	}

	authzPath := filepath.Join(svcDir, "authorizer.go")
	if err := renderAndWriteWithMode(authzPath, "service/authorizer.go.tmpl", authzData, mode, progress); err != nil {
		return err
	}

	// -- test templates --
	// TestHelperName mirrors codegen.ComputeTestHelperName: matches the
	// `app.NewTest<X>` factory the bootstrap testing generator emits for
	// this service so the scaffolded handlers_scaffold_test.go compiles even
	// when internal/<servicePackage> exists and triggers Svc-prefixing.
	testData := struct {
		ServiceName         string
		ServicePackage      string
		Module              string
		ProtoPackage        string
		ProtoImportPath     string
		ProtoConnectPackage string
		HandlerName         string
		TestHelperName      string
		Methods             []codegen.MethodTemplateData
	}{
		ServiceName:         serviceName,
		ServicePackage:      servicePackage,
		Module:              modulePath,
		ProtoPackage:        fmt.Sprintf("services/%s", servicePackage),
		ProtoImportPath:     fmt.Sprintf("services/%s", servicePackage),
		ProtoConnectPackage: fmt.Sprintf("%sv1connect", servicePackage),
		HandlerName:         fmt.Sprintf("%sService", handlerName),
		TestHelperName:      codegen.ComputeTestHelperName(servicePackage, root),
		Methods:             []codegen.MethodTemplateData{},
	}

	unitTestPath := filepath.Join(svcDir, "handlers_scaffold_test.go")
	if err := renderAndWriteWithMode(unitTestPath, "service/unit_test.go.tmpl", testData, mode, progress); err != nil {
		return err
	}

	// No one-shot integration_test.go scaffold: the unit scaffold owns
	// per-RPC self-destructing rows and handlers_crud_integration_test.go
	// owns the DB-bound CRUD surface. A second per-RPC file that hardcodes
	// WantErr for every method goes stale the moment handlers are
	// implemented and teaches green-means-nothing.

	// -- proto stub --
	// Historical behavior preserved the proto file even in ScaffoldFail mode
	// (the proto file is the user's API contract — clobbering it would erase
	// hand-written RPC definitions). ScaffoldForce overrides that guard for
	// users who explicitly asked to re-stamp the scaffold.
	protoPath := filepath.Join(root, "proto", "services", servicePackage, "v1", fmt.Sprintf("%s.proto", servicePackage))
	protoContent := fmt.Sprintf(`syntax = "proto3";

package services.%s.v1;

option go_package = "%s/gen/services/%s/v1;%sv1";

// %sService defines the %s service RPCs.
service %sService {
  // TODO: Add your RPC methods here.
}
`, servicePackage, modulePath, servicePackage, servicePackage,
		handlerName, serviceName, handlerName)
	if err := writeBytesWithMode(protoPath, []byte(protoContent), mode, progress); err != nil {
		return err
	}

	return nil
}

// renderAndWriteWithMode renders a service template then delegates the
// write decision to writeBytesWithMode.
func renderAndWriteWithMode(path, tmplName string, data any, mode ScaffoldMode, progress io.Writer) error {
	// Honor skip BEFORE rendering — rendering some templates depends on
	// codegen helpers that walk the project tree, so skipping early keeps
	// --resume cheap.
	if mode == ScaffoldResume {
		if _, err := os.Stat(path); err == nil {
			emitProgress(progress, "skipped", path)
			return nil
		}
	}
	content, err := renderServiceTemplate(tmplName, data)
	if err != nil {
		return fmt.Errorf("render %s: %w", tmplName, err)
	}
	return writeBytesWithMode(path, content, mode, progress)
}

// writeBytesWithMode writes `content` to `path` honoring the scaffold mode:
//   - ScaffoldFail: always write (preserves pre-resume default semantics).
//     The one historical exception is the proto file; callers that need to
//     preserve it pass ScaffoldFail and inspect existence themselves, but
//     this helper centralizes the existence check so the proto path can
//     simply call through with the default mode and rely on protected
//     handling here.
//   - ScaffoldResume: skip if path exists.
//   - ScaffoldForce: always write; emit an "overwriting" notice when the
//     file already exists.
//
// In ScaffoldFail mode we preserve an existing proto file at the only
// historically-protected location by skipping when the file exists AND
// looks like a proto stub the user may have edited. Other files clobber
// as before.
func writeBytesWithMode(path string, content []byte, mode ScaffoldMode, progress io.Writer) error {
	exists := false
	if _, err := os.Stat(path); err == nil {
		exists = true
	}
	switch mode {
	case ScaffoldResume:
		if exists {
			emitProgress(progress, "skipped", path)
			return nil
		}
	case ScaffoldForce:
		if exists {
			emitProgress(progress, "overwriting", path)
		}
	case ScaffoldFail:
		// Preserve the historical "don't clobber an existing proto file"
		// guard. Detect by suffix to keep the rule simple and visible.
		if exists && strings.HasSuffix(path, ".proto") {
			return nil
		}
	}
	return os.WriteFile(path, content, 0644)
}

// emitProgress writes a single human-readable status line to `w` if non-nil.
// The CLI uses this to surface --resume / --force decisions to the user.
func emitProgress(w io.Writer, kind, path string) {
	if w == nil {
		return
	}
	switch kind {
	case "skipped":
		fmt.Fprintf(w, "  ✓ skipped: %s\n", path)
	case "overwriting":
		fmt.Fprintf(w, "  ⚠ overwriting: %s\n", path)
	}
}
