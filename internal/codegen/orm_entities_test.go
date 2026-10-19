package codegen

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/schemadef"
)

// These pin the data layer's entity set (BuildORMEntities): every applied
// table, independent of CRUD rpcs. reliant's P5b spike (F12.1) found that
// a project whose CRUD-named rpcs are all `// forge:custom`, and whose
// tables mostly have no rpc at all, got ZERO entities from `forge
// generate` — the store was gated on the wire.

func pkTable(name string, pk ...string) schemadef.Table {
	t := schemadef.Table{Name: name, PKCols: pk}
	for _, c := range pk {
		t.Columns = append(t.Columns, schemadef.Column{Name: c, Type: schemadef.CanonicalType("string"), IsPK: true, NotNull: true})
	}
	t.Columns = append(t.Columns, schemadef.Column{Name: "body", Type: schemadef.CanonicalType("string")})
	return t
}

func entityNames(es []EntityDef) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name+"="+e.TableName)
	}
	return out
}

func TestORMEntities_EveryTableIndependentOfRPCs(t *testing.T) {
	tables := []schemadef.Table{
		taskTable(),                             // every CRUD rpc forge:custom
		pkTable("message_content_blocks", "id"), // no rpc at all
		pkTable("api_keys", "id"),
	}
	allCustom := taskService("CreateTask", "GetTask", "UpdateTask", "DeleteTask", "ListTasks")

	if crud := entitiesForTables(tables, []ServiceDef{allCustom}); len(crud) != 0 {
		t.Fatalf("CRUD wiring must still key on CRUD rpcs; got %v", entityNames(crud))
	}
	got, notes, err := ormEntitiesForTables(tables, []ServiceDef{allCustom})
	if err != nil {
		t.Fatal(err)
	}
	want := "ApiKey=api_keys,MessageContentBlock=message_content_blocks,Task=tasks"
	if s := strings.Join(entityNames(got), ","); s != want {
		t.Errorf("ORM entities = %s, want %s", s, want)
	}
	if len(notes) != 0 {
		t.Errorf("unexpected notes: %v", notes)
	}
}

// A table the CRUD join claims keeps the join's entity — its wire name and
// its wire fields (forge:secret rides on them) — so the store and the CRUD
// wiring compiled against it agree.
func TestORMEntities_CRUDJoinedTableKeepsItsWireEntity(t *testing.T) {
	people := pkTable("people", "id")
	svc := ServiceDef{Name: "PersonService", Package: "app.v1", Methods: []Method{{Name: "GetPerson"}},
		Messages: map[string][]MessageFieldDef{"Person": {{Name: "id", ProtoType: "string"}}}}
	got, _, err := ormEntitiesForTables([]schemadef.Table{people}, []ServiceDef{svc})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Person" || len(got[0].Fields) != 1 {
		t.Fatalf("got %+v, want the CRUD join's Person entity with its wire field", got)
	}
}

func TestORMEntities_OptOutsAndUnkeyedTables(t *testing.T) {
	optedOut := pkTable("goose_db_version", "id")
	optedOut.Comment = "owned by goose — forge:no-entity"
	tables := []schemadef.Table{
		optedOut,
		pkTable("message_order_counters", "chat_id", "kind"),
		{Name: "audit_events", Columns: []schemadef.Column{{Name: "body", Type: schemadef.CanonicalType("string")}}},
		pkTable("threads", "id"),
	}
	got, notes, err := ormEntitiesForTables(tables, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := strings.Join(entityNames(got), ","); s != "Thread=threads" {
		t.Errorf("ORM entities = %s, want only Thread=threads", s)
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"message_order_counters has a composite primary key", "audit_events has no primary key"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes do not say %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "goose_db_version") {
		t.Errorf("an opted-out table is not a gap worth a note:\n%s", joined)
	}
}

// forge:no-entity removes the table from the CRUD join as well: CRUD wiring
// over a store that is not generated could not compile.
func TestORMEntities_NoEntityAlsoLeavesTheCRUDJoin(t *testing.T) {
	tasks := taskTable()
	tasks.Comment = "forge:no-entity"
	if got := entitiesForTables([]schemadef.Table{tasks}, []ServiceDef{taskService()}); len(got) != 0 {
		t.Fatalf("a forge:no-entity table must not be a CRUD entity; got %v", entityNames(got))
	}
}

func TestORMEntities_TwoTablesOneNameIsAnError(t *testing.T) {
	_, _, err := ormEntitiesForTables([]schemadef.Table{pkTable("person", "id"), pkTable("persons", "id")}, nil)
	if err == nil || !strings.Contains(err.Error(), "person and persons both project to the entity Person") {
		t.Fatalf("err = %v", err)
	}
}

func TestTableEntityName(t *testing.T) {
	for table, want := range map[string]string{
		"message_content_blocks":    "MessageContentBlock",
		"api_keys":                  "ApiKey",
		"provider_backoff":          "ProviderBackoff",
		"daemon_attachment":         "DaemonAttachment",
		"integration_access":        "IntegrationAccess",
		"statuses":                  "Status",
		"background_process_output": "BackgroundProcessOutput",
	} {
		if got := TableEntityName(table); got != want {
			t.Errorf("TableEntityName(%q) = %q, want %q", table, got, want)
		}
	}
}

// Views are opt-in read-only entities (F12.7): `forge:entity` on the view,
// keyed by `key=<column>`, else `id`, else keyless; never a CRUD entity.
func TestORMEntities_OptedInViewsAreReadOnly(t *testing.T) {
	view := func(name, comment string, cols ...string) schemadef.Table {
		v := schemadef.Table{Name: name, Comment: comment, IsView: true}
		for _, c := range cols {
			v.Columns = append(v.Columns, schemadef.Column{Name: c, Type: schemadef.CanonicalType("string")})
		}
		return v
	}
	tables := []schemadef.Table{
		view("chats_with_activity", "forge:entity", "id", "title"),
		view("ledger_rows", "forge:entity key=entry_id", "entry_id", "amount"),
		view("totals", "forge:entity", "n"),
		view("report_rows", "a reporting view", "x"),
	}
	got, _, err := ormEntitiesForTables(tables, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := strings.Join(entityNames(got), ","); s != "ChatsWithActivity=chats_with_activity,LedgerRow=ledger_rows,Total=totals" {
		t.Fatalf("view entities = %s", s)
	}
	keys := map[string]string{}
	for _, e := range got {
		if !e.ReadOnly {
			t.Errorf("%s: a view entity must be ReadOnly", e.Name)
		}
		keys[e.Name] = e.PkField
	}
	if keys["ChatsWithActivity"] != "id" || keys["LedgerRow"] != "entry_id" || keys["Total"] != "" {
		t.Errorf("view keys = %v, want id / entry_id (declared) / none", keys)
	}
}
