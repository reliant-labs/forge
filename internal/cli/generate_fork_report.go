// Fork-skip reporting for `forge generate`.
//
// A forked Tier-1 file (`forge generate --accept`) is persistently
// opted out of codegen: WriteGeneratedFile skips it on every run, even
// with --force. Before this file existed the skip was silent, which
// produced the worst failure mode in the fork lifecycle
// (.forge/backlog.md 2026-06-03/05): a user edits forge.yaml or a
// contract, runs `forge generate`, and the forked wire_gen.go /
// bootstrap.go simply doesn't change — "adding a Deps field did
// nothing" — with no signal pointing at the fork.
//
// reportForkedSkips closes that gap: one clearly-formatted line per
// forked-skipped path on EVERY generate run (not gated behind --explain
// or an error), naming the side-render location and the reconcile
// command. Kept in its own file (not generate.go / generate_pipeline.go)
// so the touch on those conflict-hotspot files is a one-line call.
package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/generator"
)

// reportForkedSkips prints one loud line per Tier-1 path whose write
// was skipped this run because the entry is forked. Writes to w (stderr
// in production; a buffer in tests). No-op when nothing was skipped —
// the common case stays clean.
//
// Loud-once UX: entries whose Accepted flag is already true were
// reported on a previous run and stay quiet — FRICTION (cp-forge,
// 2026-06-08): 11 long-standing forks reporting every run drowned out
// new fork detections. After printing, every reported entry has
// Accepted flipped in-memory; the SaveChecksums defer in
// runGeneratePipeline persists the flip (the report defer is registered
// AFTER the save defer precisely so it runs first on the way out).
// Standing forks remain visible via `forge audit --json` (forked_files)
// and the doctor fork check; `forge unfork` clears both flags so a
// re-fork later is loud again. A nil cs falls back to report-every-time
// (defensive; the pipeline always passes real checksums).
func reportForkedSkips(w io.Writer, cs *generator.FileChecksums) {
	skips := checksums.ForkedSkipsThisRun()
	var loud []string
	for _, p := range skips {
		if cs != nil {
			if entry, ok := cs.Files[p]; ok && entry.Accepted {
				continue
			}
		}
		loud = append(loud, p)
	}
	if len(loud) == 0 {
		return
	}
	fmt.Fprintln(w)
	for _, p := range loud {
		fmt.Fprintf(w, "⚠ forked (not regenerated): %s — fresh render at %s; run 'forge unfork --merge %s' to reconcile\n",
			p, checksums.SideRenderRelPath(p), p)
	}
	fmt.Fprintf(w, "%d forked file(s) skipped. One-time notice — future runs stay quiet for these paths; `forge audit --json` lists standing forks, `forge unfork <path>` returns a file to forge ownership.\n", len(loud))
	if cs != nil {
		for _, p := range loud {
			entry := cs.Files[p]
			entry.Accepted = true
			cs.Files[p] = entry
		}
	}
}

// warnForkCoherenceOnAccept prints, for each just-accepted path that
// belongs to a fork-coherence group (checksums/groups.go), a prominent
// warning naming the sibling files that will KEEP regenerating. Forking
// one member of a coupled set is the .forge/backlog.md 2026-06-03
// build-break shape — the warning fires at the moment of forking, when
// the user can still back out (git checkout + re-run without --accept).
func warnForkCoherenceOnAccept(w io.Writer, accepted []string) {
	for _, p := range accepted {
		g, ok := checksums.CoherenceGroupFor(p)
		if !ok {
			continue
		}
		fmt.Fprintf(w, "\n⚠ %s belongs to fork-coherence group %q. These sibling files share its generated symbols and will KEEP regenerating:\n", p, g.Name)
		for _, sib := range g.SiblingPatterns(p) {
			fmt.Fprintf(w, "   - %s\n", sib)
		}
		fmt.Fprintf(w, "   When a future generate changes a sibling, the forked file can drift out of sync and break the build.\n")
		fmt.Fprintf(w, "   Prefer moving customizations to a user-owned extension point; reconcile later with 'forge unfork --merge %s'.\n", p)
	}
}

// warnIncoherentForkGroups fires at pipeline exit: for every coherence
// group with at least one forked member AND at least one non-forked
// member whose fresh render CHANGED this run, print a prominent warning
// naming the (possibly now-incoherent) forked siblings.
//
// This is the runtime pairing of warnForkCoherenceOnAccept: that one
// warns when the fork is created, this one warns on the run where the
// risk materializes. The AST-level dangling-reference check
// (generate_dangling_check.go) hard-errors on the subset it can prove;
// this warning covers symbol drift the AST check can't see (changed
// signatures, renamed fields, behavioral coupling).
//
// We deliberately don't auto-fork or auto-unfork the rest of the group
// — both would destroy state the user may need. Loud + actionable is
// the contract.
func warnIncoherentForkGroups(w io.Writer, cs *generator.FileChecksums) {
	if cs == nil {
		return
	}
	changed := checksums.ChangedRendersThisRun()
	if len(changed) == 0 {
		return
	}
	for _, g := range checksums.CoherenceGroups() {
		var forked []string
		for rel, entry := range cs.Files {
			if entry.Forked && entry.Tier != 2 && g.Matches(rel) {
				forked = append(forked, rel)
			}
		}
		if len(forked) == 0 {
			continue
		}
		var changedSiblings []string
		for _, p := range changed {
			// Changed ⇒ freshly written ⇒ not forked; no extra filter needed.
			if g.Matches(p) {
				changedSiblings = append(changedSiblings, p)
			}
		}
		if len(changedSiblings) == 0 {
			continue
		}
		sort.Strings(forked)
		fmt.Fprintf(w, "\n⚠ fork-coherence group %q: regenerated file(s) changed this run while sibling fork(s) stayed frozen:\n", g.Name)
		fmt.Fprintf(w, "   changed this run: %s\n", strings.Join(changedSiblings, ", "))
		fmt.Fprintf(w, "   forked (frozen):  %s\n", strings.Join(forked, ", "))
		fmt.Fprintf(w, "   The forked file(s) may now be incoherent with the fresh renders (build-break risk — shared generated symbols).\n")
		fmt.Fprintf(w, "   Reconcile with 'forge unfork --merge <path>', or re-own with 'forge unfork <path>' + 'forge generate'.\n")
	}
}
