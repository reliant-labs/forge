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

// TestGenerateCmdGroups renders the per-SERVICE command-group files, the three
// group anchors, and the SCAFFOLD-ONCE composition-root main.go. Post
// zero-generate-time-worker/operator-discovery: GenerateCmdGroups emits ONLY
// services + anchors + a main.go carrying ONLY services (workers/operators are
// scaffold-once OWNED code written by ScaffoldWorkerCmd/ScaffoldOperatorCmd, and
// hand-wired into main.go). It pins the typed mount method expression, the
// ABSENCE of init()-based self-registration, and that everything parses as Go.
func TestGenerateCmdGroups(t *testing.T) {
	dir := t.TempDir()
	writeTestGoMod(t, dir, "github.com/example/proj")
	if err := GenerateCmdGroups(CmdServiceGroupInput{
		Bin:      "proj",
		Services: []string{"AdminServerService", "billing"},
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
		for _, want := range []string{tc.ctor, tc.mountExpr, tc.use, "package services"} {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing %q\n%s", tc.file, want, content)
			}
		}
		// No init() self-registration — the registry is gone; main.go wires
		// the tree explicitly.
		if strings.Contains(content, "func init()") || strings.Contains(content, "cmd.RegisterServiceCmd") {
			t.Errorf("%s must not self-register via init()\n%s", tc.file, content)
		}
		// Selection must be typed — no string positional selection.
		if strings.Contains(content, `(*app.Components).MountByName`) {
			t.Errorf("%s uses string selection — must be typed mount method expression\n%s", tc.file, content)
		}
		assertParses(t, tc.file, content)
	}

	// GenerateCmdGroups must NOT emit any per-worker / per-operator subcommand
	// file — generate does zero worker/operator discovery; those are scaffold-
	// once OWNED code (ScaffoldWorkerCmd/ScaffoldOperatorCmd).
	for _, absent := range []string{
		filepath.Join("workers", "reaper.go"),
		filepath.Join("operators", "tenant.go"),
	} {
		if _, err := os.Stat(filepath.Join(base, absent)); err == nil {
			t.Errorf("GenerateCmdGroups emitted %s — worker/operator subcommands must be scaffold-once, not generated", absent)
		}
	}

	// Composition root: SCAFFOLD-ONCE main.go names the SERVICE constructors
	// explicitly and passes them to cmd.Execute. It carries NO worker/operator
	// refs or imports on the initial emit (no init() registry, no blank imports).
	{
		mainPath := filepath.Join(dir, "cmd", "proj", "main.go")
		raw, err := os.ReadFile(mainPath)
		if err != nil {
			t.Fatalf("read cmd/proj/main.go: %v", err)
		}
		content := string(raw)
		for _, want := range []string{
			"package main",
			"cmd.Execute(",
			"services.NewAdminServerCmd",
			"services.NewBillingCmd",
			`"github.com/example/proj/cmd/proj/cmd/services"`,
		} {
			if !strings.Contains(content, want) {
				t.Errorf("cmd/proj/main.go missing %q\n%s", want, content)
			}
		}
		// Owned/scaffold-once: no generated banner, no worker/operator refs on
		// the initial emit, no self-registration hooks, no blank imports.
		for _, forbidden := range []string{
			"DO NOT EDIT",
			"workers.NewReaperCmd",
			"operators.NewTenantCmd",
			"func init()",
			"RegisterServiceCmd",
			`_ "`,
		} {
			if strings.Contains(content, forbidden) {
				t.Errorf("cmd/proj/main.go must not contain %q\n%s", forbidden, content)
			}
		}
		assertParses(t, "main.go", content)
	}

	// Group anchors exist and parse (so the group packages compile even with
	// zero items). All three groups are anchored even though workers/operators
	// have no per-component files yet — main.go's (future) blank/named imports
	// of those subpackages must resolve.
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

// TestGenerateCmdGroups_MainScaffoldOnce pins that main.go is write-if-absent:
// a second GenerateCmdGroups run (or any run where main.go already exists) does
// NOT overwrite the owned file, even as the service set changes.
func TestGenerateCmdGroups_MainScaffoldOnce(t *testing.T) {
	dir := t.TempDir()
	writeTestGoMod(t, dir, "github.com/example/proj")
	mainPath := filepath.Join(dir, "cmd", "proj", "main.go")

	if err := GenerateCmdGroups(CmdServiceGroupInput{Bin: "proj", Services: []string{"billing"}}, dir, nil); err != nil {
		t.Fatalf("first GenerateCmdGroups: %v", err)
	}
	// Hand-edit the owned main.go.
	sentinel := "// HAND-OWNED SENTINEL — must survive regenerate\n"
	orig, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if err := os.WriteFile(mainPath, append([]byte(sentinel), orig...), 0o644); err != nil {
		t.Fatalf("hand-edit main.go: %v", err)
	}
	// Re-run with a DIFFERENT service set — main.go must be untouched.
	if err := GenerateCmdGroups(CmdServiceGroupInput{Bin: "proj", Services: []string{"billing", "orders"}}, dir, nil); err != nil {
		t.Fatalf("second GenerateCmdGroups: %v", err)
	}
	after, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("re-read main.go: %v", err)
	}
	if !strings.Contains(string(after), sentinel) {
		t.Errorf("scaffold-once main.go was overwritten — hand edit lost:\n%s", after)
	}
}

