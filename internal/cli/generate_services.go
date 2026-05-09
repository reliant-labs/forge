package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/naming"
)

// collectCRUDMethodNames returns the set of RPC method names that will be implemented
// by CRUD handlers. The stub generator uses this to avoid generating stubs for them.
func collectCRUDMethodNames(services []codegen.ServiceDef, projectDir string) map[string]bool {
	entities, err := codegen.ParseEntityProtos(projectDir)
	if err != nil || len(entities) == 0 {
		return nil
	}
	names := make(map[string]bool)
	for _, svc := range services {
		for _, cm := range codegen.MatchCRUDMethods(svc, entities) {
			names[cm.Method.Name] = true
		}
	}
	return names
}

// webhookOnlyServiceNames returns the set of service Go-names (e.g.
// "AdminServerService") for services declared in forge.yaml as
// webhook-only — i.e. the forge.yaml service entry has at least one
// webhook AND every RPC in the proto is a CRUD-shaped placeholder with
// no backing entity. These services scaffolded a default CRUD proto
// (Create/Get/Update/Delete/List) at `forge new --service` time, then
// later got webhooks added; the original CRUD RPCs are now leak-only
// stubs that ship as Unimplemented errors but never get called.
//
// Detection rule (matches FORGE_REVIEW_REBUILD.md §3.5):
//
//	1. service has webhooks declared in forge.yaml, AND
//	2. every RPC in the proto is one of {Create, Get, Update, Delete,
//	   List} with NO matching entity definition (i.e. crudMethodNames
//	   from collectCRUDMethodNames does NOT contain the RPC name).
//
// When both hold, GenerateMissingHandlerStubs treats the proto's RPCs
// as logically-absent: handlers_gen.go is empty and the stale stubs
// are removed.
//
// The return is keyed by the service's Go name (svc.Name) — the same
// key used elsewhere in the stub-generation pass.
func webhookOnlyServiceNames(cfg *config.ProjectConfig, services []codegen.ServiceDef, crudMethodNames map[string]bool) map[string]bool {
	if cfg == nil {
		return nil
	}
	// Index forge.yaml service entries by their kebab/snake -> Go name
	// equivalence. forge.yaml uses kebab ("admin-server"); svc.Name is
	// PascalCase ("AdminServerService"). We pascalCase the yaml name
	// and append "Service" to match.
	webhookByGoName := make(map[string]bool, len(cfg.Services))
	for _, ysvc := range cfg.Services {
		if len(ysvc.Webhooks) == 0 {
			continue
		}
		// "admin-server" + service-suffix → "AdminServerService".
		goName := naming.ToPascalCase(ysvc.Name) + "Service"
		webhookByGoName[goName] = true
	}
	if len(webhookByGoName) == 0 {
		return nil
	}

	out := make(map[string]bool)
	for _, svc := range services {
		if !webhookByGoName[svc.Name] {
			continue
		}
		// Every RPC must be a CRUD-shaped scaffold with no entity.
		// crudMethodNames lists RPCs that DID match an entity; if any
		// of the service's RPCs appear there, the service has at
		// least one real CRUD handler and we leave the stub block alone.
		//
		// "CRUD-shaped" here means either:
		//   - bare names: Create / Get / Update / Delete / List
		//     (the user-example.proto.tmpl scaffold default), OR
		//   - entity-suffixed: CreateUser, GetUser, ... (the typical
		//     real-handler shape — handled by ParseCRUDOperation).
		// Anything else is project-specific surface; we leave the
		// stub block alone.
		bareCRUD := map[string]bool{
			"Create": true, "Get": true, "Update": true,
			"Delete": true, "List": true,
		}
		allOrphan := true
		for _, m := range svc.Methods {
			if crudMethodNames[m.Name] {
				allOrphan = false
				break
			}
			if bareCRUD[m.Name] {
				continue
			}
			op, _ := codegen.ParseCRUDOperation(m.Name)
			if op == "" {
				// Non-CRUD-shaped RPC (e.g. webhook receiver helpers,
				// search endpoints). The service has real RPC surface;
				// keep emitting stubs so the user has something to fill in.
				allOrphan = false
				break
			}
		}
		if allOrphan && len(svc.Methods) > 0 {
			out[svc.Name] = true
		}
	}
	return out
}

