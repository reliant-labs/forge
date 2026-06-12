package schemadef

import (
	"os"
	"path/filepath"
	"testing"
)

func writeMig(t *testing.T, dir, name, sql string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(sql), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApplyAndIntrospect_PostgresFlavoredDDL(t *testing.T) {
	dir := t.TempDir()
	writeMig(t, dir, "00001_create_bookmarks.up.sql", `
CREATE TABLE bookmarks (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    url TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL DEFAULT '',
    tags TEXT[] NOT NULL DEFAULT '{}',
    visits BIGINT NOT NULL DEFAULT 0,
    score DOUBLE PRECISION NOT NULL DEFAULT 0,
    done BOOLEAN NOT NULL DEFAULT FALSE,
    meta JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT (now()),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT (now()),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX idx_bookmarks_url ON bookmarks (url);
`)
	// Down files must be ignored.
	writeMig(t, dir, "00001_create_bookmarks.down.sql", `DROP TABLE bookmarks;`)

	tables, err := ApplyAndIntrospect(dir)
	if err != nil {
		t.Fatalf("ApplyAndIntrospect: %v", err)
	}
	if len(tables) != 1 || tables[0].Name != "bookmarks" {
		t.Fatalf("tables = %+v, want exactly [bookmarks]", tables)
	}
	bt := tables[0]

	wantTypes := map[string]struct {
		typ     CanonicalType
		isArray bool
		notNull bool
	}{
		"id":         {TypeString, false, true},
		"url":        {TypeString, false, true},
		"title":      {TypeString, false, true},
		"tags":       {TypeString, true, true},
		"visits":     {TypeInt, false, true},
		"score":      {TypeFloat, false, true},
		"done":       {TypeBool, false, true},
		"meta":       {TypeJSON, false, true},
		"created_at": {TypeTime, false, true},
		"updated_at": {TypeTime, false, true},
		"deleted_at": {TypeTime, false, false},
	}
	if len(bt.Columns) != len(wantTypes) {
		t.Fatalf("got %d columns, want %d: %+v", len(bt.Columns), len(wantTypes), bt.Columns)
	}
	for _, col := range bt.Columns {
		w, ok := wantTypes[col.Name]
		if !ok {
			t.Errorf("unexpected column %q", col.Name)
			continue
		}
		if col.Type != w.typ || col.IsArray != w.isArray || col.NotNull != w.notNull {
			t.Errorf("column %s = {type:%s array:%v notnull:%v}, want %+v", col.Name, col.Type, col.IsArray, col.NotNull, w)
		}
	}
	if len(bt.PKCols) != 1 || bt.PKCols[0] != "id" {
		t.Errorf("PKCols = %v, want [id]", bt.PKCols)
	}
	if len(bt.Indexes) != 1 || bt.Indexes[0].Name != "idx_bookmarks_url" || bt.Indexes[0].Columns[0] != "url" {
		t.Errorf("Indexes = %+v, want idx_bookmarks_url(url)", bt.Indexes)
	}

	conv := DetectConventions(bt)
	if !conv.SoftDelete || !conv.Timestamps {
		t.Errorf("conventions = %+v, want SoftDelete+Timestamps", conv)
	}
	wantSearch := []string{"url", "title"}
	if len(conv.SearchColumns) != 2 || conv.SearchColumns[0] != wantSearch[0] || conv.SearchColumns[1] != wantSearch[1] {
		t.Errorf("SearchColumns = %v, want %v", conv.SearchColumns, wantSearch)
	}
}

func TestApplyAndIntrospect_MigrationLadderWithDataMovement(t *testing.T) {
	dir := t.TempDir()
	writeMig(t, dir, "00001_create_people.up.sql", `
CREATE TABLE people (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    name TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT (now()),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT (now())
);
`)
	// Second migration: add columns + data movement splitting a column,
	// then drop the old column. The UPDATE is plain portable SQL and
	// must execute on the shadow.
	writeMig(t, dir, "00002_split_name.up.sql", `
ALTER TABLE people ADD COLUMN first_name TEXT NOT NULL DEFAULT '';
ALTER TABLE people ADD COLUMN last_name TEXT NOT NULL DEFAULT '';
UPDATE people SET first_name = substr(name, 1, instr(name, ' ') - 1),
                  last_name  = substr(name, instr(name, ' ') + 1);
ALTER TABLE people DROP COLUMN name;
`)
	tables, err := ApplyAndIntrospect(dir)
	if err != nil {
		t.Fatalf("ApplyAndIntrospect: %v", err)
	}
	cols := map[string]bool{}
	for _, c := range tables[0].Columns {
		cols[c.Name] = true
	}
	if cols["name"] {
		t.Error("dropped column `name` still present — ALTER TABLE DROP COLUMN not applied")
	}
	if !cols["first_name"] || !cols["last_name"] {
		t.Errorf("added columns missing: %v", cols)
	}
}

func TestApplyAndIntrospect_SkipsNonSchemaPostgresisms(t *testing.T) {
	dir := t.TempDir()
	writeMig(t, dir, "00001_init.up.sql", `
CREATE TABLE things (
    id TEXT PRIMARY KEY,
    payload JSONB NOT NULL DEFAULT '{}'
);
-- Postgres-only auxiliary DDL: must be skipped, not fatal.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
COMMENT ON TABLE things IS 'a comment';
CREATE OR REPLACE FUNCTION touch_updated() RETURNS trigger AS $fn$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;
-- Postgres-only DML that the shadow can't run: skipped.
INSERT INTO things (id, payload) VALUES ('x', '{"a":1}'::jsonb);
`)
	tables, err := ApplyAndIntrospect(dir)
	if err != nil {
		t.Fatalf("ApplyAndIntrospect should skip non-schema postgresisms: %v", err)
	}
	if len(tables) != 1 || tables[0].Name != "things" {
		t.Fatalf("tables = %+v", tables)
	}
}

func TestApplyAndIntrospect_FailsLoudOnBrokenTableDDL(t *testing.T) {
	dir := t.TempDir()
	writeMig(t, dir, "00001_bad.up.sql", `
CREATE TABLE broken (
    id TEXT PRIMARY KEY,
    meta JSONB NOT NULL DEFAULT '{}'::jsonb
);
`)
	if _, err := ApplyAndIntrospect(dir); err == nil {
		t.Fatal("a CREATE TABLE the shadow can't parse must be a hard error, not a silent skip")
	}
}

func TestApplyAndIntrospect_EmptyOrMissingDir(t *testing.T) {
	tables, err := ApplyAndIntrospect(filepath.Join(t.TempDir(), "nope"))
	if err != nil || tables != nil {
		t.Fatalf("missing dir: got (%v, %v), want (nil, nil)", tables, err)
	}
}

func TestSplitStatements(t *testing.T) {
	got := SplitStatements(`
BEGIN;
CREATE TABLE a (x TEXT DEFAULT 'semi;colon');
CREATE TRIGGER trg AFTER INSERT ON a BEGIN UPDATE a SET x = 'y'; END;
COMMIT;
`)
	if len(got) != 4 {
		t.Fatalf("got %d statements, want 4:\n%q", len(got), got)
	}
}

func TestMapDeclaredType(t *testing.T) {
	cases := []struct {
		decl  string
		want  CanonicalType
		array bool
	}{
		{"TEXT", TypeString, false},
		{"varchar(255)", TypeString, false},
		{"UUID", TypeString, false},
		{"BIGSERIAL", TypeInt, false},
		{"DOUBLE PRECISION", TypeFloat, false},
		{"NUMERIC(10,2)", TypeFloat, false},
		{"TIMESTAMPTZ", TypeTime, false},
		{"timestamp with time zone", TypeTime, false},
		{"JSONB", TypeJSON, false},
		{"TEXT[]", TypeString, true},
		{"BIGINT[]", TypeInt, true},
		{"mood_enum", TypeString, false}, // unknown → string
		{"", TypeString, false},          // sqlite untyped → string
	}
	for _, c := range cases {
		got, arr := MapDeclaredType(c.decl)
		if got != c.want || arr != c.array {
			t.Errorf("MapDeclaredType(%q) = (%s,%v), want (%s,%v)", c.decl, got, arr, c.want, c.array)
		}
	}
}
