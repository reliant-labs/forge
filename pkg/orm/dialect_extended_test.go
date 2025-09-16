package orm

import (
	"fmt"
	"strings"
	"testing"
)

func TestPostgresDialect_Placeholder(t *testing.T) {
	d := &PostgresDialect{}
	tests := []struct {
		index    int
		expected string
	}{
		{0, "$1"},
		{1, "$2"},
		{9, "$10"},
		{99, "$100"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("index_%d", tt.index), func(t *testing.T) {
			got := d.Placeholder(tt.index)
			if got != tt.expected {
				t.Errorf("Placeholder(%d) = %q, want %q", tt.index, got, tt.expected)
			}
		})
	}
}

func TestPostgresDialect_MapFieldType(t *testing.T) {
	d := &PostgresDialect{}
	tests := []struct {
		input    FieldType
		expected string
	}{
		{TypeText, "TEXT"},
		{TypeVarchar, "VARCHAR"},
		{TypeInteger, "INTEGER"},
		{TypeBigInt, "BIGINT"},
		{TypeBoolean, "BOOLEAN"},
		{TypeTimestampTZ, "TIMESTAMPTZ"},
		{TypeJSONB, "JSONB"},
		{TypeBytea, "BYTEA"},
		{TypeSerial, "SERIAL"},
		{TypeBigSerial, "BIGSERIAL"},
	}
	for _, tt := range tests {
		t.Run(string(tt.input), func(t *testing.T) {
			got := d.MapFieldType(tt.input)
			if got != tt.expected {
				t.Errorf("MapFieldType(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestPostgresDialect_OnConflictClause(t *testing.T) {
	d := &PostgresDialect{}

	t.Run("with update columns", func(t *testing.T) {
		clause := d.OnConflictClause("id", []string{"name", "email"})
		if !strings.Contains(clause, "ON CONFLICT (id)") {
			t.Errorf("expected ON CONFLICT (id), got: %s", clause)
		}
		if !strings.Contains(clause, "DO UPDATE SET") {
			t.Errorf("expected DO UPDATE SET, got: %s", clause)
		}
		if !strings.Contains(clause, "name = EXCLUDED.name") {
			t.Errorf("expected name update, got: %s", clause)
		}
		if !strings.Contains(clause, "email = EXCLUDED.email") {
			t.Errorf("expected email update, got: %s", clause)
		}
	})

	t.Run("no update columns", func(t *testing.T) {
		clause := d.OnConflictClause("id", nil)
		if !strings.Contains(clause, "DO NOTHING") {
			t.Errorf("expected DO NOTHING for empty update columns, got: %s", clause)
		}
	})

	t.Run("empty update columns", func(t *testing.T) {
		clause := d.OnConflictClause("id", []string{})
		if !strings.Contains(clause, "DO NOTHING") {
			t.Errorf("expected DO NOTHING for empty update columns, got: %s", clause)
		}
	})
}

func TestPostgresDialect_ParseColumnType(t *testing.T) {
	d := &PostgresDialect{}
	tests := []struct {
		input    string
		expected FieldType
		wantErr  bool
	}{
		{"text", TypeText, false},
		{"character varying", TypeVarchar, false},
		{"varchar", TypeVarchar, false},
		{"integer", TypeInteger, false},
		{"int4", TypeInteger, false},
		{"bigint", TypeBigInt, false},
		{"int8", TypeBigInt, false},
		{"boolean", TypeBoolean, false},
		{"bool", TypeBoolean, false},
		{"timestamp with time zone", TypeTimestampTZ, false},
		{"timestamptz", TypeTimestampTZ, false},
		{"jsonb", TypeJSONB, false},
		{"bytea", TypeBytea, false},
		{"serial", TypeSerial, false},
		{"bigserial", TypeBigSerial, false},
		// Error cases
		{"", "", true},
		{"unknown_type", "", true},
		{"FLOAT", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := d.ParseColumnType(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseColumnType(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.expected {
				t.Errorf("ParseColumnType(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestPostgresDialect_SupportsReturning(t *testing.T) {
	d := &PostgresDialect{}
	if !d.SupportsReturning() {
		t.Error("PostgresDialect should support RETURNING")
	}
}

func TestPostgresDialect_Name(t *testing.T) {
	d := &PostgresDialect{}
	if d.Name() != "postgres" {
		t.Errorf("expected 'postgres', got %q", d.Name())
	}
}

func TestPostgresDialect_DriverName(t *testing.T) {
	d := &PostgresDialect{}
	if d.DriverName() != "postgres" {
		t.Errorf("expected 'postgres', got %q", d.DriverName())
	}
}

func TestPostgresDialect_TableExistsQuery(t *testing.T) {
	d := &PostgresDialect{}
	q := d.TableExistsQuery("users")
	if !strings.Contains(q, "users") {
		t.Errorf("expected query to contain table name, got: %s", q)
	}
	if !strings.Contains(q, "information_schema") {
		t.Errorf("expected query to use information_schema, got: %s", q)
	}
}

func TestPostgresDialect_TableExistsQuery_Empty(t *testing.T) {
	d := &PostgresDialect{}
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for empty table name")
		}
	}()
	d.TableExistsQuery("")
}

func TestPostgresDialect_IntrospectColumnsQuery_Empty(t *testing.T) {
	d := &PostgresDialect{}
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for empty table name")
		}
	}()
	d.IntrospectColumnsQuery("")
}

func TestPostgresDialect_IntrospectIndexesQuery_Empty(t *testing.T) {
	d := &PostgresDialect{}
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for empty table name")
		}
	}()
	d.IntrospectIndexesQuery("")
}

func TestPostgresQuoteIdentifier_Extended(t *testing.T) {
	d := &PostgresDialect{}
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple", "users", `"users"`},
		{"with double quote", `col"name`, `"col""name"`},
		{"empty", "", `""`},
		{"null byte", "col\x00name", "\"col\x00name\""},
		{"very long", strings.Repeat("x", 500), `"` + strings.Repeat("x", 500) + `"`},
		{"multiple quotes", `a""b""c`, `"a""""b""""c"`},
		{"unicode", "日本語テーブル", `"日本語テーブル"`},
		{"sql injection attempt", `"; DROP TABLE users; --`, `"""; DROP TABLE users; --"`},
		{"backticks", "`users`", "\"`users`\""},
		{"single quotes", "'users'", `"'users'"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.QuoteIdentifier(tt.input)
			if got != tt.expected {
				t.Errorf("QuoteIdentifier(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestRegisterDialect_NilPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil dialect")
		}
	}()
	RegisterDialect(nil)
}

func TestGetDialect_Registered(t *testing.T) {
	// postgres should be registered via init()
	d, err := GetDialect("postgres")
	if err != nil {
		t.Fatalf("expected postgres dialect to be registered: %v", err)
	}
	if d.Name() != "postgres" {
		t.Errorf("expected 'postgres', got %q", d.Name())
	}
}

func TestGetDialect_NotRegistered(t *testing.T) {
	_, err := GetDialect("nonexistent_db_1234")
	if err == nil {
		t.Error("expected error for unregistered dialect")
	}
	if !strings.Contains(err.Error(), "nonexistent_db_1234") {
		t.Errorf("error should mention dialect name, got: %v", err)
	}
}

func TestListDialects(t *testing.T) {
	dialects := ListDialects()
	found := false
	for _, name := range dialects {
		if name == "postgres" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'postgres' to be in ListDialects()")
	}
}
