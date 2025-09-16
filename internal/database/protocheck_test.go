package database

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompareSchemaToEntities_Clean(t *testing.T) {
	tables := []Table{
		{
			Name: "users",
			Columns: []Column{
				{Name: "id", Type: "uuid", UDTName: "uuid", Nullable: false, IsPrimary: true},
				{Name: "name", Type: "text", UDTName: "text", Nullable: false},
				{Name: "email", Type: "character varying", UDTName: "varchar", Nullable: true},
			},
		},
	}

	entities := []ProtoEntity{
		{
			MessageName: "User",
			TableName:   "users",
			Fields: []ProtoEntityField{
				{Name: "id", ProtoType: "string", IsPrimary: true, NotNull: true},
				{Name: "name", ProtoType: "string", NotNull: true},
				{Name: "email", ProtoType: "string"},
			},
		},
	}

	result := CompareSchemaToEntities(tables, entities)

	if !result.IsClean() {
		t.Errorf("expected clean result, got %d diffs:\n%s", len(result.Diffs), result.FormatText())
	}
	if result.Checked != 1 {
		t.Errorf("Checked = %d, want 1", result.Checked)
	}
}

func TestCompareSchemaToEntities_MissingTable(t *testing.T) {
	tables := []Table{
		{Name: "users", Columns: []Column{{Name: "id", Type: "uuid", UDTName: "uuid"}}},
		{Name: "orders", Columns: []Column{{Name: "id", Type: "integer", UDTName: "int4"}}},
	}

	entities := []ProtoEntity{
		{MessageName: "User", TableName: "users", Fields: []ProtoEntityField{
			{Name: "id", ProtoType: "string"},
		}},
		// orders is missing from proto
	}

	result := CompareSchemaToEntities(tables, entities)

	if result.IsClean() {
		t.Fatal("expected diffs for missing table")
	}

	found := false
	for _, d := range result.Diffs {
		if d.Kind == "missing_table" && d.Table == "orders" {
			found = true
		}
	}
	if !found {
		t.Error("expected missing_table diff for 'orders'")
	}
}

func TestCompareSchemaToEntities_MissingColumn(t *testing.T) {
	tables := []Table{
		{
			Name: "users",
			Columns: []Column{
				{Name: "id", Type: "uuid", UDTName: "uuid", Nullable: false},
				{Name: "name", Type: "text", UDTName: "text", Nullable: false},
				{Name: "age", Type: "integer", UDTName: "int4", Nullable: true},
			},
		},
	}

	entities := []ProtoEntity{
		{MessageName: "User", TableName: "users", Fields: []ProtoEntityField{
			{Name: "id", ProtoType: "string", NotNull: true},
			{Name: "name", ProtoType: "string", NotNull: true},
			// age is missing from proto
		}},
	}

	result := CompareSchemaToEntities(tables, entities)

	found := false
	for _, d := range result.Diffs {
		if d.Kind == "missing_column" && d.Column == "age" {
			found = true
		}
	}
	if !found {
		t.Error("expected missing_column diff for 'age'")
	}
}

func TestCompareSchemaToEntities_ExtraColumn(t *testing.T) {
	tables := []Table{
		{
			Name: "users",
			Columns: []Column{
				{Name: "id", Type: "uuid", UDTName: "uuid"},
			},
		},
	}

	entities := []ProtoEntity{
		{MessageName: "User", TableName: "users", Fields: []ProtoEntityField{
			{Name: "id", ProtoType: "string"},
			{Name: "extra_field", ProtoType: "string"},
		}},
	}

	result := CompareSchemaToEntities(tables, entities)

	found := false
	for _, d := range result.Diffs {
		if d.Kind == "extra_column" && d.Column == "extra_field" {
			found = true
		}
	}
	if !found {
		t.Error("expected extra_column diff for 'extra_field'")
	}
}

func TestCompareSchemaToEntities_TypeMismatch(t *testing.T) {
	tables := []Table{
		{
			Name: "users",
			Columns: []Column{
				{Name: "id", Type: "uuid", UDTName: "uuid"},
				{Name: "count", Type: "integer", UDTName: "int4"},
			},
		},
	}

	entities := []ProtoEntity{
		{MessageName: "User", TableName: "users", Fields: []ProtoEntityField{
			{Name: "id", ProtoType: "string"},
			{Name: "count", ProtoType: "string"}, // Wrong: should be int32
		}},
	}

	result := CompareSchemaToEntities(tables, entities)

	found := false
	for _, d := range result.Diffs {
		if d.Kind == "type_mismatch" && d.Column == "count" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected type_mismatch diff for 'count', got diffs: %+v", result.Diffs)
	}
}