// generateServiceStubs creates service.go, handlers.go, wrapper.go for new services.
// For existing service directories, it generates stubs only for missing RPC handlers.
// crudMethodNames contains method names that CRUD gen will implement; stubs are skipped for these.
//
// Webhook-only services (forge.yaml `webhooks:` declared, every proto RPC is a
// CRUD-shaped scaffold-leftover with no entity behind it) get the stub block
// suppressed entirely — their proto's Create/Get/Update/Delete/List RPCs are
// the leftover scaffolding from `forge new --service`, never the real handler
// surface. See FORGE_REVIEW_REBUILD.md §3.5 (admin_server CRUD-stub leak).
func generateServiceStubs(cfg *config.ProjectConfig, services []codegen.ServiceDef, projectDir string, crudMethodNames map[string]bool, cs *generator.FileChecksums) error {
	fmt.Println("\n🔧 Generating service stubs...")

	if len(services) == 0 {
		fmt.Println("  No services found in proto/services/")
		return nil
	}

	webhookOnly := webhookOnlyServiceNames(cfg, services, crudMethodNames)

	hasNewStubs := false
	for _, svc := range services {
		relServiceDir := toServiceDir(svc.Name)
		absServiceDir := filepath.Join(projectDir, relServiceDir)

		// Build the per-service skip set: anything CRUD-shaped that
		// matched an entity (already there from crudMethodNames) PLUS
		// every RPC of a webhook-only service. handlers_gen.go's filter
		// drops exactly the methods listed here.
		skipNames := crudMethodNames
		if webhookOnly[svc.Name] {
			skipNames = make(map[string]bool, len(crudMethodNames)+len(svc.Methods))
			for k, v := range crudMethodNames {
				skipNames[k] = v
			}
			for _, m := range svc.Methods {
				skipNames[m.Name] = true
			}
		}

		if dirExists(absServiceDir) {
			// Incremental: generate stubs only for missing RPC methods.
			// Threading cs ensures the rendered handlers_gen.go is recorded
			// so it doesn't show up as an orphan in `forge audit`.
			result, err := codegen.GenerateMissingHandlerStubs(svc, projectDir, absServiceDir, skipNames, cs)
			if err != nil {
				return fmt.Errorf("failed to generate missing stubs for %s: %w", svc.Name, err)
			}
			if result.AllUpToDate {
				if webhookOnly[svc.Name] {
					fmt.Printf("  ⏭️  Skipped %s/ (webhook-only — no CRUD stubs emitted)\n", relServiceDir)
				} else {
					fmt.Printf("  ⏭️  Skipped %s/ (all handlers up to date)\n", relServiceDir)
				}
			} else {
				fmt.Printf("  ✅ Generated %d new handler stub(s) in %s/handlers_gen.go: %s\n",
					len(result.NewMethods), relServiceDir, strings.Join(result.NewMethods, ", "))
				hasNewStubs = true
			}
			continue
		}

		if err := codegen.GenerateServiceStub(svc, absServiceDir, skipNames); err != nil {
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
func generateCRUDHandlers(services []codegen.ServiceDef, modulePath string, projectDir string, cs *generator.FileChecksums) error {
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
		if err := codegen.GenerateCRUDHandlers(svc, crudMethods, modulePath, projectDir, cs); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠️  CRUD generation for %s failed: %v\n", svc.Name, err)
			continue
		}
		fmt.Printf("  ✅ Generated handlers/%s/handlers_crud_gen.go (%d methods)\n", pkg, len(crudMethods))

		if err := codegen.GenerateCRUDTests(svc, crudMethods, modulePath, projectDir, cs); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠️  CRUD test generation for %s failed: %v\n", svc.Name, err)
		} else {
			fmt.Printf("  ✅ Generated handlers/%s/handlers_crud_gen_test.go (unit) + handlers_crud_integration_test.go (-tags integration)\n", pkg)
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
	// AdminServerService -> handlers/admin_server (snake-case so the generate
	// pipeline targets the same directory the scaffolder created).
	trimmed := strings.TrimSuffix(serviceName, "Service")
	if trimmed == "" {
		trimmed = serviceName
	}
	name := strings.ReplaceAll(naming.ToSnakeCase(trimmed), "-", "_")
	return fmt.Sprintf("handlers/%s", name)
}