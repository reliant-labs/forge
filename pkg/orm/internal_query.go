package orm

import (
	sq "github.com/Masterminds/squirrel"
)

// queryBuilder is an internal wrapper around the actual query building implementation.
// This abstraction allows us to swap out the underlying library without breaking changes.
type queryBuilder struct {
	dialect Dialect
}

// newQueryBuilder creates a new internal query builder for the given dialect.
// This is an internal implementation detail and should not be exported.
func newQueryBuilder(dialect Dialect) *queryBuilder {
	return &queryBuilder{dialect: dialect}
}

// buildInsert builds an INSERT ... ON CONFLICT ... DO UPDATE SET query.
// Returns the SQL string and arguments.
func (qb *queryBuilder) buildInsert(tableName string, columns []string, values []any, pkColumn string) (string, []any, error) {
	// Get the placeholder format for this dialect
	format := qb.getPlaceholderFormat()

	// Build the INSERT portion
	insert := sq.Insert(tableName).
		Columns(columns...).
		Values(values...).
		PlaceholderFormat(format)

	// Build the base INSERT SQL
	insertSQL, args, err := insert.ToSql()
	if err != nil {
		return "", nil, err
	}

	// Build the ON CONFLICT DO UPDATE SET clause manually
	// (squirrel doesn't have native UPSERT support for all dialects)
	var updateCols []string
	for _, col := range columns {
		if col != pkColumn {
			qCol := qb.dialect.QuoteIdentifier(col)
			updateCols = append(updateCols, qCol+" = EXCLUDED."+qCol)
		}
	}

	// Construct the full upsert query
	upsertSQL := insertSQL + " ON CONFLICT (" + qb.dialect.QuoteIdentifier(pkColumn) + ") DO UPDATE SET "
	for i, col := range updateCols {
		if i > 0 {
			upsertSQL += ", "
		}
		upsertSQL += col
	}

	return upsertSQL, args, nil
}

// buildSelect builds a SELECT query with a WHERE clause.
func (qb *queryBuilder) buildSelect(tableName string, whereColumn string, whereValue any) (string, []any, error) {
	format := qb.getPlaceholderFormat()

	sql, args, err := sq.Select("*").
		From(tableName).
		Where(sq.Eq{whereColumn: whereValue}).
		PlaceholderFormat(format).
		ToSql()

	return sql, args, err
}

// buildDelete builds a DELETE query with a WHERE clause.
func (qb *queryBuilder) buildDelete(tableName string, whereColumn string, whereValue any) (string, []any, error) {
	format := qb.getPlaceholderFormat()

	sql, args, err := sq.Delete(tableName).
		Where(sq.Eq{whereColumn: whereValue}).
		PlaceholderFormat(format).
		ToSql()

	return sql, args, err
}

// getPlaceholderFormat returns the appropriate squirrel placeholder format for the dialect.
func (qb *queryBuilder) getPlaceholderFormat() sq.PlaceholderFormat {
	dialectName := qb.dialect.Name()
	switch dialectName {
	case "postgres":
		return sq.Dollar // $1, $2, $3
	case "sqlite":
		return sq.Question // ?, ?, ?
	default:
		// Default to dollar for PostgreSQL compatibility
		return sq.Dollar
	}
}