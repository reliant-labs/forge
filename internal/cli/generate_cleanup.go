// Marker-driven stale-artifact cleanup.
//
// Forge's pre-2026-06-05 cleanup walked generated-artifact directories
// and removed anything whose name didn't match a re-derived "expected"
// set computed from forge.yaml — and deleted user code when the
// re-derivation disagreed with on-disk snake_case proto layouts. The
// manifest era fixed that by deleting only manifest-recorded paths; the
// self-certifying era keeps the same safety property without the
// manifest: only files that carry a forge:hash certification marker are
// candidates. The marker IS forge's authorship record, embedded in the
// file itself — paths without one are user content and forge doesn't
// get to delete them.
//
// A candidate is a marker-bearing file the current run did NOT re-emit
// (per the WrittenThisRun set). Guardrails before deletion:
//
//   - Owner-step gate. The tier1OwnerRegistry in generate_tier1_scope.go
//     maps generated paths back to the step that emits them. A path
//     whose owning step is gated off this run is left in place; that
//     step wasn't going to write it regardless, so missing-from-
//     WrittenThisRun is uninformative.
//   - Upgrade-managed paths. `forge upgrade` is their emitter; `forge
//     generate` deliberately leaves them alone.
//   - Verification. Only PRISTINE markers (certified machine output)
//     are deleted. A file whose marker fails verification was
//     hand-edited — stale or not, those bytes are the user's now, so
//     it is reported but never removed.
//   - Disowned paths are never candidates.
//
// Scoped-fallback entries (.forge/hashes.json, comment-incapable
// formats) get the same treatment keyed off the recorded hash.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/generator"
)

// upgradeManagedPaths is a single-load cache of the upgrade-managed
// path set. The set is constant for the binary's lifetime (it's
// derived from compiled-in upgrade.go file lists), so loading it once
// avoids re-walking the kind/binary matrix every cleanup pass. Tests
// that need to swap it can do so by reassigning before invoking
// cleanupStaleArtifacts.
var upgradeManagedPaths = generator.UpgradeManagedPaths()

// cleanupStaleArtifacts scans for forge-certified files the current
// pipeline run did not re-emit and returns them as deletion candidates,
// plus the list of stale-but-hand-edited files (reported, never
// deleted). Both slices are sorted for deterministic output. Candidate
// paths are returned absolute (legacy contract of the caller's
// Rel-based display); handEdited paths are project-relative.
//
// Deletion is gated on ctx.ForceCleanup: when true the function deletes
// each pristine candidate as it walks; when false it returns the
// candidate list without touching the filesystem so the caller can
// format a "would delete" warning.
func cleanupStaleArtifacts(ctx *pipelineContext) (candidates []string, handEdited []string, err error) {
	if ctx == nil {
		return nil, nil, nil
	}
	cs := ctx.Checksums

	markers := checksums.ScanMarkers(ctx.AbsPath)
	rels := make([]string, 0, len(markers))
	for rel := range markers {
		rels = append(rels, rel)
	}
	if cs != nil {
		for rel := range cs.Unstampable {
			if _, dup := markers[rel]; !dup {
				rels = append(rels, rel)
			}
		}
	}
	sort.Strings(rels)

	for _, rel := range rels {
		// This run wrote this path — definitely not stale.
		if checksums.WrittenThisRun[rel] {
			continue
		}
		// Disowned: user-owned by recorded intent (markers are stripped
		// at disown time; this is a belt-and-braces check).
		if cs.IsDisowned(rel) {
			continue
		}
		// Upgrade-managed paths: certified, but `forge generate` is not
		// the emitter — `forge upgrade` writes these on version bumps.
		if upgradeManagedPaths[rel] {
			continue
		}
		// Owner-step gate. If the owning emitter step is gated off this
		// run, we can't conclude the path is stale.
		if gate := tier1OwnerGate(rel); gate != nil && !gate(ctx) {
			continue
		}

		full := filepath.Join(ctx.ProjectDir, rel)

		// Pristineness decides delete-vs-report. For marker files the
		// scan already classified them; fallback entries compare the
		// recorded hash.
		pristine := false
		if info, ok := markers[rel]; ok {
			pristine = info.Status == checksums.Pristine
		} else if cs != nil {
			content, rerr := os.ReadFile(full)
			if rerr != nil {
				continue // gone or unreadable — nothing to clean
			}
			pristine = checksums.BodyHash(content) == cs.Unstampable[rel]
		}
		if !pristine {
			// Stale AND hand-edited: the bytes are the user's now.
			// Surface it so the drift isn't silent, but never delete.
			handEdited = append(handEdited, rel)
			continue
		}

		candidates = append(candidates, full)
		if !ctx.ForceCleanup {
			continue
		}

		if removeErr := os.Remove(full); removeErr != nil && !os.IsNotExist(removeErr) {
			// Permission-denied and friends: surface a warning rather
			// than aborting — a single locked file shouldn't tank the
			// sweep.
			fmt.Fprintf(os.Stderr, "⚠️  cleanup partial: could not remove %s (%v)\n", full, removeErr)
			continue
		}
		if cs != nil {
			delete(cs.Unstampable, rel)
		}

		// Best-effort: prune the now-empty parent directory. A single
		// os.Remove on the dir succeeds only if it's empty; "directory
		// not empty" is the dominant, expected case.
		if rmDirErr := os.Remove(filepath.Dir(full)); rmDirErr != nil && !os.IsNotExist(rmDirErr) {
			if ctx.Verbose {
				fmt.Fprintf(os.Stderr, "  ℹ️  cleanup: empty-dir prune failed for %s (%v) — usually expected (sibling files present)\n", filepath.Dir(full), rmDirErr)
			}
		}
	}

	sort.Strings(candidates)
	sort.Strings(handEdited)
	return candidates, handEdited, nil
}
