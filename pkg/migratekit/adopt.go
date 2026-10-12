package migratekit

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/golang-migrate/migrate/v4"

	"github.com/reliant-labs/forge/pkg/migratekit/compat"
)

// ADOPTING A DATABASE ANOTHER TOOL MIGRATED.
//
// A project that moves onto forge from goose squashes goose's history into one
// baseline migration that declares it (`-- forge:adopts goose <versions>`, see
// compat/adopt.go). Two kinds of database then meet the same binary:
//
//   - a FRESH one, which applies the baseline as SQL like any migration;
//   - one GOOSE ALREADY MIGRATED, whose schema already is the baseline. Running
//     the baseline there fails on its first CREATE TABLE and leaves the schema
//     marked dirty — or, for IF NOT EXISTS DDL, half-succeeds.
//
// So before anything else reads the record, a database golang-migrate has
// never touched is checked for goose's table. If it has one, the baseline is
// ADOPTED — recorded as applied, none of its SQL run — but only when goose's
// applied set is EXACTLY the set the baseline lists:
//
//   - a listed version goose never applied means the database is BEHIND the
//     baseline. This binary no longer carries that migration, so it cannot
//     finish the history; the previous release can.
//   - an applied version the list does not hold means the database is not the
//     schema the baseline describes (a branch's migration on a shared
//     database, or a baseline cut before the last goose migration merged).
//
// Both refuse with *AdoptionError and change nothing. Everything after the
// baseline then applies as ordinary forge migrations, on the same run.
//
// WHY IT RUNS ON THE NORMAL PATH AND NOT AS A COMMAND. The databases this is for
// include desktop installs, where nobody can run a one-off step, and an
// initContainer that runs `db migrate up` and nothing else. A declaration in
// the embedded baseline reaches every one of them; a command reaches the ones
// someone remembers.
//
// WHY goose's TABLE IS LEFT ALONE. It is the previous release's whole view of
// the schema. Leaving it intact is what keeps rolling BACK to that release
// safe: goose finds every version it knows applied and does nothing. It also
// means adoption never re-runs: once golang-migrate records a version, this
// check never looks at goose again.

// gooseTable is goose's default version table. Unqualified, as goose itself
// creates it: it resolves through the search_path, like every table here.
const gooseTable = "goose_db_version"

// adoptionListCap bounds how many versions an AdoptionError prints per list.
// A database behind by a whole history would otherwise print 146 timestamps;
// the error's fields still carry every one.
const adoptionListCap = 12

// Adopted reports a database this call adopted: the baseline was recorded as
// applied and none of its SQL ran.
type Adopted struct {
	// Tool is the migration tool whose history was adopted (compat.ToolGoose).
	Tool string
	// Version is the baseline's version, now the schema's.
	Version uint
	// Name is the baseline's filename.
	Name string
	// Versions is how many of the tool's versions were verified applied.
	Versions int
}

// AdoptionError is a database another tool migrated whose recorded history is
// not the history the baseline replaces. Nothing was applied.
//
// It is a type so a caller (and a test) can tell "this database is not the
// schema the baseline describes" from every other migration failure. The two
// lists call for different fixes, and the message names both.
type AdoptionError struct {
	// Tool is the tool that migrated the database (compat.ToolGoose).
	Tool string
	// Table is the tool's version table that was read.
	Table string
	// Baseline is the migration that declares the adoption.
	Baseline string
	// Declared is how many versions the baseline lists.
	Declared int
	// Missing is every listed version the database never applied: it is
	// BEHIND the baseline.
	Missing []int64
	// Unexpected is every version the database applied that the baseline
	// does not list.
	Unexpected []int64
}

