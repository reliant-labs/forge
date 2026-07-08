// Stage-then-validate rollback journal for `forge generate`.
//
// FRICTION cp-forge fr-40f7ec9bd9: `forge generate --force` on a clean
// clone rewrote Tier-1 files across the whole tree (CI workflows, skills,
// mocks, ORM, KCL), ran `go mod tidy`, and only THEN failed the final
// "go build (validate generated code)" step — exiting non-zero with the
// tree left mid-regen and recovery left to the user's `git checkout`.
// A generate run that fails its own validation must not leave the tree
// in a state the user has to hand-repair.
//
// The fix is a write journal recorded at the SINGLE chokepoint every
// forge write flows through (WriteGeneratedFile / WriteScaffoldIfMissing
// / writeUnstampable, plus the in-place restamp and disown marker-strip).
// Before any of those mutate a path on disk, the journal captures the
// path's EXACT pre-run bytes (or records that it did not exist). On a
// post-write failure — most importantly the final `go build` validate —
// the pipeline calls RestoreRollback, which rewrites every journaled path
// back to its captured pre-run state (re-creating, overwriting, or
// deleting as needed). On success the pipeline calls CommitRollback,
// which simply drops the journal.
//
// Scope — deliberately bounded to forge-WRITTEN files:
//
//   - The journal restores exactly the files forge's writers touched
//     this run (Tier-1 codegen, scaffold-once "yours" files, comment-incapable
//     outputs, restamps, disown marker strips). That is the "mid-regen
//     broken tree" the friction names.
//   - It does NOT snapshot the whole working tree. External-tool churn
//     (`buf generate` into gen/, `go mod tidy` rewriting go.mod/go.sum,
//     `sqlc`, KCL render) is deterministic from the proto/config inputs
//     and is NOT what leaves a half-regenerated Tier-1 tree; snapshotting
//     it would manufacture spurious rollback diffs (e.g. a legitimately
//     re-tidied go.sum) on every failed run.
//   - goimports/restamp rewrite files that are THEMSELVES already in the
//     journal (forge wrote them earlier this run), so restoring the
//     journal also undoes those in-place rewrites.
//
// The journal is process-global (like the rest of this package's per-run
// state) and reset by BeginRollbackJournal at the head of each pipeline
// run. Non-pipeline callers (forge upgrade, project creation) never call
// Begin, so journaling stays OFF and their writes are recorded nowhere —
// they have their own recovery stories.
package checksums

import (
	"os"
	"path/filepath"
	"sort"
)

// rollbackEntry is one journaled path's pre-run state. existed=false
// means the path was absent before forge first wrote it this run, so the
// restore deletes it; existed=true means content holds the exact bytes to
// restore (with the captured file mode).
type rollbackEntry struct {
	existed bool
	content []byte
	mode    os.FileMode
}

// rollbackJournal is the per-run capture set: relPath -> pre-run state.
// nil when journaling is OFF (the default — non-pipeline callers never
// enable it). Populated lazily: each path is captured exactly once, on
// the first write that targets it this run.
var rollbackJournal map[string]rollbackEntry

// BeginRollbackJournal turns journaling ON and clears any prior capture.
// Called once at the head of a `forge generate` run, before any writer
// fires. After this, every forge write captures its target's pre-run
// state (once per path) so RestoreRollback can undo the whole run.
func BeginRollbackJournal() {
	rollbackJournal = map[string]rollbackEntry{}
}

// CommitRollback drops the journal without restoring anything — the
// run's writes stand. Called on the success path; also turns journaling
// back OFF so a subsequent non-pipeline write in the same process isn't
// silently recorded.
func CommitRollback() {
	rollbackJournal = nil
}

// RollbackEnabled reports whether journaling is currently ON. Exposed so
// tests can assert the pipeline armed/disarmed it correctly.
func RollbackEnabled() bool { return rollbackJournal != nil }

