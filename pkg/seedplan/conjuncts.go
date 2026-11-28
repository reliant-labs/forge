package seedplan

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/pkg/schemadef"
)

// Per-column facts stated INSIDE a CHECK: its top-level AND terms.
//
// An AND is true only when every term is, so a term that names one column and
// a literal is a requirement on that column alone, whatever else the
// constraint says beside it. Two such terms defeated the seeder in one real
// schema (reliant's machine_pools):
//
//	selector jsonb NOT NULL DEFAULT '{}'::jsonb,
//	CONSTRAINT machine_pools_selector_present CHECK (selector <> '{}'::jsonb),
//	CONSTRAINT machine_pools_bounds CHECK (min_members >= 0 AND max_members >= 0
//	    AND max_runs_per_member >= 0 AND (max_members = 0 OR min_members <= max_members))
//
//   - The selector CHECK EXCLUDES the very literal the column's DEFAULT holds.
//     No pass read an exclusion, so a minimal row (a test factory's FK parent)
//     left the column to its DEFAULT, a full row wrote the DEFAULT's document,
//     and both inserted the one value the CHECK forbids. Every generated
//     create-request factory whose parents reached the table failed with
//     "violates check constraint machine_pools_selector_present".
//   - The bounds CHECK is an ordering (min_members <= max_members, guarded)
//     beside three single-column bounds. The ordering pass read only
//     column-to-column comparisons, so the literal terms made it refuse the
//     whole constraint as "not a two-column ordering comparison", and the
//     bounds pass read only single-column CHECKs, so the literal terms were
//     not honored either.
//
// So both are read here, term by term: an excluded literal (`col <> lit`, and
// the NOT IN / `<> ALL (ARRAY[…])` spellings) wherever it is a top-level term
// of any CHECK, and a numeric bound (`col >= N`, …) when it is a top-level
// term of a multi-column CHECK (a single-column CHECK's bound is
// boundFromCheckDef's, as before). The ordering pass then steps over a bound
// term, because the bounds model now owns it.

// checkConjuncts returns the top-level AND terms of a CHECK definition, each
// unwrapped. ok is false when the definition is not a CHECK.
func checkConjuncts(def string) ([]string, bool) {
	body, ok := checkBody(def)
	if !ok {
		return nil, false
	}
	parts := splitTopLevel(unwrapParens(body), "AND")
	for i := range parts {
		parts[i] = unwrapParens(parts[i])
	}
	return parts, true
}

// conjunctBound reads one term as a numeric bound on a single column — `col
// >= 0`, `col < 10` — in postgres's canonical spelling. ok is false for every
// other term, including a column-to-column comparison (an ordering edge).
func conjunctBound(term string) (string, NumBound, bool) {
	m := unionCmpRE.FindStringSubmatch(term)
	if m == nil {
		return "", NumBound{}, false
	}
	b, ok := unionCmpBound(m[2], strings.TrimSpace(m[3]))
	if !ok {
		return "", NumBound{}, false
	}
	return m[1], b, true
}

// multiColumnConjunctBounds returns the per-column bounds a MULTI-column
// CHECK states as top-level terms. A term naming a column the constraint does
// not span is not what the matcher thought it was and is ignored.
func multiColumnConjunctBounds(ck schemadef.CheckConstraint) map[string]NumBound {
	if len(ck.Columns) < 2 {
		return nil
	}
	terms, ok := checkConjuncts(ck.Def)
	if !ok {
		return nil
	}
	var out map[string]NumBound
	for _, term := range terms {
		col, b, ok := conjunctBound(term)
		if !ok || !spansColumn(ck, col) {
			continue
		}
		if out == nil {
			out = map[string]NumBound{}
		}
		out[col] = intersectBounds(out[col], b)
	}
	return out
}

// conjunctBoundsRefusal rejects empty intervals on NOT NULL columns before
// synthesis can clamp a value into a range that cannot satisfy the CHECK.
// Nullable columns can satisfy the CHECK with NULL (UNKNOWN), even when no
// non-NULL value satisfies their bounds.
func conjunctBoundsRefusal(tables []schemadef.Table) error {
	for _, t := range tables {
		for _, ck := range t.Checks {
			bounds := multiColumnConjunctBounds(ck)
			for _, col := range t.Columns {
				b := bounds[col.Name]
				if col.NotNull && b.Min != nil && b.Max != nil && *b.Min > *b.Max {
					return fmt.Errorf("seeddata: %s CHECK %s has contradictory bounds on %s: minimum %d exceeds maximum %d", t.Name, ck.Name, col.Name, *b.Min, *b.Max)
				}
			}
		}
	}
	return nil
}

// spansColumn reports whether postgres says ck spans col.
func spansColumn(ck schemadef.CheckConstraint, col string) bool {
	for _, c := range ck.Columns {
		if c == col {
			return true
		}
	}
	return false
}

