package orm

import (
	"database/sql/driver"
	"testing"

	"github.com/lib/pq"
)

// TestArrayValue_NilSlicesBindEmptyArray pins the fix for the NOT NULL
// array-column class: a nil slice must bind an empty postgres array
// (`{}`), never NULL — array columns are conventionally
// `NOT NULL DEFAULT '{}'`, so binding NULL violates the constraint and
// fails every INSERT that omits the optional slice. Non-nil slices bind
// their elements.
func TestArrayValue_NilSlicesBindEmptyArray(t *testing.T) {
	// The driver.Value of pq.Array is the postgres array literal; assert on
	// that text to prove nil → "{}" and populated → "{...}".
	literal := func(v any) string {
		val, err := ArrayValue(nil, v).(driver.Valuer).Value()
		if err != nil {
			t.Fatalf("Value(): %v", err)
		}
		s, ok := val.(string)
		if !ok {
			t.Fatalf("array driver value = %T, want string literal", val)
		}
		return s
	}

	var nilStrings []string
	if got := literal(nilStrings); got != "{}" {
		t.Errorf("nil []string → %q, want \"{}\" (NOT NULL array columns reject NULL)", got)
	}
	var nilStringArray StringArray
	if got := literal(nilStringArray); got != "{}" {
		t.Errorf("nil StringArray → %q, want \"{}\"", got)
	}
	var nilInts []int64
	if got := literal(nilInts); got != "{}" {
		t.Errorf("nil []int64 → %q, want \"{}\"", got)
	}
	if got := literal([]string{"a", "b"}); got != `{"a","b"}` {
		t.Errorf("populated []string → %q, want %q", got, `{"a","b"}`)
	}
}

// TestArrayValue_RoundTripThroughStringArray proves the write encoding
// (pq.Array literal) round-trips through the StringArray scanner — the
// generated write/read pair must agree on the postgres text format.
func TestArrayValue_RoundTripThroughStringArray(t *testing.T) {
	in := []string{"go", "sql", "comma,inside"}
	lit, err := ArrayValue(nil, in).(driver.Valuer).Value()
	if err != nil {
		t.Fatalf("Value(): %v", err)
	}
	var out StringArray
	if err := out.Scan(lit); err != nil {
		t.Fatalf("Scan(%v): %v", lit, err)
	}
	if len(out) != len(in) {
		t.Fatalf("round-trip length = %d, want %d (%v)", len(out), len(in), out)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("element %d = %q, want %q (embedded comma must survive)", i, out[i], in[i])
		}
	}
}

// guard: pq.Array of a nil slice would otherwise bind NULL — this is the
// behavior ArrayValue corrects, documented here so the rationale is clear.
func TestArrayValue_PqArrayNilWouldBeNull(t *testing.T) {
	var nilStrings []string
	val, err := pq.Array(nilStrings).(driver.Valuer).Value()
	if err != nil {
		t.Fatalf("Value(): %v", err)
	}
	if val != nil {
		t.Logf("note: pq.Array(nil) bound %v (%T); ArrayValue overrides to {}", val, val)
	}
}
