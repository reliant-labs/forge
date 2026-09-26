package compat

import (
	"testing"
	"testing/fstest"
)

// TestDeclaredMatchesOnlyALineComment pins what counts as a declaration.
//
// The negative cases are the ones that matter. A migration that DISCUSSES
// compatibility in a block comment, or a string literal that happens to
// contain the words, must not be read as having claimed it: a false positive
// here is a rollback forge waves through onto a schema the older code cannot
// run against.
func TestDeclaredMatchesOnlyALineComment(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{"exact", Directive + "\nALTER TABLE t ADD COLUMN c TEXT;", true},
		{"indented and spaced", "  --   forge:backward-compatible\nSELECT 1;", true},
		{"case-insensitive", "-- FORGE:BACKWARD-COMPATIBLE\n", true},
		{"after other statements", "ALTER TABLE t ADD COLUMN c TEXT;\n-- forge:backward-compatible\n", true},
		{"absent", "ALTER TABLE t ADD COLUMN c TEXT;", false},
		{"inside a block comment", "/*\n forge:backward-compatible \n*/\nSELECT 1;", false},
		{"inside a string", "SELECT '-- forge:backward-compatible';", false},
		{"with a rationale after it", "-- forge:backward-compatible — additive column, N-1 never reads it\n", true},
		{"a longer word", "-- forge:backward-compatible-ish\n", false},
		{"trailing on a statement", "SELECT 1; -- forge:backward-compatible", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Declared(tc.sql); got != tc.want {
				t.Errorf("Declared(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

// TestScanOrdersByVersionAndReadsOnlyUpFiles covers the file grammar: only
// `<digits>_<name>.up.<ext>` files are migrations, down files never carry the
// contract (it is a statement about what the UP leaves behind), and the order
// is numeric rather than lexical — 10 sorts after 9.
func TestScanOrdersByVersionAndReadsOnlyUpFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/10_ten.up.sql":    {Data: []byte(Directive + "\n")},
		"migrations/10_ten.down.sql":  {Data: []byte(Directive + "\n")},
		"migrations/9_nine.up.sql":    {Data: []byte("DROP TABLE x;")},
		"migrations/.gitkeep":         {Data: nil},
		"migrations/README.md":        {Data: []byte("# notes")},
		"migrations/00001_one.up.sql": {Data: []byte("CREATE TABLE a (id INT);")},
	}
	got, err := Scan(fsys, "migrations")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []Migration{
		{Version: 1, Name: "00001_one.up.sql"},
		{Version: 9, Name: "9_nine.up.sql"},
		{Version: 10, Name: "10_ten.up.sql", BackwardCompatible: true},
	}
	if len(got) != len(want) {
		t.Fatalf("Scan = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Scan[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestScanDistinguishesMissingFromEmpty: no migrations is a normal state, a
// directory that cannot be read is a packaging bug — they must not collapse.
func TestScanDistinguishesMissingFromEmpty(t *testing.T) {
	if got, err := Scan(fstest.MapFS{"migrations/.gitkeep": {}}, "migrations"); err != nil || len(got) != 0 {
		t.Errorf("empty dir: Scan = %v, %v; want no migrations and no error", got, err)
	}
	if _, err := Scan(fstest.MapFS{}, "migrations"); err == nil {
		t.Error("missing dir: Scan returned no error; an unreadable migration set must not read as empty")
	}
}

// TestCompareIsByMembershipNotMaxVersion is the control-plane v1.7.0 shape
// (91 additive + declared, 92 a drop that is not) plus the case a max-version
// comparison gets wrong: a migration numbered BELOW the older release's newest
// that the older release never had (merged out of order). The older binary
// has not seen it, so it is part of what a rollback crosses.
func TestCompareIsByMembershipNotMaxVersion(t *testing.T) {
	older := []Migration{{Version: 88}, {Version: 90}}
	newer := []Migration{
		{Version: 88},
		{Version: 89, Name: "89_out_of_order.up.sql"},
		{Version: 90},
		{Version: 91, Name: "91_add.up.sql", BackwardCompatible: true},
		{Version: 92, Name: "92_drop.up.sql"},
	}
	d := Compare(newer, older)
	var ahead []uint
	for _, m := range d.Ahead {
		ahead = append(ahead, m.Version)
	}
	if len(ahead) != 3 || ahead[0] != 89 || ahead[1] != 91 || ahead[2] != 92 {
		t.Fatalf("Ahead = %v, want [89 91 92]", ahead)
	}
	bad := d.Incompatible()
	if len(bad) != 2 || bad[0].Version != 89 || bad[1].Version != 92 {
		t.Errorf("Incompatible = %+v, want 89 and 92 (91 declared itself)", bad)
	}
	if got := Compare(older, older).Ahead; len(got) != 0 {
		t.Errorf("same set: Ahead = %+v, want none", got)
	}
}
