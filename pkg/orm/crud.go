package orm

import (
	"context"
	"fmt"
)

// Save inserts or updates a model in the database using an upsert operation.
// Works with both Client and transaction contexts.
//
// Example:
//
//	user := &User{Id: "123", Email: "test@example.com"}
//	err := orm.Save(ctx, client, user)
func Save(ctx context.Context, db Context, model Model) error {
	schema := model.Schema()
	tableName := model.TableName()
	columns, values := model.Values()

	// Find primary key column
	var pkColumn string
	for _, field := range schema.Fields {
		if field.PrimaryKey {
			pkColumn = field.Name
			break
		}
	}

	if pkColumn == "" {
		return fmt.Errorf("no primary key defined for table %s", tableName)
	}

	// Use internal query builder (hides squirrel implementation)
	qb := newQueryBuilder(db.Dialect())
	query, args, err := qb.buildInsert(tableName, columns, values, pkColumn)
	if err != nil {
		return fmt.Errorf("failed to build insert query: %w", err)
	}

	_, err = db.Exec(ctx, query, args...)
	return err
}

// Delete removes a model from the database by its primary key.
// Works with both Client and transaction contexts.
//
// Example:
//
//	user := &User{Id: "123"}
//	err := orm.Delete(ctx, client, user)
func Delete(ctx context.Context, db Context, model Model) error {
	schema := model.Schema()
	tableName := model.TableName()

	// Find primary key field
	var pkColumn string
	for _, field := range schema.Fields {
		if field.PrimaryKey {
			pkColumn = field.Name
			break
		}
	}

	if pkColumn == "" {
		return fmt.Errorf("no primary key defined for table %s", tableName)
	}

	pkValue := model.PrimaryKey()

	// Use internal query builder
	qb := newQueryBuilder(db.Dialect())
	query, args, err := qb.buildDelete(tableName, pkColumn, pkValue)
	if err != nil {
		return fmt.Errorf("failed to build delete query: %w", err)
	}

	_, err = db.Exec(ctx, query, args...)
	return err
}

// Get retrieves a single record by primary key and populates the provided model.
// The model must implement both Model and Scanner interfaces.
//
// Example:
//
//	user := &User{}
//	err := orm.Get(ctx, client, user, "user-123")
func Get[T any, PT interface {
	*T
	Model
	Scanner
}](ctx context.Context, db Context, model PT, id any) error {
	schema := model.Schema()
	tableName := model.TableName()

	// Find primary key field
	var pkColumn string
	for _, field := range schema.Fields {
		if field.PrimaryKey {
			pkColumn = field.Name
			break
		}
	}

	if pkColumn == "" {
		return fmt.Errorf("no primary key defined for table %s", tableName)
	}

	// Use internal query builder
	qb := newQueryBuilder(db.Dialect())
	query, args, err := qb.buildSelect(tableName, pkColumn, id)
	if err != nil {
		return fmt.Errorf("failed to build select query: %w", err)
	}

	row := db.QueryRow(ctx, query, args...)
	return model.Scan(row)
}

// Count returns the number of records matching the given options.
// Uses generics to derive table information from the model type.
//
// Example:
//
//	count, err := orm.Count[User](ctx, client,
//	    orm.WhereEq("active", true),
//	)
func Count[T any, PT interface {
	*T
	Model
	Scanner
}](ctx context.Context, db Context, opts ...QueryOption) (int64, error) {
	var zero T
	model := PT(&zero)
	tableName := model.TableName()

	qb := NewQueryBuilder(db, tableName, []string{"COUNT(*)"})
	for _, opt := range opts {
		opt(qb)
	}

	// Clear any ordering — it's meaningless for COUNT.
	qb.orderByClauses = nil

	query, args := qb.Build()
	row := db.QueryRow(ctx, query, args...)

	var count int64
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// List retrieves multiple records with optional filtering and returns a slice of models.
// Uses generics to provide type-safe results without casting.
//
// Example:
//
//	users, err := orm.List[User](ctx, client,
//	    orm.WithWhere("email", orm.Like, "%@example.com"),
//	    orm.WithLimit(10),
//	)
func List[T any, PT interface {
	*T
	Model
	Scanner
}](ctx context.Context, db Context, opts ...QueryOption) ([]*T, error) {
	// Create a zero value to get schema info
	var zero T
	model := PT(&zero)

	schema := model.Schema()
	tableName := model.TableName()

	// Extract column names
	columns := make([]string, 0, len(schema.Fields))
	for _, field := range schema.Fields {
		columns = append(columns, field.Name)
	}

	qb := NewQueryBuilder(db, tableName, columns)
	for _, opt := range opts {
		opt(qb)
	}

	rows, err := qb.Execute(ctx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make([]*T, 0)
	for rows.Next() {
		var item T
		itemModel := PT(&item)
		if err := itemModel.Scan(rows); err != nil {
			return nil, err
		}
		results = append(results, &item)
	}

	return results, rows.Err()
}
