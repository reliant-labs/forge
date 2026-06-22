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

// writeTestGoMod drops a minimal go.mod so GenerateCmdGroups can resolve
// the project module path for the generated import lines.
func writeTestGoMod(t *testing.T, dir, module string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+module+"\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
}

// TestCmdServiceItemsFromNames pins the projection from raw service-name
// spellings onto group-item rows: kebab Use values (the same runtime names
// the app inventory rows + typed Mount<Svc> methods carry), PascalCase
// suffixes, and the reserved-name filter.
func TestCmdServiceItemsFromNames(t *testing.T) {
	const module = "github.com/example/proj"
	items, skipped := cmdServiceItemsFromNames(module, "proj", []string{
		"AdminServerService", // proto spelling
		"billing",            // forge.yaml spelling
		"db",                 // reserved: collides with the db command
		"VersionService",     // reserved after trimming: "version"
	}, nil)

	wantItems := []CmdGroupItem{
		{Module: module, Bin: "proj", Name: "admin-server", FieldName: "AdminServer", MountFieldName: "AdminServer"},
		{Module: module, Bin: "proj", Name: "billing", FieldName: "Billing", MountFieldName: "Billing"},
	}
	if !reflect.DeepEqual(items, wantItems) {
		t.Errorf("items = %+v, want %+v", items, wantItems)
	}
	wantSkipped := []string{"db", "version"}
	if !reflect.DeepEqual(skipped, wantSkipped) {
		t.Errorf("skipped = %+v, want %+v", skipped, wantSkipped)
	}
}

// TestGenerateCmdGroups renders the per-service / per-worker / per-operator
// command-group files and the group anchors, pinning the load-bearing parts:
// ONE FILE PER ITEM under cmd/<bin>/cmd/<group>/, the New<X>Cmd constructor,
// the TYPED mount method expression on the cmd.Serve() call (selection by
// compile-time type, not a string), init()-based self-registration, and that
// everything parses as Go.
func TestGenerateCmdGroups(t *testing.T) {
	dir := t.TempDir()
	writeTestGoMod(t, dir, "github.com/example/proj")
	if err := GenerateCmdGroups(CmdServiceGroupInput{
		Bin:       "proj",
		Services:  []string{"AdminServerService", "billing"},
		Workers:   []BootstrapWorkerData{{Name: "reaper", FieldName: "Reaper"}},
		Operators: []BootstrapOperatorData{{Name: "tenant", FieldName: "Tenant"}},
	}, dir, nil); err != nil {
		t.Fatalf("GenerateCmdGroups: %v", err)
	}

	base := filepath.Join(dir, "cmd", "proj", "cmd")

	// Services: one file per service, typed mount method expression.
	for _, tc := range []struct {
		file      string
		ctor      string
		mountExpr string
		use       string
	}{
		{filepath.Join("services", "admin-server.go"), "func NewAdminServerCmd(deps cmd.Deps)", "(*app.Components).MountAdminServer", `Use:   "admin-server",`},
		{filepath.Join("services", "billing.go"), "func NewBillingCmd(deps cmd.Deps)", "(*app.Components).MountBilling", `Use:   "billing",`},
	} {
		raw, err := os.ReadFile(filepath.Join(base, tc.file))
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		content := string(raw)
		for _, want := range []string{tc.ctor, tc.mountExpr, tc.use, "package services", "cmd.RegisterServiceCmd("} {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing %q\n%s", tc.file, want, content)
			}
		}
		// Selection must be typed — no string positional selection.
		if strings.Contains(content, `(*app.Components).MountByName`) {
			t.Errorf("%s uses string selection — must be typed mount method expression\n%s", tc.file, content)
		}
		assertParses(t, tc.file, content)
	}

	// Workers: one file per worker, cmd.MountNone + named supervised subset.
	for _, tc := range []struct {
		file, ctor, use, reg string
	}{
		{filepath.Join("workers", "reaper.go"), "func NewReaperCmd(deps cmd.Deps)", `Use:   "reaper",`, "cmd.RegisterWorkerCmd("},
	} {
		raw, err := os.ReadFile(filepath.Join(base, tc.file))
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		content := string(raw)
		for _, want := range []string{tc.ctor, tc.use, tc.reg, "package workers", "cmd.MountNone", `WorkerNames:   []string{"reaper"}`} {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing %q\n%s", tc.file, want, content)
			}
		}
		assertParses(t, tc.file, content)
	}

	// Operators: one file per operator.
	{
		file := filepath.Join("operators", "tenant.go")
		raw, err := os.ReadFile(filepath.Join(base, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		content := string(raw)
		for _, want := range []string{"func NewTenantCmd(deps cmd.Deps)", `Use:   "tenant",`, "cmd.RegisterOperatorCmd(", "package operators", "cmd.MountNone", `OperatorNames: []string{"tenant"}`} {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing %q\n%s", file, want, content)
			}
		}
		assertParses(t, file, content)
	}

	// Group anchors exist and parse (so main.go's blank imports resolve even
	// with zero items).
	for _, anchor := range []struct{ file, pkg string }{
		{filepath.Join("services", "register_gen.go"), "package services"},
		{filepath.Join("workers", "register_gen.go"), "package workers"},
		{filepath.Join("operators", "register_gen.go"), "package operators"},
	} {
		raw, err := os.ReadFile(filepath.Join(base, anchor.file))
		if err != nil {
			t.Fatalf("read %s: %v", anchor.file, err)
		}
		content := string(raw)
		if !strings.Contains(content, anchor.pkg) {
			t.Errorf("%s missing %q\n%s", anchor.file, anchor.pkg, content)
		}
		assertParses(t, anchor.file, content)
	}
}

