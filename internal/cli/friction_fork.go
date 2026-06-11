// Fork-acceptance friction capture.
//
// A fork is design feedback: when a user (or agent) runs `forge
// generate --accept` or `forge generate accept-fork` on a generated
// file, they are saying the generated API/abstraction couldn't express
// what they needed. That signal is worth exactly as much as it is
// durable — so we capture it AT THE MOMENT OF FORKING, through the same
// append-only .forge/friction.jsonl machinery `forge friction add`
// uses (one O_APPEND write per entry, never read-modify-write; see
// friction.go for the durability rationale).
//
// Design constraints, in order:
//
//   - Never prompt. Agents drive these commands non-interactively; a
//     blocking "why did you fork this?" prompt would hang every
//     automated run. When --reason is absent we record a placeholder
//     entry (the fork itself is still signal) and print one loud nudge
//     pointing at --reason.
//   - Never block the acceptance. By the time we record, the checksum
//     flip has already been persisted — failing the command over a
//     friction-log write would leave the user with a succeeded accept
//     and a failed exit code. Write failures warn loudly and continue.
//   - One entry per path. `forge friction list --area fork` and the
//     audit forked_files reason lookup both key on the per-path
//     context, so a batched accept of N files must produce N
//     independently-queryable records.
package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// frictionAreaFork is the area tag every fork-acceptance entry carries.
// `forge friction list --area fork` is the canonical query surface, so
// the tag is a constant — a typo'd area would silently orphan entries
// from that query.
const frictionAreaFork = "fork"

// forkReasonNudge is the one loud line printed when a fork is accepted
// without --reason. Phrasing is load-bearing: it names the flag, says
// WHY it matters (fork reasons are design feedback), and points at the
// query that surfaces them — all without prompting (agents run this).
const forkReasonNudge = "tip: record WHY with --reason — fork reasons are design feedback (forge friction list --area fork)"

// forkReasonUnstatedText renders the placeholder text recorded when a
// fork is accepted without --reason. Still one entry per path: the
// fork EVENT is signal even when the why went unstated, and the entry
// anchors the path so a later audit shows the gap.
func forkReasonUnstatedText(relPath string) string {
	return fmt.Sprintf("fork reason unstated — %s adopted out of forge ownership", relPath)
}

// recordForkFriction appends one friction entry per just-accepted fork
// path and prints a short confirmation (plus the --reason nudge when
// the reason is empty) to w. source distinguishes the two acceptance
// surfaces ("generate --accept" vs "accept-fork") so upstream triage
// can tell guard-driven forks from bulk pre-silencing.
//
// Best-effort by contract: errors warn on w and never propagate — see
// the package comment for why the acceptance must not fail here. A
// zero-length relPaths is a no-op (nothing accepted ⇒ friction log
// untouched, no nudge).
func recordForkFriction(projectRoot, source, reason string, relPaths []string, w io.Writer) {
	if len(relPaths) == 0 {
		return
	}
	path := filepath.Join(projectRoot, filepath.FromSlash(frictionFileRelPath))
	written := 0
	for _, rel := range relPaths {
		text := reason
		if text == "" {
			text = forkReasonUnstatedText(rel)
		}
		// severity=note: the entry describes a deliberate user decision,
		// not a defect — it must never trip a p-level triage filter.
		entry := newFrictionEntry("note", frictionAreaFork, source, []string{rel}, text)
		if err := appendFrictionEntry(path, entry); err != nil {
			fmt.Fprintf(w, "⚠️  could not record fork friction entry for %s: %v\n", rel, err)
			continue
		}
		written++
	}
	if written > 0 {
		fmt.Fprintf(w, "📝 recorded %d fork friction entr%s -> %s (view: forge friction list --area fork)\n",
			written, pluralIES(written), frictionFileRelPath)
	}
	if reason == "" {
		fmt.Fprintln(w, forkReasonNudge)
	}
}

// forkFrictionReasons loads the friction log ONCE and returns, per
// forked path, the text of the NEWEST area=fork entry whose context
// names that path. Consumed by the audit codegen category to attach a
// "reason" to each forked_files row — keyed by context element because
// recordForkFriction writes context=[<relpath>] by construction.
//
// Best-effort like every friction read: an unreadable/missing log
// yields an empty map (audit must not fail over a side-channel), and
// malformed lines are already skipped inside loadFrictionEntries.
func forkFrictionReasons(projectDir string) map[string]string {
	path := filepath.Join(projectDir, filepath.FromSlash(frictionFileRelPath))
	entries, _, err := loadFrictionEntries(path)
	if err != nil || len(entries) == 0 {
		return nil
	}
	newest := map[string]time.Time{}
	reasons := map[string]string{}
	for _, e := range entries {
		if !strings.EqualFold(e.Area, frictionAreaFork) {
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
