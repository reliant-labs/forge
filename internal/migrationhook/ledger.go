package migrationhook

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/reliant-labs/forge/internal/config"
)

// Ledger is what a built-in tool's history table says about one database:
// the migrations it records as applied, and the one it records as failed
// part-way. It is how forge tells "fully migrated" from "behind" without
// trusting the schema's shape, which a pending data-only migration or a
// migration that failed after its DDL leaves looking current.
type Ledger struct {
	// Table is the history table read.
	Table LedgerTable
	// Applied are the versions recorded as applied.
	Applied []string
	// Through is a version every version at or below which counts as
	// applied: golang-migrate's current version (it records only that),
	// or a flyway/atlas baseline. Empty when there is none.
	Through string
	// Dirty is the version recorded as failed part-way, "" when none:
	// golang-migrate's dirty flag, a flyway row that did not succeed, an
	// atlas revision with an error or fewer statements applied than it has.
	Dirty string
}

// ledgerQuerier is the one database method ReadLedger needs.
type ledgerQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// HasLedger reports whether forge knows how to read h's tool's history
// (ReadLedger): every built-in tool, not custom.
func (h Hook) HasLedger() bool {
	return len(h.LedgerTables()) > 0
}

// ledgerTable is the history table h's tool writes: the one its argv names,
// else the tool's default (the last of LedgerTables).
func (h Hook) ledgerTable() (LedgerTable, bool) {
	tables := h.LedgerTables()
	if len(tables) == 0 {
		return LedgerTable{}, false
	}
	return tables[len(tables)-1], true
}

// ReadLedger reads h's tool's history in db. A database whose history table
// does not exist has applied nothing: the zero Ledger (with Table set) and
// exists false. A tool forge cannot read (custom) is an error.
func (h Hook) ReadLedger(ctx context.Context, db ledgerQuerier) (l Ledger, exists bool, err error) {
	t, ok := h.ledgerTable()
	if !ok {
		return Ledger{}, false, fmt.Errorf("forge cannot read the history of migration tool %s", h.Tool)
	}
	l.Table = t
	ident := quoteIdent(t.Name)
	if t.Schema != "" {
		ident = quoteIdent(t.Schema) + "." + ident
	}
	rows, err := db.QueryContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, ident)
	if err != nil {
		return l, false, fmt.Errorf("look for %s: %w", ident, err)
	}
	exists, err = scanOne[bool](rows)
	if err != nil || !exists {
		return l, false, err
	}

	var query string
	switch h.Tool {
	case config.MigrationToolGolangMigrate:
		// One row: the current version and whether it failed part-way.
		query = `SELECT version::text, dirty FROM ` + ident + ` ORDER BY version DESC LIMIT 1`
	case config.MigrationToolGoose:
		// Replayed in order: a later is_applied=false row is a down.
		query = `SELECT version_id::text, is_applied FROM ` + ident + ` WHERE version_id <> 0 ORDER BY id`
	case config.MigrationToolDbmate:
		query = `SELECT version::text, true FROM ` + ident
	case config.MigrationToolFlyway:
		query = `SELECT version, success, type FROM ` + ident + ` WHERE version IS NOT NULL ORDER BY installed_rank`
	case config.MigrationToolAtlas:
		query = `SELECT version, applied >= total AND COALESCE(error, '') = '', type FROM ` + ident + ` WHERE version NOT LIKE '.%' ORDER BY version`
	default:
		return l, false, fmt.Errorf("forge cannot read the history of migration tool %s", h.Tool)
	}
	rows, err = db.QueryContext(ctx, query)
	if err != nil {
		return l, true, fmt.Errorf("read %s: %w", ident, err)
	}
	defer func() { _ = rows.Close() }()
	state := map[string]bool{}
	var order []string
	for rows.Next() {
		var version string
		var ok bool
		var kind sql.NullString
		dest := []any{&version, &ok}
		if h.Tool == config.MigrationToolFlyway || h.Tool == config.MigrationToolAtlas {
			dest = append(dest, &kind)
		}
		if err := rows.Scan(dest...); err != nil {
			return l, true, fmt.Errorf("read %s: %w", ident, err)
		}
		switch h.Tool {
		case config.MigrationToolGolangMigrate:
			// golang-migrate records the version it is applying before
			// it applies it: a dirty version failed part-way, a clean one
			// is applied along with every version below it.
			if ok {
				l.Dirty = version
			} else {
				l.Through = version
			}
			continue
		case config.MigrationToolFlyway:
			switch kind.String {
			case "SCHEMA", "DELETE":
				continue
			case "BASELINE":
				l.Through = maxVersion(l.Through, version)
				continue
			}
		case config.MigrationToolAtlas:
			// atlas's RevisionType is a bit set; 1 is a baseline.
			if n, err := strconv.Atoi(kind.String); err == nil && n&1 != 0 {
				l.Through = maxVersion(l.Through, version)
				continue
			}
		}
		if _, seen := state[version]; !seen {
			order = append(order, version)
		}
		state[version] = ok
	}
	if err := rows.Err(); err != nil {
		return l, true, fmt.Errorf("read %s: %w", ident, err)
	}
	// goose's false is a down (the version is not applied); flyway's and
	// atlas's is a migration that failed part-way.
	failedMeansDirty := h.Tool == config.MigrationToolFlyway || h.Tool == config.MigrationToolAtlas
	for _, v := range order {
		switch {
		case state[v]:
			l.Applied = append(l.Applied, v)
		case failedMeansDirty && l.Dirty == "":
			l.Dirty = v
		}
	}
	return l, true, nil
}

// Behind returns the first version l records as applied that have does not
// (it is neither among have's Applied nor at or below its Through), or ""
// when have has applied everything l has. l is the ledger of a database the
// tool brought fully up to date (the shadow), have the target's.
func (l Ledger) Behind(have Ledger) string {
	applied := map[string]bool{}
	for _, v := range have.Applied {
		applied[v] = true
	}
	covered := func(v string) bool {
		return applied[v] || (have.Through != "" && CompareVersions(v, have.Through) <= 0)
	}
	for _, v := range l.Applied {
		if !covered(v) {
			return v
		}
	}
	if l.Through != "" && !covered(l.Through) {
		return l.Through
	}
	return ""
}

// Empty reports whether the ledger records nothing applied.
func (l Ledger) Empty() bool {
	return len(l.Applied) == 0 && l.Through == ""
}

// CompareVersions orders two migration versions the way the tools do:
// numerically, segment by segment, where a segment is a run of digits
// (`20240101120000`, `1.2.10`, `3_1`). It returns -1, 0 or 1. A version with
// a non-digit run falls back to comparing that run as text.
func CompareVersions(a, b string) int {
	as, bs := versionSegments(a), versionSegments(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		if i >= len(as) {
			return -1
		}
		if i >= len(bs) {
			return 1
		}
		if c := compareSegment(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return 0
}

func versionSegments(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return !unicode.IsDigit(r) && !unicode.IsLetter(r) })
}

func compareSegment(a, b string) int {
	if isDigits(a) && isDigits(b) {
		a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
	}
	return strings.Compare(a, b)
}

func isDigits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

func maxVersion(a, b string) string {
	if a == "" || CompareVersions(b, a) > 0 {
		return b
	}
	return a
}

// quoteIdent quotes a postgres identifier.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func scanOne[T any](rows *sql.Rows) (T, error) {
	var v T
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return v, err
		}
		return v, sql.ErrNoRows
	}
	if err := rows.Scan(&v); err != nil {
		return v, err
	}
	return v, rows.Err()
}