// TestGenerateCmdGroups_Reserved: a service whose runtime name collides with
// a built-in gets a NOTE comment in the services anchor, not a shadowing
// subcommand or a per-service file. Files must still parse.
func TestGenerateCmdGroups_Reserved(t *testing.T) {
	dir := t.TempDir()
	writeTestGoMod(t, dir, "github.com/example/proj")
	if err := GenerateCmdGroups(CmdServiceGroupInput{Bin: "proj", Services: []string{"db", "billing"}}, dir, nil); err != nil {
		t.Fatalf("GenerateCmdGroups: %v", err)
	}
	svcDir := filepath.Join(dir, "cmd", "proj", "cmd", "services")

	if _, err := os.Stat(filepath.Join(svcDir, "db.go")); err == nil {
		t.Error("reserved name 'db' must NOT get a per-service file")
	}
	rawAnchor, err := os.ReadFile(filepath.Join(svcDir, "register_gen.go"))
	if err != nil {
		t.Fatalf("read services/register_gen.go: %v", err)
	}
	anchor := string(rawAnchor)
	if !strings.Contains(anchor, `service "db" collides with a built-in`) {
		t.Errorf("reserved name 'db' must get a NOTE comment:\n%s", anchor)
	}
	if _, serr := os.Stat(filepath.Join(svcDir, "billing.go")); serr != nil {
		t.Errorf("non-reserved 'billing' must get a per-service file: %v", serr)
	}
}

// TestGenerateCmdGroups_ZeroComponents: a binary with no services/workers/
// operators still gets the (anchor-only) files so each group package compiles
// (main.go's blank imports resolve). They must parse.
func TestGenerateCmdGroups_ZeroComponents(t *testing.T) {
	dir := t.TempDir()
	writeTestGoMod(t, dir, "github.com/example/proj")
	if err := GenerateCmdGroups(CmdServiceGroupInput{Bin: "proj"}, dir, nil); err != nil {
		t.Fatalf("GenerateCmdGroups: %v", err)
	}
	base := filepath.Join(dir, "cmd", "proj", "cmd")
	for _, anchor := range []string{
		filepath.Join("services", "register_gen.go"),
		filepath.Join("workers", "register_gen.go"),
		filepath.Join("operators", "register_gen.go"),
	} {
		raw, err := os.ReadFile(filepath.Join(base, anchor))
		if err != nil {
			t.Fatalf("read %s: %v", anchor, err)
		}
		assertParses(t, anchor, string(raw))
	}
}

