// The scaffold-once BIRTH LEDGER — the record that makes "written once"
// mean once, including across a deletion.
//
// FRICTION: a generated CRUD lifecycle test (handlers_crud_test.go) whose
// own banner reads "yours: scaffolded once, never touched again — forge
// will not overwrite this file" came BACK three times in one session. The
// author had hardened their migrations with CHECK constraints, the
// scaffolded lifecycle test did not satisfy them, and they deleted it.
// Every subsequent `forge generate` wrote it again (15:44, 15:48, 15:49).
//
// The cause was the shape of the guard, not the guard's placement. Every
// scaffold-once writer in forge asked ONE question:
//
//	if _, err := os.Stat(path); err == nil { return }   // exists → skip
//
// which implements "restore if absent", not "write once". Existence is a
// two-state answer to a THREE-state question:
//
//	never scaffolded      → write it (birth)
//	scaffolded, present   → leave it (the user's bytes)
//	scaffolded, DELETED   → leave it DELETED (also the user's decision)
//
// The third state is invisible to os.Stat: a deleted file and a file that
// was never born look identical on disk. Deleting is a legitimate act of
// ownership — an author who does not want forge's lifecycle test should be
// able to remove it and have it stay removed — so the missing state is the
// one that carries the user's intent, and forge was silently overriding it.
//
// This ledger supplies the third state: the set of project-relative paths
// forge has EVER scaffolded. Present in the ledger + absent on disk =
// deliberately deleted, and forge does not write it again.
//
// ── Why a new state file, given the package doc says scaffold files need
// no record ───────────────────────────────────────────────────────────
//
// That claim (see the package comment in checksums.go, and migrate.go's
// DroppedTier2) is exactly the assumption this defect disproves. The
// existing ledgers cannot answer the question:
//
//   - .forge/disowned.json records a DIFFERENT, opposite decision ("forge
//     owns this path but I am taking it") and is keyed to Tier-1 paths.
//   - .forge/hashes.json holds render hashes for comment-incapable Tier-1
//     outputs only; scaffold-once files carry no marker and no hash by
//     design, so there is nothing to look them up by.
//   - the forge:hash marker scan reads certification out of the FILE, and
//     a deleted file has no bytes to read.
//   - the rollback journal is per-run and dropped on commit.
//
// Every one of those is keyed on something the deleted file would have to
// still exist to provide. So the record has to be external, and it has to
// be committed — a teammate cloning the repo must inherit "we removed the
// lifecycle test", not re-acquire it on their first generate.
//
// It is deliberately PATHS ONLY — no hashes, no content, no history. The
// manifest-era failure this package spent a release escaping was a global
// hash ledger that churned on every run and made 22 of 69 commits pure
// bookkeeping. This file changes only when a scaffold is BORN or a
// scaffolded path is deliberately re-adopted, which is a handful of lines
// at project birth and silence forever after.
//
// ── Re-scaffolding on purpose ─────────────────────────────────────────
//
// Deleting the file is no longer the reset (that is the whole point), so
// the reset is deleting the ENTRY: remove the path from
// .forge/scaffolded.json and the next `forge generate` scaffolds it fresh.
// The file is human-readable, sorted, and committed precisely so that this
// is an ordinary reviewable edit.
package checksums

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ScaffoldedFile is the birth ledger's project-relative location. Committed
// (see the .gitignore negation beside disowned.json / hashes.json): the
// decision to delete a scaffold belongs to the repository, not to one
// developer's working copy.
const ScaffoldedFile = ".forge/scaffolded.json"

// ScaffoldEntry is one path's birth record. The timestamp is provenance
// for a human reading the file ("when did forge decide it had done this?");
// nothing in the code branches on it.
type ScaffoldEntry struct {
	ScaffoldedAt string `json:"scaffolded_at,omitempty"`
}

