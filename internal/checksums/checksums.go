// Package checksums tracks sha256 digests of forge-generated files so
// `forge generate`, `forge upgrade`, and `forge audit` can distinguish
// "user-edited" files from "untouched generator output".
//
// The package is the lowest layer in the dependency stack so that both
// `internal/codegen` (the per-feature emitters) and `internal/generator`
// (the project-level scaffolder) can import it without an import cycle.
// `internal/generator` re-exports the public symbols as type aliases so
// existing call sites continue to compile unchanged.
package checksums

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const checksumFile = ".forge/checksums.json"

// historyLimit caps how many prior-render checksums each file entry keeps.
// Long-running projects re-render Tier-2 files many times; without a bound
// the checksums.json file would grow unbounded. 20 is small enough to keep
// the file readable when a human peeks at it, large enough that ordinary
// usage never falls off the back of the window.
const historyLimit = 20

// ResetSkipWrite clears the written-this-run set. Called at the start of
// each pipeline run to avoid leaking state across forge invocations in
// tests or long-lived processes. (Historically this also cleared the
// fork-era SkipWrite opt-out set, which is gone — disowned paths are
// Tier-2 manifest entries, not per-run state.)
func ResetSkipWrite() {
	WrittenThisRun = map[string]bool{}
}

// WrittenThisRun is a per-pipeline-run set of relative paths that the
// current `forge generate` invocation has successfully written (or
// re-recorded) via the `WriteGeneratedFile*` family. Populated as a
// side effect of RecordFile so every emit/accept path is captured at
// a single chokepoint.
//
// The manifest-driven stale-artifact sweep consults this set: an entry
// in `FileChecksums.Files` whose path is NOT in WrittenThisRun is a
// candidate for removal (forge tracked the path but didn't re-emit it
// this run, e.g. because the service was renamed or removed). Reset
// between runs alongside SkipWrite.
//
// Stored as a package-level map (not on FileChecksums) for the same
// reason as SkipWrite: pipeline-run state, not persistent state.
var WrittenThisRun = map[string]bool{}

// MarkWrittenThisRun records that relPath was written or accept-promoted
// during the current run. Exposed publicly so tests that bypass the
// WriteGeneratedFile* chokepoint (e.g. by populating FileChecksums via
// constructor) can still simulate the post-emit set when exercising
// downstream cleanup logic.
func MarkWrittenThisRun(relPath string) { WrittenThisRun[relPath] = true }


// sideRenderOnly is the per-run set of paths whose Tier-1 writes are
// redirected to `.forge/render/<relpath>` instead of the real file.
// Populated by `forge generate --explain-drift`: the drift guard lets
// the pipeline proceed so the emitters produce a fresh render to diff
// against, but the user's drifted on-disk content must survive — the
// whole point of the flag is to SHOW the user what regeneration would
// change before they choose between the extension point, --force, and
// `forge disown`.
var sideRenderOnly = map[string]bool{}

// AddSideRenderOnly marks relPath as side-render-only for the current
// run. Idempotent.
func AddSideRenderOnly(relPath string) { sideRenderOnly[relPath] = true }

// ResetPerRunState clears the per-pipeline-run tracking sets (the
// --explain-drift side-render redirects, the heal-notice dedupe set,
// and the --no-heal strict mode). Called at the start of each pipeline
// run alongside ResetSkipWrite / ResetTier2State so a long-lived
// process (tests, watch mode) doesn't leak state across invocations.
func ResetPerRunState() {
	sideRenderOnly = map[string]bool{}
	healNoticed = map[string]bool{}
	DisableAutoHeal = false
}

// DisableAutoHeal is the `forge generate --no-heal` strict mode: when
// true, on-disk content that matches a PRIOR render in checksum history
// (but not the latest) is treated as a hand-edit instead of stale
// codegen — IsFileModified reports it modified, writes skip it, and
// CheckTier1Drift reports it as drift (flagged HistoricalMatch).
//
// The default (false) keeps the history auto-heal that `forge upgrade`
// depends on: stale codegen regenerates cleanly without --force. The
// strict mode exists because a hand-edit can hash-collide with history
// in the human sense — the user deliberately reverts a file to content
// forge once rendered (FRICTION cp-forge fr-2c1c2328c7: a hand-edit to
// pkg/app/bootstrap.go equaled a prior render and was silently
// reverted). Auto-heal stays the default but is LOUD (HealNoticeFn);
// --no-heal is the escape hatch the notice teaches.
var DisableAutoHeal bool