// TestGenerateCmdGroups_MountNameCollision is the regression for the
// control-plane disown: when a handler service's package collides cross-role
// with an internal package (handler internal/handlers/billing `package
// billing` + domain internal/billing `package billing`), inventory_gen
// renames the typed mount method to MountSvcBilling. The cmd-group service
// command MUST call that exact name — not MountBilling — or the generated
// cmd/<bin>/cmd/services/billing.go fails to compile. Both halves now derive
// the FieldName from the same ResolveCollisionNaming source, so they agree.
func TestGenerateCmdGroups_MountNameCollision(t *testing.T) {
	dir := t.TempDir()
	writeTestGoMod(t, dir, "github.com/example/proj")

	// Handler service: internal/handlers/billing, package billing.
	mustWriteFile(t, filepath.Join(dir, "internal", "handlers", "billing", "service.go"),
		"package billing\n\ntype Service struct{}\n")
	// Cross-role collider: domain pkg internal/billing, also package billing.
	mustWriteFile(t, filepath.Join(dir, "internal", "billing", "billing.go"),
		"package billing\n\ntype Service interface{}\n")
	// A non-colliding service for contrast: internal/handlers/user, package user
	// (no internal/user domain pkg) — must stay MountUser.
	mustWriteFile(t, filepath.Join(dir, "internal", "handlers", "user", "service.go"),
		"package user\n\ntype Service struct{}\n")

	if err := GenerateCmdGroups(CmdServiceGroupInput{
		Bin:      "proj",
		Services: []string{"BillingService", "UserService"},
		Packages: []BootstrapPackageData{{Package: "billing"}},
	}, dir, nil); err != nil {
		t.Fatalf("GenerateCmdGroups: %v", err)
	}

	base := filepath.Join(dir, "cmd", "proj", "cmd", "services")

	// Colliding service: cmd-group must call the collision-aware mount METHOD,
	// matching the Svc-prefixed name inventory_gen emits.
	billing := mustReadFile(t, filepath.Join(base, "billing.go"))
	if !strings.Contains(billing, "(*app.Components).MountSvcBilling") {
		t.Errorf("billing.go must call collision-aware (*app.Components).MountSvcBilling:\n%s", billing)
	}
	if strings.Contains(billing, "(*app.Components).MountBilling,") {
		t.Errorf("billing.go must NOT call the plain MountBilling (mismatch with inventory_gen):\n%s", billing)
	}
	// The constructor name stays PLAIN (NewBillingCmd) + Use "billing" — it is a
	// local group-package symbol, not subject to the cross-role rename. Only the
	// mount method reference is collision-aware. This matches the control-plane
	// hand-fix exactly (NewBillingCmd + MountSvcBilling).
	if !strings.Contains(billing, "func NewBillingCmd(deps cmd.Deps)") {
		t.Errorf("billing.go constructor must stay NewBillingCmd:\n%s", billing)
	}
	if !strings.Contains(billing, `Use:   "billing",`) {
		t.Errorf("billing.go Use must stay \"billing\":\n%s", billing)
	}
	assertParses(t, "billing.go", billing)

	// Non-colliding service is unchanged — plain MountUser.
	user := mustReadFile(t, filepath.Join(base, "user.go"))
	if !strings.Contains(user, "(*app.Components).MountUser") {
		t.Errorf("user.go must call plain (*app.Components).MountUser:\n%s", user)
	}
	assertParses(t, "user.go", user)
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func assertParses(t *testing.T, name, content string) {
	t.Helper()
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, name, content, parser.AllErrors); perr != nil {
		t.Fatalf("rendered %s does not parse: %v\n%s", name, perr, content)
	}
}
