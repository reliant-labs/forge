package database

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLToProtoType(t *testing.T) {
	tests := []struct {
		sqlType        string
		udtName        string
		wantProto      string
		wantTimestamp  bool
	}{
		{"integer", "int4", "int32", false},
		{"smallint", "int2", "int32", false},
		{"bigint", "int8", "int64", false},
		{"real", "float4", "float", false},
		{"double precision", "float8", "double", false},
		{"boolean", "bool", "bool", false},
		{"text", "text", "string", false},
		{"character varying", "varchar", "string", false},
		{"character", "bpchar", "string", false},
		{"uuid", "uuid", "string", false},
		{"bytea", "bytea", "bytes", false},
		{"numeric", "numeric", "string", false},
		{"json", "json", "string", false},
		{"jsonb", "jsonb", "string", false},
		{"timestamp without time zone", "timestamp", "google.protobuf.Timestamp", true},
		{"timestamp with time zone", "timestamptz", "google.protobuf.Timestamp", true},
		{"date", "date", "string", false},
		{"user-defined", "uuid", "string", false},
		{"user-defined", "jsonb", "string", false},
		// Array types
		{"array", "_int4", "repeated int32", false},
		{"array", "_text", "repeated string", false},
		// UDT fallbacks
		{"user-defined", "int4", "int32", false},
		{"user-defined", "bool", "bool", false},
		// Unknown type defaults to string
		{"unknown_custom_type", "unknown_custom_type", "string", false},
	}

	for _, tt := range tests {
		t.Run(tt.sqlType+"_"+tt.udtName, func(t *testing.T) {
			gotProto, gotTs := SQLToProtoType(tt.sqlType, tt.udtName)
			if gotProto != tt.wantProto {
				t.Errorf("SQLToProtoType(%q, %q) proto = %q, want %q", tt.sqlType, tt.udtName, gotProto, tt.wantProto)
			}
			if gotTs != tt.wantTimestamp {
				t.Errorf("SQLToProtoType(%q, %q) needsTimestamp = %v, want %v", tt.sqlType, tt.udtName, gotTs, tt.wantTimestamp)
			}
		})
	}
}

