package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// GenerateAuthorizer generates authorizer_gen.go for each service that has
// methods with authorization annotations. The generated file contains a
// methodRoles map and a role-checking CanAccess/Can implementation.
func GenerateAuthorizer(services []ServiceDef, modulePath string, targetDir string) error {
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

		content, err := templates.ServiceTemplates.Render("authorizer_gen.go.tmpl", data)
		if err != nil {
			return fmt.Errorf("render authorizer_gen.go.tmpl for %s: %w", svc.Name, err)
		}

		outPath := filepath.Join(svcDir, "authorizer_gen.go")
		if err := os.WriteFile(outPath, content, 0644); err != nil {
			return fmt.Errorf("write authorizer_gen.go for %s: %w", svc.Name, err)
		}
	}

	return nil
}