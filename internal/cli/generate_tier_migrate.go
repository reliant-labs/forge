// Tier-2 (user-owned-after-scaffold) exemption for the drift scan.
//
// Some forge-certified files are deliberately NOT stomp-guarded even
// though they carry a forge:hash marker: the upgrade-managed
// "checksum-protected" starters (Dockerfile, Taskfile.yml,
// .golangci.yml, …) and the one-shot .github scaffolds. The marker's
// job there is to let `forge upgrade` distinguish "still the pristine
// scaffold" (auto-update on version bumps) from "user customized it"
// (skip) — editing them is SANCTIONED, so a failed verification must
// not abort `forge generate`.
//
// generator.Tier2ManagedPaths is the registry of those paths. This file
// hosts the cached set plus the filter the drift consumers (stomp
// guard, audit, ci verify-generated) apply to the raw ScanTier1Drift
// result.
//
// (Historical note: this file used to flip stale tier=1 manifest
// entries to tier=2. The manifest is gone — the reclassification story
// is now: the legacy-manifest migration never stamps Tier-2-managed
// paths, the Tier-2 writer un-stamps reclassified pipeline outputs on
// their next scaffold pass, and this filter exempts the rest.)
package cli

import (
	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/generator"
)

// tier2MigratedPaths is a single-load cache of the Tier-2 managed path
// set (constant for the binary's lifetime — derived from compiled-in
// template registries). Tests that need a synthetic set can reassign it.
var tier2MigratedPaths = generator.Tier2ManagedPaths()

// filterTier2Managed drops drift entries for paths whose canonical
// template tier is Tier-2 (user-owned after scaffold) — hand-edits
// there are sanctioned, not drift.
func filterTier2Managed(drift []checksums.Tier1DriftEntry) []checksums.Tier1DriftEntry {
	kept := drift[:0:0]
	for _, d := range drift {
		if tier2MigratedPaths[d.Path] {
			continue
		}
		kept = append(kept, d)
	}
	return kept
}

// scanProjectDrift is the shared drift probe: the raw self-certification
// scan minus the Tier-2-managed exemption. Every consumer that reports
// "hand-edited Tier-1 files" (the stomp guard, `forge audit`, `forge ci
// verify-generated`) goes through this so they tell one story.
func scanProjectDrift(root string, cs *generator.FileChecksums) []checksums.Tier1DriftEntry {
	return filterTier2Managed(checksums.ScanTier1Drift(root, cs))
}
