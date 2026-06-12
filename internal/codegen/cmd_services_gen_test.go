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
// same runtime names the bootstrap rows carry), PascalCase var
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
// load-bearing parts: one cobra var per registered service (the
// serviceCmd<X> family mirrors serviceRow<X>), the runServer
// single-name delegate, the serverCmd flag inheritance, and that the
// output parses as Go.
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
		// The provenance pointer: subcommands are a projection of the
		// user-owned registration rows, not of forge.yaml.
		"pkg/app/services.go",
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

// TestGenerateCmdServices_ZeroServices: a binary with no registered
// services still gets the (header-only) file so the package shape is
// stable and the provenance comment names the registration file. The
// file must parse and must NOT import cobra (unused import).
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

// TestGenerateCmdCommands pins the write-once contract of the
// user-owned extension point: first call scaffolds userCommands();
// later calls never overwrite.
func TestGenerateCmdCommands(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateCmdCommands(dir); err != nil {
		t.Fatalf("GenerateCmdCommands: %v", err)
	}
	dest := filepath.Join(dir, "cmd", "commands.go")
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read commands.go: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "func userCommands() []*cobra.Command {") {
		t.Errorf("commands.go missing the userCommands hook:\n%s", content)
	}
	if !strings.Contains(content, "yours: scaffolded once") {
		t.Error("commands.go missing the user-owned banner")
	}
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "commands.go", content, parser.AllErrors); perr != nil {
		t.Fatalf("commands.go does not parse: %v\n%s", perr, content)
	}

	// Write-once: a user edit survives regeneration.
	sentinel := []byte("package main\n\n// user edit sentinel\n")
	if err := os.WriteFile(dest, sentinel, 0644); err != nil {
		t.Fatal(err)
	}
	if err := GenerateCmdCommands(dir); err != nil {
		t.Fatalf("GenerateCmdCommands (second call): %v", err)
	}
	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(sentinel) {
		t.Error("GenerateCmdCommands overwrote a user-owned cmd/commands.go")
	}
}