func TestCompareSchemaToEntities_NullableMismatch(t *testing.T) {
	tables := []Table{
		{
			Name: "users",
			Columns: []Column{
				{Name: "id", Type: "uuid", UDTName: "uuid", Nullable: false},
				{Name: "name", Type: "text", UDTName: "text", Nullable: false},
			},
		},
	}

	entities := []ProtoEntity{
		{MessageName: "User", TableName: "users", Fields: []ProtoEntityField{
			{Name: "id", ProtoType: "string", NotNull: true},
			{Name: "name", ProtoType: "string", NotNull: false}, // Mismatch: DB says NOT NULL
		}},
	}

	result := CompareSchemaToEntities(tables, entities)

	found := false
	for _, d := range result.Diffs {
		if d.Kind == "nullable_mismatch" && d.Column == "name" {
			found = true
		}
	}
	if !found {
		t.Error("expected nullable_mismatch diff for 'name'")
	}
}

func TestCheckResultFormatText_Clean(t *testing.T) {
	r := CheckResult{Checked: 3}
	text := r.FormatText()
	if text != "All 3 table(s) match their proto definitions.\n" {
		t.Errorf("unexpected clean text: %q", text)
	}
}

func TestCheckResultFormatText_WithDiffs(t *testing.T) {
	r := CheckResult{
		Checked: 2,
		Diffs: []Diff{
			{Table: "orders", Kind: "missing_table", Detail: "table \"orders\" exists in DB but has no proto entity"},
			{Table: "users", Kind: "type_mismatch", Column: "age", Detail: "DB type int maps to int32 but proto has string"},
		},
	}

	text := r.FormatText()
	if !containsAll(text, "[MISSING TABLE]", "[TYPE MISMATCH]", "orders", "users", "age") {
		t.Errorf("unexpected format:\n%s", text)
	}
}

func TestParseProtoEntities(t *testing.T) {
	// Write a test proto file.
	dir := t.TempDir()
	protoContent := `syntax = "proto3";

package db.v1;

import "google/protobuf/timestamp.proto";
import "forge/options/v1/entity.proto";
import "forge/options/v1/field.proto";

option go_package = "github.com/example/gen/db/v1;dbv1";

message Account {
  option (forge.options.v1.entity_options) = {
    table_name: "accounts"
    soft_delete: true
  };

  string id = 1 [(forge.options.v1.field_options) = {primary_key: true, not_null: true}];
  string name = 2 [(forge.options.v1.field_options) = {not_null: true}];
  string domain = 3;
  google.protobuf.Timestamp created_at = 10;
}

// A message without entity_options should be ignored.
message HelperMessage {
  string value = 1;
}
`
	if err := os.WriteFile(filepath.Join(dir, "test.proto"), []byte(protoContent), 0644); err != nil {
		t.Fatalf("writing test proto: %v", err)
	}

	entities, err := ParseProtoEntities(dir)
	if err != nil {
		t.Fatalf("ParseProtoEntities error: %v", err)
	}

	if len(entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(entities))
	}

	e := entities[0]
	if e.MessageName != "Account" {
		t.Errorf("MessageName = %q, want %q", e.MessageName, "Account")
	}
	if e.TableName != "accounts" {
		t.Errorf("TableName = %q, want %q", e.TableName, "accounts")
	}
	if len(e.Fields) != 4 {
		t.Fatalf("expected 4 fields, got %d: %+v", len(e.Fields), e.Fields)
	}

	// Check field parsing.
	idField := e.Fields[0]
	if idField.Name != "id" || idField.ProtoType != "string" || !idField.IsPrimary || !idField.NotNull {
		t.Errorf("id field: %+v", idField)
	}

	nameField := e.Fields[1]
	if nameField.Name != "name" || !nameField.NotNull || nameField.IsPrimary {
		t.Errorf("name field: %+v", nameField)
	}

	domainField := e.Fields[2]
	if domainField.Name != "domain" || domainField.NotNull {
		t.Errorf("domain field: %+v", domainField)
	}

	createdField := e.Fields[3]
	if createdField.Name != "created_at" || createdField.ProtoType != "google.protobuf.Timestamp" {
		t.Errorf("created_at field: %+v", createdField)
	}
}

func TestProtoTypesCompatible(t *testing.T) {
	tests := []struct {
		expected string
		actual   string
		want     bool
	}{
		{"string", "string", true},
		{"int32", "int32", true},
		{"int32", "string", false},
		{"google.protobuf.Timestamp", "google.protobuf.Timestamp", true},
		{"google.protobuf.Timestamp", "Timestamp", true},
		{"Timestamp", "google.protobuf.Timestamp", true},
	}

	for _, tt := range tests {
		t.Run(tt.expected+"_vs_"+tt.actual, func(t *testing.T) {
			got := protoTypesCompatible(tt.expected, tt.actual)
			if got != tt.want {
				t.Errorf("protoTypesCompatible(%q, %q) = %v, want %v", tt.expected, tt.actual, got, tt.want)
			}
		})
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && containsStr(s, sub)
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
