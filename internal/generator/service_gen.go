package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/naming"
)

// ServicePackageName returns the Go-package-safe form of a service name.
// Hyphens are valid in CLI/config strings and forge.yaml entries, but Go
// package declarations and proto package segments require [a-z0-9_]. So
// "admin-server" becomes "admin_server", while "api" stays "api".
func ServicePackageName(serviceName string) string {
	return strings.ReplaceAll(strings.ToLower(serviceName), "-", "_")
}

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
	servicePackage := ServicePackageName(serviceName)
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

	svcContent, err := renderServiceTemplate("service/service.go.tmpl", svcData)
	if err != nil {
		return fmt.Errorf("render service.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, "service.go"), svcContent, 0644); err != nil {
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

	authzContent, err := renderServiceTemplate("service/authorizer.go.tmpl", authzData)
	if err != nil {
		return fmt.Errorf("render authorizer.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, "authorizer.go"), authzContent, 0644); err != nil {
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

	unitTestContent, err := renderServiceTemplate("service/unit_test.go.tmpl", testData)
	if err != nil {
		return fmt.Errorf("render unit_test.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, "handlers_scaffold_test.go"), unitTestContent, 0644); err != nil {
		return err
	}

	integrationTestContent, err := renderServiceTemplate("service/integration_test.go.tmpl", testData)
	if err != nil {
		return fmt.Errorf("render integration_test.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, "integration_test.go"), integrationTestContent, 0644); err != nil {
		return err
	}

	// -- proto stub (only if not already present) --
	protoPath := filepath.Join(root, "proto", "services", servicePackage, "v1", fmt.Sprintf("%s.proto", servicePackage))
	if _, err := os.Stat(protoPath); os.IsNotExist(err) {
		protoContent := fmt.Sprintf(`syntax = "proto3";

package services.%s.v1;

option go_package = "%s/gen/services/%s/v1;%sv1";

// %sService defines the %s service RPCs.
service %sService {
  // TODO: Add your RPC methods here.
}
`, servicePackage, modulePath, servicePackage, servicePackage,
			handlerName, serviceName, handlerName)
		if err := os.WriteFile(protoPath, []byte(protoContent), 0644); err != nil {
			return err
		}
	}

	return nil
}