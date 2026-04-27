package templates

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestRenderTemplate_StripsBuildIgnoreFromRenderedOutput(t *testing.T) {
	content, err := WebhookTemplates.Render("webhook_routes_gen.go.tmpl", map[string]any{
		"Package":  "tasks",
		"Webhooks": []map[string]any{{"Name": "github", "PascalName": "Github"}},
	})
	if err != nil {
		t.Fatalf("WebhookTemplates.Render() error = %v", err)
	}

	rendered := string(content)
	if strings.HasPrefix(rendered, "//go:build ignore") {
		t.Fatal("rendered template should not retain //go:build ignore header")
	}
	if !strings.Contains(rendered, "func (s *Service) RegisterWebhookRoutes") {
		t.Fatal("rendered template should include webhook route registration")
	}
}

// TestBootstrapTemplate_ZeroServices verifies that bootstrap.go.tmpl renders
// valid Go code when all component lists (services, workers, operators,
// packages) are empty — the "CLI-only" project scenario.
func TestBootstrapTemplate_ZeroServices(t *testing.T) {
	data := struct {
		Module   string
		Services []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
		Packages []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
		Workers []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
		Operators []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
		HasDatabase  bool
		OrmEnabled   bool
		HasFallible  bool
		ConfigFields map[string]bool
	}{
		Module:       "example.com/myproject",
		ConfigFields: map[string]bool{},
	}

	content, err := ProjectTemplates.Render("bootstrap.go.tmpl", data)
	if err != nil {
		t.Fatalf("Render bootstrap.go.tmpl: %v", err)
	}

	rendered := string(content)

	// Must contain key functions
	if !strings.Contains(rendered, "func Bootstrap(") {
		t.Fatal("missing Bootstrap function")
	}
	if !strings.Contains(rendered, "func BootstrapOnly(") {
		t.Fatal("missing BootstrapOnly function")
	}
	if !strings.Contains(rendered, "func (a *App) Shutdown(") {
		t.Fatal("missing Shutdown method")
	}

	// Must NOT contain service-specific imports
	if strings.Contains(rendered, "pkg/middleware") {
		t.Fatal("zero-service bootstrap should not import middleware")
	}

	// Verify it parses as valid Go
	fset := token.NewFileSet()
	_, parseErr := parser.ParseFile(fset, "bootstrap.go", rendered, parser.AllErrors)
	if parseErr != nil {
		t.Fatalf("rendered bootstrap.go does not parse as valid Go:\n%v\n\nSource:\n%s", parseErr, rendered)
	}
}

// TestBootstrapTestingTemplate_ZeroServices verifies that bootstrap_testing.go.tmpl
// renders valid Go when all component lists are empty.
func TestBootstrapTestingTemplate_ZeroServices(t *testing.T) {
	data := struct {
		Module   string
		Services []struct {
			Name, Package, FieldName, ProtoServiceName string
			Fallible                                   bool
			HasDB                                      bool
		}
		Packages []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
		MultiTenantEnabled bool
		AnyServiceHasDB    bool
	}{
		Module: "example.com/myproject",
	}

	content, err := ProjectTemplates.Render("bootstrap_testing.go.tmpl", data)
	if err != nil {
		t.Fatalf("Render bootstrap_testing.go.tmpl: %v", err)
	}

	rendered := string(content)

	// Must contain basic test helpers
	if !strings.Contains(rendered, "type TestOption func") {
		t.Fatal("missing TestOption type")
	}
	if !strings.Contains(rendered, "func defaultTestConfig(") {
		t.Fatal("missing defaultTestConfig")
	}

	// Verify it parses as valid Go
	fset := token.NewFileSet()
	_, parseErr := parser.ParseFile(fset, "testing.go", rendered, parser.AllErrors)
	if parseErr != nil {
		t.Fatalf("rendered testing.go does not parse as valid Go:\n%v\n\nSource:\n%s", parseErr, rendered)
	}
}