package orm

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// fakeDialect is a minimal Dialect implementation for testing QueryBuilder SQL output.
type fakeDialect struct {
	name string
}

func (d *fakeDialect) Name() string      { return d.name }
func (d *fakeDialect) DriverName() string { return d.name }
func (d *fakeDialect) Placeholder(i int) string {
	if d.name == "sqlite" {
		return "?"
	}
	return fmt.Sprintf("$%d", i+1)
}
func (d *fakeDialect) QuoteIdentifier(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
func (d *fakeDialect) MapFieldType(ft FieldType) string                { return string(ft) }
func (d *fakeDialect) SupportsReturning() bool                        { return d.name == "postgres" }
func (d *fakeDialect) OnConflictClause(c string, u []string) string   { return "" }
func (d *fakeDialect) TableExistsQuery(t string) string               { return "" }
func (d *fakeDialect) ListTablesQuery() string                        { return "" }
func (d *fakeDialect) IntrospectColumnsQuery(t string) string         { return "" }
func (d *fakeDialect) IntrospectIndexesQuery(t string) string         { return "" }
func (d *fakeDialect) ParseColumnType(t string) (FieldType, error)    { return TypeText, nil }
func (d *fakeDialect) ScanColumn(r *sql.Rows) (IntrospectedColumn, error) {
	return IntrospectedColumn{}, nil
}
func (d *fakeDialect) ScanIndex(r *sql.Rows) (string, string, bool, error) {
	return "", "", false, nil
}

// fakeCtx implements Context for testing purposes.
type fakeCtx struct {
	dialect Dialect
}

func (f *fakeCtx) Dialect() Dialect { return f.dialect }
func (f *fakeCtx) Exec(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
	return nil, nil
}
func (f *fakeCtx) Query(_ context.Context, _ string, _ ...interface{}) (*sql.Rows, error) {
	return nil, nil
}
func (f *fakeCtx) QueryRow(_ context.Context, _ string, _ ...interface{}) *sql.Row {
	return nil
}

// newTestQueryBuilder creates a QueryBuilder backed by a fakeCtx for the given dialect name.
func newTestQueryBuilder(dialectName, table string, cols []string) *QueryBuilder {
	ctx := &fakeCtx{dialect: &fakeDialect{name: dialectName}}
	return NewQueryBuilder(ctx, table, cols)
}

func TestQueryBuilder_Build_SelectAll(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", nil)
	sql, args := qb.Build()
	if sql != "SELECT * FROM users" {
		t.Errorf("unexpected SQL: %s", sql)
	}
	if len(args) != 0 {
		t.Errorf("expected no args, got %v", args)
	}
}

func TestQueryBuilder_Build_SelectColumns(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id", "name", "email"})
	sql, args := qb.Build()
	if sql != `SELECT "id", "name", "email" FROM users` {
		t.Errorf("unexpected SQL: %s", sql)
	}
	if len(args) != 0 {
		t.Errorf("expected no args, got %v", args)
	}
}

func TestQueryBuilder_Build_WhereEq_Postgres(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id", "name"})
	qb.Where("name", Eq, "alice")
	sql, args := qb.Build()

	expected := `SELECT "id", "name" FROM users WHERE "name" = $1`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 1 || args[0] != "alice" {
		t.Errorf("expected args [alice], got %v", args)
	}
}

func TestQueryBuilder_Build_WhereEq_SQLite(t *testing.T) {
	qb := newTestQueryBuilder("sqlite", "users", []string{"id", "name"})
	qb.Where("name", Eq, "alice")
	sql, args := qb.Build()

	expected := `SELECT "id", "name" FROM users WHERE "name" = ?`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 1 || args[0] != "alice" {
		t.Errorf("expected args [alice], got %v", args)
	}
}

func TestQueryBuilder_Build_MultipleWhere(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"*"})
	qb.Where("age", GreaterThan, 18)
	qb.Where("active", Eq, true)
	sql, args := qb.Build()

	expected := `SELECT * FROM users WHERE "age" > $1 AND "active" = $2`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %d", len(args))
	}
}

func TestQueryBuilder_Build_IsNull(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id"})
	qb.Where("deleted_at", IsNull, nil)
	sql, args := qb.Build()

	expected := `SELECT "id" FROM users WHERE "deleted_at" IS NULL`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 0 {
		t.Errorf("expected no args, got %v", args)
	}
}

func TestQueryBuilder_Build_IsNotNull(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id"})
	qb.Where("verified_at", IsNotNull, nil)
	sql, args := qb.Build()

	expected := `SELECT "id" FROM users WHERE "verified_at" IS NOT NULL`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 0 {
		t.Errorf("expected no args, got %v", args)
	}
}