func TestTableNameToMessageName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"users", "User"},
		{"user_accounts", "UserAccount"},
		{"order_items", "OrderItem"},
		{"categories", "Category"},
		{"addresses", "Address"},
		{"statuses", "Status"},
		{"schema_migrations", "SchemaMigration"},
		{"bus", "Bus"},
		{"access", "Access"},
		{"matrices", "Matrix"},
		{"analyses", "Analysis"},
		{"people", "Person"},
		{"data", "Datum"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := tableNameToMessageName(tt.input)
			if got != tt.want {
				t.Errorf("tableNameToMessageName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTableToProtoMessage(t *testing.T) {
	table := Table{
		Name:       "users",
		Schema:     "public",
		PrimaryKey: []string{"id"},
		Columns: []Column{
			{Name: "id", Type: "uuid", UDTName: "uuid", Nullable: false, IsPrimary: true},
			{Name: "name", Type: "text", UDTName: "text", Nullable: false},
			{Name: "email", Type: "character varying", UDTName: "varchar", Nullable: true},
			{Name: "created_at", Type: "timestamp with time zone", UDTName: "timestamptz", Nullable: false},
			{Name: "updated_at", Type: "timestamp with time zone", UDTName: "timestamptz", Nullable: false},
			{Name: "deleted_at", Type: "timestamp with time zone", UDTName: "timestamptz", Nullable: true},
		},
		ForeignKeys: []ForeignKey{},
	}

	msg := TableToProtoMessage(table)

	if msg.MessageName != "User" {
		t.Errorf("MessageName = %q, want %q", msg.MessageName, "User")
	}
	if msg.TableName != "users" {
		t.Errorf("TableName = %q, want %q", msg.TableName, "users")
	}
	if !msg.HasTimestamps {
		t.Error("expected HasTimestamps = true")
	}
	if !msg.HasSoftDelete {
		t.Error("expected HasSoftDelete = true")
	}
	if !msg.NeedsTimestamp {
		t.Error("expected NeedsTimestamp = true")
	}
	if len(msg.Fields) != 6 {
		t.Fatalf("expected 6 fields, got %d", len(msg.Fields))
	}

	// Check first field (primary key).
	f := msg.Fields[0]
	if f.ProtoName != "id" || f.ProtoType != "string" || !f.IsPrimary || f.Number != 1 {
		t.Errorf("field 0: got %+v", f)
	}
	if !f.HasFieldOptions() {
		t.Error("id field should have field_options")
	}
	if !strings.Contains(f.FieldOptionsString(), "primary_key: true") {
		t.Errorf("id field_options = %q, expected primary_key: true", f.FieldOptionsString())
	}

	// Check name field (not null, not primary).
	f = msg.Fields[1]
	if f.ProtoName != "name" || !f.NotNull {
		t.Errorf("field 1: got %+v", f)
	}
	if !strings.Contains(f.FieldOptionsString(), "not_null: true") {
		t.Errorf("name field_options = %q, expected not_null: true", f.FieldOptionsString())
	}

	// Check email field (nullable).
	f = msg.Fields[2]
	if f.HasFieldOptions() {
		t.Error("nullable email field should not have field_options")
	}
}

func TestTableToProtoMessageWithForeignKeys(t *testing.T) {
	table := Table{
		Name: "orders",
		Columns: []Column{
			{Name: "id", Type: "integer", UDTName: "int4", IsPrimary: true, Nullable: false},
			{Name: "user_id", Type: "integer", UDTName: "int4", Nullable: false},
		},
		ForeignKeys: []ForeignKey{
			{Name: "fk_orders_user_id", Column: "user_id", ReferencedTable: "users", ReferencedColumn: "id"},
		},
	}

	msg := TableToProtoMessage(table)
	f := msg.Fields[1] // user_id

	if f.References != "users.id" {
		t.Errorf("user_id References = %q, want %q", f.References, "users.id")
	}
	if !strings.Contains(f.FieldOptionsString(), `references: "users.id"`) {
		t.Errorf("field_options = %q, expected references", f.FieldOptionsString())
	}
}

func TestGenerateProtoFiles(t *testing.T) {
	dir := t.TempDir()

	tables := []Table{
		{
			Name:       "users",
			Schema:     "public",
			PrimaryKey: []string{"id"},
			Columns: []Column{
				{Name: "id", Type: "uuid", UDTName: "uuid", Nullable: false, IsPrimary: true},
				{Name: "name", Type: "text", UDTName: "text", Nullable: false},
				{Name: "created_at", Type: "timestamp with time zone", UDTName: "timestamptz", Nullable: false},
				{Name: "updated_at", Type: "timestamp with time zone", UDTName: "timestamptz", Nullable: false},
			},
		},
		{
			Name:   "orders",
			Schema: "public",
			Columns: []Column{
				{Name: "id", Type: "integer", UDTName: "int4", Nullable: false, IsPrimary: true},
				{Name: "total", Type: "bigint", UDTName: "int8", Nullable: false},
			},
		},
	}

	if err := GenerateProtoFiles(tables, dir, "github.com/example/project"); err != nil {
		t.Fatalf("GenerateProtoFiles error: %v", err)
	}

	// Verify users.proto.
	usersProto, err := os.ReadFile(filepath.Join(dir, "users.proto"))
	if err != nil {
		t.Fatalf("reading users.proto: %v", err)
	}
	usersContent := string(usersProto)

	for _, want := range []string{
		`syntax = "proto3";`,
		`package db.v1;`,
		`import "google/protobuf/timestamp.proto";`,
		`import "forge/options/v1/entity.proto";`,
		`import "forge/options/v1/field.proto";`,
		`option go_package = "github.com/example/project/gen/db/v1;dbv1";`,
		`message User {`,
		`table_name: "users"`,
		`timestamps: true`,
		`string id = 1`,
		`primary_key: true`,
		`string name = 2`,
		`not_null: true`,
		`google.protobuf.Timestamp created_at = 3`,
		`google.protobuf.Timestamp updated_at = 4`,
	} {
		if !strings.Contains(usersContent, want) {
			t.Errorf("users.proto missing %q\nGot:\n%s", want, usersContent)
		}
	}

	// Verify orders.proto exists and doesn't import timestamp.
	ordersProto, err := os.ReadFile(filepath.Join(dir, "orders.proto"))
	if err != nil {
		t.Fatalf("reading orders.proto: %v", err)
	}
	ordersContent := string(ordersProto)

	if strings.Contains(ordersContent, "google/protobuf/timestamp.proto") {
		t.Error("orders.proto should not import timestamp.proto")
	}
	if !strings.Contains(ordersContent, "message Order {") {
		t.Errorf("orders.proto missing 'message Order {'\nGot:\n%s", ordersContent)
	}
	if !strings.Contains(ordersContent, "int32 id = 1") {
		t.Errorf("orders.proto missing 'int32 id = 1'\nGot:\n%s", ordersContent)
	}
	if !strings.Contains(ordersContent, "int64 total = 2") {
		t.Errorf("orders.proto missing 'int64 total = 2'\nGot:\n%s", ordersContent)
	}
}

func TestProtoFieldFieldOptionsString(t *testing.T) {
	tests := []struct {
		name  string
		field ProtoField
		want  string
	}{
		{
			name:  "primary key",
			field: ProtoField{IsPrimary: true, NotNull: true},
			want:  "primary_key: true",
		},
		{
			name:  "not null only",
			field: ProtoField{NotNull: true},
			want:  "not_null: true",
		},
		{
			name:  "with references",
			field: ProtoField{NotNull: true, References: "users.id"},
			want:  `not_null: true, references: "users.id"`,
		},
		{
			name:  "primary key with reference",
			field: ProtoField{IsPrimary: true, NotNull: true, References: "users.id"},
			want:  `primary_key: true, references: "users.id"`,
		},
		{
			name:  "no options",
			field: ProtoField{},
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.field.FieldOptionsString()
			if got != tt.want {
				t.Errorf("FieldOptionsString() = %q, want %q", got, tt.want)
			}
		})
	}
}