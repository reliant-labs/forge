package migrationlint

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBlockingIndexBuild pins NS FG-35: a plain CREATE INDEX on a table an
// earlier migration created is a warning; a concurrent build, or an index on
// a table the same file creates, is not.
func TestBlockingIndexBuild(t *testing.T) {
	cases := []struct {
		name, sql string
		want      int
	}{
		{"plain index on an existing table", "CREATE INDEX idx_products_name ON products (name);", 1},
		{"unique, if not exists, schema-qualified", "CREATE UNIQUE INDEX IF NOT EXISTS ux ON public.products (sku);", 1},
		{"unnamed index", "create index on products (name);", 1},
		{"concurrently", "CREATE INDEX CONCURRENTLY idx_products_name ON products (name);", 0},
		{"table created in the same file", "CREATE TABLE products (id text PRIMARY KEY, name text);\nCREATE INDEX idx_products_name ON products (name);", 0},
		{"quoted table created in the same file", "CREATE TABLE IF NOT EXISTS public.\"Products\" (id text);\nCREATE INDEX i ON \"Products\" (id);", 0},
		{"another table created in the same file", "CREATE TABLE orders (id text);\nCREATE INDEX i ON products (name);", 1},
		{"in a comment", "-- CREATE INDEX i ON products (name);\nSELECT 1;", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []Finding
			for _, f := range lintStatements("m.up.sql", tc.sql, DefaultConfig()) {
				if f.Rule == RuleBlockingIndexBuild {
					got = append(got, f)
				}
			}
			if len(got) != tc.want {
				t.Fatalf("findings = %+v, want %d", got, tc.want)
			}
			for _, f := range got {
				if f.Severity != SeverityWarn {
					t.Errorf("severity = %q, want a warning: static lint cannot see the table's size", f.Severity)
				}
			}
		})
	}
}

// The rule runs through the directory pass and leaves the verdict green.
func TestBlockingIndexBuildIsAWarningInTheDirectoryPass(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("20260101000000_products.up.sql", "-- forge:backward-compatible — new table\nCREATE TABLE products (id text PRIMARY KEY, name text);\n")
	write("20260102000000_index_products_name.up.sql", "-- forge:backward-compatible — an index\nCREATE INDEX idx_products_name ON products (name);\n")
	res, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range res.Findings {
		if f.Rule == RuleBlockingIndexBuild {
			found = true
			if f.Line != 2 {
				t.Errorf("line = %d, want 2", f.Line)
			}
		}
	}
	if !found || res.HasErrors() {
		t.Fatalf("findings = %+v; want one blocking-index-build warning and no error", res.Findings)
	}
}