func TestQueryBuilder_Build_InOperator(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "orders", []string{"id"})
	qb.Where("status", In, []string{"pending", "active"})
	sql, args := qb.Build()

	expected := `SELECT "id" FROM orders WHERE "status" IN ($1, $2)`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 2 || args[0] != "pending" || args[1] != "active" {
		t.Errorf("expected args [pending, active], got %v", args)
	}
}

func TestQueryBuilder_Build_NotInOperator(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "orders", []string{"id"})
	qb.Where("status", NotIn, []string{"cancelled"})
	sql, args := qb.Build()

	expected := `SELECT "id" FROM orders WHERE "status" NOT IN ($1)`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 1 || args[0] != "cancelled" {
		t.Errorf("expected args [cancelled], got %v", args)
	}
}

func TestQueryBuilder_Build_OrderBy(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id", "name"})
	qb.OrderBy("name", Asc)
	sql, _ := qb.Build()

	expected := `SELECT "id", "name" FROM users ORDER BY "name" ASC`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
}

func TestQueryBuilder_Build_MultipleOrderBy(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id", "name"})
	qb.OrderBy("name", Asc).OrderBy("id", Desc)
	sql, _ := qb.Build()

	expected := `SELECT "id", "name" FROM users ORDER BY "name" ASC, "id" DESC`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
}

func TestQueryBuilder_Build_LimitOffset_Postgres(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id"})
	qb.Limit(10).Offset(20)
	sql, args := qb.Build()

	expected := `SELECT "id" FROM users LIMIT $1 OFFSET $2`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 2 || args[0] != 10 || args[1] != 20 {
		t.Errorf("expected args [10, 20], got %v", args)
	}
}

func TestQueryBuilder_Build_LimitOffset_SQLite(t *testing.T) {
	qb := newTestQueryBuilder("sqlite", "users", []string{"id"})
	qb.Limit(10).Offset(20)
	sql, args := qb.Build()

	expected := `SELECT "id" FROM users LIMIT ? OFFSET ?`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 2 || args[0] != 10 || args[1] != 20 {
		t.Errorf("expected args [10, 20], got %v", args)
	}
}

func TestQueryBuilder_Build_FullQuery_Postgres(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id", "name", "email"})
	qb.Where("active", Eq, true).
		Where("deleted_at", IsNull, nil).
		OrderBy("name", Asc).
		Limit(25).
		Offset(50)
	sql, args := qb.Build()

	expected := `SELECT "id", "name", "email" FROM users WHERE "active" = $1 AND "deleted_at" IS NULL ORDER BY "name" ASC LIMIT $2 OFFSET $3`
	if sql != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, sql)
	}
	if len(args) != 3 {
		t.Errorf("expected 3 args, got %d: %v", len(args), args)
	}
	if args[0] != true || args[1] != 25 || args[2] != 50 {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestQueryBuilder_Build_FullQuery_SQLite(t *testing.T) {
	qb := newTestQueryBuilder("sqlite", "users", []string{"id", "name"})
	qb.Where("active", Eq, true).
		Where("age", GreaterThanOrEq, 18).
		OrderBy("name", Desc).
		Limit(10).
		Offset(0)
	sql, args := qb.Build()

	expected := `SELECT "id", "name" FROM users WHERE "active" = ? AND "age" >= ? ORDER BY "name" DESC LIMIT ? OFFSET ?`
	if sql != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, sql)
	}
	if len(args) != 4 {
		t.Errorf("expected 4 args, got %d: %v", len(args), args)
	}
}

