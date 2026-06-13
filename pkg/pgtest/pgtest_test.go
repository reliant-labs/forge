package pgtest_test

import (
	"testing"

	"github.com/reliant-labs/forge/pkg/pgtest"
)

// TestNew_RealPostgres boots the shared embedded postgres (or the
// FORGE_TEST_POSTGRES_URL server), creates an isolated database, and
// proves real-postgres DDL the SQLite shadow could never run — a
// schema-qualified table, TIMESTAMPTZ/JSONB/TEXT[] columns, a '::type'
// cast default — applies and round-trips. Skipped under -short: it
// boots a real server (download on first run).
func TestNew_RealPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("boots real postgres; skipped under -short")
	}
	db, cleanup, err := pgtest.New()
	if err != nil {
		t.Fatalf("pgtest.New: %v", err)
	}
	defer cleanup()

	stmts := []string{
		`CREATE SCHEMA controlplane`,
		`CREATE TABLE controlplane.widget (
			id BIGSERIAL PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			tags TEXT[] NOT NULL DEFAULT '{}'::text[],
			meta JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`INSERT INTO controlplane.widget (tenant_id) VALUES ('t1')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM controlplane.widget`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("row count = %d, want 1", n)
	}
}
