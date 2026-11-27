// File: pkg/seedplan/check_excluded_pg_test.go
//
// Two CHECK shapes a real schema (reliant's machine_pools) combined, each of
// which defeated the seeder on its own:
//
//   - `selector <> '{}'::jsonb` beside `DEFAULT '{}'::jsonb`: an EXCLUDED
//     literal the column's own DEFAULT holds. A minimal row left the column
//     to its DEFAULT, and a full row wrote the DEFAULT's document, so both
//     inserted the one value the CHECK forbids;
//   - a bounds CHECK that ANDs single-column literal bounds with a guarded
//     ordering (`min >= 0 AND max >= 0 AND (max = 0 OR min <= max)`): the
//     ordering pass refused the whole constraint as "not a two-column
//     ordering comparison" because of the literal conjuncts.
//
// Measured: every generated create-request factory whose parents reach the
// table failed with "violates check constraint machine_pools_selector_present".

package seedplan

import (
	"context"
	"strings"
	"testing"
)

const machinePoolsDDL = `
CREATE TABLE machine_pools (
    id text PRIMARY KEY,
    user_id text NOT NULL,
    name text NOT NULL,
    selector jsonb NOT NULL DEFAULT '{}'::jsonb,
    min_members integer NOT NULL DEFAULT 0,
    max_members integer NOT NULL DEFAULT 0,
    member_size text NOT NULL DEFAULT '',
    max_runs_per_member integer NOT NULL DEFAULT 0,
    scale_up text NOT NULL DEFAULT 'never',
    label text NOT NULL DEFAULT '' CHECK (label <> ''),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT machine_pools_user_name_key UNIQUE (user_id, name),
    CONSTRAINT machine_pools_name_present CHECK (length(trim(name)) > 0),
    CONSTRAINT machine_pools_selector_present CHECK (selector <> '{}'::jsonb),
    CONSTRAINT machine_pools_bounds CHECK (min_members >= 0 AND max_members >= 0 AND max_runs_per_member >= 0 AND (max_members = 0 OR min_members <= max_members)),
    CONSTRAINT machine_pools_scale_up CHECK (scale_up IN ('never'))
);
CREATE TABLE pool_grants (
    id text PRIMARY KEY,
    machine_pool_id text NOT NULL REFERENCES machine_pools (id) ON DELETE CASCADE
);
`

// TestExcludedLiteralAndBoundsConjunction_SeedBothModes is the reproduction:
// both the minimal plan (a test factory's parents) and the full plan (the
// dev dataset) insert into the schema, with no warning about either CHECK.
func TestExcludedLiteralAndBoundsConjunction_SeedBothModes(t *testing.T) {
	tables := applyDDL(t, machinePoolsDDL)
	for _, minimal := range []bool{true, false} {
		name := "full"
		if minimal {
			name = "minimal"
		}
		t.Run(name, func(t *testing.T) {
			p, err := BuildPlan(tables, PoolsFromTables(tables), Config{Rows: 3, Minimal: minimal})
			if err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}
			p.SetBounds(BoundsFromTables(tables))
			for _, w := range p.Warnings() {
				if strings.Contains(w, "machine_pools_bounds") || strings.Contains(w, "machine_pools_selector_present") {
					t.Errorf("a placeable constraint was refused: %s", w)
				}
			}
			db := minimalHarness(t, machinePoolsDDL)
			ctx := context.Background()
			for _, stmt := range p.Statements() {
				if _, err := db.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("seeded row rejected by its own schema: %v\n%s", err, stmt)
				}
			}
			var n int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM machine_pools WHERE selector <> '{}'::jsonb AND label <> ''`).Scan(&n); err != nil || n == 0 {
				t.Fatalf("no machine_pools row seeded (n=%d, err=%v)", n, err)
			}
		})
	}
}
