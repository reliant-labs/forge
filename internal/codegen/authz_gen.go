package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/templates"
)

// AuthzMethodData holds per-method authorization metadata for the authorizer template.
type AuthzMethodData struct {
	Procedure     string   // full RPC procedure path, e.g. "/services.users.v1.UserService/CreateUser"
	RequiredRoles []string // roles that grant access (empty = any authenticated user)
	AuthRequired  bool     // whether auth is required for this method
}

// AuthzTemplateData holds the data shape expected by authorizer_gen.go.tmpl.
type AuthzTemplateData struct {
	Package     string            // Go package name, e.g. "users"
	ServiceName string            // proto service name, e.g. "UserService"
	Module      string            // Go module path
	Methods     []AuthzMethodData // per-method authorization data
}

// GenerateAuthorizer generates authorizer_gen.go for each service whose
// handler directory exists. The generated file contains a methodRoles map
// and a role-checking CanAccess/Can implementation. It is always generated
// (even with zero annotated methods) so that the companion authorizer.go
// can unconditionally reference GeneratedAuthorizer without compilation
// errors.
//
// cs is the project's checksum tracker — passing it ensures every emitted
// authorizer_gen.go is recorded so `forge audit` doesn't flag it as an
// orphan. A nil cs is tolerated.
func GenerateAuthorizer(services []ServiceDef, modulePath string, targetDir string, cs *checksums.FileChecksums) error {
	for _, svc := range services {
		pkg := strings.ToLower(strings.TrimSuffix(svc.Name, "Service"))
		svcDir := filepath.Join(targetDir, "handlers", pkg)

		// Only generate if the service directory exists (was scaffolded)
		if _, err := os.Stat(svcDir); os.IsNotExist(err) {
			continue
		}

		var methods []AuthzMethodData
		for _, m := range svc.Methods {
			methods = append(methods, AuthzMethodData{
				Procedure:     fmt.Sprintf("/%s.%s/%s", svc.Package, svc.Name, m.Name),
				RequiredRoles: m.RequiredRoles,
				AuthRequired:  m.AuthRequired,
			})
		}

		data := AuthzTemplateData{
			Package:     pkg,
			ServiceName: svc.Name,
			Module:      modulePath,
			Methods:     methods,
		}

		content, err := templates.ServiceTemplates().Render("authorizer_gen.go.tmpl", data)
		if err != nil {
			return fmt.Errorf("render authorizer_gen.go.tmpl for %s: %w", svc.Name, err)
		}

		relPath := filepath.Join("handlers", pkg, "authorizer_gen.go")
		if _, err := checksums.WriteGeneratedFile(targetDir, relPath, content, cs, true); err != nil {
			return fmt.Errorf("write authorizer_gen.go for %s: %w", svc.Name, err)
		}
	}

	// Also generate authorizer_gen.go for service directories that exist but
	// have no corresponding ServiceDef (e.g., scaffold created the handler
	// dir before any RPCs were defined in the proto). This ensures
	// authorizer.go can always reference GeneratedAuthorizer.
	handlersDir := filepath.Join(targetDir, "handlers")
	entries, err := os.ReadDir(handlersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read handlers dir: %w", err)
	}

	// Build set of packages already generated above.
	generated := make(map[string]bool, len(services))
	for _, svc := range services {
		generated[strings.ToLower(strings.TrimSuffix(svc.Name, "Service"))] = true
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pkg := entry.Name()
		if generated[pkg] {
			continue
		}
		// Only generate if authorizer.go exists (confirming this is a service dir)
		if _, err := os.Stat(filepath.Join(handlersDir, pkg, "authorizer.go")); os.IsNotExist(err) {
			continue
		}

		data := AuthzTemplateData{
			Package:     pkg,
			ServiceName: strings.ToUpper(pkg[:1]) + pkg[1:] + "Service",
			Module:      modulePath,
			Methods:     nil,
		}

		content, err := templates.ServiceTemplates().Render("authorizer_gen.go.tmpl", data)
		if err != nil {
			return fmt.Errorf("render authorizer_gen.go.tmpl for %s: %w", pkg, err)
		}

		relPath := filepath.Join("handlers", pkg, "authorizer_gen.go")
		if _, err := checksums.WriteGeneratedFile(targetDir, relPath, content, cs, true); err != nil {
			return fmt.Errorf("write authorizer_gen.go for %s: %w", pkg, err)
		}
	}

	return nil
}
