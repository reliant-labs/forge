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
// run. Non-pipeline callers (forge project upgrade, project creation) never call
// Begin, so journaling stays OFF and their writes are recorded nowhere —
// they have their own recovery stories.
package checksums

import (
	"bytes"
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

// rollbackRoot is the project root the current journal is armed for.
// Needed by RecordPreWriteAbs, whose call sites (the scaffold-once raw
// writers in internal/codegen) address files by a single joined path and
// have no separate (root, relPath) pair to hand us. Set by
// BeginRollbackJournal; cleared with the journal.
var rollbackRoot string

// BeginRollbackJournal turns journaling ON and clears any prior capture.
// Called once at the head of a `forge generate` run, before any writer
// fires, with the project root the run operates on. After this, every
// forge write captures its target's pre-run state (once per path) so
// RestoreRollback can undo the whole run.
func BeginRollbackJournal(root string) {
	rollbackJournal = map[string]rollbackEntry{}
	if abs, err := filepath.Abs(root); err == nil {
		rollbackRoot = abs
	} else {
		rollbackRoot = root
	}
}

// CommitRollback drops the journal without restoring anything — the
// run's writes stand. Called on the success path; also turns journaling
// back OFF so a subsequent non-pipeline write in the same process isn't
// silently recorded.
func CommitRollback() {
	rollbackJournal = nil
	rollbackRoot = ""
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

// RecordPreWriteAbs is RecordPreWrite for call sites that only hold the
// joined destination path (the scaffold-once raw writers in
// internal/codegen address files as filepath.Join(projectDir, rel) and
// never thread the pair through). The path is resolved against the
// journal's armed root; a path outside that root is not captured — the
// journal restores relative to the root, so an outside path is not ours
// to rewind.
//
// This closes the defect where a failed generate run reverted the Tier-1
// files it wrote but left the scaffold-once files written THE SAME RUN
// (e.g. handlers_crud.go surviving while its handlers_crud_ops_gen.go
// dependency was rolled back), stranding the tree in a state that
// compiles against neither the pre-run nor the post-run world.
func RecordPreWriteAbs(path string) {
	if rollbackJournal == nil {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	rel, err := filepath.Rel(rollbackRoot, abs)
	if err != nil || rel == "." || filepath.IsAbs(rel) || hasDotDotPrefix(rel) {
		return
	}
	recordPreWrite(rollbackRoot, rel)
}

// SnapshotJournalTargets copies the CURRENT on-disk content of every
// journaled path — i.e. the failed run's output — into preserveDir,
// mirroring each file's project-relative path. Called BEFORE
// RestoreRollback on the failure path so the revert (which is the right
// default: the user's tree must come back clean) does not also destroy
// the only evidence of WHY the run failed: a `go build` validate error
// cites file:line coordinates in generated sources, and pre-fix the
// revert deleted those sources, leaving the user to debug a compiler
// error against code they could not read.
//
// The preserve directory is recreated fresh on every call so successive
// failures never interleave. Journaled paths whose current bytes equal
// their captured pre-run bytes are skipped (nothing new to inspect), as
// are paths that no longer exist. Best-effort per file — a copy failure
// drops that file from the returned list, never aborts. Returns the
// sorted relative paths preserved; nil when journaling is OFF, the
// journal is empty, or nothing qualified.
func SnapshotJournalTargets(root, preserveDir string) []string {
	if len(rollbackJournal) == 0 {
		return nil
	}
	if err := os.RemoveAll(preserveDir); err != nil {
		return nil
	}
	preserved := make([]string, 0, len(rollbackJournal))
	for relPath, entry := range rollbackJournal {
		current, err := os.ReadFile(filepath.Join(root, relPath))
		if err != nil {
			continue // gone or unreadable — nothing to preserve
		}
		if entry.existed && bytes.Equal(current, entry.content) {
			continue // byte-identical to pre-run — the revert won't touch it
		}
		dest := filepath.Join(preserveDir, relPath)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			continue
		}
		if err := os.WriteFile(dest, current, 0o644); err != nil {
			continue
		}
		preserved = append(preserved, relPath)
	}
	if len(preserved) == 0 {
		return nil
	}
	sort.Strings(preserved)
	return preserved
}

// WriteSummary is the run's write ledger: what forge's writers actually
// did to the tree, derived from the same journal the rollback uses.
//
// The three fields answer three DIFFERENT questions, and conflating them
// is what made `✅ Code generation complete!` unfalsifiable:
//
//	Touched == 0            no emitter fired at all. The pipeline ran and
//	                        produced nothing. This is the silent-no-op
//	                        signature (`--steps mocks` omitting its own
//	                        emitter printed success from exactly here).
//	Touched > 0, changed 0  every emitter fired and the bytes already
//	                        matched — a healthy idempotent re-run.
//	changed > 0             real work landed; the names are the proof.
type WriteSummary struct {
	// Touched is every path a forge writer targeted this run, whether or
	// not the bytes changed.
	Touched int
	// Created are paths that did not exist before the run.
	Created []string
	// Updated are paths that existed and whose bytes differ now.
	Updated []string
}

// Changed returns the number of paths whose on-disk bytes differ from
// their pre-run state.
func (s WriteSummary) Changed() int { return len(s.Created) + len(s.Updated) }

// SummarizeWrites compares every journaled path's CURRENT bytes against
// the pre-run bytes the journal captured, and reports what changed. It
// mutates nothing — call it any time before CommitRollback (which drops
// the journal) to learn what the run actually did.
//
// Returns a zero WriteSummary when journaling is OFF, so callers that run
// outside the pipeline degrade to "no claim" rather than to a false one.
func SummarizeWrites(root string) WriteSummary {
	if rollbackJournal == nil {
		return WriteSummary{}
	}
	sum := WriteSummary{Touched: len(rollbackJournal)}
	for relPath, entry := range rollbackJournal {
		current, err := os.ReadFile(filepath.Join(root, relPath))
		if err != nil {
			// Targeted but not readable now: the write failed, or a later
			// step removed it. Either way the bytes are not what they
			// were, and silence would be the wrong answer — but we cannot
			// call it created or updated, so it stays counted in Touched
			// only.
			continue
		}
		if !entry.existed {
			sum.Created = append(sum.Created, relPath)
			continue
		}
		if !bytes.Equal(current, entry.content) {
			sum.Updated = append(sum.Updated, relPath)
		}
	}
	sort.Strings(sum.Created)
	sort.Strings(sum.Updated)
	return sum
}

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
		// A scaffold-once file created THIS run is being un-created, so its
		// birth record must go with it. Leaving the record behind would make
		// the failed run consume the path's one and only birth: the ledger
		// would read "already scaffolded", the file would be gone, and no
		// later run would ever emit it. No-op for paths that were never
		// scaffold-once.
		ForgetScaffold(root, relPath)
		pruneEmptyParents(root, filepath.Dir(full))
		restored = append(restored, relPath)
	}
	rollbackJournal = nil
	rollbackRoot = ""
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
