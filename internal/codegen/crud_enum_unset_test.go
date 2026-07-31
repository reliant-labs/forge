package codegen

import (
	"strings"
	"testing"
)

// crud_enum_unset_test.go — the write half of the "the proto3 zero sentinel
// is not a storable value" decision.
//
// Birth writes an enum column as
//
//	status TEXT NOT NULL DEFAULT 'ORDER_STATUS_OPEN' CHECK (status IN (...))
//
// and the CHECK does not admit the zero sentinel (see
// internal/scaffold/entityproto_enum_sentinel_test.go). A DEFAULT only fires
// for an INSERT that OMITS the column, so a create op that unconditionally
// writes `e.Status = req.Status.String()` guarantees the DEFAULT can NEVER
// fire: every INSERT names the column, and an unset request names it with
// the one value the column refuses.
//
// proto3 gives a plain enum no presence — an unset field and a field set to
// the zero are identical on the wire — so "the caller did not say" is the
// ONLY thing the zero can mean, and what an unsaid value stores is what the
// schema's DEFAULT declares. These tests pin that the generated write path
// says so in code you can read, with no ORM tag magic (the reasoning
// schemaDefaultAssigns already documents for columns the request never
// carried; this is the same answer for columns it carries unset).

// enumUnsetEntity builds an entity with one enum wire field over one TEXT
// column, so a test can vary only the column's nullability and DEFAULT.
func enumUnsetEntity(col EntityColumn) (ServiceDef, EntityDef) {
	svc := ServiceDef{
		Package: "orders.v1",
		Schemas: map[string][]SchemaFieldDef{
			"orders.v1.Order": {{Name: "status", Kind: "enum", TypeName: "orders.v1.OrderStatus"}},
		},
	}
	entity := EntityDef{
		Name: "Order",
		Fields: []EntityField{{
			Name: "status", GoName: "Status", ProtoType: "enum", GoType: "OrderStatus",
			Kind: FieldKindEnum, MessageType: "orders.v1.OrderStatus",
		}},
		Columns: []EntityColumn{col},
	}
	return svc, entity
}

func TestEnumWrite_UnsetStoresTheColumnDefault(t *testing.T) {
	svc, entity := enumUnsetEntity(EntityColumn{
		Name: "status", Type: "string", NotNull: true, DeclType: "TEXT",
		Default: "'ORDER_STATUS_OPEN'::text",
	})
	conv, unmapped := BuildEntityConv(svc, entity)
	if len(unmapped) != 0 {
		t.Fatalf("enum over a defaulted TEXT column must map: %+v", unmapped)
	}
	from := strings.Join(conv.FromProtoAssigns, "\n")

	// The DEFAULT is the value an unset enum stores...
	if !strings.Contains(from, `e.Status = "ORDER_STATUS_OPEN"`) {
		t.Errorf("an unset enum must store the column DEFAULT; got:\n%s", from)
	}
	// ...and a value the caller DID send still wins.
	if !strings.Contains(from, "if m.Status != 0 {\n\t\te.Status = m.Status.String()\n\t}") {
		t.Errorf("a set enum must still store its own value name; got:\n%s", from)
	}
	// The unguarded form — the assignment as the block's own first
	// statement — is what made the DEFAULT unreachable.
	if strings.HasPrefix(from, "e.Status = m.Status.String()") {
		t.Errorf("unguarded assignment writes the sentinel on an unset enum, so the DEFAULT can never fire:\n%s", from)
	}
}

func TestEnumWrite_UnsetOnACreateRequestStoresTheColumnDefault(t *testing.T) {
	svc, entity := enumUnsetEntity(EntityColumn{
		Name: "status", Type: "string", NotNull: true, DeclType: "TEXT",
		Default: "'ORDER_STATUS_OPEN'::text",
	})
	svc.Schemas["orders.v1.CreateOrderRequest"] = []SchemaFieldDef{
		{Name: "status", Kind: "enum", TypeName: "orders.v1.OrderStatus"},
	}
	m := Method{Name: "CreateOrder", InputType: "CreateOrderRequest", InputTypeFQ: "orders.v1.CreateOrderRequest"}

	assigns, unmapped := buildCreateAssigns(svc, m, entity)
	if len(unmapped) != 0 {
		t.Fatalf("enum over a defaulted TEXT column must map: %+v", unmapped)
	}
	got := strings.Join(assigns, "\n")
	if !strings.Contains(got, `e.Status = "ORDER_STATUS_OPEN"`) ||
		!strings.Contains(got, "if req.Status != 0 {\n\t\te.Status = req.Status.String()\n\t}") {
		t.Errorf("create op must let an unset enum land the column DEFAULT; got:\n%s", got)
	}
}

// A nullable enum column already spells "unset" as NULL, so there is nothing
// to copy from a DEFAULT — the zero simply leaves the column alone.
func TestEnumWrite_UnsetOnANullableColumnStoresNull(t *testing.T) {
	svc, entity := enumUnsetEntity(EntityColumn{
		Name: "status", Type: "string", DeclType: "TEXT",
	})
	conv, unmapped := BuildEntityConv(svc, entity)
	if len(unmapped) != 0 {
		t.Fatalf("enum over a nullable TEXT column must map: %+v", unmapped)
	}
	from := strings.Join(conv.FromProtoAssigns, "\n")
	if !strings.Contains(from, "if m.Status != 0 {\n\t\tv := m.Status.String()\n\t\te.Status = &v\n\t}") {
		t.Errorf("an unset enum over a nullable column must leave it NULL; got:\n%s", from)
	}
}

// The optional (explicit-presence) wire field over a nullable column: BOTH
// "absent" and "present but zero" mean unset, and both store NULL.
func TestEnumWrite_OptionalZeroStoresNull(t *testing.T) {
	svc, entity := enumUnsetEntity(EntityColumn{
		Name: "status", Type: "string", DeclType: "TEXT",
	})
	svc.Schemas["orders.v1.Order"][0].Optional = true
	entity.Fields[0].Optional = true

	conv, unmapped := BuildEntityConv(svc, entity)
	if len(unmapped) != 0 {
		t.Fatalf("optional enum over a nullable TEXT column must map: %+v", unmapped)
	}
	from := strings.Join(conv.FromProtoAssigns, "\n")
	if !strings.Contains(from, "if m.Status != nil && *m.Status != 0 {") {
		t.Errorf("an explicitly-zero optional enum must store NULL, not the sentinel name; got:\n%s", from)
	}
}

// A column with no DEFAULT has nothing to say about what unset means, so the
// generator invents nothing: the value goes to the database as sent and the
// CHECK rejects the sentinel loudly. Guessing here would be forge deciding a
// domain question the schema declined to answer.
func TestEnumWrite_NoDefaultLeavesTheWriteAlone(t *testing.T) {
	svc, entity := enumUnsetEntity(EntityColumn{
		Name: "status", Type: "string", NotNull: true, DeclType: "TEXT",
	})
	conv, _ := BuildEntityConv(svc, entity)
	from := strings.Join(conv.FromProtoAssigns, "\n")
	if from != "e.Status = m.Status.String()" {
		t.Errorf("with no column DEFAULT the write path is unchanged; got:\n%s", from)
	}
}