func (e *AdoptionError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "this database was migrated by %s (%s), and its history is not the one %s replaces", e.Tool, e.Table, e.Baseline)
	if len(e.Missing) > 0 {
		fmt.Fprintf(&b, "; it has NOT applied %d of the %d %s migration(s) the baseline lists: %s — this binary no longer carries them, "+
			"so it cannot finish %s's history: run the last release that still migrates with %s against this database first "+
			"(it applies them), then this one, which adopts the database without re-running anything",
			len(e.Missing), e.Declared, e.Tool, capVersions(e.Missing), e.Tool, e.Tool)
	}
	if len(e.Unexpected) > 0 {
		fmt.Fprintf(&b, "; it HAS applied %d %s migration(s) the baseline does not list: %s — either the baseline was cut before they "+
			"merged (re-cut it from %s's full history so its `%s %s` lines include them), or they are a branch's migrations "+
			"applied to a shared database (reconcile the schema by hand, then remove their rows: "+
			"`DELETE FROM %s WHERE version_id IN (...)`)",
			len(e.Unexpected), e.Tool, capVersions(e.Unexpected), e.Tool, compat.AdoptsDirective, e.Tool, e.Table)
	}
	b.WriteString(". NOTHING WAS APPLIED. See the forge skill db/deploy-migrations, \"Adopting a database another tool migrated\"")
	return b.String()
}

// capVersions renders up to adoptionListCap versions and counts the rest.
func capVersions(vs []int64) string {
	n := len(vs)
	if n > adoptionListCap {
		n = adoptionListCap
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprint(vs[i])
	}
	s := strings.Join(parts, ", ")
	if len(vs) > n {
		s += fmt.Sprintf(" … and %d more", len(vs)-n)
	}
	return s
}

// adopter returns the migration that declares an adoption. scanSource has
// already refused any but the first.
func (s sourceSet) adopter() (compat.Migration, bool) {
	if len(s) > 0 && s[0].Adopts != nil {
		return s[0], true
	}
	return compat.Migration{}, false
}

// validateAdoption refuses an adoption declared anywhere but the FIRST
// migration. A database another tool migrated has run none of this project's
// migrations, so the only one it can already reflect is the one that replaced
// that tool's history — the baseline every other migration builds on.
// Declaring it later would record the earlier migrations as applied on a
// database that never ran them.
//
// It refuses at Open and at boot on every database, fresh ones included, so
// the mistake fails the first test run rather than the first adopted
// database. `forge lint` reports the same rule at review time.
func validateAdoption(source sourceSet) error {
	for i, m := range source {
		if m.Adopts == nil || i == 0 {
			continue
		}
		return fmt.Errorf("migration %s declares %q, but only the FIRST migration — the baseline that replaced %s's history — may: "+
			"a database %s migrated has run none of this project's migrations, and %s comes before it",
			m.Name, compat.AdoptsDirective, m.Adopts.Tool, m.Adopts.Tool, source[0].Name)
	}
	return nil
}

