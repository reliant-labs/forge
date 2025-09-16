package generator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/naming"
)

// GenerateServiceFiles generates all files for a single Go service:
//   - handlers/<name>/service.go       (from service/service.go.tmpl)
//   - handlers/<name>/handlers.go      (from service/handlers.go.tmpl)
//   - proto/services/<name>/v1/<name>.proto (inline stub, skipped if exists)
//
// Both the "new project" and "add service" flows delegate here so the
// generated output is always identical.
func GenerateServiceFiles(root, modulePath, serviceName, projectName string, port int) error {
	svcDir := filepath.Join(root, "handlers", serviceName)

	// Create directories
	dirs := []string{
		svcDir,
		filepath.Join(root, "proto", "services", serviceName, "v1"),
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
		Module              string
		ProtoImportPath     string
		ProtoConnectPackage string
		ProtoFileSymbol     string
		HandlerName         string
		Port                string
		Methods             []string
	}{
		ServiceName:         serviceName,
		Module:              modulePath,
		ProtoImportPath:     fmt.Sprintf("services/%s", serviceName),
		ProtoConnectPackage: fmt.Sprintf("%sv1connect", serviceName),
		ProtoFileSymbol:     fmt.Sprintf("File_services_%s_v1_%s_proto", serviceName, serviceName),
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

	// -- handlers.go (via service/handlers.go.tmpl) --
	handlersData := struct {
		ServiceName  string
		Module       string
		ProtoPackage string
		Methods      []codegen.MethodTemplateData
	}{
		ServiceName:  serviceName,
		Module:       modulePath,
		ProtoPackage: fmt.Sprintf("services/%s", serviceName),
		Methods:      []codegen.MethodTemplateData{},
	}

	handlersContent, err := renderServiceTemplate("service/handlers.go.tmpl", handlersData)
	if err != nil {
		return fmt.Errorf("render handlers.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, "handlers.go"), handlersContent, 0644); err != nil {
		return err
	}

	// -- authorizer.go (via service/authorizer.go.tmpl) --
	authzData := struct {
		PackageName string
		ServiceName string
		Module      string
	}{
		PackageName: serviceName,
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
	testData := struct {
		ServiceName         string
		Module              string
		ProtoPackage        string
		ProtoImportPath     string
		ProtoConnectPackage string
		HandlerName         string
		Methods             []codegen.MethodTemplateData
	}{
		ServiceName:         serviceName,
		Module:              modulePath,
		ProtoPackage:        fmt.Sprintf("services/%s", serviceName),
		ProtoImportPath:     fmt.Sprintf("services/%s", serviceName),
		ProtoConnectPackage: fmt.Sprintf("%sv1connect", serviceName),
		HandlerName:         fmt.Sprintf("%sService", handlerName),
		Methods:             []codegen.MethodTemplateData{},
	}

	unitTestContent, err := renderServiceTemplate("service/unit_test.go.tmpl", testData)
	if err != nil {
		return fmt.Errorf("render unit_test.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, "handlers_test.go"), unitTestContent, 0644); err != nil {
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
	protoPath := filepath.Join(root, "proto", "services", serviceName, "v1", fmt.Sprintf("%s.proto", serviceName))
	if _, err := os.Stat(protoPath); os.IsNotExist(err) {
		protoContent := fmt.Sprintf(`syntax = "proto3";

package services.%s.v1;

option go_package = "%s/gen/services/%s/v1;%sv1";

// %sService defines the %s service RPCs.
service %sService {
  // TODO: Add your RPC methods here.
}
`, serviceName, modulePath, serviceName, serviceName,
			handlerName, serviceName, handlerName)
		if err := os.WriteFile(protoPath, []byte(protoContent), 0644); err != nil {
			return err
		}
	}

	return nil
}