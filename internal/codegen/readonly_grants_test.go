package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/dbgrants"
	"github.com/reliant-labs/forge/pkg/schemadef"
)

// The read-only role's grants are projected from the applied schema and the
// wire messages' `forge:secret` markers. These pin the projection on a fixture
// schema with no database; the Postgres side (the role really cannot select the
// column) is pkg/migratekit/grants_pg_test.go.

func col(name string) schemadef.Column {
	return schemadef.Column{Name: name, Type: schemadef.CanonicalType("string")}
}

func fixtureTable(name string, cols ...string) schemadef.Table {
	t := schemadef.Table{Name: name, Schema: "public", PKCols: []string{"id"}}
	for _, c := range cols {
		t.Columns = append(t.Columns, col(c))
	}
	return t
}

// secretService is a service whose `Account` entity message marks api_token
// `forge:secret`, and which declares CRUD for it.
func secretService(entity string, methods []string, fields ...SchemaFieldDef) ServiceDef {
	svc := ServiceDef{Name: entity + "Service", Package: "app.v1", Schemas: map[string][]SchemaFieldDef{"app.v1." + entity: fields}}
	for _, m := range methods {
		svc.Methods = append(svc.Methods, Method{Name: m})
	}
	return svc
}

func secretFields(secret ...string) []SchemaFieldDef {
	var out []SchemaFieldDef
	for _, n := range []string{"id", "email", "api_token"} {
		f := SchemaFieldDef{Name: n, Kind: "string"}
		for _, s := range secret {
			if s == n {
				f.Secret = true
			}
		}
		out = append(out, f)
	}
	return out
}