// scaffoldedJSON is the wire shape, mirroring disownedJSON / hashesJSON.
type scaffoldedJSON struct {
	ForgeVersion string                   `json:"forge_version,omitempty"`
	Files        map[string]ScaffoldEntry `json:"files"`
}

// scaffoldLedgers memoizes the per-root ledger for the life of the process,
// keyed by absolute project root. Scaffold writers fire many times per run
// and several of them (the internal/codegen raw writers) hold only a joined
// absolute path, so the load has to be lazy and cached rather than threaded
// through every signature.
var scaffoldLedgers = map[string]map[string]ScaffoldEntry{}

// ResetScaffoldLedgerCache drops the memoized ledgers so the next query
// re-reads from disk. Called at the head of a pipeline run (a long-lived
// process must not answer from a previous invocation's cache) and by tests
// that build several projects in one binary.
func ResetScaffoldLedgerCache() { scaffoldLedgers = map[string]map[string]ScaffoldEntry{} }

// loadScaffoldLedger returns root's ledger, reading it from disk on first
// use. A missing or unparseable file yields an EMPTY ledger rather than an
// error: the ledger's only job is to suppress a re-write, so the failure
// mode of an unreadable ledger must be forge's historical behavior
// (scaffold it), never a refusal to scaffold a project that has no ledger.
func loadScaffoldLedger(root string) map[string]ScaffoldEntry {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	if led, ok := scaffoldLedgers[abs]; ok {
		return led
	}
	led := map[string]ScaffoldEntry{}
	if data, rerr := os.ReadFile(filepath.Join(abs, ScaffoldedFile)); rerr == nil {
		var s scaffoldedJSON
		if json.Unmarshal(data, &s) == nil && s.Files != nil {
			led = s.Files
		}
	}
	scaffoldLedgers[abs] = led
	return led
}

// saveScaffoldLedger persists root's ledger, removing the file when the
// ledger is empty (the same no-bookkeeping-churn rule Save applies to the
// other two state files). Best-effort by the same discipline as the load:
// a ledger we cannot write must not abort a write that already succeeded.
func saveScaffoldLedger(root string) {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	led := scaffoldLedgers[abs]
	full := filepath.Join(abs, ScaffoldedFile)
	if len(led) == 0 {
		_ = os.Remove(full)
		return
	}
	if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
		return
	}
	data, merr := json.MarshalIndent(scaffoldedJSON{Files: led}, "", "  ")
	if merr != nil {
		return
	}
	_ = os.WriteFile(full, append(data, '\n'), 0o644)
}

// ScaffoldRecorded reports whether forge has EVER scaffolded relPath in
// this project. Combined with the file's presence on disk it resolves the
// three-state question in the file doc:
//
//	!recorded            → never scaffolded: write it
//	recorded && present  → the user's bytes: leave them
//	recorded && absent   → the user deleted it: leave it deleted
func ScaffoldRecorded(root, relPath string) bool {
	_, ok := loadScaffoldLedger(root)[filepath.ToSlash(relPath)]
	return ok
}

// RecordScaffold marks relPath as scaffolded and persists the ledger
// immediately.
//
// Immediate persistence (rather than a save hook at the end of the run) is
// deliberate: scaffold-once writes happen from FOUR entry points — the
// generate pipeline, `forge new`, `forge project upgrade`, and the
// `forge scaffold ...` commands — and only the first has a save hook. A
// ledger that recorded a birth but persisted it only on the pipeline path
// would leave every other path with the old restore-if-absent behavior,
// which is the defect this file exists to close.
//
// Idempotent: re-recording a known path neither changes the entry nor
// rewrites the file, so a steady-state `forge generate` produces no ledger
// diff.
func RecordScaffold(root, relPath string) {
	rel := filepath.ToSlash(relPath)
	led := loadScaffoldLedger(root)
	if _, ok := led[rel]; ok {
		return // already recorded — no churn
	}
	led[rel] = ScaffoldEntry{ScaffoldedAt: time.Now().UTC().Format(time.RFC3339)}
	saveScaffoldLedger(root)
}

