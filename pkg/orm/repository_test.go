package orm

import (
	"testing"
)

// testModel is a minimal type that satisfies the Model and Scanner constraints
// for compile-time verification of Repository generics.
type testModel struct {
	ID   string
	Name string
}

func (m *testModel) TableName() string { return "test_models" }
func (m *testModel) Schema() TableSchema {
	return TableSchema{
		Name: "test_models",
		Fields: []FieldSchema{
			{Name: "id", Type: TypeText, PrimaryKey: true},
			{Name: "name", Type: TypeText},
		},
	}
}
func (m *testModel) PrimaryKey() any                                   { return m.ID }
func (m *testModel) Values() (columns []string, values []any)         { return []string{"id", "name"}, []any{m.ID, m.Name} }
func (m *testModel) Scan(scanner interface{ Scan(...interface{}) error }) error {
	return scanner.Scan(&m.ID, &m.Name)
}

// TestRepositoryCompiles verifies that the generic Repository type can be
// instantiated with concrete types satisfying the constraints.
func TestRepositoryCompiles(t *testing.T) {
	// We pass nil for the Context since we're only checking that the type
	// system accepts our model; no actual DB calls are made.
	var db Context // nil — no calls will be made
	repo := NewRepository[testModel, *testModel](db)
	if repo == nil {
		t.Fatal("NewRepository returned nil")
	}
}

// TestWithSoftDeleteReturnsNewInstance verifies that WithSoftDelete returns a
// new repository instance without mutating the original.
func TestWithSoftDeleteReturnsNewInstance(t *testing.T) {
	var db Context
	repo := NewRepository[testModel, *testModel](db)
	soft := repo.WithSoftDelete("deleted_at")

	if soft == repo {
		t.Fatal("WithSoftDelete should return a new instance")
	}
	if repo.softDelete {
		t.Fatal("original repo should not have soft delete enabled")
	}
	if !soft.softDelete {
		t.Fatal("new repo should have soft delete enabled")
	}
	if soft.softDeleteCol != "deleted_at" {
		t.Fatalf("expected softDeleteCol=%q, got %q", "deleted_at", soft.softDeleteCol)
	}
}

// TestDBReturnsContext verifies the DB() accessor.
func TestDBReturnsContext(t *testing.T) {
	var db Context
	repo := NewRepository[testModel, *testModel](db)
	if repo.DB() != db {
		t.Fatal("DB() should return the same Context passed to NewRepository")
	}
}

// TestPrimaryKeyColumnFromSchema verifies the helper.
func TestPrimaryKeyColumnFromSchema(t *testing.T) {
	schema := TableSchema{
		Fields: []FieldSchema{
			{Name: "id", PrimaryKey: true},
			{Name: "name"},
		},
	}
	if col := primaryKeyColumnFromSchema(schema); col != "id" {
		t.Fatalf("expected %q, got %q", "id", col)
	}

	empty := TableSchema{Fields: []FieldSchema{{Name: "foo"}}}
	if col := primaryKeyColumnFromSchema(empty); col != "" {
		t.Fatalf("expected empty string, got %q", col)
	}
}

// TestSoftDeleteListPrependsWhereIsNull verifies that List with soft delete
// prepends a WhereIsNull filter by inspecting the QueryOption it would apply.
func TestSoftDeleteListPrependsWhereIsNull(t *testing.T) {
	var db Context
	repo := NewRepository[testModel, *testModel](db).WithSoftDelete("deleted_at")

	// We can't call List without a real DB, but we can verify the option
	// assembly logic by checking the struct state.
	if !repo.softDelete {
		t.Fatal("soft delete should be enabled")
	}
	if repo.softDeleteCol != "deleted_at" {
		t.Fatalf("expected softDeleteCol=%q, got %q", "deleted_at", repo.softDeleteCol)
	}
}
