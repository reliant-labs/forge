// Package compat reads the BACKWARD-COMPATIBILITY contract a migration
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
// what follows from it: the migrator records the declared versions in the
// database as it applies them, so an OLDER binary that meets a schema ahead
// of it — the previous release's pods, still serving or rescheduled, during
// every rolling deploy — can prove the extra versions are safe rather than
// hoping (see migratekit.SchemaAheadError).
//
// WHY THIS IS ITS OWN PACKAGE. It is a comment parser every project's
// migrator links; keeping it free of golang-migrate lets any tool read the
// declaration without that dependency, so it imports nothing but the
// standard library.
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
	// Descriptor is the filename between the version prefix and `.up.` —
	// `retire_plan_compute_free` in `00091_retire_plan_compute_free.up.sql`.
	// It is what identifies WHICH migration a version is: two branches that
	// both claim 91 differ here, while a change of zero-padding does not.
	Descriptor string
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
		out = append(out, Migration{Version: uint(v), Name: e.Name(), Descriptor: m[2], BackwardCompatible: Declared(string(raw))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}
