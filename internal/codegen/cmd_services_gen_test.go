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

// writeTestGoMod drops a minimal go.mod so GenerateCmdServices can resolve
// the project module path for the generated import lines.
func writeTestGoMod(t *testing.T, dir, module string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+module+"\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
}

// TestCmdServiceSubcommandsFromNames pins the projection from raw
// service-name spellings onto subcommand rows: kebab Use values (the same
// runtime names the app.Inventory rows + typed Mount<Svc> methods carry),
// PascalCase suffixes, and the reserved-name filter.
func TestCmdServiceSubcommandsFromNames(t *testing.T) {
	const module = "github.com/example/proj"
	data := CmdServiceSubcommandsFromNames(module, []string{
		"AdminServerService", // proto spelling
		"billing",            // forge.yaml spelling
		"db",                 // reserved: collides with the db command
		"VersionService",     // reserved after trimming: "version"
	})

	wantSvcs := []CmdServiceSubcommand{
		{Module: module, Name: "admin-server", FieldName: "AdminServer"},
		{Module: module, Name: "billing", FieldName: "Billing"},
	}
	if !reflect.DeepEqual(data.Services, wantSvcs) {
		t.Errorf("Services = %+v, want %+v", data.Services, wantSvcs)
	}
	wantSkipped := []string{"db", "version"}
	if !reflect.DeepEqual(data.Skipped, wantSkipped) {
		t.Errorf("Skipped = %+v, want %+v", data.Skipped, wantSkipped)
	}
}

// TestGenerateCmdServices renders the per-service subcommand files and the
// registration roster, pinning the load-bearing parts: ONE FILE PER SERVICE
// (internal/cli/svc_<name>.go), the new<Svc>Cmd constructor, the TYPED mount
// method expression on the serve() call (selection by compile-time type, not
// a string), the addServiceCmds roster, and that everything parses as Go.
func TestGenerateCmdServices(t *testing.T) {
	dir := t.TempDir()
	writeTestGoMod(t, dir, "github.com/example/proj")
	if err := GenerateCmdServices([]string{"AdminServerService", "billing"}, dir, nil); err != nil {
		t.Fatalf("GenerateCmdServices: %v", err)
	}

	cliDir := filepath.Join(dir, "internal", "cli")

	// One file per service.
	for _, tc := range []struct {
		file        string
		ctor        string
		mountExpr   string
		use         string
	}{
		{"svc_admin-server.go", "func newAdminServerCmd(deps Deps)", "(*app.Services).MountAdminServer", `Use:   "admin-server",`},
		{"svc_billing.go", "func newBillingCmd(deps Deps)", "(*app.Services).MountBilling", `Use:   "billing",`},
	} {
		raw, err := os.ReadFile(filepath.Join(cliDir, tc.file))
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		content := string(raw)
		for _, want := range []string{tc.ctor, tc.mountExpr, tc.use} {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing %q\n%s", tc.file, want, content)
			}
		}
		// Selection must be typed — no string positional selection.
		if strings.Contains(content, `[]string{"`) {
			t.Errorf("%s uses string selection — must be typed mount method expression\n%s", tc.file, content)
		}
		fset := token.NewFileSet()
		if _, perr := parser.ParseFile(fset, tc.file, content, parser.AllErrors); perr != nil {
			t.Fatalf("rendered %s does not parse: %v\n%s", tc.file, perr, content)
		}
	}

	// The registration roster.
	rawReg, err := os.ReadFile(filepath.Join(cliDir, "svc_register_gen.go"))
	if err != nil {
		t.Fatalf("read svc_register_gen.go: %v", err)
	}
	reg := string(rawReg)
	for _, want := range []string{
		"func addServiceCmds(root *cobra.Command, deps Deps)",
		"root.AddCommand(newAdminServerCmd(deps))",
		"root.AddCommand(newBillingCmd(deps))",
	} {
		if !strings.Contains(reg, want) {
			t.Errorf("svc_register_gen.go missing %q\n%s", want, reg)
		}
	}
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "svc_register_gen.go", reg, parser.AllErrors); perr != nil {
		t.Fatalf("rendered svc_register_gen.go does not parse: %v\n%s", perr, reg)
	}
}

// TestGenerateCmdServices_Reserved: a service whose runtime name collides
// with a built-in gets a NOTE comment in the roster, not a shadowing
// subcommand or a per-service file. Files must still parse.
func TestGenerateCmdServices_Reserved(t *testing.T) {
	dir := t.TempDir()
	writeTestGoMod(t, dir, "github.com/example/proj")
	if err := GenerateCmdServices([]string{"db", "billing"}, dir, nil); err != nil {
		t.Fatalf("GenerateCmdServices: %v", err)
	}
	cliDir := filepath.Join(dir, "internal", "cli")

	if _, err := os.Stat(filepath.Join(cliDir, "svc_db.go")); err == nil {
		t.Error("reserved name 'db' must NOT get a per-service file")
	}
	rawReg, err := os.ReadFile(filepath.Join(cliDir, "svc_register_gen.go"))
	if err != nil {
		t.Fatalf("read svc_register_gen.go: %v", err)
	}
	reg := string(rawReg)
	if strings.Contains(reg, "newDbCmd") || strings.Contains(reg, "newDBCmd(deps))") {
		t.Errorf("reserved name 'db' must NOT get a subcommand registration:\n%s", reg)
	}
	if !strings.Contains(reg, `service "db" collides with a built-in`) {
		t.Errorf("reserved name 'db' must get a NOTE comment:\n%s", reg)
	}
	if !strings.Contains(reg, "root.AddCommand(newBillingCmd(deps))") {
		t.Errorf("non-reserved 'billing' must still get a subcommand:\n%s", reg)
	}
	if _, serr := os.Stat(filepath.Join(cliDir, "svc_billing.go")); serr != nil {
		t.Errorf("non-reserved 'billing' must get a per-service file: %v", serr)
	}
}

// TestGenerateCmdServices_ZeroServices: a binary with no services still
// gets the (roster-only) file so the package shape is stable. It must parse.
func TestGenerateCmdServices_ZeroServices(t *testing.T) {
	dir := t.TempDir()
	writeTestGoMod(t, dir, "github.com/example/proj")
	if err := GenerateCmdServices(nil, dir, nil); err != nil {
		t.Fatalf("GenerateCmdServices: %v", err)
	}
	rawReg, err := os.ReadFile(filepath.Join(dir, "internal", "cli", "svc_register_gen.go"))
	if err != nil {
		t.Fatalf("read svc_register_gen.go: %v", err)
	}
	reg := string(rawReg)
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "svc_register_gen.go", reg, parser.AllErrors); perr != nil {
		t.Fatalf("zero-service svc_register_gen.go does not parse: %v\n%s", perr, reg)
	}
}