// TestScaffoldWorkerCmd / TestScaffoldOperatorCmd pin the scaffold-once per-
// component subcommand files `forge add worker/operator` writes (the work the
// retired generate-time cmd-groups loop used to do). One file per component,
// write-if-absent, typed self-composed supervised subset, no init() registry.
func TestScaffoldWorkerCmd(t *testing.T) {
	dir := t.TempDir()
	writeTestGoMod(t, dir, "github.com/example/proj")

	wrote, err := ScaffoldWorkerCmd(dir, "proj", "reaper")
	if err != nil {
		t.Fatalf("ScaffoldWorkerCmd: %v", err)
	}
	if !wrote {
		t.Fatalf("expected a fresh worker subcommand file to be written")
	}
	path := filepath.Join(dir, "cmd", "proj", "cmd", "workers", "reaper.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reaper.go: %v", err)
	}
	content := string(raw)
	for _, want := range []string{"func NewReaperCmd(deps cmd.Deps)", `Use:   "reaper",`, "package workers", "cmd.MountNone", "cmd.ServeSpec{", `[]serverkit.Worker{c.WorkerReaper()}`} {
		if !strings.Contains(content, want) {
			t.Errorf("reaper.go missing %q\n%s", want, content)
		}
	}
	if strings.Contains(content, "DO NOT EDIT") || strings.Contains(content, "func init()") {
		t.Errorf("scaffold-once worker subcommand must not carry generated banner / init():\n%s", content)
	}
	assertParses(t, "reaper.go", content)

	// Write-if-absent: a second call must NOT overwrite a hand-edited file.
	if err := os.WriteFile(path, []byte("// SENTINEL\n"+content), 0o644); err != nil {
		t.Fatal(err)
	}
	wrote2, err := ScaffoldWorkerCmd(dir, "proj", "reaper")
	if err != nil {
		t.Fatalf("second ScaffoldWorkerCmd: %v", err)
	}
	if wrote2 {
		t.Errorf("ScaffoldWorkerCmd overwrote an existing owned file")
	}
	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), "// SENTINEL") {
		t.Errorf("owned worker subcommand was overwritten")
	}
}

func TestScaffoldOperatorCmd(t *testing.T) {
	dir := t.TempDir()
	writeTestGoMod(t, dir, "github.com/example/proj")

	if _, err := ScaffoldOperatorCmd(dir, "proj", "tenant"); err != nil {
		t.Fatalf("ScaffoldOperatorCmd: %v", err)
	}
	path := filepath.Join(dir, "cmd", "proj", "cmd", "operators", "tenant.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tenant.go: %v", err)
	}
	content := string(raw)
	for _, want := range []string{"func NewTenantCmd(deps cmd.Deps)", `Use:   "tenant",`, "package operators", "cmd.MountNone", "cmd.ServeSpec{", `[]app.OperatorEntry{c.OperatorTenant()}`} {
		if !strings.Contains(content, want) {
			t.Errorf("tenant.go missing %q\n%s", want, content)
		}
	}
	assertParses(t, "tenant.go", content)
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
// operators still gets the (anchor-only) group files so each group package
// compiles, plus a composition-root main.go with a bare cmd.Execute() (no group
// imports — an unused import would not compile). Everything must parse.
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

	// main.go: bare cmd.Execute(), no group subpackage imports.
	raw, err := os.ReadFile(filepath.Join(dir, "cmd", "proj", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/proj/main.go: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "cmd.Execute()") {
		t.Errorf("zero-component main.go should be a bare cmd.Execute():\n%s", content)
	}
	if strings.Contains(content, "/cmd/services") || strings.Contains(content, "/cmd/workers") || strings.Contains(content, "/cmd/operators") {
		t.Errorf("zero-component main.go must not import empty group packages (unused import):\n%s", content)
	}
	assertParses(t, "main.go", content)
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
