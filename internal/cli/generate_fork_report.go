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

	"github.com/reliant-labs/forge/internal/checksums"
)

// reportForkedSkips prints one loud line per Tier-1 path whose write
// was skipped this run because the entry is forked. Writes to w (stderr
// in production; a buffer in tests). No-op when nothing was skipped —
// the common case stays clean.
func reportForkedSkips(w io.Writer) {
	skips := checksums.ForkedSkipsThisRun()
	if len(skips) == 0 {
		return
	}
	fmt.Fprintln(w)
	for _, p := range skips {
		fmt.Fprintf(w, "⚠ forked (not regenerated): %s — fresh render at %s; run 'forge unfork --merge %s' to reconcile\n",
			p, checksums.SideRenderRelPath(p), p)
	}
}
