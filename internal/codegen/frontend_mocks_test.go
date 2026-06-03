package codegen

import (
	"strings"
	"testing"
)

// TestProtoTypeToTSType_BigInt locks the protobuf-es v2 default mapping
// for 64-bit integer scalars to TypeScript `bigint` (not `number`).
func TestProtoTypeToTSType_BigInt(t *testing.T) {
	tests := []struct {
		proto string
		want  string
	}{
		{"int32", "number"},
		{"uint32", "number"},
		{"sint32", "number"},
		{"fixed32", "number"},
		{"sfixed32", "number"},
		{"int64", "bigint"},
		{"uint64", "bigint"},
		{"sint64", "bigint"},
		{"fixed64", "bigint"},
		{"sfixed64", "bigint"},
		{"float", "number"},
		{"double", "number"},
		{"bool", "boolean"},
		{"string", "string"},
		{"enum", "number"},
		{"message", "object"},
	}
	for _, tc := range tests {
		got := protoTypeToTSType(tc.proto)
		if got != tc.want {
			t.Errorf("protoTypeToTSType(%q) = %q, want %q", tc.proto, got, tc.want)
		}
	}
}

// TestMockGenerateValue_BigIntInteger verifies 64-bit ints emit bigint literal.
func TestMockGenerateValue_BigIntInteger(t *testing.T) {
	for _, tc := range []struct{ col, protoType string }{
		{"count", "int64"},
		{"quantity", "uint64"},
		{"amount", "sint64"},
		{"price", "fixed64"},
		{"position", "sfixed64"},
	} {
		f := EntityField{Name: tc.col, ProtoType: tc.protoType}
		got := mockGenerateValue("things", f, 0)
		if !strings.HasSuffix(got, "n") {
			t.Errorf("mockGenerateValue(%s, %s) = %q, want bigint suffix `n`", tc.col, tc.protoType, got)
		}
		if strings.HasPrefix(got, "\"") {
			t.Errorf("mockGenerateValue(%s, %s) = %q, expected bigint literal not string", tc.col, tc.protoType, got)
		}
	}
}

// TestMockGenerateValue_BigIntID verifies bigint-typed primary key emits BigInt("...").
func TestMockGenerateValue_BigIntID(t *testing.T) {
	f := EntityField{Name: "id", ProtoType: "int64"}
	got := mockGenerateValue("trades", f, 0)
	if !strings.HasPrefix(got, "BigInt(") {
		t.Errorf("bigint id: got %q, expected BigInt(...) wrapper", got)
	}
}

// TestMockGenerateValue_StringIDUnchanged guards the UUID-string id path.
func TestMockGenerateValue_StringIDUnchanged(t *testing.T) {
	f := EntityField{Name: "id", ProtoType: "string"}
	got := mockGenerateValue("trades", f, 0)
	if !strings.HasPrefix(got, "\"") {
		t.Errorf("string id: got %q, expected quoted UUID literal", got)
	}
	if strings.Contains(got, "BigInt") {
		t.Errorf("string id: got %q, must not wrap in BigInt", got)
	}
}

// TestMockGenerateValue_BigIntForeignKey verifies *_id FK fields emit BigInt(...) when bigint-typed.
func TestMockGenerateValue_BigIntForeignKey(t *testing.T) {
	f := EntityField{Name: "trader_id", ProtoType: "uint64"}
	got := mockGenerateValue("orders", f, 3)
	if !strings.HasPrefix(got, "BigInt(") {
		t.Errorf("bigint fk: got %q, expected BigInt(...) wrapper", got)
	}
}

// TestMockGenerateValue_Int32StillNumber guards 32-bit ints stay plain number literals.
func TestMockGenerateValue_Int32StillNumber(t *testing.T) {
	f := EntityField{Name: "count", ProtoType: "int32"}
	got := mockGenerateValue("things", f, 0)
	if strings.HasSuffix(got, "n") {
		t.Errorf("int32 count: got %q, must not have bigint `n` suffix", got)
	}
}
