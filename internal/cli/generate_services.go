package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

// generateServiceStubs creates service.go, handlers.go, wrapper.go for new services.
// For existing service directories, it generates stubs only for missing RPC handlers.
func generateServiceStubs(cfg *config.ProjectConfig, services []codegen.ServiceDef, projectDir string) error {
	fmt.Println("\n🔧 Generating service stubs...")

	if len(services) == 0 {
		fmt.Println("  No services found in proto/services/")
		return nil
	}

	hasNewStubs := false
	for _, svc := range services {
		relServiceDir := toServiceDir(svc.Name)
		absServiceDir := filepath.Join(projectDir, relServiceDir)

		if dirExists(absServiceDir) {
			// Incremental: generate stubs only for missing RPC methods
			result, err := codegen.GenerateMissingHandlerStubs(svc, absServiceDir)
			if err != nil {
				return fmt.Errorf("failed to generate missing stubs for %s: %w", svc.Name, err)
			}
			if result.AllUpToDate {
				fmt.Printf("  ⏭️  Skipped %s/ (all handlers up to date)\n", relServiceDir)
			} else {
				fmt.Printf("  ✅ Generated %d new handler stub(s) in %s/handlers_gen.go: %s\n",
					len(result.NewMethods), relServiceDir, strings.Join(result.NewMethods, ", "))
				hasNewStubs = true
			}
			continue
		}

		if err := codegen.GenerateServiceStub(svc, absServiceDir); err != nil {
			return fmt.Errorf("failed to generate stub for %s: %w", svc.Name, err)
		}
		fmt.Printf("  ✅ Created %s/\n", relServiceDir)
	}

	if hasNewStubs {
		fmt.Println("  💡 Run 'go build ./...' to verify the new stubs compile")
	}

	return nil
}

// generateCRUDHandlers generates CRUD handler implementations by matching
// service RPC methods against entity protos in proto/db/.
func generateCRUDHandlers(services []codegen.ServiceDef, modulePath string, projectDir string) error {
	entities, err := codegen.ParseEntityProtos(projectDir)
	if err != nil {
		return fmt.Errorf("parse entity protos: %w", err)
	}
	if len(entities) == 0 {
		return nil
	}

	fmt.Println("\n🔧 Generating CRUD handlers...")
	generated := 0
	for _, svc := range services {
		crudMethods := codegen.MatchCRUDMethods(svc, entities)
		if len(crudMethods) == 0 {
			continue
		}

		pkg := strings.ToLower(strings.TrimSuffix(svc.Name, "Service"))
		if err := codegen.GenerateCRUDHandlers(svc, crudMethods, modulePath, projectDir); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠️  CRUD generation for %s failed: %v\n", svc.Name, err)
			continue
		}
		fmt.Printf("  ✅ Generated handlers/%s/handlers_crud_gen.go (%d methods)\n", pkg, len(crudMethods))

		if err := codegen.GenerateCRUDTests(svc, crudMethods, modulePath, projectDir); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠️  CRUD test generation for %s failed: %v\n", svc.Name, err)
		} else {
			fmt.Printf("  ✅ Generated handlers/%s/handlers_crud_test_gen.go\n", pkg)
		}
		generated++
	}

	if generated == 0 {
		fmt.Println("  ℹ️  No CRUD patterns matched")
	}
	return nil
}

// generateServiceMocks always regenerates mocks from proto definitions.
func generateServiceMocks(services []codegen.ServiceDef, projectDir string) error {
	fmt.Println("🔧 Generating service mocks...")

	if len(services) == 0 {
		return nil
	}

	for _, svc := range services {
		written, err := codegen.GenerateMock(svc, filepath.Join(projectDir, "handlers/mocks"))
		if err != nil {
			return fmt.Errorf("failed to generate mock for %s: %w", svc.Name, err)
		}
		mockName := strings.ToLower(strings.TrimSuffix(svc.Name, "Service"))
		if written {
			fmt.Printf("  ✅ Updated handlers/mocks/%s_mock.go\n", mockName)
		} else {
			fmt.Printf("  ⏭️  Skipped handlers/mocks/%s_mock.go (no RPCs)\n", mockName)
		}
	}

	return nil
}

func toServiceDir(serviceName string) string {
	// EchoService -> handlers/echo
	name := strings.ToLower(strings.TrimSuffix(serviceName, "Service"))
	return fmt.Sprintf("handlers/%s", name)
}
