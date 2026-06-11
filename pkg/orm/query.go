package orm

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
)

// nextPlaceholder returns the next dialect-appropriate placeholder (e.g. $1 for
// Postgres, ? for SQLite) and advances the internal argument counter.
func (qb *QueryBuilder) nextPlaceholder() string {
	placeholder := qb.ctx.Dialect().Placeholder(qb.argCounter)
	qb.argCounter++
	return placeholder
}

// Operator represents a SQL comparison operator
type Operator string

const (
	Eq              Operator = "="
	NotEq           Operator = "!="
	GreaterThan     Operator = ">"
	GreaterThanOrEq Operator = ">="
	LessThan        Operator = "<"
	LessThanOrEq    Operator = "<="
	Like            Operator = "LIKE"
	ILike           Operator = "ILIKE"
	In              Operator = "IN"
	NotIn           Operator = "NOT IN"
	IsNull          Operator = "IS NULL"
	IsNotNull       Operator = "IS NOT NULL"
)

// Order represents sort order
type Order string

const (
	Asc  Order = "ASC"
	Desc Order = "DESC"
)

// QueryBuilder provides a fluent API for building SQL queries
type QueryBuilder struct {
	ctx            Context
	tableName      string
	columns        []string
	whereClauses   []whereClause
	orILikeGroups  []orILikeGroup
	orderByClauses []orderByClause
	limitValue     *int
	offsetValue    *int
	args           []interface{}
	argCounter     int
}

type whereClause struct {
	column   string
	operator Operator
	value    interface{}
}

// orILikeGroup is one OR-joined group of case-insensitive LIKE matches
// against the same value: (LOWER(c1) LIKE LOWER($1) OR LOWER(c2) LIKE
// LOWER($2) ...). The whole group is AND-ed with the other WHERE
// clauses. This is the substrate for `search` filters that span an
// entity's string columns.
type orILikeGroup struct {
	columns []string
	value   interface{}
}

type orderByClause struct {
	column string
	order  Order
}

// NewQueryBuilder creates a new query builder
func NewQueryBuilder(ctx Context, tableName string, columns []string) *QueryBuilder {
	return &QueryBuilder{
		ctx:       ctx,
		tableName: tableName,
		columns:   columns,
	}
}

// NewTxQueryBuilder creates a new query builder for a transaction
// Deprecated: Use NewQueryBuilder with a transaction Context instead
func NewTxQueryBuilder(tx *Tx, tableName string, columns []string) *QueryBuilder {
	return &QueryBuilder{
		ctx:       tx,
		tableName: tableName,
		columns:   columns,
	}
}

// Where adds a WHERE clause
func (qb *QueryBuilder) Where(column string, op Operator, value interface{}) *QueryBuilder {
	qb.whereClauses = append(qb.whereClauses, whereClause{
		column:   column,
		operator: op,
		value:    value,
	})
	return qb
}

// OrderBy adds an ORDER BY clause
func (qb *QueryBuilder) OrderBy(column string, order Order) *QueryBuilder {
	qb.orderByClauses = append(qb.orderByClauses, orderByClause{
		column: column,
		order:  order,
	})
	return qb
}

// Limit sets the LIMIT
func (qb *QueryBuilder) Limit(limit int) *QueryBuilder {
	qb.limitValue = &limit
	return qb
}

// Offset sets the OFFSET
func (qb *QueryBuilder) Offset(offset int) *QueryBuilder {
	qb.offsetValue = &offset
	return qb
}

