package orm

import (
	"testing"
)

// applyOption applies a QueryOption to a minimal QueryBuilder and returns the resulting where clause.
func applyOption(t *testing.T, opt QueryOption) whereClause {
	t.Helper()
	qb := &QueryBuilder{}
	opt(qb)
	if len(qb.whereClauses) != 1 {
		t.Fatalf("expected 1 where clause, got %d", len(qb.whereClauses))
	}
	return qb.whereClauses[0]
}

func TestWhereEq(t *testing.T) {
	wc := applyOption(t, WhereEq("name", "alice"))
	if wc.column != "name" || wc.operator != Eq || wc.value != "alice" {
		t.Errorf("unexpected clause: %+v", wc)
	}
}

func TestWhereNotEq(t *testing.T) {
	wc := applyOption(t, WhereNotEq("status", "deleted"))
	if wc.column != "status" || wc.operator != NotEq || wc.value != "deleted" {
		t.Errorf("unexpected clause: %+v", wc)
	}
}

func TestWhereGt(t *testing.T) {
	wc := applyOption(t, WhereGt("age", 18))
	if wc.column != "age" || wc.operator != GreaterThan || wc.value != 18 {
		t.Errorf("unexpected clause: %+v", wc)
	}
}

func TestWhereGte(t *testing.T) {
	wc := applyOption(t, WhereGte("age", 21))
	if wc.column != "age" || wc.operator != GreaterThanOrEq || wc.value != 21 {
		t.Errorf("unexpected clause: %+v", wc)
	}
}

func TestWhereLt(t *testing.T) {
	wc := applyOption(t, WhereLt("price", 100))
	if wc.column != "price" || wc.operator != LessThan || wc.value != 100 {
		t.Errorf("unexpected clause: %+v", wc)
	}
}

func TestWhereLte(t *testing.T) {
	wc := applyOption(t, WhereLte("price", 50))
	if wc.column != "price" || wc.operator != LessThanOrEq || wc.value != 50 {
		t.Errorf("unexpected clause: %+v", wc)
	}
}

func TestWhereLike(t *testing.T) {
	wc := applyOption(t, WhereLike("name", "%bob%"))
	if wc.column != "name" || wc.operator != Like || wc.value != "%bob%" {
		t.Errorf("unexpected clause: %+v", wc)
	}
}

func TestWhereILike(t *testing.T) {
	wc := applyOption(t, WhereILike("email", "%@example%"))
	if wc.column != "email" || wc.operator != ILike || wc.value != "%@example%" {
		t.Errorf("unexpected clause: %+v", wc)
	}
}

func TestWhereIn(t *testing.T) {
	vals := []int{1, 2, 3}
	wc := applyOption(t, WhereIn("id", vals))
	if wc.column != "id" || wc.operator != In {
		t.Errorf("unexpected clause: %+v", wc)
	}
}

func TestWhereNotIn(t *testing.T) {
	vals := []string{"a", "b"}
	wc := applyOption(t, WhereNotIn("code", vals))
	if wc.column != "code" || wc.operator != NotIn {
		t.Errorf("unexpected clause: %+v", wc)
	}
}

func TestWhereIsNull(t *testing.T) {
	wc := applyOption(t, WhereIsNull("deleted_at"))
	if wc.column != "deleted_at" || wc.operator != IsNull || wc.value != nil {
		t.Errorf("unexpected clause: %+v", wc)
	}
}

func TestWhereIsNotNull(t *testing.T) {
	wc := applyOption(t, WhereIsNotNull("verified_at"))
	if wc.column != "verified_at" || wc.operator != IsNotNull || wc.value != nil {
		t.Errorf("unexpected clause: %+v", wc)
	}
}