// ForgetScaffold drops relPath's birth record, returning the path to the
// "never scaffolded" state so the next run scaffolds it fresh.
//
// The caller that matters is the rollback journal: a failed `forge
// generate` DELETES the scaffold files it created this run, and a birth
// record surviving that deletion would permanently suppress a file that
// never actually shipped — the run would have consumed the project's one
// chance to be born. See RestoreRollback.
func ForgetScaffold(root, relPath string) {
	rel := filepath.ToSlash(relPath)
	led := loadScaffoldLedger(root)
	if _, ok := led[rel]; !ok {
		return
	}
	delete(led, rel)
	saveScaffoldLedger(root)
}

// ScaffoldLedgerPaths returns root's recorded paths, sorted. Exposed for
// tests and for `forge project audit`-style reporting; callers must not
// mutate the ledger through it.
func ScaffoldLedgerPaths(root string) []string {
	led := loadScaffoldLedger(root)
	out := make([]string, 0, len(led))
	for p := range led {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// SplitScaffoldPath resolves an absolute destination path into the
// (projectRoot, relPath) pair the ledger is keyed on, by walking up to the
// nearest directory holding a forge.yaml.
//
// It exists because several scaffold-once writers — the raw ones in
// internal/codegen — were built to take a single joined path and never
// thread the pair through (the same shape that forced RecordPreWriteAbs to
// exist for the rollback journal). Rather than re-plumb every call site,
// the root is recovered here.
//
// When journaling is armed its root is authoritative: the pipeline knows
// which project it is generating, and a nested module inside the project
// (a frontend workspace, an examples/ tree) must not be mistaken for a
// separate project with its own ledger.
//
// Returns ok=false when no project root can be established — a bare temp
// directory in a unit test, for instance. Callers then fall back to the
// historical presence check rather than refusing to write.
func SplitScaffoldPath(path string) (root, relPath string, ok bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", false
	}
	if rollbackRoot != "" {
		if rel, rerr := filepath.Rel(rollbackRoot, abs); rerr == nil &&
			rel != "." && !filepath.IsAbs(rel) && !hasDotDotPrefix(rel) {
			return rollbackRoot, filepath.ToSlash(rel), true
		}
	}
	dir := filepath.Dir(abs)
	for {
		if _, serr := os.Stat(filepath.Join(dir, "forge.yaml")); serr == nil {
			rel, rerr := filepath.Rel(dir, abs)
			if rerr != nil {
				return "", "", false
			}
			return dir, filepath.ToSlash(rel), true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

// ScaffoldOnceDecision is the single answer every scaffold-once writer in
// forge asks for, so that the three-state rule is stated ONCE instead of
// being re-derived (and mis-derived as a bare os.Stat) at each site.
//
// Returns write=true only for a genuine birth. When it returns false the
// caller must not write; when it returns true the caller writes and then
// calls RecordScaffold.
//
// The `present` arm ADOPTS: a file that exists but has no birth record is
// a project scaffolded by an older forge, so the record is backfilled here.
// Without that backfill, every project created before this ledger existed
// would keep the old behavior forever — the user's first deletion would
// look like "never scaffolded" and be undone exactly once more.
func ScaffoldOnceDecision(root, relPath string) (write bool) {
	full := filepath.Join(root, relPath)
	if _, err := os.Stat(full); err == nil {
		// Present. The user owns the bytes either way; backfill the birth
		// record so a LATER deletion is recognizable as a deletion.
		RecordScaffold(root, relPath)
		return false
	} else if !os.IsNotExist(err) {
		// Unreadable (permissions, a directory in the way). Not a birth we
		// can vouch for — writing over it is the destructive guess.
		return false
	}
	// Absent: birth iff forge has never scaffolded it here.
	return !ScaffoldRecorded(root, relPath)
}
