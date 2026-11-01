package dbgrants

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// # The apply-time re-check
//
// A grants file is a projection of the schema at the moment `forge generate`
// ran, and its rows name columns, not catalog objects. A migration that drops
// a declared column and re-adds one under the same name, with no generate in
// between, leaves a row that still names it, and the loop would grant the NEW
// column whatever its comment says. `forge lint --db-grants` and
// verify-generated catch the stale file before a merge; this is the defence
// behind them, at the database.
//
// So the script grants a listed column only while the column's CURRENT catalog
// comment still allows it, and refuses it otherwise:
//
//   - every role: the column must not declare forge:secret;
//   - the dashboard role, in addition: the table and the column must each
//     declare forge:dashboard in their marker list, and the column must not
//     declare forge:no-dashboard.
//
// The allow is read with the same grammar `forge generate` reads it with
// (schemadef.MarkerListPattern, held equal to dashboardAllowPattern by
// TestDashboardAllowPatternIsSchemadefs), so a current file never trips it.
// The denies match a whole token anywhere, as their Go readers do; reading
// one more broadly than Go can only withhold.
//
// What it cannot re-check is what the catalog does not hold: a proto-declared
// forge:secret (the file simply does not list such a column), and a column's
// credential-shaped name (unchanged by a drop and re-add under that name).
//
// The script names what it refused in a WARNING, and leaves the list in the
// session setting [refusedSetting] for [Apply] to return, so the migrator logs
// it: database/sql does not surface a server notice.

// refusedSetting is the session setting the script leaves its refused columns
// in, a JSON array of `schema.table.column` (each part quoted as needed).
const refusedSetting = "forge.grants_refused"

// dashboardAllowPattern is schemadef.MarkerListPattern("forge:dashboard"),
// copied rather than imported so the runtime package does not depend on the
// schema reader (which brings a Postgres driver). It is written in the
// regular-expression syntax Go's regexp and Postgres's ARE share, with
// POSIX classes rather than backslash escapes so its meaning does not depend
// on standard_conforming_strings.
const dashboardAllowPattern = `^[[:blank:]]*(forge:[A-Za-z][A-Za-z0-9_-]*(=[^[:space:]]*)?[[:blank:]]+)*forge:dashboard([[:space:]]|$)`

// secretDenyPattern and dashboardDenyPattern match a deny anywhere in a
// comment, as schemadef's DeclaresSecret and DeclaresNoDashboard do.
const (
	secretDenyPattern    = `forge:secret([^A-Za-z0-9_=-]|$)`
	dashboardDenyPattern = `forge:(secret([^A-Za-z0-9_=-]|$)|no-dashboard([^A-Za-z0-9_:-]|$))`
)

// recheckExpr is the SQL boolean the script requires of a listed column, over
// pg_attribute a.
func recheckExpr(dashboard bool) string {
	col := "coalesce(col_description(a.attrelid, a.attnum), '')"
	if !dashboard {
		return col + " !~ " + quoteLiteral(secretDenyPattern)
	}
	tbl := "coalesce(obj_description(a.attrelid, 'pg_class'), '')"
	allow := quoteLiteral("(?n)" + dashboardAllowPattern)
	return "(" + tbl + " ~ " + allow + " AND " + col + " ~ " + allow + " AND " + col + " !~ " + quoteLiteral(dashboardDenyPattern) + ")"
}

// Apply runs script, a [Render] result, on c. The script is one statement, so
// it is atomic: a failure leaves the previous grants in place.
//
// refused are the columns the grants file lists that the apply-time re-check
// withheld because their catalog comment no longer allows them, as
// `schema.table.column`: a file older than the schema. The caller logs them.
func Apply(ctx context.Context, c Conn, script string) (refused []string, err error) {
	return apply(ctx, c, script, "read-only")
}

// ApplyDashboard is [Apply] for a [RenderDashboard] script, so a failure names
// the role it was converging.
func ApplyDashboard(ctx context.Context, c Conn, script string) (refused []string, err error) {
	return apply(ctx, c, script, "dashboard")
}

func apply(ctx context.Context, c Conn, script, what string) ([]string, error) {
	// The refused list is a session setting, so the script and the read must
	// share a session; a pool hands each statement any connection.
	if db, ok := c.(*sql.DB); ok {
		conn, err := db.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("dbgrants: apply %s grants: %w", what, err)
		}
		defer func() { _ = conn.Close() }()
		c = conn
	}
	// Cleared first: a script rendered before the re-check sets nothing, and
	// must not report what an earlier script on this session refused.
	if _, err := c.ExecContext(ctx, `SELECT set_config($1, '', false)`, refusedSetting); err != nil {
		return nil, fmt.Errorf("dbgrants: apply %s grants: %w", what, err)
	}
	if _, err := c.ExecContext(ctx, script); err != nil {
		return nil, fmt.Errorf("dbgrants: apply %s grants: %w", what, err)
	}
	var raw sql.NullString
	if err := c.QueryRowContext(ctx, `SELECT current_setting($1, true)`, refusedSetting).Scan(&raw); err != nil {
		return nil, fmt.Errorf("dbgrants: read what the %s grants refused: %w", what, err)
	}
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var refused []string
	if err := json.Unmarshal([]byte(raw.String), &refused); err != nil {
		return nil, fmt.Errorf("dbgrants: decode what the %s grants refused: %w", what, err)
	}
	if len(refused) == 0 {
		return nil, nil
	}
	return refused, nil
}