// recordPreWrite captures relPath's current on-disk state into the
// journal, exactly once. No-op when journaling is OFF, or when this path
// was already captured this run (the FIRST capture holds the true pre-run
// bytes; later writes this run are forge's own and must not overwrite the
// baseline). Capture failures are swallowed: a path we cannot read is one
// we cannot faithfully restore, and journaling must never itself abort a
// write — the pipeline's own error handling owns the failure surface.
func recordPreWrite(root, relPath string) {
	if rollbackJournal == nil {
		return
	}
	if _, seen := rollbackJournal[relPath]; seen {
		return
	}
	full := filepath.Join(root, relPath)
	info, statErr := os.Stat(full)
	if statErr != nil {
		// Absent (or unreadable) before this run: restore = delete.
		rollbackJournal[relPath] = rollbackEntry{existed: false}
		return
	}
	content, readErr := os.ReadFile(full)
	if readErr != nil {
		// Exists but unreadable — best effort: treat as absent so a
		// failed run at least removes the half-written forge output rather
		// than leaving a corrupt file claiming to be pristine.
		rollbackJournal[relPath] = rollbackEntry{existed: false}
		return
	}
	rollbackJournal[relPath] = rollbackEntry{existed: true, content: content, mode: info.Mode().Perm()}
}

// RecordPreWrite is the exported shim for pipeline steps that mutate a
// forge-owned path DIRECTLY (a raw os.Remove / os.WriteFile) instead of
// through the WriteGeneratedFile* chokepoint. Call it immediately before
// the mutation so the rollback journal can restore the path on a failed
// run. No-op when journaling is OFF or the path was already captured.
func RecordPreWrite(root, relPath string) { recordPreWrite(root, relPath) }

// RestoreRollback rewinds every journaled path to its captured pre-run
// state and returns the sorted list of paths it restored. A path that
// existed before the run is rewritten with its original bytes + mode; a
// path that did NOT exist is removed (deleting forge's freshly-written
// output and pruning any now-empty parent directories forge created).
// Best-effort per path: an individual restore error does not abort the
// rest (a partially-restored tree still beats a fully mid-regen one), but
// the path is omitted from the returned list so the caller can report
// exactly what was recovered. Clears the journal and turns journaling
// OFF — a restored run is over.
func RestoreRollback(root string) []string {
	if rollbackJournal == nil {
		return nil
	}
	restored := make([]string, 0, len(rollbackJournal))
	for relPath, entry := range rollbackJournal {
		full := filepath.Join(root, relPath)
		if entry.existed {
			mode := entry.mode
			if mode == 0 {
				mode = 0o644
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				continue
			}
			if err := os.WriteFile(full, entry.content, mode); err != nil {
				continue
			}
			restored = append(restored, relPath)
			continue
		}
		// Did not exist pre-run: delete forge's new output.
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			continue
		}
		pruneEmptyParents(root, filepath.Dir(full))
		restored = append(restored, relPath)
	}
	rollbackJournal = nil
	sort.Strings(restored)
	return restored
}

// pruneEmptyParents removes now-empty directories from dir up toward
// (but never including) root. A forge write may have created nested dirs
// (handlers/<svc>/, internal/db/) that should not linger after a
// rollback deletes the only file inside them. Stops at the first
// non-empty (or unremovable) directory, and never ascends past root.
func pruneEmptyParents(root, dir string) {
	rootClean := filepath.Clean(root)
	for {
		dir = filepath.Clean(dir)
		if dir == rootClean || !isUnder(dir, rootClean) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// isUnder reports whether dir is strictly within root (a proper
// descendant), guarding the prune walk against ascending past the
// project root via a stray relative component.
func isUnder(dir, root string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !filepath.IsAbs(rel) &&
		!hasDotDotPrefix(rel)
}

// hasDotDotPrefix reports whether rel escapes its base via a leading
// "..". filepath.Rel can return paths like "../sibling"; those must not
// be pruned.
func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 2 && rel[0] == '.' && rel[1] == '.'
}
