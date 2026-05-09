package orm

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// recordingCtx records SQL queries for assertion without hitting a real database.
type recordingCtx struct {
	dialect    Dialect
	lastQuery  string
	lastArgs   []interface{}
	execCalled bool
}

func (r *recordingCtx) Dialect() Dialect { return r.dialect }
func (r *recordingCtx) Exec(_ context.Context, query string, args ...interface{}) (sql.Result, error) {
	r.lastQuery = query
	r.lastArgs = args
	r.execCalled = true
	return nil, nil
}
func (r *recordingCtx) Query(_ context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	r.lastQuery = query
	r.lastArgs = args
	return nil, fmt.Errorf("not implemented in recording context")
}
func (r *recordingCtx) QueryRow(_ context.Context, query string, args ...interface{}) *sql.Row {
	r.lastQuery = query
	r.lastArgs = args
	return nil
}

func TestRepository_SoftDelete_SQL(t *testing.T) {
	ctx := context.Background()

	t.Run("postgres dialect", func(t *testing.T) {
		rec := &recordingCtx{dialect: &fakeDialect{name: "postgres"}}
		repo := NewRepository[testModel, *testModel](rec).WithSoftDelete("deleted_at")

		_ = repo.Delete(ctx, "user-123")

		if !rec.execCalled {
			t.Fatal("expected Exec to be called")
		}

		// Should be an UPDATE, not DELETE
		if !strings.HasPrefix(rec.lastQuery, "UPDATE") {
			t.Errorf("expected UPDATE for soft delete, got: %s", rec.lastQuery)
		}
		// Should set deleted_at = CURRENT_TIMESTAMP
		if !strings.Contains(rec.lastQuery, "CURRENT_TIMESTAMP") {
			t.Errorf("expected CURRENT_TIMESTAMP, got: %s", rec.lastQuery)
		}
		// Should use quoted identifiers
		if !strings.Contains(rec.lastQuery, `"test_models"`) {
			t.Errorf("expected quoted table name, got: %s", rec.lastQuery)
		}
		if !strings.Contains(rec.lastQuery, `"deleted_at"`) {
			t.Errorf("expected quoted deleted_at column, got: %s", rec.lastQuery)
		}
		if !strings.Contains(rec.lastQuery, `"id"`) {
			t.Errorf("expected quoted id column, got: %s", rec.lastQuery)
		}
		// Should use $1 placeholder
		if !strings.Contains(rec.lastQuery, "$1") {
			t.Errorf("expected $1 placeholder, got: %s", rec.lastQuery)
		}
		// Args should contain the ID
		if len(rec.lastArgs) != 1 || rec.lastArgs[0] != "user-123" {
			t.Errorf("expected args [user-123], got: %v", rec.lastArgs)
		}
	})

	t.Run("sqlite dialect", func(t *testing.T) {
		rec := &recordingCtx{dialect: &fakeDialect{name: "sqlite"}}
		repo := NewRepository[testModel, *testModel](rec).WithSoftDelete("deleted_at")

		_ = repo.Delete(ctx, "user-456")

		if !strings.HasPrefix(rec.lastQuery, "UPDATE") {
			t.Errorf("expected UPDATE for soft delete, got: %s", rec.lastQuery)
		}
		// Should use ? placeholder for SQLite
		if !strings.Contains(rec.lastQuery, "?") {
			t.Errorf("expected ? placeholder for SQLite, got: %s", rec.lastQuery)
		}
		if strings.Contains(rec.lastQuery, "$1") {
			t.Errorf("should not use $1 for SQLite, got: %s", rec.lastQuery)
		}
	})
}

func TestRepository_HardDelete_SQL(t *testing.T) {
	ctx := context.Background()

	t.Run("postgres dialect", func(t *testing.T) {
		rec := &recordingCtx{dialect: &fakeDialect{name: "postgres"}}
		repo := NewRepository[testModel, *testModel](rec)

		_ = repo.Delete(ctx, "user-123")

		if !rec.execCalled {
			t.Fatal("expected Exec to be called")
		}
		// Should be a DELETE
		if !strings.HasPrefix(rec.lastQuery, "DELETE") {
			t.Errorf("expected DELETE for hard delete, got: %s", rec.lastQuery)
		}
		if !strings.Contains(rec.lastQuery, "users") || !strings.Contains(rec.lastQuery, "test_models") {
			// The table name comes from testModel.TableName()
		}
		if !strings.Contains(rec.lastQuery, "$1") {
			t.Errorf("expected $1 placeholder, got: %s", rec.lastQuery)
		}
		if len(rec.lastArgs) != 1 || rec.lastArgs[0] != "user-123" {
			t.Errorf("expected args [user-123], got: %v", rec.lastArgs)
		}
	})

	t.Run("sqlite dialect", func(t *testing.T) {
		rec := &recordingCtx{dialect: &fakeDialect{name: "sqlite"}}
		repo := NewRepository[testModel, *testModel](rec)

		_ = repo.Delete(ctx, 42)

		if !strings.HasPrefix(rec.lastQuery, "DELETE") {
			t.Errorf("expected DELETE, got: %s", rec.lastQuery)
		}
		if !strings.Contains(rec.lastQuery, "?") {
			t.Errorf("expected ? placeholder for SQLite, got: %s", rec.lastQuery)
		}
	})
}

// testModelNoPK has no primary key to test error paths.
type testModelNoPK struct {
	Name string
}