// adopt adopts a database another tool migrated, when the embedded baseline
// declares that history and golang-migrate has recorded nothing yet. It
// returns the state to proceed from — re-read, because this call or a racing
// replica may have just changed it — and what it adopted, if anything.
//
// CONCURRENCY. Both callers already hold the run lock (runlock.go), so
// replicas of a release adopt one at a time, through either entry point. On
// top of that, the check-and-record takes appliedLockKey and RE-READS the state
// under it. That is what keeps it correct against a binary that predates the
// run lock, and it serializes adoption with bootstrapApplied, which decides
// the applied record's first rows just as adoption does. A replica that waited
// finds the version recorded and does nothing. The version is written through
// golang-migrate itself (Force, under its own lock and into whatever table it
// is configured with) while this transaction still holds appliedLockKey, so
// no replica can observe a half-adopted database.
//
// CRASH SAFETY. The applied row is written in this transaction and the
// version by Force before it commits. A crash before Force leaves nothing; a
// crash between Force and commit leaves the version recorded without the
// row, which the next run's bootstrapApplied fills in as it would for any
// database that predates the record.
func adopt(ctx context.Context, db *sql.DB, m *migrate.Migrate, source sourceSet, state State) (State, *Adopted, error) {
	baseline, ok := source.adopter()
	if !ok || state.Applied {
		return state, nil, nil
	}
	tool := baseline.Adopts.Tool
	fail := func(err error) (State, *Adopted, error) {
		return state, nil, fmt.Errorf("adopt %s-migrated database: %w", tool, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SELECT pg_advisory_xact_lock(%d)", int64(appliedLockKey))); err != nil {
		return fail(err)
	}

	// Another replica may have adopted (and even applied more) while this
	// one waited for the lock.
	state, err = stateOf(m)
	if err != nil {
		return fail(err)
	}
	if state.Applied {
		return state, nil, nil
	}

	applied, found, err := readGooseApplied(ctx, tx)
	if err != nil {
		return fail(err)
	}
	if !found {
		// A fresh database: the baseline applies as SQL.
		return state, nil, nil
	}
	missing, unexpected := diffAdoption(baseline.Adopts.Versions, applied)
	if len(missing) > 0 || len(unexpected) > 0 {
		return state, nil, &AdoptionError{
			Tool: tool, Table: gooseTable, Baseline: baseline.Name, Declared: len(baseline.Adopts.Versions),
			Missing: missing, Unexpected: unexpected,
		}
	}

	stmts := []string{
		"CREATE TABLE IF NOT EXISTS " + appliedTable + ` (
			version BIGINT PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		fmt.Sprintf("INSERT INTO %s (version, name) VALUES (%d, %s) ON CONFLICT (version) DO NOTHING",
			appliedTable, baseline.Version, quoteLiteral(baseline.Descriptor)),
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fail(err)
		}
	}
	if err := m.Force(int(baseline.Version)); err != nil {
		return fail(fmt.Errorf("record %s as applied: %w", baseline.Name, err))
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	after, err := stateOf(m)
	if err != nil {
		return fail(err)
	}
	return after, &Adopted{Tool: tool, Version: baseline.Version, Name: baseline.Name, Versions: len(baseline.Adopts.Versions)}, nil
}

// readGooseApplied returns the set of versions goose considers applied, and
// whether goose's table exists at all.
//
// goose keeps an append log, not a set: one row per apply, and — in older
// releases — one more with is_applied=false per rollback (newer ones delete the
// row instead). The LATEST row for a version decides, exactly as goose itself
// reads it (EnsureDBVersion walks rows by descending id). Version 0 is the
// row goose writes when it creates the table, not a migration.
func readGooseApplied(ctx context.Context, tx *sql.Tx) (map[int64]bool, bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT to_regclass('"+gooseTable+"') IS NOT NULL").Scan(&exists); err != nil {
		return nil, false, fmt.Errorf("look for %s: %w", gooseTable, err)
	}
	if !exists {
		return nil, false, nil
	}
	rows, err := tx.QueryContext(ctx, "SELECT version_id, is_applied FROM "+gooseTable+" ORDER BY id")
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", gooseTable, err)
	}
	defer func() { _ = rows.Close() }()
	latest := map[int64]bool{}
	for rows.Next() {
		var (
			v       int64
			applied bool
		)
		if err := rows.Scan(&v, &applied); err != nil {
			return nil, true, fmt.Errorf("read %s: %w", gooseTable, err)
		}
		latest[v] = applied
	}
	if err := rows.Err(); err != nil {
		return nil, true, fmt.Errorf("read %s: %w", gooseTable, err)
	}
	out := map[int64]bool{}
	for v, applied := range latest {
		if applied && v != 0 {
			out[v] = true
		}
	}
	return out, true, nil
}

// diffAdoption compares the versions a baseline declares against the set a
// database applied. Set membership, never a highest-version comparison: see
// compat/adopt.go on why the maximum proves nothing under out-of-order
// application.
func diffAdoption(declared []int64, applied map[int64]bool) (missing, unexpected []int64) {
	want := make(map[int64]bool, len(declared))
	for _, v := range declared {
		want[v] = true
		if !applied[v] {
			missing = append(missing, v)
		}
	}
	for v := range applied {
		if !want[v] {
			unexpected = append(unexpected, v)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	sort.Slice(unexpected, func(i, j int) bool { return unexpected[i] < unexpected[j] })
	return missing, unexpected
}
