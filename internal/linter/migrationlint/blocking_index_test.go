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
		{"quoted table created in the same file", "CREATE TABLE public.\"Products\" (id text);\nCREATE INDEX i ON \"Products\" (id);", 0},
		{"another table created in the same file", "CREATE TABLE orders (id text);\nCREATE INDEX i ON products (name);", 1},
		{"in a comment", "-- CREATE INDEX i ON products (name);\nSELECT 1;", 0},
		// Emptiness is proved only up to the first statement that could
		// write a row: an INSERT (or any DML, or a function call) before the
		// index ends it.
		{"created, then filled, then indexed", "CREATE TABLE products (id text, name text);\nINSERT INTO products VALUES ('a', 'b');\nCREATE INDEX i ON products (name);", 1},
		{"created, a function call, then indexed", "CREATE TABLE products (id text);\nSELECT backfill_products();\nCREATE INDEX i ON products (id);", 1},
		{"created, row-free DDL, then indexed", "CREATE TABLE products (id text);\nALTER TABLE products ADD COLUMN name text;\nCOMMENT ON TABLE products IS 'x';\nCREATE INDEX i ON products (name);", 0},
		{"indexed before the file creates it", "CREATE INDEX i ON products (name);\nCREATE TABLE IF NOT EXISTS products (id text, name text);", 1},
		{"CREATE TABLE IF NOT EXISTS proves nothing", "CREATE TABLE IF NOT EXISTS products (id text);\nCREATE INDEX i ON products (id);", 1},
		{"CREATE TABLE AS creates it full", "CREATE TABLE products AS SELECT * FROM old_products;\nCREATE INDEX i ON products (id);", 1},
		{"a generated column is not CREATE TABLE AS", "CREATE TABLE products (id text, n int GENERATED ALWAYS AS (1) STORED);\nCREATE INDEX i ON products (id);", 0},
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