func TestQueryBuilder_Build_Like(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id"})
	qb.Where("email", Like, "%@example.com")
	sql, args := qb.Build()

	expected := `SELECT "id" FROM users WHERE "email" LIKE $1`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 1 || args[0] != "%@example.com" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestQueryBuilder_Build_ILike_Postgres(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id"})
	qb.Where("email", ILike, "%@EXAMPLE.COM")
	sql, args := qb.Build()

	expected := `SELECT "id" FROM users WHERE "email" ILIKE $1`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 1 || args[0] != "%@EXAMPLE.COM" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestQueryBuilder_Build_ILike_SQLite(t *testing.T) {
	qb := newTestQueryBuilder("sqlite", "users", []string{"id"})
	qb.Where("email", ILike, "%@EXAMPLE.COM")
	sql, args := qb.Build()

	expected := `SELECT "id" FROM users WHERE LOWER("email") LIKE LOWER(?)` 
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 1 || args[0] != "%@EXAMPLE.COM" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestQueryBuilder_Build_WhereArgCounter(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "items", []string{"id"})
	qb.Where("price", LessThan, 100).
		Where("category", Eq, "electronics").
		Limit(5)
	sql, args := qb.Build()

	expected := `SELECT "id" FROM items WHERE "price" < $1 AND "category" = $2 LIMIT $3`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 3 || args[0] != 100 || args[1] != "electronics" || args[2] != 5 {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestQueryBuilder_Build_ResetOnSecondBuild(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id"})
	qb.Where("name", Eq, "alice")

	sql1, args1 := qb.Build()
	sql2, args2 := qb.Build()

	if sql1 != sql2 {
		t.Errorf("Build() should be idempotent: %q vs %q", sql1, sql2)
	}
	if len(args1) != len(args2) {
		t.Errorf("args length should match: %v vs %v", args1, args2)
	}
}

func TestQueryOptions_WithWhere(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id"})
	opt := WithWhere("name", Eq, "bob")
	opt(qb)

	sql, args := qb.Build()
	expected := `SELECT "id" FROM users WHERE "name" = $1`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 1 || args[0] != "bob" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestQueryOptions_WithLimit(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id"})
	opt := WithLimit(42)
	opt(qb)

	sql, args := qb.Build()
	expected := `SELECT "id" FROM users LIMIT $1`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 1 || args[0] != 42 {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestQueryOptions_WithOffset(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id"})
	opt := WithOffset(10)
	opt(qb)

	sql, args := qb.Build()
	expected := `SELECT "id" FROM users OFFSET $1`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 1 || args[0] != 10 {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestQueryOptions_WithOrderBy(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"id"})
	opt := WithOrderBy("created_at", Desc)
	opt(qb)

	sql, _ := qb.Build()
	expected := `SELECT "id" FROM users ORDER BY "created_at" DESC`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
}

func TestQueryBuilder_Build_AllOperators(t *testing.T) {
	tests := []struct {
		name     string
		op       Operator
		value    interface{}
		wantSQL  string
		wantArgs int
	}{
		{"Eq", Eq, "val", `SELECT "id" FROM t WHERE "col" = $1`, 1},
		{"NotEq", NotEq, "val", `SELECT "id" FROM t WHERE "col" != $1`, 1},
		{"GreaterThan", GreaterThan, 5, `SELECT "id" FROM t WHERE "col" > $1`, 1},
		{"GreaterThanOrEq", GreaterThanOrEq, 5, `SELECT "id" FROM t WHERE "col" >= $1`, 1},
		{"LessThan", LessThan, 5, `SELECT "id" FROM t WHERE "col" < $1`, 1},
		{"LessThanOrEq", LessThanOrEq, 5, `SELECT "id" FROM t WHERE "col" <= $1`, 1},
		{"Like", Like, "%val%", `SELECT "id" FROM t WHERE "col" LIKE $1`, 1},
		{"ILike", ILike, "%val%", `SELECT "id" FROM t WHERE "col" ILIKE $1`, 1},
		{"In", In, []int{1, 2}, `SELECT "id" FROM t WHERE "col" IN ($1, $2)`, 2},
		{"NotIn", NotIn, []int{1}, `SELECT "id" FROM t WHERE "col" NOT IN ($1)`, 1},
		{"IsNull", IsNull, nil, `SELECT "id" FROM t WHERE "col" IS NULL`, 0},
		{"IsNotNull", IsNotNull, nil, `SELECT "id" FROM t WHERE "col" IS NOT NULL`, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			qb := newTestQueryBuilder("postgres", "t", []string{"id"})
			qb.Where("col", tt.op, tt.value)
			sql, args := qb.Build()
			if sql != tt.wantSQL {
				t.Errorf("expected %q, got %q", tt.wantSQL, sql)
			}
			if len(args) != tt.wantArgs {
				t.Errorf("expected %d args, got %d", tt.wantArgs, len(args))
			}
		})
	}
}

func TestQueryBuilder_Build_SelectCountStar(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "users", []string{"COUNT(*)"})
	sql, args := qb.Build()
	if sql != "SELECT COUNT(*) FROM users" {
		t.Errorf("unexpected SQL: %s", sql)
	}
	if len(args) != 0 {
		t.Errorf("expected no args, got %v", args)
	}
}

func TestQueryBuilder_Build_InOperator_SingleValue(t *testing.T) {
	qb := newTestQueryBuilder("postgres", "orders", []string{"id"})
	qb.Where("status", In, "single")
	sql, args := qb.Build()

	expected := `SELECT "id" FROM orders WHERE "status" IN ($1)`
	if sql != expected {
		t.Errorf("expected %q, got %q", expected, sql)
	}
	if len(args) != 1 || args[0] != "single" {
		t.Errorf("expected args [single], got %v", args)
	}
}