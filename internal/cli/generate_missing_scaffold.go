// Missing scaffold-once artifact reporting.
//
// FRICTION (measured dogfood run, 2026-07): an agent scaffolded a project,
// reviewed the born migrations, and correctly found three real schema
// defects in them (a nullable column that should be NOT NULL, an FK with no
// referential action, an index whose column order did not match the query).
// It fixed all three. That corrected schema invalidated the fixtures in the
// already-born internal/handlers/library/handlers_crud_test.go, whose
// literals were derived from the schema AS IT WAS AT BIRTH.
//
// So it deleted the file, expecting `forge generate` to re-derive it against
// the schema as it now stood. Generate did not — correctly, because the
// birth ledger records that forge has already scaffolded that path and
// deleting a scaffold is an act of ownership (see
// internal/checksums/scaffoldledger.go). But generate also said NOTHING. It
// printed its success banner over a project that had silently lost a test
// file, and the author spent roughly fourteen further tool calls and an hour
// trying to reproduce the birth condition by hand — copying the project to a
// scratch directory, stripping the CRUD rpcs out of the proto, deleting
// db/migrations, deleting the handler directory — none of which could work,
// because the one thing that brought the file back was an edit to
// .forge/scaffolded.json that nothing in forge's output had ever named. That
// edit is now a command, `forge project rescaffold <path>` (rescaffold.go),
// and this notice names it.
//
// The bug is not the suppression. The suppression is the feature, and this
// file does not touch it: forge still never resurrects a file the user
// removed. The bug is that the suppression was INVISIBLE, and an invisible
// correct decision is indistinguishable from a malfunction.
//
// So generate now reports it, once per absent path, naming the FILE and the
// exact ACTION that restores it. Deliberately GENERAL over the ledger rather
// than special-cased to the CRUD lifecycle test: forge scaffolds many
// one-shot files (internal/app/auth.go, cmd/<bin>/main.go, the frontend
// pages, the per-RPC handler stubs), every one of them can be deleted for
// the same reason, and a check that knew only about handlers_crud_test.go
// would be overfitted to the single instance that happened to be measured.
//
// It is a NOTICE, not a warning, and it does not route through warnOrFail:
// an intentionally deleted scaffold is a supported end state — the author
// who does not want forge's lifecycle test removes it and is done — so this
// must never be promotable into a --strict failure that punishes a project
// for a legitimate choice. What the author needs is the sentence they could
// not find, not a build that stops.
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
)

// missingScaffoldNotice renders the notice for the absent scaffold-once
// paths, or "" when there are none (the healthy steady state, which must
// stay completely silent — a notice that prints on every run is one users
// learn to skip).
//
// Every line carries the two facts the run needed and could not get: WHICH
// file, and WHAT single action brings it back. A notice that said only
// "something drifted" would have cost the same hour.
func missingScaffoldNotice(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)

	var b strings.Builder
	fmt.Fprintf(&b, "\nℹ️  %d scaffold-once file(s) forge has written before are absent — left absent on purpose:\n", len(sorted))
	for _, p := range sorted {
		fmt.Fprintf(&b, "   - %s\n", p)
	}
	b.WriteString("    These are yours from birth: forge writes each exactly once and never again,\n")
	fmt.Fprintf(&b, "    and DELETING one is an act of ownership %s records, so re-running\n", checksums.ScaffoldedFile)
	b.WriteString("    generate does not bring it back. If that is what you meant, nothing to do;\n")
	b.WriteString("    this notice is informational and the run did not fail.\n")
	b.WriteString("    To have forge scaffold one FRESH against the project as it stands today\n")
	b.WriteString("    (e.g. a CRUD lifecycle test whose fixtures no longer match migrations you have\n")
	b.WriteString("    since corrected), rescaffold it:\n")
	fmt.Fprintf(&b, "      %s\n", rescaffoldHint(sorted))
	return b.String()
}

// rescaffoldHint renders the copy-pasteable command that re-creates absent
// scaffold-once files.
//
// With exactly one absent path the command is concrete — an agent can run it
// verbatim, which is the whole point. With several, it takes a <path>
// placeholder rather than silently picking one: the run this notice exists
// for lost an hour to a remedy it could not locate, and a command that
// confidently re-creates files the author deleted on purpose (every file in
// the list, or the alphabetically-first one, which is rarely the one they
// want back) is a worse failure than one that asks them to substitute a path
// already listed three lines above.
func rescaffoldHint(sorted []string) string {
	if len(sorted) == 1 {
		return rescaffoldCmd(sorted[0])
	}
	return rescaffoldCmd("<path>...")
}

// scaffoldSkipLine is what a scaffold-once writer prints for a path it did not
// write, and it has to tell the truth about WHY.
//
// It used to say "<path> exists — yours to edit, leaving it untouched"
// unconditionally, including for a workflow the user had deleted: the
// scaffold-once decision refuses both a present file and a deleted one, and
// the message only knew the first reason. A line claiming a file exists while
// `ls` says otherwise sends the reader looking for a bug in the wrong place.
// The two branches differ in whether the reader has anything to DO, so they
// differ in verbosity. "exists — yours to edit" is identical on every run
// against an unchanged tree and reports forge correctly leaving a file
// alone: routine. "absent, but deleted by you" names a file that is NOT
// there and the command that brings it back, which is the sentence the run
// in this file's header spent an hour failing to find: never suppressed.
func scaffoldSkipLine(root, relPath string) (line string, routine bool) {
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relPath))); err == nil {
		return fmt.Sprintf("  ⏭️  %s exists — yours to edit, leaving it untouched", relPath), true
	}
	return fmt.Sprintf("  ⏭️  %s is absent, but deleted by you (%s) — leaving it deleted. Re-create it: %s",
		relPath, checksums.ScaffoldedFile, rescaffoldCmd(relPath)), false
}

// reportMissingScaffolds writes the notice for root's absent scaffold-once
// paths to w. Returns whether anything was reported, so callers (and tests)
// can tell "nothing absent" from "reported".
func reportMissingScaffolds(w io.Writer, root string) bool {
	notice := missingScaffoldNotice(checksums.AbsentScaffolds(root))
	if notice == "" {
		return false
	}
	fmt.Fprint(w, notice)
	return true
}
