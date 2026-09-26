// Package compat reads the ROLLBACK-COMPATIBILITY contract a migration
// declares about itself.
//
// THE CONTRACT. A migration's .up.sql may carry the line
//
//	-- forge:backward-compatible
//
// which is its author's statement that the PREVIOUS release's code keeps
// working against the schema this migration leaves behind — the "expand"
// half of expand/contract. An ADD COLUMN that old code never reads is
// backward-compatible; a DROP COLUMN old code still SELECTs is not.
//
// WHY A DECLARATION AND NOT AN INFERENCE. Whether N-1 code survives a schema
// is a fact about that code, not about the SQL, and nothing short of running
// it can prove it. An inference from the SQL would be wrong in both
// directions: `DROP TABLE` of a table nothing reads is safe, and `ADD COLUMN
// ... NOT NULL` without a default breaks every old INSERT. So the author,
// who knows, states it once, in the file, in review — and forge enforces
// what follows from it:
//
//   - `forge env promote --rollback` refuses to move an environment back
//     across a migration that does not declare it (the older release would
//     run against a schema its code was never written for), and prints the
//     step-down runbook instead;
//   - the migrator records the declared versions in the database as it
//     applies them, so an OLDER binary that meets a schema ahead of it can
//     prove the extra versions are safe rather than hoping (see
//     migratekit.SchemaAheadError).
//
// WHY THIS IS ITS OWN PACKAGE. It is read by two very different programs: the
// forge CLI (promote, lint) and every project's migrator. The CLI must not
// link golang-migrate for a comment parser, so this package imports nothing
// but the standard library.
package compat

import (
	"bufio"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Directive is the exact line a migration carries to declare itself
// backward-compatible. Exported so remediation text, the linter and the
// scaffolded comment all quote the same spelling the parser matches.
const Directive = "-- forge:backward-compatible"

// directiveRe matches Directive as a whole SQL line comment, case-insensitive,
// tolerating indentation and the `--   forge:` spacing people actually type.
//
// The directive must END at whitespace, an em dash or end of line — not at a
// `\b`. A word boundary sits between `e` and `-`, so `\b` would read
// `forge:backward-compatible-ish` as the declaration; a rationale may follow
// after a space (`-- forge:backward-compatible — additive column`).
//
// Only a LINE COMMENT counts. A mention inside a block comment or a string is
// prose about the directive, not the directive, and treating it as a
// declaration would let a migration that merely discusses compatibility claim
// it.
var directiveRe = regexp.MustCompile(`(?i)^\s*--\s*forge:backward-compatible(?:\s|$)`)

// upFileRe is golang-migrate's filename grammar, restricted to the up half:
// `<digits>_<name>.up.<ext>`. Stated here rather than imported so this
// package stays dependency-free; it is the same regex golang-migrate's
// source.Parse uses.
var upFileRe = regexp.MustCompile(`^([0-9]+)_(.*)\.up\.(.*)$`)

// Declared reports whether one migration's up SQL declares itself
// backward-compatible.
func Declared(upSQL string) bool {
	sc := bufio.NewScanner(strings.NewReader(upSQL))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		if directiveRe.MatchString(sc.Text()) {
			return true
		}
	}
	return false
}

// Migration is one up migration's version and what it declares.
type Migration struct {
	// Version is the numeric prefix — the value golang-migrate records in
	// schema_migrations.
	Version uint
	// Name is the filename, for messages.
	Name string
	// BackwardCompatible is whether the file carries Directive.
	BackwardCompatible bool
}

// Scan reads every up migration in dir of fsys, in version order.
//
// A directory holding no up migrations returns an empty slice, not an error:
// a project that has not written one yet is a normal state. An unreadable
// directory IS an error, because "no migrations" and "could not look" call
// for different fixes.
func Scan(fsys fs.FS, dir string) ([]Migration, error) {
	if dir == "" {
		dir = "."
	}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations (%s): %w", dir, err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := upFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, perr := strconv.ParseUint(m[1], 10, 64)
		if perr != nil {
			return nil, fmt.Errorf("migration %s: version %q is not a number: %w", e.Name(), m[1], perr)
		}
		raw, rerr := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if rerr != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), rerr)
		}
		out = append(out, Migration{Version: uint(v), Name: e.Name(), BackwardCompatible: Declared(string(raw))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Delta is the schema difference between two releases' migration sets: the
// versions the NEWER set has that the OLDER one does not.
//
// It is what a rollback crosses. Running the older release against a
// database the newer one migrated means running it against every migration
// in Delta, so Delta is exactly the set whose compatibility matters.
type Delta struct {
	// Ahead is every migration in the newer set absent from the older,
	// in version order.
	Ahead []Migration
}

// Incompatible returns the migrations in the delta that do NOT declare
// themselves backward-compatible. Empty means the older release's code can
// run against the newer schema as-is.
func (d Delta) Incompatible() []Migration {
	var out []Migration
	for _, m := range d.Ahead {
		if !m.BackwardCompatible {
			out = append(out, m)
		}
	}
	return out
}

// Compare computes the delta a move from `newer` back to `older` crosses.
//
// Membership is by VERSION, not by "greater than older's max". A version
// present in newer and absent from older is ahead even when it is numbered
// below older's newest — that is what a migration merged out of order looks
// like, and the older binary has never seen it either.
func Compare(newer, older []Migration) Delta {
	known := make(map[uint]bool, len(older))
	for _, m := range older {
		known[m.Version] = true
	}
	var d Delta
	for _, m := range newer {
		if !known[m.Version] {
			d.Ahead = append(d.Ahead, m)
		}
	}
	return d
}