// Build constructs the SQL query and returns it with args
func (qb *QueryBuilder) Build() (string, []interface{}) {
	var sb strings.Builder
	qb.args = []interface{}{}
	qb.argCounter = 0

	// SELECT clause
	sb.WriteString("SELECT ")
	if len(qb.columns) == 0 {
		sb.WriteString("*")
	} else {
		quoted := make([]string, len(qb.columns))
		for i, col := range qb.columns {
			if col == "*" || col == "COUNT(*)" || strings.Contains(col, "(") {
				quoted[i] = col
			} else {
				quoted[i] = qb.ctx.Dialect().QuoteIdentifier(col)
			}
		}
		sb.WriteString(strings.Join(quoted, ", "))
	}

	// FROM clause
	sb.WriteString(fmt.Sprintf(" FROM %s", qb.tableName))

	// WHERE clause. Empty-column OR groups are skipped, so the WHERE
	// keyword is only emitted when at least one clause will follow.
	nonEmptyGroups := 0
	for _, grp := range qb.orILikeGroups {
		if len(grp.columns) > 0 {
			nonEmptyGroups++
		}
	}
	if len(qb.whereClauses) > 0 || nonEmptyGroups > 0 {
		sb.WriteString(" WHERE ")
		wrote := 0
		for _, clause := range qb.whereClauses {
			if wrote > 0 {
				sb.WriteString(" AND ")
			}
			qb.buildWhereClause(&sb, clause)
			wrote++
		}
		for _, grp := range qb.orILikeGroups {
			if len(grp.columns) == 0 {
				continue
			}
			if wrote > 0 {
				sb.WriteString(" AND ")
			}
			qb.buildOrILikeGroup(&sb, grp)
			wrote++
		}
	}

	// ORDER BY clause
	if len(qb.orderByClauses) > 0 {
		sb.WriteString(" ORDER BY ")
		for i, clause := range qb.orderByClauses {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(fmt.Sprintf("%s %s", qb.ctx.Dialect().QuoteIdentifier(clause.column), clause.order))
		}
	}

	// LIMIT clause
	if qb.limitValue != nil {
		sb.WriteString(fmt.Sprintf(" LIMIT %s", qb.nextPlaceholder()))
		qb.args = append(qb.args, *qb.limitValue)
	}

	// OFFSET clause
	if qb.offsetValue != nil {
		sb.WriteString(fmt.Sprintf(" OFFSET %s", qb.nextPlaceholder()))
		qb.args = append(qb.args, *qb.offsetValue)
	}

	return sb.String(), qb.args
}

func (qb *QueryBuilder) buildWhereClause(sb *strings.Builder, clause whereClause) {
	quotedCol := qb.ctx.Dialect().QuoteIdentifier(clause.column)
	switch clause.operator {
	case IsNull, IsNotNull:
		sb.WriteString(fmt.Sprintf("%s %s", quotedCol, clause.operator))
	case In, NotIn:
		// Expand slice into individual placeholders
		v := reflect.ValueOf(clause.value)
		if v.Kind() == reflect.Slice {
			placeholders := make([]string, v.Len())
			for i := 0; i < v.Len(); i++ {
				placeholders[i] = qb.nextPlaceholder()
				qb.args = append(qb.args, v.Index(i).Interface())
			}
			sb.WriteString(fmt.Sprintf("%s %s (%s)",
				quotedCol,
				clause.operator,
				strings.Join(placeholders, ", ")))
		} else {
			// Single value fallback
			sb.WriteString(fmt.Sprintf("%s %s (%s)",
				quotedCol,
				clause.operator,
				qb.nextPlaceholder()))
			qb.args = append(qb.args, clause.value)
		}
	case ILike:
		// ILIKE is Postgres-only. For other dialects, use LOWER() LIKE LOWER()
		if qb.ctx.Dialect().Name() == "postgres" {
			sb.WriteString(fmt.Sprintf("%s ILIKE %s", quotedCol, qb.nextPlaceholder()))
		} else {
			sb.WriteString(fmt.Sprintf("LOWER(%s) LIKE LOWER(%s)", quotedCol, qb.nextPlaceholder()))
		}
		qb.args = append(qb.args, clause.value)
	default:
		sb.WriteString(fmt.Sprintf("%s %s %s", quotedCol, clause.operator, qb.nextPlaceholder()))
		qb.args = append(qb.args, clause.value)
	}
}