// HealNoticeFn is invoked once per file per run when a WriteGeneratedFile*
// call is about to overwrite on-disk content that matches a historical
// (but not the latest) render — the "auto-heal stale codegen" path.
// Healing must never be silent: the on-disk bytes are about to change
// and, if the historical match was actually a deliberate user revert,
// this notice is the only trace of the overwrite.
//
// Package var so the CLI can redirect the report; the default prints to
// stderr. Never nil it out — assign a no-op func in tests instead.
var HealNoticeFn = func(relPath string) {
	fmt.Fprintf(os.Stderr,
		"♻️  healing stale codegen: %s — on-disk content matches a prior forge render (not the latest); overwriting with the current template. If that content was a deliberate edit, restore it and re-run with --no-heal, then move the edit to an extension point or `forge disown` the file.\n",
		relPath)
}

// healNoticed is the per-run dedupe set for HealNoticeFn — multiple
// emitters may write the same path in one pipeline run; the user needs
// one notice per file, not one per write.
var healNoticed = map[string]bool{}

// maybeNoticeHeal fires HealNoticeFn when writing newContent to relPath
// would overwrite on-disk content that matches a historical (non-latest)
// render. No notice when nothing is destroyed: untracked path, missing
// file, on-disk == current hash (ordinary regen), on-disk == newContent
// (no byte change), or a true hand-edit (the caller skips those writes
// before reaching here).
func maybeNoticeHeal(cs *FileChecksums, root, relPath string, newContent []byte) {
	if cs == nil || healNoticed[relPath] {
		return
	}
	entry, ok := cs.Files[relPath]
	if !ok {
		return
	}
	onDisk, err := os.ReadFile(filepath.Join(root, relPath))
	if err != nil {
		return
	}
	h := Hash(onDisk)
	if h == entry.Hash || h == Hash(newContent) {
		return
	}
	historical := false
	for _, prior := range entry.History {
		if h == prior {
			historical = true
			break
		}
	}
	if !historical {
		return
	}
	healNoticed[relPath] = true
	if HealNoticeFn != nil {
		HealNoticeFn(relPath)
	}
}

// Tier2OverwriteFn is the per-file hook the Tier-2 writer consults when
// it has detected a hand-edited Tier-2 file. Returning true clobbers the
// user's edits with the freshly rendered content; returning false (or
// leaving the hook nil) preserves them — the historic safe default that
// Tier-2's "scaffold once, never overwrite" contract promises.
//
// `forge generate --reset-tier2` installs a hook here that prompts the
// user (y/N) per file. `--reset-tier2 --yes` installs a hook that
// returns true without prompting.
//
// Plumbed as a package-level hook rather than a parameter so we don't
// have to churn the dozen call sites of WriteGeneratedFileTier2 to add
// a new positional flag for the small fraction of runs that actually
// reset Tier-2.
var Tier2OverwriteFn func(relPath string) bool

// Tier2PreservedCount counts how many Tier-2 writes WriteGeneratedFileTier2
// skipped due to a modified-file detection during the current pipeline
// run. The pipeline reads this at exit to print the
//
//	"--force preserved N hand-edited Tier-2 file(s); pass --reset-tier2 ..."
//
// summary line. Reset between runs alongside SkipWrite + Tier2OverwriteFn.
var Tier2PreservedCount int

// ResetTier2State clears the Tier-2 hook + preserved counter. Called at
// the start of each pipeline run so a previous --reset-tier2 hook from
// a long-lived test process doesn't leak.
func ResetTier2State() {
	Tier2OverwriteFn = nil
	Tier2PreservedCount = 0
}