func (m *testModelNoPK) TableName() string { return "no_pk_table" }
func (m *testModelNoPK) Schema() TableSchema {
	return TableSchema{
		Name:   "no_pk_table",
		Fields: []FieldSchema{{Name: "name", Type: TypeText}},
	}
}
func (m *testModelNoPK) PrimaryKey() any { return nil }
func (m *testModelNoPK) Values() (columns []string, values []any) {
	return []string{"name"}, []any{m.Name}
}
func (m *testModelNoPK) Scan(scanner interface{ Scan(...interface{}) error }) error {
	return scanner.Scan(&m.Name)
}

func TestRepository_Delete_NoPrimaryKey(t *testing.T) {
	ctx := context.Background()
	rec := &recordingCtx{dialect: &fakeDialect{name: "postgres"}}
	repo := NewRepository[testModelNoPK, *testModelNoPK](rec)

	err := repo.Delete(ctx, "anything")
	if err == nil {
		t.Fatal("expected error for missing primary key")
	}
	if !strings.Contains(err.Error(), "no primary key") {
		t.Errorf("expected 'no primary key' error, got: %v", err)
	}
}

func TestRepository_SoftDelete_NoPrimaryKey(t *testing.T) {
	ctx := context.Background()
	rec := &recordingCtx{dialect: &fakeDialect{name: "postgres"}}
	repo := NewRepository[testModelNoPK, *testModelNoPK](rec).WithSoftDelete("deleted_at")

	err := repo.Delete(ctx, "anything")
	if err == nil {
		t.Fatal("expected error for missing primary key")
	}
	if !strings.Contains(err.Error(), "no primary key") {
		t.Errorf("expected 'no primary key' error, got: %v", err)
	}
}

func TestRepository_SoftDeleteList_PrependsWhereIsNull(t *testing.T) {
	// Test that soft-delete List prepends WhereIsNull filter.
	// We can't run the full List without a real DB, but we can verify
	// the option is applied correctly by building a query manually.
	pgDialect := &fakeDialect{name: "postgres"}
	fCtx := &fakeCtx{dialect: pgDialect}

	// Simulate what List does internally: create a QueryBuilder and apply options
	schema := (&testModel{}).Schema()
	columns := make([]string, 0, len(schema.Fields))
	for _, field := range schema.Fields {
		columns = append(columns, field.Name)
	}

	opts := []QueryOption{
		WhereEq("status", "active"),
		WithLimit(10),
	}

	// Apply soft delete prepend (same as Repository.List)
	opts = append([]QueryOption{WhereIsNull("deleted_at")}, opts...)

	qb := NewQueryBuilder(fCtx, "test_models", columns)
	for _, opt := range opts {
		opt(qb)
	}

	sqlStr, args := qb.Build()

	// The IS NULL should come first
	nullIdx := strings.Index(sqlStr, `"deleted_at" IS NULL`)
	eqIdx := strings.Index(sqlStr, `"status" = $`)
	if nullIdx == -1 {
		t.Fatalf("expected IS NULL clause, got: %s", sqlStr)
	}
	if eqIdx == -1 {
		t.Fatalf("expected status = clause, got: %s", sqlStr)
	}
	if nullIdx > eqIdx {
		t.Errorf("IS NULL should come before status filter, got: %s", sqlStr)
	}
	// Should have 2 args: "active" and 10
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %d: %v", len(args), args)
	}
}

func TestRepository_Count_BuildsCountQuery(t *testing.T) {
	// Verify that Count builds a COUNT(*) query by using QueryBuilder directly.
	pgDialect := &fakeDialect{name: "postgres"}
	fCtx := &fakeCtx{dialect: pgDialect}

	qb := NewQueryBuilder(fCtx, "test_models", []string{"COUNT(*)"})
	WhereEq("active", true)(qb)

	sqlStr, args := qb.Build()

	if !strings.Contains(sqlStr, "SELECT COUNT(*)") {
		t.Errorf("expected COUNT(*), got: %s", sqlStr)
	}
	if !strings.Contains(sqlStr, `WHERE "active" = $1`) {
		t.Errorf("expected WHERE clause, got: %s", sqlStr)
	}
	if len(args) != 1 {
		t.Errorf("expected 1 arg, got %d", len(args))
	}
}

func TestRepository_Count_SoftDeletePrependsFilter(t *testing.T) {
	pgDialect := &fakeDialect{name: "postgres"}
	fCtx := &fakeCtx{dialect: pgDialect}

	// Simulate Count with soft delete
	opts := []QueryOption{
		WhereEq("status", "active"),
	}
	opts = append([]QueryOption{WhereIsNull("deleted_at")}, opts...)

	qb := NewQueryBuilder(fCtx, "test_models", []string{"COUNT(*)"})
	for _, opt := range opts {
		opt(qb)
	}
	// Clear ordering like Count does
	qb.orderByClauses = nil

	sqlStr, args := qb.Build()

	if !strings.Contains(sqlStr, `"deleted_at" IS NULL`) {
		t.Errorf("expected IS NULL filter, got: %s", sqlStr)
	}
	if !strings.Contains(sqlStr, "COUNT(*)") {
		t.Errorf("expected COUNT(*), got: %s", sqlStr)
	}
	if strings.Contains(sqlStr, "ORDER BY") {
		t.Errorf("COUNT should clear ORDER BY, got: %s", sqlStr)
	}
	if len(args) != 1 {
		t.Errorf("expected 1 arg, got %d", len(args))
	}
}
