package seedplan

// Config controls seed volume and determinism. It is a plain value with
// Effective* accessors so a zero Config yields sane defaults — the CLI maps
// the project's forge.yaml database.seed block onto it.
type Config struct {
	// Rows is the default number of rows per table (default 20 — fills a
	// default page and exercises pagination).
	Rows int
	// Salt perturbs synthesis: change it for a different-but-stable dataset.
	Salt int
	// RowsPerTable overrides Rows for specific tables.
	RowsPerTable map[string]int
	// Tables scopes the plan to these tables plus every table they reach
	// through a NOT NULL foreign key (a row there cannot exist without a
	// parent row). nil means every table in the schema. A nullable
	// reference to a table outside the scope is written NULL, exactly as a
	// reference to any unseedable table is.
	//
	// Scoping exists because "fill every table" is the wrong default for a
	// schema that holds more than CRUD demo data: a payments ledger, an
	// idempotency log, a reservations table whose rows mean money moved.
	Tables []string
	// Minimal plans the SMALLEST row the schema accepts instead of the
	// fullest one. A column is written only when the database will not
	// supply an acceptable value itself — a key, a NOT NULL column with no
	// DEFAULT, a NOT NULL reference, a UNIQUE or ordered NOT NULL column, a
	// DEFAULT the column's own CHECK rejects — and every other column is
	// left to its DEFAULT, or NULL. A discriminated-union or status-guard
	// CHECK takes the branch closest to that default row, the same branch on
	// every row. See minimal.go.
	//
	// The dev dataset wants the opposite (every column populated, every
	// branch covered), so this is off for `forge db seed`. It is what a test
	// factory wants: a freshly created row, at its initial lifecycle state,
	// with nothing set that the schema did not ask for.
	Minimal bool
}

const defaultRows = 20

// DefaultConfig returns the canonical defaults. Salt defaults to 0 so it
// aligns with an unset forge.yaml database.seed.salt; change it for a
// different-but-stable dataset.
func DefaultConfig() Config {
	return Config{Rows: defaultRows, Salt: 0}
}

// EffectiveRows returns the row count for a table, honoring RowsPerTable then
// Rows then the built-in default. It never returns < 1 so any table referenced
// by a NOT NULL foreign key has a parent row to point at.
func (c Config) EffectiveRows(table string) int {
	if n, ok := c.RowsPerTable[table]; ok {
		if n < 1 {
			return 1
		}
		return n
	}
	if c.Rows > 0 {
		return c.Rows
	}
	return defaultRows
}

// EffectiveSalt returns the determinism salt.
func (c Config) EffectiveSalt() int { return c.Salt }
