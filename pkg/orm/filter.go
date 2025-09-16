package orm

// Filter convenience functions that return QueryOption.
// Each wraps WithWhere using the appropriate Operator.

func WhereEq(column string, value any) QueryOption {
	return WithWhere(column, Eq, value)
}

func WhereNotEq(column string, value any) QueryOption {
	return WithWhere(column, NotEq, value)
}

func WhereGt(column string, value any) QueryOption {
	return WithWhere(column, GreaterThan, value)
}

func WhereGte(column string, value any) QueryOption {
	return WithWhere(column, GreaterThanOrEq, value)
}

func WhereLt(column string, value any) QueryOption {
	return WithWhere(column, LessThan, value)
}

func WhereLte(column string, value any) QueryOption {
	return WithWhere(column, LessThanOrEq, value)
}

func WhereLike(column string, value any) QueryOption {
	return WithWhere(column, Like, value)
}

func WhereILike(column string, value any) QueryOption {
	return WithWhere(column, ILike, value)
}

func WhereIn(column string, values any) QueryOption {
	return WithWhere(column, In, values)
}

func WhereNotIn(column string, values any) QueryOption {
	return WithWhere(column, NotIn, values)
}

func WhereIsNull(column string) QueryOption {
	return WithWhere(column, IsNull, nil)
}

func WhereIsNotNull(column string) QueryOption {
	return WithWhere(column, IsNotNull, nil)
}
