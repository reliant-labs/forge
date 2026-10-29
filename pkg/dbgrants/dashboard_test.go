package dbgrants

import (
	"reflect"
	"strings"
	"testing"
)

func TestDashboardNames(t *testing.T) {
	if DashboardRoleName != "forge_dash" || DashboardSecretSuffix != "-forge-dash" {
		t.Errorf("role %q suffix %q", DashboardRoleName, DashboardSecretSuffix)
	}
	if got := DashboardSecretName("order-db"); got != "order-db-forge-dash" {
		t.Errorf("DashboardSecretName = %q", got)
	}
	if DashboardSecretName("x") == SecretName("x") {
		t.Error("the two roles share a Secret")
	}
}

func dashFixture() []Table {
	return []Table{
		{Schema: "public", Name: "usage_events", Dashboard: true, Columns: []Column{
			{Name: "id"},
			{Name: "model"},
			{Name: "cost_cents", Kind: KindScalar},
			{Name: "user_email", NoDashboard: true},
			{Name: "api_token", Secret: true},
			{Name: "key_hash"}, // credential-shaped, nobody decided
			{Name: "prompt_hash", Readable: true},
			{Name: "token_secret", Secret: true, NoDashboard: true},
		}},
		{Schema: "public", Name: "users", Columns: []Column{{Name: "id"}, {Name: "email"}}},
	}
}

func cols(tb Table) string {
	var out []string
	for _, c := range tb.Columns {
		out = append(out, c.Name)
	}
	return strings.Join(out, ",")
}

// The projection is: marked tables ∩ what forge_ro may read − no-dashboard.
func TestDashboardTables(t *testing.T) {
	got := DashboardTables(dashFixture())
	if len(got) != 2 {
		t.Fatalf("%d tables, want both kept (an unmarked table must stay declared so its schema is revoked)", len(got))
	}
	if g, want := cols(got[0]), "id,model,cost_cents,prompt_hash"; g != want {
		t.Errorf("usage_events columns = %s, want %s", g, want)
	}
	if got[1].Name != "users" || len(got[1].Columns) != 0 {
		t.Errorf("an unmarked table is granted columns: %+v", got[1])
	}

	// A strict subset of what forge_ro may read.
	ro := map[string]bool{}
	for _, tb := range dashFixture() {
		for _, c := range tb.Columns {
			if c.Disposition() == Granted {
				ro[tb.Name+"."+c.Name] = true
			}
		}
	}
	for _, tb := range got {
		for _, c := range tb.Columns {
			if !ro[tb.Name+"."+c.Name] {
				t.Errorf("%s.%s is granted to the dashboard role but not to forge_ro", tb.Name, c.Name)
			}
		}
	}
}

// The marker does not widen anything by itself: a table with no
// forge:no-dashboard columns gets exactly forge_ro's columns.
func TestDashboardTablesEqualReadOnlyWhenNothingIsNarrowed(t *testing.T) {
	in := []Table{{Schema: "public", Name: "t", Dashboard: true, Columns: []Column{{Name: "a"}, {Name: "b"}, {Name: "c", Secret: true}}}}
	if g := cols(DashboardTables(in)[0]); g != "a,b" {
		t.Errorf("columns = %s, want a,b", g)
	}
}

func TestRenderDashboardScript(t *testing.T) {
	script, err := RenderDashboard(dashFixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"'forge_dash'",
		"('public', 'usage_events', ARRAY['cost_cents', 'id', 'model', 'prompt_hash']::text[])",
		"('public', 'users', ARRAY[]::text[])",
		"REVOKE ALL ON ALL TABLES IN SCHEMA",
		"default_transaction_read_only",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	for _, leak := range []string{"user_email", "api_token", "key_hash", "token_secret"} {
		// Withheld columns may be NAMED in the audit comment, never in the
		// VALUES list that is data for the GRANT loop.
		for _, line := range strings.Split(script, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "(") && strings.Contains(line, "'"+leak+"'") {
				t.Errorf("%s is in the granted column list: %s", leak, line)
			}
		}
	}
	if strings.Contains(script, "forge_ro") {
		t.Error("the dashboard script names forge_ro")
	}
	// Deterministic and independent of input order.
	rev := dashFixture()
	rev[0], rev[1] = rev[1], rev[0]
	again, err := RenderDashboard(rev)
	if err != nil {
		t.Fatal(err)
	}
	if again != script {
		t.Error("the script depends on table order")
	}
}

// With no marked table the script still declares the schemas, so applying it
// REVOKEs a role whose last marker was removed.
func TestRenderDashboardWithNoMarkedTableStillRevokes(t *testing.T) {
	tables := dashFixture()
	for i := range tables {
		tables[i].Dashboard = false
	}
	script, err := RenderDashboard(tables)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "REVOKE ALL ON ALL TABLES IN SCHEMA") || !strings.Contains(script, "('public')") {
		t.Errorf("no revoke for the declared schema:\n%s", script)
	}
	if strings.Contains(script, "ARRAY['") {
		t.Errorf("a column is granted with no table marked:\n%s", script)
	}
}

// The read-only script is untouched by the dashboard additions: the same
// tables render the same bytes whatever Dashboard/NoDashboard say.
func TestReadOnlyRenderIgnoresDashboardMarkers(t *testing.T) {
	a, err := Render(dashFixture())
	if err != nil {
		t.Fatal(err)
	}
	plain := dashFixture()
	for i := range plain {
		plain[i].Dashboard = false
		for j := range plain[i].Columns {
			plain[i].Columns[j].NoDashboard = false
		}
	}
	b, err := Render(plain)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("forge_ro's grants moved with the dashboard markers")
	}
	if !strings.Contains(a, "read-only") || strings.Contains(a, "dashboard") {
		t.Errorf("the read-only script's prose changed:\n%s", a)
	}
}

func TestDashboardTablesDoesNotAliasItsInput(t *testing.T) {
	in := dashFixture()
	before := cols(in[0])
	_ = DashboardTables(in)
	if cols(in[0]) != before || !reflect.DeepEqual(in, dashFixture()) {
		t.Error("DashboardTables modified its input")
	}
}