// FileChecksums tracks sha256 digests of forge-generated files.
//
// Wire format is JSON. Two shapes are supported on disk:
//
//  1. Legacy ("flat"): files maps relative path -> hex sha256 string.
//  2. Current ("entry"): files maps relative path -> {hash, history[]}
//     where history is the list of every checksum forge has rendered for
//     that file (deduplicated, bounded by historyLimit). The most recent
//     entry sits at the end of history; hash always equals the last entry.
//
// On load, both shapes are accepted — a legacy hex string is promoted into
// an entry whose history is seeded with the same hash. On save, we always
// emit the entry shape. This means a one-time round-trip migrates legacy
// checksum files transparently.
//
// History is the mechanism that closes the "stale codegen vs user-modified"
// gap (FORGE_BACKLOG.md). When a template is updated and the on-disk file
// matches a *prior* render rather than the current one, forge knows the
// file is stale codegen — not a user edit — and can auto-update it cleanly
// without --force.
type FileChecksums struct {
	ForgeVersion string                       `json:"forge_version"`
	Files        map[string]FileChecksumEntry `json:"files"` // relative path -> entry
}

// FileChecksumEntry is the per-file checksum + history record.
//
// Hash is the checksum of the most recent render forge produced for this
// path. History is the deduplicated, bounded list of *every* checksum forge
// has rendered for this path, oldest first. Hash always equals the last
// element of History (when History is non-empty).
//
// Tier classifies the lifecycle of the file:
//
//   - 1 — forge-owned, generated every run (`// Code generated by forge
//     ... DO NOT EDIT.`). The Tier-1 stomp guard refuses to overwrite a
//     Tier-1 file that's been hand-edited unless `--force` is set; the
//     sanctioned exit from Tier-1 is `forge disown` (one-way → Tier-2).
//   - 2 — user-owned. Either scaffold-once (`// yours: scaffolded once,
//     never touched again — forge will not overwrite this file`) or
//     disowned (see Disowned below). Forge never
//     overwrites Tier-2 content; the stomp guard ignores it.
//   - 0 — unset / legacy. Treated as Tier-1-equivalent for the stomp
//     guard, since pre-tier checksums covered exclusively Tier-1 files.
//     New writes should always specify a tier; the unset-→-1 default is
//     a backstop for migration only.
//
// On JSON load, missing History is treated as empty — backwards-compat with
// pre-history checksum files. On JSON save, the legacy flat-string shape is
// never emitted (we always write the structured form).
type FileChecksumEntry struct {
	Hash    string   `json:"hash"`
	History []string `json:"history,omitempty"`
	Tier    int      `json:"tier,omitempty"`
	// Disowned records a one-way ownership transfer: the path was Tier-1
	// (forge-owned) until the user ran `forge disown`, which flipped it
	// to Tier-2 (user-owned) permanently. The marker distinguishes
	// disowned files from ordinary scaffold-once starters in `forge
	// audit` / `forge doctor`. Re-adoption is by deletion: remove the
	// file and run `forge generate` — the emitter re-emits and the entry
	// returns to Tier-1 (marker cleared).
	Disowned bool `json:"disowned,omitempty"`
	// DisownedAt records when the path was disowned (RFC3339 UTC), so
	// audit can report how long a file has been outside forge ownership.
	DisownedAt string `json:"disowned_at,omitempty"`
	// Forked / Accepted / ForkedAt are the LEGACY fork-era fields
	// (`forge generate --accept` before forks were removed). They are
	// never written by current forge; they exist only so the pipeline's
	// legacy-fork migration (and `forge unfork`, the one-release
	// migration tool) can read entries recorded by older versions and
	// convert them to Disowned. New code must not set them.
	Forked   bool   `json:"forked,omitempty"`
	Accepted bool   `json:"accepted,omitempty"`
	ForkedAt string `json:"forked_at,omitempty"`
	// Exports lists the names of public top-level identifiers (functions,
	// types, vars) declared in the most recently rendered version of the
	// file. Used by the post-emit rename-detection pass: when a Tier-1
	// file's new render drops a name that's still present in the prior
	// Exports list, the project may have hand-written callers still
	// referencing the old name. Recorded only for Go files (.go) — other
	// languages fall through to an empty list. FRICTION 2026-06-02:
	// cp-forge dogfood pass surfaced silent `forgedb.Migrations()` →
	// `forgedb.MigrationsFS` rename leaving callers orphaned.
	Exports []string `json:"exports,omitempty"`
}

// Load loads the checksum file from the project root.
//
// Accepts both the legacy flat shape (files: path -> hash string) and the
// current structured shape (files: path -> {hash, history[]}). Legacy
// entries get their hash promoted into a 1-element history on load.
func Load(root string) (*FileChecksums, error) {
	path := filepath.Join(root, checksumFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &FileChecksums{Files: make(map[string]FileChecksumEntry)}, nil
		}
		return nil, err
	}
	cs, err := unmarshalChecksums(data)
	if err != nil {
		return nil, err
	}
	if cs.Files == nil {
		cs.Files = make(map[string]FileChecksumEntry)
	}
	return cs, nil
}

