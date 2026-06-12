package orm

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestJSONValueAndScanRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   any
		dst  func() (ptr any, get func() any)
	}{
		{
			name: "string slice",
			in:   []string{"go", "bookmarks", "comma,inside"},
			dst: func() (any, func() any) {
				var v []string
				return &v, func() any { return v }
			},
		},
		{
			name: "int64 slice",
			in:   []int64{1, -2, 9000000000},
			dst: func() (any, func() any) {
				var v []int64
				return &v, func() any { return v }
			},
		},
		{
			name: "bool slice",
			in:   []bool{true, false, true},
			dst: func() (any, func() any) {
				var v []bool
				return &v, func() any { return v }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val, err := JSON(tc.in).Value()
			if err != nil {
				t.Fatalf("Value() error = %v", err)
			}
			ptr, get := tc.dst()
			if err := ScanJSON(ptr).Scan(val); err != nil {
				t.Fatalf("Scan() error = %v", err)
			}
			if !reflect.DeepEqual(get(), tc.in) {
				t.Errorf("round trip = %#v, want %#v", get(), tc.in)
			}
		})
	}
}

func TestJSONNilSliceRoundTripsAsNULL(t *testing.T) {
	var in []string
	val, err := JSON(in).Value()
	if err != nil {
		t.Fatalf("Value() error = %v", err)
	}
	if val != nil {
		t.Errorf("nil slice should store SQL NULL, got %#v", val)
	}
	var out []string
	if err := ScanJSON(&out).Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error = %v", err)
	}
	if out != nil {
		t.Errorf("NULL should scan to nil slice, got %#v", out)
	}
}

func TestJSONScanAcceptsBytesAndString(t *testing.T) {
	for _, src := range []any{[]byte(`["a","b"]`), `["a","b"]`} {
		var out []string
		if err := ScanJSON(&out).Scan(src); err != nil {
			t.Fatalf("Scan(%T) error = %v", src, err)
		}
		if !reflect.DeepEqual(out, []string{"a", "b"}) {
			t.Errorf("Scan(%T) = %#v", src, out)
		}
	}
}

// TestJSONThroughSQLite drives the helpers through a real database/sql
// driver — the same path the generated scan/insert code uses — so the
// "driver.Valuer in, sql.Scanner out" contract is pinned against an
// actual driver, not just direct calls.
func TestJSONThroughSQLite(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE bookmarks (id TEXT PRIMARY KEY, tags JSONB)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	tags := []string{"go", "orm"}
	if _, err := db.ExecContext(ctx, `INSERT INTO bookmarks (id, tags) VALUES (?, ?)`, "b1", JSON(tags)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var got []string
	if err := db.QueryRowContext(ctx, `SELECT tags FROM bookmarks WHERE id = ?`, "b1").Scan(ScanJSON(&got)); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !reflect.DeepEqual(got, tags) {
		t.Errorf("round trip through sqlite = %#v, want %#v", got, tags)
	}

	// NULL column → zero value.
	if _, err := db.ExecContext(ctx, `INSERT INTO bookmarks (id, tags) VALUES (?, ?)`, "b2", JSON([]string(nil))); err != nil {
		t.Fatalf("insert nil: %v", err)
	}
	var empty []string
	if err := db.QueryRowContext(ctx, `SELECT tags FROM bookmarks WHERE id = ?`, "b2").Scan(ScanJSON(&empty)); err != nil {
		t.Fatalf("scan nil: %v", err)
	}
	if empty != nil {
		t.Errorf("NULL tags should scan to nil, got %#v", empty)
	}
}