// buildOrILikeGroup emits one parenthesized OR group of case-insensitive
// LIKE matches. Mirrors the ILike operator's dialect handling: native
// ILIKE on Postgres, LOWER() LIKE LOWER() elsewhere.
func (qb *QueryBuilder) buildOrILikeGroup(sb *strings.Builder, grp orILikeGroup) {
	postgres := qb.ctx.Dialect().Name() == "postgres"
	sb.WriteString("(")
	for i, col := range grp.columns {
		if i > 0 {
			sb.WriteString(" OR ")
		}
		quotedCol := qb.ctx.Dialect().QuoteIdentifier(col)
		if postgres {
			sb.WriteString(fmt.Sprintf("%s ILIKE %s", quotedCol, qb.nextPlaceholder()))
		} else {
			sb.WriteString(fmt.Sprintf("LOWER(%s) LIKE LOWER(%s)", quotedCol, qb.nextPlaceholder()))
		}
		qb.args = append(qb.args, grp.value)
	}
	sb.WriteString(")")
}

// WhereAnyILike adds an OR-joined group of case-insensitive LIKE matches
// against the same value, AND-ed with the other WHERE clauses.
func (qb *QueryBuilder) WhereAnyILike(columns []string, value interface{}) *QueryBuilder {
	qb.orILikeGroups = append(qb.orILikeGroups, orILikeGroup{columns: columns, value: value})
	return qb
}

// Execute runs the query and returns rows
func (qb *QueryBuilder) Execute(ctx context.Context) (*sql.Rows, error) {
	query, args := qb.Build()
	return qb.ctx.Query(ctx, query, args...)
}

// QueryOption is a functional option for List operations
type QueryOption func(*QueryBuilder)

// WithLimit sets the limit for the query
func WithLimit(limit int) QueryOption {
	return func(qb *QueryBuilder) {
		qb.Limit(limit)
	}
}

// WithOffset sets the offset for the query
func WithOffset(offset int) QueryOption {
	return func(qb *QueryBuilder) {
		qb.Offset(offset)
	}
}

// WithOrderBy adds an order by clause
func WithOrderBy(column string, order Order) QueryOption {
	return func(qb *QueryBuilder) {
		qb.OrderBy(column, order)
	}
}

// WithWhere adds a where clause
func WithWhere(column string, op Operator, value interface{}) QueryOption {
	return func(qb *QueryBuilder) {
		qb.Where(column, op, value)
	}
}

// ValidateOrderBy validates a comma-separated ORDER BY clause against a
// column allowlist.
//
// Two layers of validation:
//
//  1. Shape: only identifier characters (letters, digits, underscores)
//     in column names, with an optional ASC/DESC direction token.
//  2. Allowlist: when allowedColumns is non-empty, every column must be
//     one of the declared columns. Shape validation alone is NOT enough:
//     an undeclared-but-identifier-shaped column (`order_by=password_hash`)
//     reaches the database, where some engines (SQLite's double-quoted
//     string fallback) silently treat it as a constant — an ordering
//     no-op the caller never finds out about.
//
// Generated entity code exports its declared column list (e.g.
// db.ItemColumns) precisely so handlers can pass it here. A nil/empty
// allowlist skips layer 2 for callers that genuinely accept any
// identifier (none of forge's generated callers do).
func ValidateOrderBy(clause string, allowedColumns []string) error {
	if clause == "" {
		return nil
	}
	allowed := make(map[string]bool, len(allowedColumns))
	for _, c := range allowedColumns {
		allowed[strings.ToLower(c)] = true
	}
	parts := strings.Split(clause, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return fmt.Errorf("empty order-by clause")
		}
		tokens := strings.Fields(part)
		if len(tokens) == 0 || len(tokens) > 2 {
			return fmt.Errorf("invalid order-by clause: %q", part)
		}
		// Validate column name: only identifier chars
		col := tokens[0]
		for _, r := range col {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
				return fmt.Errorf("invalid character %q in order-by column %q", string(r), col)
			}
		}
		if len(allowed) > 0 && !allowed[strings.ToLower(col)] {
			return fmt.Errorf("unknown order-by column %q (allowed: %s)", col, strings.Join(allowedColumns, ", "))
		}
		// Validate direction if present
		if len(tokens) == 2 {
			dir := strings.ToUpper(tokens[1])
			if dir != "ASC" && dir != "DESC" {
				return fmt.Errorf("invalid order-by direction %q (must be ASC or DESC)", tokens[1])
			}
		}
	}
	return nil
}
