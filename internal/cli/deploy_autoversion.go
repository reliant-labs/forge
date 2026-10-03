package cli

// The automatic release version a no-version `forge env deploy` cuts, and the
// rule that stops it cutting twice for one checkout (owner decision O-15,
// doc §7.2 "Auto version").
//
// `<YYYYMMDD>.<HHMMSS>-<tree12>`: the UTC cut time and the first 12 hex of the
// checkout's provenance tree.
//
// WHY THIS SHAPE.
//
//   - It is unique per project (T15) without a read-modify-write of the
//     ledger, so two deploys cannot race for a name.
//   - It SORTS by time, which is what makes `forge env status` and the
//     promote direction logic read correctly against hand-named releases.
//   - The tree makes it identify CONTENT, which is what the reuse rule below
//     needs. Two deploys of one checkout produce one release.
//
// WHAT IS DELIBERATELY NOT IN THE NAME. A dirty or branch build carries that
// fact in its PROVENANCE, never in its version (owner decision O-7: no naming
// rule). A name that encoded it would be a policy expressed as a string, and
// every consumer would have to parse it to recover what the provenance
// already states exactly.

import (
	"context"
	"fmt"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// autoVersionTimeFormat is the UTC timestamp half.
const autoVersionTimeFormat = "20060102.150405"

// autoVersionShortHash is how much of the tree (or commit) the name carries.
// Twelve hex is git's own abbreviation length for a large repository: long
// enough that a collision is not a practical concern, short enough that the
// version stays readable in a status table.
const autoVersionShortHash = 12

// autoVersionFor is the version a no-version deploy would cut for this
// provenance, at this instant.
//
// An UNHASHED tree (F-16: `CaptureProvenance` gave up on a very large
// untracked set) falls back to the commit, and such a release is NEVER reused
// — see reusableReleaseForTree. The fallback keeps the name meaningful rather
// than emitting a bare timestamp, but a commit does not identify the built
// CONTENT when the tree could not be hashed, so it must not be treated as if
// it did.
func autoVersionFor(prov release.Provenance, now time.Time) string {
	stamp := now.UTC().Format(autoVersionTimeFormat)
	switch {
	case prov.Tree != "":
		return stamp + "-" + shortHash(prov.Tree)
	case prov.Commit != "":
		return stamp + "-" + shortHash(prov.Commit)
	default:
		// Not a git tree at all. The timestamp alone is still unique per
		// project and still sorts, which is everything the ledger needs.
		return stamp
	}
}

func shortHash(h string) string {
	if len(h) <= autoVersionShortHash {
		return h
	}
	return h[:autoVersionShortHash]
}

// reusableReleaseForTree looks for a release this env's ledger already holds
// whose provenance tree equals this checkout's, and returns it.
//
// THIS IS WHAT MAKES A RETRIED DEPLOY IDEMPOTENT. Without it, every re-run of
// `forge env deploy <env>` — after a declined confirmation, a failed rollout, a
// CI retry — would cut a new version for bytes that already have one, and the
// ledger would fill with near-duplicates whose only difference is the minute
// they were named. One tree is built and cut once.
//
// AN UNHASHED TREE IS NEVER REUSED (F-16). When `provenance.tree` is empty
// forge does not know what the content was, so two builds that merely share a
// commit may differ — a dirty worktree, an untracked file that got too large
// to hash. Reusing on the commit would pin a release to bytes it was not cut
// from, which is the one mistake the whole release model exists to prevent.
// So an unhashed tree always cuts.
//
// A ledger read failure is NOT fatal: it means "no reusable release found",
// and the deploy cuts. The alternative — failing a deploy because a list call
// hiccuped — would turn an optimisation into an outage.
func reusableReleaseForTree(ctx context.Context, ledger releaseLedger, tree string) (*release.Release, error) {
	if tree == "" || ledger == nil {
		return nil, nil
	}
	releases, err := ledger.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range releases {
		rel := releases[i]
		if rel.Provenance != nil && rel.Provenance.Tree == tree {
			return &rel, nil
		}
	}
	return nil, nil
}

// printAutoVersionReuse says why no release was cut. Worth a line: a deploy
// that silently reported a version the operator did not name, and did not cut,
// reads as if it skipped a step.
func printAutoVersionReuse(rel release.Release) {
	fmt.Printf("[deploy] reusing release %s — its provenance tree matches this checkout, so there is nothing new to cut\n",
		rel.Version)
}
