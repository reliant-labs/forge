package codegen

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestCmdServiceSubcommandsFromNames pins the projection from raw
// service-name spellings onto subcommand rows: kebab Use values (the
// same runtime names the app.Inventory rows carry), PascalCase var
// suffixes, and the reserved-name filter.
func TestCmdServiceSubcommandsFromNames(t *testing.T) {
	data := CmdServiceSubcommandsFromNames([]string{
		"AdminServerService", // proto spelling
		"billing",            // forge.yaml spelling
		"db",                 // reserved: collides with cmd/db.go
		"VersionService",     // reserved after trimming: "version"
	})

	wantSvcs := []CmdServiceSubcommand{
		{Name: "admin-server", FieldName: "AdminServer"},
		{Name: "billing", FieldName: "Billing"},
	}
	if !reflect.DeepEqual(data.Services, wantSvcs) {
		t.Errorf("Services = %+v, want %+v", data.Services, wantSvcs)
	}
	wantSkipped := []string{"db", "version"}
	if !reflect.DeepEqual(data.Skipped, wantSkipped) {
		t.Errorf("Skipped = %+v, want %+v", data.Skipped, wantSkipped)
	}
}

// TestGenerateCmdServices renders cmd/services_gen.go and pins the
// load-bearing parts: one REAL cobra subcommand var per service (the
// serviceCmd<X> family), the runServer single-name delegate (selection
// by the subcommand's own baked-in identity, not a runtime arg), the
// serverCmd flag inheritance, the registration, and that the output
// parses as Go.
func TestGenerateCmdServices(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateCmdServices([]string{"AdminServerService", "billing"}, dir, nil); err != nil {
		t.Fatalf("GenerateCmdServices: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "cmd", "services_gen.go"))
	if err != nil {
		t.Fatalf("read services_gen.go: %v", err)
	}
	content := string(raw)

	for _, want := range []string{
		"var serviceCmdAdminServer = &cobra.Command{",
		`Use:   "admin-server",`,
		`return runServer(cmd, []string{"admin-server"})`,
		"serviceCmdAdminServer.Flags().AddFlagSet(serverCmd.Flags())",
		"rootCmd.AddCommand(serviceCmdAdminServer)",
		"var serviceCmdBilling = &cobra.Command{",
		`return runServer(cmd, []string{"billing"})`,
		// The provenance pointer: subcommands select over the data-only
		// app.Inventory, not over forge.yaml.
		"internal/app Inventory",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("services_gen.go missing %q\n%s", want, content)
		}
	}

	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "services_gen.go", content, parser.AllErrors); perr != nil {
		t.Fatalf("rendered services_gen.go does not parse: %v\n%s", perr, content)
	}
}

// TestGenerateCmdServices_Reserved: a service whose runtime name
// collides with a built-in subcommand gets a NOTE comment, not a
// shadowing subcommand. The file must still parse.
func TestGenerateCmdServices_Reserved(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateCmdServices([]string{"db", "billing"}, dir, nil); err != nil {
		t.Fatalf("GenerateCmdServices: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "cmd", "services_gen.go"))
	if err != nil {
		t.Fatalf("read services_gen.go: %v", err)
	}
	content := string(raw)
	if strings.Contains(content, "var serviceCmdDb") {
		t.Errorf("reserved name 'db' must NOT get a subcommand:\n%s", content)
	}
	if !strings.Contains(content, `service "db" collides with a built-in`) {
		t.Errorf("reserved name 'db' must get a NOTE comment:\n%s", content)
	}
	if !strings.Contains(content, "var serviceCmdBilling") {
		t.Errorf("non-reserved 'billing' must still get a subcommand:\n%s", content)
	}
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "services_gen.go", content, parser.AllErrors); perr != nil {
		t.Fatalf("rendered services_gen.go does not parse: %v\n%s", perr, content)
	}
}

// TestGenerateCmdServices_ZeroServices: a binary with no services still
// gets the (header-only) file so the package shape is stable. The file
// must parse and must NOT import cobra (unused import would not compile).
func TestGenerateCmdServices_ZeroServices(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateCmdServices(nil, dir, nil); err != nil {
		t.Fatalf("GenerateCmdServices: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "cmd", "services_gen.go"))
	if err != nil {
		t.Fatalf("read services_gen.go: %v", err)
	}
	content := string(raw)
	if strings.Contains(content, "spf13/cobra") {
		t.Error("zero-service services_gen.go must not import cobra (unused import would not compile)")
	}
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "services_gen.go", content, parser.AllErrors); perr != nil {
		t.Fatalf("zero-service services_gen.go does not parse: %v\n%s", perr, content)
	}
}
