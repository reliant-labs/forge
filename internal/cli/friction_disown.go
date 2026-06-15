// Disown friction capture.
//
// A disown is design feedback: when a user (or agent) runs `forge
// disown` (or the deprecated `forge generate --accept` alias) on a
// generated file, they are saying the generated API/abstraction
// couldn't express what they needed. That signal is worth exactly as
// much as it is durable — so we capture it AT THE MOMENT OF DISOWNING,
// through the same append-only .forge/friction.jsonl machinery `forge
// friction add` uses (one O_APPEND write per entry, never
// read-modify-write; see friction.go for the durability rationale).
//
// Design constraints, in order:
//
//   - Never prompt. Agents drive these commands non-interactively; a
//     blocking "why did you disown this?" prompt would hang every
//     automated run. The commands require --reason up-front instead;
//     the placeholder branch survives only as a backstop for callers
//     that reach the recorder without one.
//   - Never block the disown. By the time we record, the checksum flip
//     has already been persisted — failing the command over a friction-
//     log write would leave the user with a succeeded disown and a
//     failed exit code. Write failures warn loudly and continue.
//   - One entry per path. `forge friction list --area disown` and the
//     audit disowned_files reason lookup both key on the per-path
//     context, so a batched disown of N files must produce N
//     independently-queryable records.
package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// frictionAreaDisown is the area tag every disown entry carries.
// `forge friction list --area disown` is the canonical query surface, so
// the tag is a constant — a typo'd area would silently orphan entries
// from that query.
const frictionAreaDisown = "disown"

// frictionAreaForkLegacy is the area tag the fork era used. Read-only:
// the audit reason-join still matches it so reasons recorded by `forge
// generate --accept --reason` under older forge versions stay attached
// to their (now migrated-to-disowned) paths.
const frictionAreaForkLegacy = "fork"

// disownReasonNudge is the one loud line printed when a disown lands
// without --reason (only reachable through internal callers — the user-
// facing commands refuse up-front). Phrasing is load-bearing: it names
// the flag, says WHY it matters, and points at the query that surfaces
// the entries — all without prompting (agents run this).
const disownReasonNudge = "tip: record WHY with --reason — disown reasons are design feedback (forge friction list --area disown)"

// disownReasonUnstatedText renders the placeholder text recorded when a
// disown lands without --reason. Still one entry per path: the disown
// EVENT is signal even when the why went unstated, and the entry
// anchors the path so a later audit shows the gap.
func disownReasonUnstatedText(relPath string) string {
	return fmt.Sprintf("disown reason unstated — %s transferred out of forge ownership", relPath)
}

// recordDisownFriction appends one friction entry per just-disowned
// path and prints a short confirmation to w. source distinguishes the
// surfaces ("disown" vs the deprecated "generate --accept" /
// "accept-fork" aliases) so upstream triage can tell direct disowns
// from guard-driven ones.
//
// Best-effort by contract: errors warn on w and never propagate — see
// the package comment for why the disown must not fail here. A
// zero-length relPaths is a no-op.
func recordDisownFriction(projectRoot, source, reason string, relPaths []string, w io.Writer) {
	if len(relPaths) == 0 {
		return
	}
	path := filepath.Join(projectRoot, filepath.FromSlash(frictionFileRelPath))
	written := 0
	for _, rel := range relPaths {
		text := reason
		if text == "" {
			text = disownReasonUnstatedText(rel)
		}
		// severity=note: the entry describes a deliberate user decision,
		// not a defect — it must never trip a p-level triage filter.
		entry := newFrictionEntry("note", frictionAreaDisown, source, []string{rel}, text)
		if err := appendFrictionEntry(path, entry); err != nil {
			fmt.Fprintf(w, "⚠️  could not record disown friction entry for %s: %v\n", rel, err)
			continue
		}
		written++
	}
	if written > 0 {
		fmt.Fprintf(w, "📝 recorded %d disown friction entr%s -> %s (view: forge friction list --area disown)\n",
			written, pluralIES(written), frictionFileRelPath)
	}
	if reason == "" {
		fmt.Fprintln(w, disownReasonNudge)
	}
}

// disownFrictionReasons loads the friction log ONCE and returns, per
// disowned path, the text of the NEWEST area=disown (or legacy
// area=fork) entry whose context names that path. Consumed by the audit
// codegen category to attach a "reason" to each disowned_files row —
// keyed by context element because recordDisownFriction writes
// context=[<relpath>] by construction (and the fork-era recorder did
// the same, which is what keeps reasons attached across the
// fork→disown migration).
//
// Best-effort like every friction read: an unreadable/missing log
// yields an empty map (audit must not fail over a side-channel), and
// malformed lines are already skipped inside loadFrictionEntries.
func disownFrictionReasons(projectDir string) map[string]string {
	path := filepath.Join(projectDir, filepath.FromSlash(frictionFileRelPath))
	entries, _, err := loadFrictionEntries(path)
	if err != nil || len(entries) == 0 {
		return nil
	}
	newest := map[string]time.Time{}
	reasons := map[string]string{}
	for _, e := range entries {
		if !strings.EqualFold(e.Area, frictionAreaDisown) && !strings.EqualFold(e.Area, frictionAreaForkLegacy) {
			continue
		}
		for _, c := range e.Context {
			// Strictly-older entries lose; a same-second tie goes to the
			// later line (append order IS recording order within the file).
			if t, ok := newest[c]; ok && e.RecordedAt.Before(t) {
				continue
			}
			newest[c] = e.RecordedAt
			reasons[c] = e.Text
		}
	}
	return reasons
}
