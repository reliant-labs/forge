package orm

import (
	"context"
	"fmt"
)

// Repository provides type-safe CRUD operations for a model.
// T is the concrete type, PT is a pointer to T that implements Model and Scanner.
type Repository[T any, PT interface {
	*T
	Model
	Scanner
}] struct {
	db            Context
	softDelete    bool
	softDeleteCol string
}

// NewRepository creates a new Repository backed by the given Context.
// Works with both *Client (for normal operations) and *Tx (inside transactions).
func NewRepository[T any, PT interface {
	*T
	Model
	Scanner
}](db Context) *Repository[T, PT] {
	return &Repository[T, PT]{db: db}
}

// WithSoftDelete returns a copy of the repository with soft delete enabled.
// The column should be a nullable timestamp column (e.g., "deleted_at").
// When enabled:
//   - Delete sets the column to CURRENT_TIMESTAMP instead of deleting the row
//   - List and Count automatically filter out soft-deleted rows
func (r *Repository[T, PT]) WithSoftDelete(column string) *Repository[T, PT] {
	return &Repository[T, PT]{
		db:            r.db,
		softDelete:    true,
		softDeleteCol: column,
	}
}

// Save inserts or updates the entity. Delegates to orm.Save.
func (r *Repository[T, PT]) Save(ctx context.Context, entity PT) error {
	return Save(ctx, r.db, entity)
}

// Get retrieves an entity by primary key. Delegates to orm.Get.
// If soft delete is enabled, automatically filters out soft-deleted rows.
func (r *Repository[T, PT]) Get(ctx context.Context, id any) (*T, error) {
	if r.softDelete {
		var zero T
		model := PT(&zero)
		schema := model.Schema()
		pkCol := ""
		for _, f := range schema.Fields {
			if f.PrimaryKey {
				pkCol = f.Name
				break
			}
		}
		if pkCol == "" {
			return nil, ErrNoPrimaryKey
		}
		results, err := r.List(ctx, WhereEq(pkCol, id), WithLimit(1))
		if err != nil {
			return nil, err
		}
		if len(results) == 0 {
			return nil, ErrNoRows
		}
		return results[0], nil
	}
	// Non-soft-delete path: use direct Get
	var zero T
	model := PT(&zero)
	if err := Get[T, PT](ctx, r.db, model, id); err != nil {
		return nil, err
	}
	return &zero, nil
}

// Delete removes an entity by primary key.
// If soft delete is enabled, sets the soft delete column to CURRENT_TIMESTAMP.
// Otherwise, performs a hard delete via raw SQL.
func (r *Repository[T, PT]) Delete(ctx context.Context, id any) error {
	var zero T
	model := PT(&zero)
	schema := model.Schema()
	table := model.TableName()
	dialect := r.db.Dialect()

	pkColumn := primaryKeyColumnFromSchema(schema)
	if pkColumn == "" {
		return fmt.Errorf("no primary key defined for table %s", table)
	}

	if r.softDelete {
		sql := fmt.Sprintf(
			"UPDATE %s SET %s = CURRENT_TIMESTAMP WHERE %s = %s",
			dialect.QuoteIdentifier(table),
			dialect.QuoteIdentifier(r.softDeleteCol),
			dialect.QuoteIdentifier(pkColumn),
			dialect.Placeholder(0),
		)
		_, err := r.db.Exec(ctx, sql, id)
		return err
	}

	// Hard delete via the internal query builder for dialect-safe SQL.
	qb := newQueryBuilder(dialect)
	query, args, err := qb.buildDelete(table, pkColumn, id)
	if err != nil {
		return fmt.Errorf("failed to build delete query: %w", err)
	}
	_, err = r.db.Exec(ctx, query, args...)
	return err
}

// List retrieves entities matching the given options.
// If soft delete is enabled, automatically filters out soft-deleted rows.
func (r *Repository[T, PT]) List(ctx context.Context, opts ...QueryOption) ([]*T, error) {
	if r.softDelete {
		opts = append([]QueryOption{WhereIsNull(r.softDeleteCol)}, opts...)
	}
	return List[T, PT](ctx, r.db, opts...)
}

// Count returns the number of entities matching the given options.
// If soft delete is enabled, automatically filters out soft-deleted rows.
func (r *Repository[T, PT]) Count(ctx context.Context, opts ...QueryOption) (int64, error) {
	if r.softDelete {
		opts = append([]QueryOption{WhereIsNull(r.softDeleteCol)}, opts...)
	}
	return Count[T, PT](ctx, r.db, opts...)
}

// DB returns the underlying Context, useful for raw queries or creating
// sub-repositories in transactions.
func (r *Repository[T, PT]) DB() Context {
	return r.db
}

// primaryKeyColumnFromSchema returns the primary key column name from the schema,
// or an empty string if none is found.
func primaryKeyColumnFromSchema(schema TableSchema) string {
	for _, field := range schema.Fields {
		if field.PrimaryKey {
			return field.Name
		}
	}
	return ""
}
