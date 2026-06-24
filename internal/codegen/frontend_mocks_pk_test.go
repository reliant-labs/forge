package codegen

import (
	"strings"
	"testing"
)

// TestEffectiveMockProtoType_RepeatedScalar pins that a repeated-scalar entity
// field is re-encoded with a `repeated ` prefix so the mock value/type
// generators emit an array literal + `string[]` TS type. EntityField.ProtoType
// holds only the element kind for repeated scalars (the repeated-ness lives in
// Kind/GoType), so without this a `repeated string` field would mock a bare
// scalar and fail `tsc` against the protobuf-es `string[]` field.
func TestEffectiveMockProtoType_RepeatedScalar(t *testing.T) {
	repeated := EntityField{Name: "models", ProtoType: "string", GoType: "[]string", Kind: FieldKindRepeatedScalar}
	if got := effectiveMockProtoType(repeated); got != "repeated string" {
		t.Fatalf("effectiveMockProtoType(repeated string) = %q, want %q", got, "repeated string")
	}
	if ts := protoTypeToTSType(effectiveMockProtoType(repeated)); ts != "string[]" {
		t.Errorf("TS type = %q, want %q", ts, "string[]")
	}
	ef := repeated
	ef.ProtoType = effectiveMockProtoType(repeated)
	if v := mockGenerateValue("llm_keys", ef, 0); !strings.HasPrefix(v, "[") || !strings.HasSuffix(v, "]") {
		t.Errorf("mock value = %q, want a TS array literal", v)
	}

	// A plain scalar is unchanged.
	scalar := EntityField{Name: "name", ProtoType: "string", GoType: "string", Kind: FieldKindScalar}
	if got := effectiveMockProtoType(scalar); got != "string" {
		t.Errorf("effectiveMockProtoType(scalar) = %q, want %q", got, "string")
	}
}

// TestMockPkFieldCamel_ResolvesAgainstWireFields pins the contract that the
// mutable mock-store key references a field that EXISTS on the entity's wire
// message. The store maps over mock RECORDS (projections of the wire message),
// so keying by a column the proto omits fails `tsc` with "Property 'x' does
// not exist on type 'Entity'".
func TestMockPkFieldCamel_ResolvesAgainstWireFields(t *testing.T) {
	field := func(name string) EntityField { return EntityField{Name: name, ProtoType: "string"} }

	tests := []struct {
		name   string
		entity EntityDef
		want   string
	}{
		{
			name: "db pk is a wire field",
			entity: EntityDef{
				Name: "Daemon", TableName: "daemons", PkField: "id",
				Fields: []EntityField{field("id"), field("name")},
			},
			want: "id",
		},
		{
			name: "db pk absent from wire message → conventional <singular>_id",
			// usage_events: surrogate `id` PK in the table, but the published
			// UsageEvent proto omits `id` and exposes `usage_event_id`.
			entity: EntityDef{
				Name: "UsageEvent", TableName: "usage_events", PkField: "id",
				Fields: []EntityField{field("usage_event_id"), field("provider")},
			},
			want: "usageEventId",
		},
		{
			name: "no pk, no conventional key → first wire field",
			entity: EntityDef{
				Name: "Thing", TableName: "things", PkField: "",
				Fields: []EntityField{field("ref"), field("label")},
			},
			want: "ref",
		},
		{
			name: "empty entity → id fallback",
			entity: EntityDef{
				Name: "Empty", TableName: "empties", PkField: "id",
			},
			want: "id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mockPkFieldCamel(tt.entity); got != tt.want {
				t.Errorf("mockPkFieldCamel(%s) = %q, want %q", tt.entity.Name, got, tt.want)
			}
		})
	}
}
