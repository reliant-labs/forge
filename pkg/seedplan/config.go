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