func grantTableByName(t *testing.T, got []dbgrants.Table, name string) dbgrants.Table {
	t.Helper()
	for _, g := range got {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("no grant table %q in %+v", name, got)
	return dbgrants.Table{}
}

func secretColumns(tb dbgrants.Table) string {
	var out []string
	for _, c := range tb.Columns {
		if c.Secret {
			out = append(out, c.Name)
		}
	}
	return strings.Join(out, ",")
}

// A secret field on the CRUD-joined entity withholds its column; every other
// column stays readable.
func TestReadOnlyGrantTables_SecretFieldOfACRUDEntityIsWithheld(t *testing.T) {
	tables := []schemadef.Table{fixtureTable("accounts", "id", "email", "api_token")}
	svc := secretService("Account", []string{"GetAccount", "ListAccounts"}, secretFields("api_token")...)
	got := ReadOnlyGrantTables(tables, []ServiceDef{svc})
	if s := secretColumns(grantTableByName(t, got, "accounts")); s != "api_token" {
		t.Errorf("secret columns = %q, want api_token only", s)
	}
}

// A table no service has CRUD rpcs for (every rpc `forge:custom`, or none)
// still has its secrets withheld: the marker lives on the wire message, and the
// message exists whether or not CRUD does.
func TestReadOnlyGrantTables_SecretOfATableWithNoCRUDIsWithheld(t *testing.T) {
	tables := []schemadef.Table{fixtureTable("accounts", "id", "email", "api_token")}
	svc := secretService("Account", nil, secretFields("api_token")...)
	got := ReadOnlyGrantTables(tables, []ServiceDef{svc})
	if s := secretColumns(grantTableByName(t, got, "accounts")); s != "api_token" {
		t.Errorf("secret columns = %q, want api_token: a table with no CRUD rpcs must not lose the marker", s)
	}
}

// An unmanaged service (`forge:unmanaged`) is still a service whose message can
// carry the marker.
func TestReadOnlyGrantTables_SecretInAnUnmanagedServiceIsWithheld(t *testing.T) {
	tables := []schemadef.Table{fixtureTable("accounts", "id", "email", "api_token")}
	svc := secretService("Account", []string{"GetAccount"}, secretFields("api_token")...)
	svc.Unmanaged = true
	got := ReadOnlyGrantTables(tables, []ServiceDef{svc})
	if s := secretColumns(grantTableByName(t, got, "accounts")); s != "api_token" {
		t.Errorf("secret columns = %q, want api_token", s)
	}
}

// The marker on one table's message does not leak onto another table that
// happens to share a column name.
func TestReadOnlyGrantTables_MarkerIsPerTable(t *testing.T) {
	tables := []schemadef.Table{
		fixtureTable("accounts", "id", "email", "api_token"),
		fixtureTable("audits", "id", "api_token"),
	}
	svc := secretService("Account", []string{"GetAccount"}, secretFields("api_token")...)
	got := ReadOnlyGrantTables(tables, []ServiceDef{svc})
	if s := secretColumns(grantTableByName(t, got, "audits")); s != "" {
		t.Errorf("audits secret columns = %q, want none: only Account's message marks a secret", s)
	}
}

// Every table is projected, with or without a message; views are not.
func TestReadOnlyGrantTables_EveryBaseTableAndNoView(t *testing.T) {
	view := fixtureTable("account_summaries", "id", "api_token")
	view.IsView = true
	tables := []schemadef.Table{fixtureTable("notes", "id", "body"), view}
	got := ReadOnlyGrantTables(tables, nil)
	if len(got) != 1 || got[0].Name != "notes" || secretColumns(got[0]) != "" {
		t.Errorf("got %+v, want only the base table notes, nothing secret", got)
	}
}

func TestReadOnlyGrantTables_DefaultsTheSchemaAndSorts(t *testing.T) {
	a, b := fixtureTable("b_table", "id"), fixtureTable("a_table", "id")
	a.Schema, b.Schema = "", "billing"
	got := ReadOnlyGrantTables([]schemadef.Table{a, b}, nil)
	if len(got) != 2 || got[0].Schema != "billing" || got[1].Schema != "public" {
		t.Errorf("got %+v, want billing.a_table then public.b_table", got)
	}
}

// The rendered file never lists a secret column in a GRANT, and lists the rest.
func TestGenerateReadOnlyGrants_RenderedScriptWithholdsTheSecret(t *testing.T) {
	tables := []schemadef.Table{fixtureTable("accounts", "id", "email", "api_token")}
	svc := secretService("Account", []string{"GetAccount"}, secretFields("api_token")...)
	script, err := dbgrants.Render(ReadOnlyGrantTables(tables, []ServiceDef{svc}))
	if err != nil {
		t.Fatal(err)
	}
	grantLine := ""
	for _, l := range strings.Split(script, "\n") {
		if strings.Contains(l, "'accounts'") {
			grantLine = l
		}
	}
	if !strings.Contains(grantLine, "'email'") || !strings.Contains(grantLine, "'id'") {
		t.Errorf("accounts row = %q, want id and email", grantLine)
	}
	if strings.Contains(grantLine, "api_token") {
		t.Errorf("the secret column is in the grant: %q", grantLine)
	}
}

// With no migrations there is nothing to project and nothing is written.
func TestGenerateReadOnlyGrants_NoMigrationsWritesNothing(t *testing.T) {
	dir := t.TempDir()
	wrote, err := GenerateReadOnlyGrants(dir, nil, nil)
	if err != nil || wrote {
		t.Fatalf("wrote=%v err=%v, want neither", wrote, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "db", "grants")); !os.IsNotExist(err) {
		t.Errorf("db/grants exists with no migrations: %v", err)
	}
}

// The embed names the grants file only once it exists: `//go:embed` does not
// compile against an empty match, and the embed is rewritten when it appears.
func TestGenerateMigrateEmbedsTheGrantsOnlyWhenTheyExist(t *testing.T) {
	read := func(dir string) string {
		b, err := os.ReadFile(filepath.Join(dir, "db", "embed_gen.go"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	dir := t.TempDir()
	if err := GenerateMigrate(dir, "example.com/proj", true, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(dir); strings.Contains(got, "grants") {
		t.Errorf("embed names grants that do not exist:\n%s", got)
	}

	grants := filepath.Join(dir, filepath.FromSlash(dbgrants.GrantsPath))
	if err := os.MkdirAll(filepath.Dir(grants), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grants, []byte("-- grants\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := GenerateMigrate(dir, "example.com/proj", true, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(dir); !strings.Contains(got, "//go:embed migrations/*.sql grants\n") {
		t.Errorf("embed does not include the grants:\n%s", got)
	}
}

// ---- the dashboard role's projection ----

func dashTable(name, comment string, cols map[string]string, order ...string) schemadef.Table {
	t := schemadef.Table{Name: name, Schema: "public", PKCols: []string{"id"}, Comment: comment}
	for _, n := range order {
		c := col(n)
		c.Comment = cols[n]
		t.Columns = append(t.Columns, c)
	}
	return t
}

func dashColumns(t *testing.T, script, table string) string {
	t.Helper()
	for _, l := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimPrefix(strings.TrimSpace(l), ", "), "('public', '"+table+"'") {
			return l
		}
	}
	t.Fatalf("no row for %s in:\n%s", table, script)
	return ""
}

// Marked tables, unmarked tables, a secret column, a no-dashboard column and a
// view, in one fixture: the dashboard script grants the marked table's columns
// minus the secret and no-dashboard ones, and nothing of anything else.
func TestDashboardGrants_Projection(t *testing.T) {
	usage := dashTable("usage_events", "forge:append-only forge:dashboard", map[string]string{
		"api_token":  "forge:secret",
		"user_email": "forge:no-dashboard",
		"prompt":     "forge:readable — free text, but forge_ro may read it; forge:no-dashboard",
	}, "id", "model", "cost", "user_email", "api_token", "prompt")
	users := dashTable("users", "", nil, "id", "email")
	view := dashTable("usage_rollup", "forge:dashboard", nil, "id", "model")
	view.IsView = true
	tables := []schemadef.Table{usage, users, view}

	grant := ReadOnlyGrantTables(tables, nil)
	if len(grant) != 2 {
		t.Fatalf("%d grant tables, want the 2 base tables (a view is never granted)", len(grant))
	}
	ut := grantTableByName(t, grant, "usage_events")
	if !ut.Dashboard || grantTableByName(t, grant, "users").Dashboard {
		t.Errorf("Dashboard flags: usage_events=%v users=%v", ut.Dashboard, grantTableByName(t, grant, "users").Dashboard)
	}

	script, err := dbgrants.RenderDashboard(grant)
	if err != nil {
		t.Fatal(err)
	}
	row := dashColumns(t, script, "usage_events")
	for _, want := range []string{"'id'", "'model'", "'cost'"} {
		if !strings.Contains(row, want) {
			t.Errorf("usage_events row %q lacks %s", row, want)
		}
	}
	for _, leak := range []string{"user_email", "api_token", "'prompt'"} {
		if strings.Contains(row, leak) {
			t.Errorf("usage_events row %q grants %s", row, leak)
		}
	}
	if u := dashColumns(t, script, "users"); strings.Contains(u, "'id'") || strings.Contains(u, "'email'") {
		t.Errorf("an unmarked table is granted: %q", u)
	}
	if strings.Contains(script, "usage_rollup") {
		t.Error("a view appears in the dashboard grants")
	}

	// forge_ro is unaffected by the dashboard markers.
	ro, err := dbgrants.Render(grant)
	if err != nil {
		t.Fatal(err)
	}
	if r := dashColumns(t, ro, "usage_events"); !strings.Contains(r, "'user_email'") || strings.Contains(r, "api_token") {
		t.Errorf("forge_ro's row for usage_events = %q, want user_email granted and api_token withheld", r)
	}
	if r := dashColumns(t, ro, "users"); !strings.Contains(r, "'email'") {
		t.Errorf("forge_ro lost the unmarked users table: %q", r)
	}
}

// A credential-shaped column nobody decided stays withheld from the dashboard
// role, marked table or not, and forge:readable grants it to both roles.
func TestDashboardGrants_CredentialShapedColumnsFollowTheReadOnlyDecision(t *testing.T) {
	tb := dashTable("keys", "forge:dashboard", map[string]string{"key_hash": "forge:readable — hash of the prompt"},
		"id", "access_token", "key_hash", "label")
	script, err := dbgrants.RenderDashboard(ReadOnlyGrantTables([]schemadef.Table{tb}, nil))
	if err != nil {
		t.Fatal(err)
	}
	row := dashColumns(t, script, "keys")
	if strings.Contains(row, "access_token") || !strings.Contains(row, "'key_hash'") || !strings.Contains(row, "'label'") {
		t.Errorf("row = %q, want key_hash and label but not the undecided access_token", row)
	}
}

// The generated file carries the generated header and the role it grants.
func TestDashboardGrantsFile_Header(t *testing.T) {
	script, err := dbgrants.RenderDashboard(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(DashboardGrantsFile(script))
	for _, want := range []string{"Code generated by forge. DO NOT EDIT.", "forge_dash", "forge:dashboard", "forge:no-dashboard"} {
		if !strings.Contains(got, want) {
			t.Errorf("header lacks %q:\n%s", want, got)
		}
	}
}
