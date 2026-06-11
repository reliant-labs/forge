package templates

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestBootstrapTemplate_UnservedServiceGuard pins the BootstrapOnly
// name-guard for types-only (forge.yaml serve: false) services: each
// unserved name gets a case arm returning a pointed error (mentioning
// served_by when set), and the guard disappears when no service is
// unserved.
func TestBootstrapTemplate_UnservedServiceGuard(t *testing.T) {
	type svc struct {
		Name, Package, ImportPath, FieldName, Alias string
		Fallible, HasWebhooks                       bool
		ConnectPkg, ProtoServiceName                string
	}
	type unserved struct{ Name, ServedBy string }
	mkData := func(guards []unserved) any {
		return struct {
			Module              string
			LeaderElectionID    string
			Services            []svc
			Packages            []struct{}
			Workers             []struct{}
			Operators           []struct{}
			ConfigFields        map[string]bool
			RESTEnabled         bool
			ConnectImports      []string
			DiagnosticsEnabled  bool
			StrictWiringEnabled bool
			UnservedServices    []unserved
		}{
			Module:           "example.com/myproject",
			LeaderElectionID: "myproject-leader",
			Services: []svc{
				{Name: "api", Package: "api", ImportPath: "api", FieldName: "API", Alias: "apihandler"},
			},
			ConfigFields:     map[string]bool{},
			UnservedServices: guards,
		}
	}

	t.Run("guard-renders-per-unserved-name", func(t *testing.T) {
		content, err := ProjectTemplates().Render("bootstrap.go.tmpl",
			mkData([]unserved{{Name: "project", ServedBy: "control-plane"}, {Name: "ledger"}}))
		if err != nil {
			t.Fatalf("Render bootstrap.go.tmpl: %v", err)
		}
		rendered := string(content)
		for _, want := range []string{
			`case "project":`,
			`case "ledger":`,
			"types-only in forge.yaml (serve: false; served by control-plane)",
			"types-only in forge.yaml (serve: false)",
		} {
			if !strings.Contains(rendered, want) {
				t.Errorf("rendered bootstrap missing %q", want)
			}
		}
		fset := token.NewFileSet()
		if _, parseErr := parser.ParseFile(fset, "bootstrap.go", rendered, parser.AllErrors); parseErr != nil {
			t.Fatalf("rendered bootstrap.go does not parse:\n%v\n\nSource:\n%s", parseErr, rendered)
		}
	})

	t.Run("no-guard-when-all-served", func(t *testing.T) {
		content, err := ProjectTemplates().Render("bootstrap.go.tmpl", mkData(nil))
		if err != nil {
			t.Fatalf("Render bootstrap.go.tmpl: %v", err)
		}
		if strings.Contains(string(content), "types-only") {
			t.Errorf("all-served bootstrap must not carry the unserved guard")
		}
	})
}