// conjunctBoundsFromTables is the multi-column half of BoundsFromTables: the
// bounds stated as top-level terms of multi-column CHECKs. The live
// introspector reads only single-column CHECKs, so the live plan merges this
// in as well (see planFromTables).
func conjunctBoundsFromTables(tables []schemadef.Table) CheckBounds {
	out := CheckBounds{}
	for _, t := range tables {
		for _, ck := range t.Checks {
			for col, b := range multiColumnConjunctBounds(ck) {
				if out[t.Name] == nil {
					out[t.Name] = map[string]NumBound{}
				}
				out[t.Name][col] = intersectBounds(out[t.Name][col], b)
			}
		}
	}
	return out
}

// mergeCheckBounds returns a with every bound of b intersected in.
func mergeCheckBounds(a, b CheckBounds) CheckBounds {
	if a == nil {
		a = CheckBounds{}
	}
	for table, cols := range b {
		for col, nb := range cols {
			if a[table] == nil {
				a[table] = map[string]NumBound{}
			}
			a[table][col] = intersectBounds(a[table][col], nb)
		}
	}
	return a
}

// ExcludedLiterals returns the raw values the table's CHECKs forbid col to
// hold: every `col <> 'x'`, `col NOT IN (…)` or `col <> ALL (ARRAY[…])` that
// is a top-level AND term of a CHECK (a whole single-term CHECK included).
// A term under an OR is not an exclusion — the OR may be satisfied elsewhere —
// and is left to the union and guard passes, which read it as a branch.
//
// Exported for codegen's lifecycle-test fixtures, which mint create requests
// against the same schema and must not pick an excluded value either.
func ExcludedLiterals(t schemadef.Table, col string) []string {
	var out []string
	for _, ck := range t.Checks {
		if !spansColumn(ck, col) {
			continue
		}
		terms, ok := checkConjuncts(ck.Def)
		if !ok {
			continue
		}
		for _, term := range terms {
			name, listed, listedAreExempt, ok := parseGuardNegative(term)
			if !ok || listedAreExempt || name != col {
				continue
			}
			for _, v := range listed {
				out = appendDistinct(out, v)
			}
		}
	}
	return out
}

// literalExcluded reports whether raw is one of the excluded values, compared
// the way the column's type compares: a JSON document by its parsed value
// (`{"a":1}` and `{ "a": 1 }` are one jsonb), a number numerically, anything
// else by its text.
func literalExcluded(col schemadef.Column, raw string, excluded []string) bool {
	for _, ex := range excluded {
		if sameColumnValue(col, raw, ex) {
			return true
		}
	}
	return false
}

func sameColumnValue(col schemadef.Column, a, b string) bool {
	if a == b {
		return true
	}
	switch col.Type {
	case schemadef.TypeJSON:
		var va, vb any
		da, db := json.NewDecoder(strings.NewReader(a)), json.NewDecoder(strings.NewReader(b))
		da.UseNumber()
		db.UseNumber()
		if json.Valid([]byte(a)) && json.Valid([]byte(b)) && da.Decode(&va) == nil && db.Decode(&vb) == nil {
			return sameJSONValue(va, vb)
		}
	case schemadef.TypeInt:
		// PostgreSQL integers are signed and at most 64 bits. Do not round
		// bigint literals through float64 (which loses integers above 2^53).
		ia, errA := strconv.ParseInt(a, 10, 64)
		ib, errB := strconv.ParseInt(b, 10, 64)
		return errA == nil && errB == nil && ia == ib
	case schemadef.TypeFloat:
		decl, _, _ := strings.Cut(strings.ToUpper(strings.TrimSpace(col.DeclType)), "(")
		if decl = strings.TrimSpace(decl); decl == "NUMERIC" || decl == "DECIMAL" {
			// These share the canonical float type, but SQL compares their
			// decimal values exactly, including fractional and exponent forms.
			ra, okA := new(big.Rat).SetString(a)
			rb, okB := new(big.Rat).SetString(b)
			return okA && okB && ra.Cmp(rb) == 0
		}
		fa, errA := strconv.ParseFloat(a, 64)
		fb, errB := strconv.ParseFloat(b, 64)
		return errA == nil && errB == nil && fa == fb
	}
	return false
}

// sameJSONValue compares decoded JSON recursively, retaining PostgreSQL's exact
// numeric equality even inside objects and arrays (1, 1.0 and 1e0 are equal).
func sameJSONValue(a, b any) bool {
	switch a := a.(type) {
	case json.Number:
		b, ok := b.(json.Number)
		if !ok {
			return false
		}
		ra, okA := new(big.Rat).SetString(string(a))
		rb, okB := new(big.Rat).SetString(string(b))
		return okA && okB && ra.Cmp(rb) == 0
	case []any:
		b, ok := b.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i := range a {
			if !sameJSONValue(a[i], b[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		b, ok := b.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for key, value := range a {
			other, exists := b[key]
			if !exists || !sameJSONValue(value, other) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

// CheckExcludes reports whether the table's CHECKs forbid col to hold raw
// (see ExcludedLiterals), comparing as the column's type compares. Exported
// for codegen's lifecycle-test fixtures.
func CheckExcludes(t schemadef.Table, col schemadef.Column, raw string) bool {
	return literalExcluded(col, raw, ExcludedLiterals(t, col.Name))
}
