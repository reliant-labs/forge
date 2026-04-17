package codegen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEntityProtos_BasicEntity(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "proto", "db", "v1")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	proto := `syntax = "proto3";
package db.v1;
option go_package = "example.com/test/gen/db/v1;dbv1";

message Patient {
  int64 id = 1;
  string name = 2;
  string email = 3;
  int64 doctor_id = 4;
}
`
	if err := os.WriteFile(filepath.Join(dbDir, "entities.proto"), []byte(proto), 0o644); err != nil {
		t.Fatal(err)
	}

	entities, err := ParseEntityProtos(dir)
	if err != nil {
		t.Fatalf("ParseEntityProtos() error = %v", err)
	}

	if len(entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(entities))
	}

	ent := entities[0]
	if ent.Name != "Patient" {
		t.Errorf("expected name Patient, got %s", ent.Name)
	}
	if ent.TableName != "patients" {
		t.Errorf("expected table patients, got %s", ent.TableName)
	}
	if ent.PkField != "id" {
		t.Errorf("expected pk field id, got %s", ent.PkField)
	}
	if ent.PkGoType != "int64" {
		t.Errorf("expected pk go type int64, got %s", ent.PkGoType)
	}
	if len(ent.Fields) != 4 {
		t.Fatalf("expected 4 fields, got %d", len(ent.Fields))
	}

	// Check field details
	idField := ent.Fields[0]
	if idField.Name != "id" || idField.GoName != "ID" || idField.GoType != "int64" {
		t.Errorf("id field: got name=%s goName=%s goType=%s", idField.Name, idField.GoName, idField.GoType)
	}

	nameField := ent.Fields[1]
	if nameField.Name != "name" || nameField.GoName != "Name" || nameField.GoType != "string" {
		t.Errorf("name field: got name=%s goName=%s goType=%s", nameField.Name, nameField.GoName, nameField.GoType)
	}

	doctorField := ent.Fields[3]
	if !doctorField.IsFK {
		t.Error("expected doctor_id to be FK")
	}
	if doctorField.FKTable != "doctors" {
		t.Errorf("expected FK table doctors, got %s", doctorField.FKTable)
	}
}

func TestParseEntityProtos_NoIdField_Skipped(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "proto", "db", "v1")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Message without "id" field should not be treated as entity
	proto := `syntax = "proto3";
package db.v1;
option go_package = "example.com/test/gen/db/v1;dbv1";

message Config {
  string key = 1;
  string value = 2;
}
`
	if err := os.WriteFile(filepath.Join(dbDir, "config.proto"), []byte(proto), 0o644); err != nil {
		t.Fatal(err)
	}

	entities, err := ParseEntityProtos(dir)
	if err != nil {
		t.Fatalf("ParseEntityProtos() error = %v", err)
	}

	if len(entities) != 0 {
		t.Errorf("expected 0 entities for message without id, got %d", len(entities))
	}
}

func TestParseEntityProtos_NoDBDir(t *testing.T) {
	dir := t.TempDir()

	entities, err := ParseEntityProtos(dir)
	if err != nil {
		t.Fatalf("ParseEntityProtos() error = %v", err)
	}

	if entities != nil {
		t.Errorf("expected nil for missing db dir, got %v", entities)
	}
}

func TestParseEntityProtos_MultipleEntities(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "proto", "db", "v1")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	proto := `syntax = "proto3";
package db.v1;
option go_package = "example.com/test/gen/db/v1;dbv1";

message Patient {
  int64 id = 1;
  string name = 2;
}

message Invoice {
  string id = 1;
  int64 patient_id = 2;
  double amount = 3;
}
`
	if err := os.WriteFile(filepath.Join(dbDir, "entities.proto"), []byte(proto), 0o644); err != nil {
		t.Fatal(err)
	}

	entities, err := ParseEntityProtos(dir)
	if err != nil {
		t.Fatalf("ParseEntityProtos() error = %v", err)
	}

	if len(entities) != 2 {
		t.Fatalf("expected 2 entities, got %d", len(entities))
	}

	// Patient
	if entities[0].Name != "Patient" || entities[0].PkGoType != "int64" {
		t.Errorf("Patient: name=%s pkType=%s", entities[0].Name, entities[0].PkGoType)
	}

	// Invoice with string PK
	if entities[1].Name != "Invoice" || entities[1].PkGoType != "string" {
		t.Errorf("Invoice: name=%s pkType=%s", entities[1].Name, entities[1].PkGoType)
	}

	// Invoice.amount should be float64
	amountField := entities[1].Fields[2]
	if amountField.GoType != "float64" {
		t.Errorf("Invoice.amount: expected float64, got %s", amountField.GoType)
	}
}

func TestToGoFieldName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"id", "ID"},
		{"patient_id", "PatientID"},
		{"name", "Name"},
		{"first_name", "FirstName"},
		{"created_at", "CreatedAt"},
		{"http_url", "HTTPURL"},
		{"api_key", "APIKey"},
	}

	for _, tt := range tests {
		got := toGoFieldName(tt.input)
		if got != tt.want {
			t.Errorf("toGoFieldName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestEntityProtoTypeToGoType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"string", "string"},
		{"int32", "int32"},
		{"int64", "int64"},
		{"uint32", "uint32"},
		{"uint64", "uint64"},
		{"bool", "bool"},
		{"float", "float32"},
		{"double", "float64"},
		{"bytes", "[]byte"},
		{"google.protobuf.Timestamp", "timestamppb.Timestamp"},
		{"UnknownType", "string"}, // default
	}

	for _, tt := range tests {
		got := entityProtoTypeToGoType(tt.input)
		if got != tt.want {
			t.Errorf("entityProtoTypeToGoType(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