// unmarshalChecksums decodes either the legacy flat shape or the current
// structured shape into a *FileChecksums. Splits decode logic out so it's
// independently testable.
func unmarshalChecksums(data []byte) (*FileChecksums, error) {
	// Try the structured shape first.
	var structured FileChecksums
	if err := json.Unmarshal(data, &structured); err == nil && structured.Files != nil {
		// If any value decoded as zero (Hash==""), it's likely the legacy
		// shape sneaking through (Go's json package will populate the
		// struct with a zero value when the source is a string). Fall
		// through to the legacy decode path.
		legacy := false
		for _, e := range structured.Files {
			if e.Hash == "" {
				legacy = true
				break
			}
		}
		if !legacy {
			// Backfill history for entries that have a hash but no history
			// (mid-migration files written by an older forge).
			for k, e := range structured.Files {
				if len(e.History) == 0 && e.Hash != "" {
					e.History = []string{e.Hash}
					structured.Files[k] = e
				}
			}
			return &structured, nil
		}
	}

	// Legacy flat shape: files: path -> hex string.
	var legacy struct {
		ForgeVersion string            `json:"forge_version"`
		Files        map[string]string `json:"files"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, err
	}
	cs := &FileChecksums{
		ForgeVersion: legacy.ForgeVersion,
		Files:        make(map[string]FileChecksumEntry, len(legacy.Files)),
	}
	for path, hash := range legacy.Files {
		cs.Files[path] = FileChecksumEntry{Hash: hash, History: []string{hash}}
	}
	return cs, nil
}

// Save writes the checksum file in the structured shape.
func Save(root string, cs *FileChecksums) error {
	path := filepath.Join(root, checksumFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Hash returns the sha256 hex digest of content.
func Hash(content []byte) string {
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])
}

// IsFileModified checks if a generated file has been modified by the user.
// Returns true if the file exists but its hash matches neither the stored
// current checksum nor any prior-render in history.
//
// Returns false if the file doesn't exist, was never tracked, or matches
// any checksum forge has previously rendered for this path. The "matches
// any prior render" branch is what lets stale codegen sail through upgrade
// cleanly — forge knows it generated this content, even if the template
// has since moved on. Under DisableAutoHeal (--no-heal) that branch is
// off: a historical match counts as a hand-edit.
func (cs *FileChecksums) IsFileModified(root, relPath string) bool {
	entry, ok := cs.Files[relPath]
	if !ok {
		return false // never tracked, not "modified"
	}
	content, err := os.ReadFile(filepath.Join(root, relPath))
	if err != nil {
		return false // file doesn't exist
	}
	h := Hash(content)
	if h == entry.Hash {
		return false
	}
	if !DisableAutoHeal {
		for _, prior := range entry.History {
			if h == prior {
				return false
			}
		}
	}
	return true
}

// MatchesAnyKnownRender reports whether content's checksum equals the
// current Hash or any entry in History for this path. Distinct from
// IsFileModified in that it operates on raw content (not on-disk reads)
// and treats untracked paths as "not matching" rather than "not modified".
func (cs *FileChecksums) MatchesAnyKnownRender(relPath string, content []byte) bool {
	entry, ok := cs.Files[relPath]
	if !ok {
		return false
	}
	h := Hash(content)
	if h == entry.Hash {
		return true
	}
	for _, prior := range entry.History {
		if h == prior {
			return true
		}
	}
	return false
}

// RecordFile stores the checksum for a generated file and appends it to
// the file's render history.
//
// History is deduplicated (no contiguous duplicates and no resurrected
// older entries) and bounded by historyLimit. The most recent entry sits
// at the end of History; Hash always mirrors the tail.
func (cs *FileChecksums) RecordFile(relPath string, content []byte) {
	if cs.Files == nil {
		cs.Files = make(map[string]FileChecksumEntry)
	}
	h := Hash(content)
	entry := cs.Files[relPath]
	entry.Hash = h

	// Dedupe: if the new hash is already in history, drop the prior
	// occurrence and re-append at the tail. This keeps the "most recent
	// at the end" invariant and prevents history from churning when a
	// template flips back-and-forth between two shapes.
	filtered := entry.History[:0]
	for _, prior := range entry.History {
		if prior == h {
			continue
		}
		filtered = append(filtered, prior)
	}
	filtered = append(filtered, h)

	// Bound: keep the most recent historyLimit entries.
	if len(filtered) > historyLimit {
		filtered = filtered[len(filtered)-historyLimit:]
	}
	// Copy to a fresh slice to avoid aliasing the original backing array.
	entry.History = append([]string(nil), filtered...)
	cs.Files[relPath] = entry

	// Mark the path as "written this run" so the manifest-driven
	// stale-artifact sweep knows not to treat it as orphaned. Every
	// WriteGeneratedFile* helper funnels through here on a successful
	// write; DisownPaths does too, which is the right semantics
	// (forge actively touched the path this run, no matter how).
	WrittenThisRun[relPath] = true
}

// WriteGeneratedFile writes content to a file and records its checksum.
// If the file has been user-modified (checksum mismatch and no history
// match) and force is false, the write is skipped. Returns true if the
// file was written, false if skipped.
//
// A nil cs is tolerated — the file is written but no checksum is tracked.
// This keeps the helper safe to call from code paths that don't have a
// *FileChecksums in scope (e.g. legacy callers being incrementally ported).
//
// Tier defaults to 0 (legacy) — the stomp guard treats this as Tier-1
// for back-compat. Prefer the typed wrappers (WriteGeneratedFileTier1 /
// WriteGeneratedFileTier2) at new call sites so the manifest is explicit.
func WriteGeneratedFile(root, relPath string, content []byte, cs *FileChecksums, force bool) (bool, error) {
	reAdopting := false
	if cs != nil {
		if entry, ok := cs.Files[relPath]; ok && (entry.Disowned || entry.Forked) {
			// Disowned entry (or a legacy fork-era one awaiting
			// migration): the file is user-owned — never overwrite, even
			// with force=true: --force discards current-run hand-edits to
			// forge-owned files, it does not undo a recorded ownership
			// transfer.
			//
			// Exception — re-adoption: if the file is GONE the user
			// deleted it to hand it back to forge ("delete the file, run
			// `forge generate`"). Fall through and re-emit; the entry
			// returns to Tier-1 below.
			//
			// Plain (non-disowned) Tier-2 entries deliberately do NOT
			// take this branch: a Tier-1 emitter targeting one is the
			// tier-reclassification upgrade path (e.g. scaffold-era
			// skill files migrating to Tier-1), and the IsFileModified
			// guard below still protects user edits there.
			if _, statErr := os.Stat(filepath.Join(root, relPath)); statErr == nil {
				return false, nil
			} else if !os.IsNotExist(statErr) {
				return false, statErr
			}
			reAdopting = true
		}
	}
	if sideRenderOnly[relPath] {
		// `--explain-drift` redirect: park the fresh render for the
		// post-pipeline diff, leave the user's drifted content (and the
		// checksum entry) untouched. Must run BEFORE the IsFileModified
		// skip below — these paths are drifted by definition, and the
		// silent skip would discard exactly the render we need.
		if err := WriteSideRenderNoBase(root, relPath, content); err != nil {
			return false, err
		}
		return false, nil
	}
	if cs != nil && !force && cs.IsFileModified(root, relPath) {
		// User has modified this file — skip
		return false, nil
	}

	// Auto-heal is never silent: if the bytes about to be replaced match
	// a historical (non-latest) render, say so — that hash equality is
	// the one case where a deliberate user revert is indistinguishable
	// from stale codegen (FRICTION cp-forge fr-2c1c2328c7).
	maybeNoticeHeal(cs, root, relPath, content)

	fullPath := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(fullPath, content, 0o644); err != nil {
		return false, err
	}
	if cs != nil {
		cs.RecordFile(relPath, content)
		if reAdopting {
			// Re-adoption completes here: the user deleted a disowned
			// file and forge just re-emitted it, so the entry returns to
			// Tier-1 (forge-owned) and the disowned + legacy markers are
			// cleared. (WriteGeneratedFileTier1 would re-stamp Tier
			// anyway; doing it at the chokepoint also covers legacy
			// tier-agnostic callers.)
			entry := cs.Files[relPath]
			entry.Tier = 1
			entry.Disowned = false
			entry.DisownedAt = ""
			entry.Forked = false
			entry.Accepted = false
			entry.ForkedAt = ""
			cs.Files[relPath] = entry
		}
	}
	return true, nil
}

// WriteGeneratedFileTier1 writes a Tier-1 (regenerated-every-run) file
// and tags the checksum entry as Tier-1. The Tier-1 tag drives the
// pre-pipeline stomp guard: a future `forge generate` run will refuse to
// overwrite a Tier-1 file that's been hand-edited unless the user passes
// `--force` (clobber); the sanctioned permanent exit is `forge disown`
// (one-way transfer to user ownership).
//
// Behaviorally identical to WriteGeneratedFile when force is true — the
// Tier-1 tag is purely metadata that downstream guards consult. The
// caller is responsible for upholding the Tier-1 contract by including
// the `// Code generated by forge ... DO NOT EDIT.` banner in `content`.
func WriteGeneratedFileTier1(root, relPath string, content []byte, cs *FileChecksums, force bool) (bool, error) {
	wrote, err := WriteGeneratedFile(root, relPath, content, cs, force)
	if err != nil || !wrote || cs == nil {
		return wrote, err
	}
	entry := cs.Files[relPath]
	entry.Tier = 1
	cs.Files[relPath] = entry
	return wrote, nil
}

// WriteGeneratedFileTier2 writes a Tier-2 (scaffold-once user-owned)
// file. The stomp guard ignores Tier-2 entries — forge never auto-
// regenerates these, so a user edit is the expected steady state, not
// drift. Use this for "// yours: scaffolded once ..." templates.
//
// Tier-2 ignores `force=true`: it is for Tier-1 "clobber my hand-edits"
// semantics, and applying that to Tier-2 would break the scaffold-once
// contract (`forge generate --force` would silently nuke a hand-written
// service.go). To explicitly opt-in to Tier-2 overwrite, callers can
// install Tier2OverwriteFn (the cobra RunE does this when the user
// passes `--reset-tier2`).
func WriteGeneratedFileTier2(root, relPath string, content []byte, cs *FileChecksums, force bool) (bool, error) {
	_ = force // intentionally ignored — see doc above.

	if cs != nil {
		if entry, ok := cs.Files[relPath]; ok && entry.Disowned {
			// Disowned entries record the USER's content hash (captured
			// at disown time), so the modified-file check below would not
			// protect an unedited disowned file from a re-scaffold. Skip
			// outright while the file exists; a deleted disowned file
			// falls through and is re-scaffolded (re-adoption parity with
			// the Tier-1 writer).
			if _, statErr := os.Stat(filepath.Join(root, relPath)); statErr == nil {
				return false, nil
			} else if !os.IsNotExist(statErr) {
				return false, statErr
			}
		}
	}

	// Tier-2 modification check is `--reset-tier2`-gated, not force-gated.
	modified := cs != nil && cs.IsFileModified(root, relPath)
	if modified {
		overwrite := false
		if Tier2OverwriteFn != nil {
			overwrite = Tier2OverwriteFn(relPath)
		}
		if !overwrite {
			Tier2PreservedCount++
			return false, nil
		}
	}

	// Same loud-heal contract as the Tier-1 writer: overwriting content
	// that matches a historical render is announced, never silent.
	maybeNoticeHeal(cs, root, relPath, content)

	fullPath := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(fullPath, content, 0o644); err != nil {
		return false, err
	}
	if cs != nil {
		cs.RecordFile(relPath, content)
		entry := cs.Files[relPath]
		entry.Tier = 2
		// A re-scaffold of a deleted disowned file is back to being an
		// ordinary pristine starter — clear the marker.
		entry.Disowned = false
		entry.DisownedAt = ""
		cs.Files[relPath] = entry
	}
	return true, nil
}

// Tier1DriftEntry reports a single Tier-1 file whose on-disk content
// does not match any recorded render (current Hash or any History
// entry). The slice returned by CheckTier1Drift is sorted to give
// callers a stable error message order.
//
// Files that aren't tracked, can't be read (e.g. removed by the user),
// or aren't tagged Tier-1 are skipped — only positive evidence of a
// hand-edit on a Tier-1 file is reported.
type Tier1DriftEntry struct {
	Path         string
	RecordedHash string
	OnDiskHash   string
	HistoryDepth int // count of prior renders we compared against
	// HistoricalMatch marks an entry that matched a PRIOR render in
	// History — only reported as drift under DisableAutoHeal
	// (--no-heal); the default mode auto-heals these. The flag lets the
	// guard message explain that the file would regenerate cleanly
	// without --no-heal.
	HistoricalMatch bool
}

// CheckTier1Drift walks all Tier-1 entries in cs.Files and returns the
// list of files whose on-disk content doesn't match any recorded render.
// Used as a pre-pipeline guard so `forge generate` errors loudly instead
// of silently overwriting hand-edited Tier-1 files.
//
// Legacy entries (Tier == 0) are treated as Tier-1 for back-compat with
// pre-tier checksum files — the stomp guard predates the explicit tier
// tag and we don't want a forge upgrade to suddenly stop guarding files
// the user expects to be guarded.
func (cs *FileChecksums) CheckTier1Drift(root string) []Tier1DriftEntry {
	if cs == nil || len(cs.Files) == 0 {
		return nil
	}
	var drift []Tier1DriftEntry
	for relPath, entry := range cs.Files {
		// Legacy fork-era entries (pre-disown migration) are skipped —
		// the pipeline migration converts them to Disowned/Tier-2 before
		// this guard runs, but out-of-pipeline callers may still see them.
		if entry.Forked {
			continue
		}
		if entry.Tier != 0 && entry.Tier != 1 {
			continue
		}
		full := filepath.Join(root, relPath)
		content, err := os.ReadFile(full)
		if err != nil {
			continue // file gone or unreadable — not a "stomp" candidate
		}
		h := Hash(content)
		if h == entry.Hash {
			continue
		}
		matchedHistory := false
		for _, prior := range entry.History {
			if h == prior {
				matchedHistory = true
				break
			}
		}
		if matchedHistory && !DisableAutoHeal {
			// Stale codegen — the loud auto-heal in WriteGeneratedFile*
			// regenerates it (and notices); not a stomp candidate. Under
			// --no-heal the match IS reported, flagged so the guard
			// message can explain.
			continue
		}
		drift = append(drift, Tier1DriftEntry{
			Path:            relPath,
			RecordedHash:    entry.Hash,
			OnDiskHash:      h,
			HistoryDepth:    len(entry.History),
			HistoricalMatch: matchedHistory,
		})
	}
	// Stable ordering for deterministic error messages.
	sortDrift(drift)
	return drift
}

// DisownPaths performs the one-way ownership transfer for each path:
// the on-disk content is recorded as the entry's current hash, the
// entry flips to Tier-2 (user-owned) and is marked Disowned with a
// timestamp. After this, no `WriteGeneratedFile*` call ever touches the
// path again (while the file exists) — there is no fork limbo, no
// side-render parking, no reconcile-later state. Re-adoption is by
// deletion: remove the file and run `forge generate`.
//
// Legacy fork-era flags on the entry are cleared as part of the flip,
// so converting an old `forked: true` entry through this helper leaves
// a clean disowned record.
func (cs *FileChecksums) DisownPaths(root string, relPaths []string) error {
	if cs == nil {
		return nil
	}
	for _, p := range relPaths {
		full := filepath.Join(root, p)
		content, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		cs.RecordFile(p, content)
		entry := cs.Files[p]
		entry.Tier = 2
		entry.Disowned = true
		entry.DisownedAt = nowRFC3339()
		entry.Forked = false
		entry.Accepted = false
		entry.ForkedAt = ""
		cs.Files[p] = entry
	}
	return nil
}

// sortDrift sorts a slice of Tier1DriftEntry by Path, ascending.
// Inlined to avoid an import of "sort" at the top of the file just for
// one stable-order pass.
func sortDrift(drift []Tier1DriftEntry) {
	for i := 1; i < len(drift); i++ {
		for j := i; j > 0 && drift[j-1].Path > drift[j].Path; j-- {
			drift[j-1], drift[j] = drift[j], drift[j-1]
		}
	}
}

// nowRFC3339 returns the current UTC time in RFC3339. Indirected
// through a package var so tests can pin a deterministic timestamp.
var nowRFC3339 = func() string {
	return time.Now().UTC().Format(time.RFC3339)
}
