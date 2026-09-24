package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// Tests for the promote change set: `forge env promote --plan` / `--json`.
//
// Every test here runs through the bindingStore and promoteGitReader seams, so
// none of them needs a cluster, a registry, or (except where the point IS the
// file) a ledger on disk. The premise of each case is STATED — "prod is bound
// to these digests", "this commit is not in the checkout" — rather than staged
// into a temp directory and re-derived.

// fakeGit is a promoteGitReader whose history is a literal.
//
// The states that matter most for a plan are properties of a repository's
// history — a commit cut on a branch this checkout does not have, a git that
// cannot run — and those are tedious and slow to stage for real. Here they are
// one field each.
type fakeGit struct {
	// have is the set of commits present in this "checkout". A commit
	// absent from it is the "cut on another branch" case.
	have map[string]bool
	// commits is what CommitsBetween returns, keyed "from..to" so a test
	// can assert the DIRECTION the range was read in.
	commits map[string][]string
	// err makes CommitsBetween fail, for the git-unavailable case.
	err error
	// rangesAsked records every from..to actually requested.
	rangesAsked []string
}

func (f *fakeGit) HasCommit(_ context.Context, _, commit string) bool {
	return f.have[commit]
}

func (f *fakeGit) CommitsBetween(_ context.Context, _, from, to string) ([]string, error) {
	f.rangesAsked = append(f.rangesAsked, from+".."+to)
	if f.err != nil {
		return nil, f.err
	}
	return f.commits[from+".."+to], nil
}

// allCommitsPresent is the common case: every commit the plan asks about is in
// the checkout.
func allCommitsPresent(commits ...string) *fakeGit {
	have := map[string]bool{}
	for _, c := range commits {
		have[c] = true
	}
	return &fakeGit{have: have, commits: map[string][]string{}}
}

// parseFixtureTime parses an RFC3339 fixture literal ("" is the zero time).
func parseFixtureTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return ts
}

// rel builds a release ledger with one shared OCI digest per named image.
func rel(version, createdAt, commit string, dirty bool, images map[string]string) release.Release {
	arts := map[string]release.Artifact{}
	for name, digest := range images {
		arts[name] = release.Artifact{
			Kind:    release.KindOCI,
			Mode:    release.ModeShared,
			Digests: map[string]string{release.SharedVariant: digest},
		}
	}
	return release.Release{
		Version:   version,
		CreatedAt: parseFixtureTime(createdAt),
		Git:       release.Git{Commit: commit, Dirty: dirty},
		Artifacts: arts,
	}
}

// changeFor pulls one image's classification out of a plan.
func changeFor(t *testing.T, plan promotePlan, image string) promoteImageChange {
	t.Helper()
	for _, img := range plan.Images {
		if img.Image == image {
			return img
		}
	}
	t.Fatalf("image %q missing from the plan; got %+v", image, plan.Images)
	return promoteImageChange{}
}

// ─── Image classification ────────────────────────────────────────────────────

// TestPromotePlan_ClassifiesEveryImageChange is the headline case: one promote
// that does all four things at once, so a classifier that collapsed any pair
// of them cannot pass.
//
// The ADDED/REMOVED half is the reason this is not a digest-map diff. This
// mirrors control-plane's real shape, where the prod release line carries
// `internal-console` and the staging line does not — so promoting between
// those lines changes the image SET, not just the digests.
func TestPromotePlan_ClassifiesEveryImageChange(t *testing.T) {
	releases := []release.Release{
		rel("v1.5.15", "2026-03-01T00:00:00Z", "cccccccccccc", false, map[string]string{
			"control-plane":    sha("same"),  // unchanged
			"reliant":          sha("new"),   // changed
			"internal-console": sha("addme"), // added
		}),
		rel("v1.3.0", "2026-01-01T00:00:00Z", "aaaaaaaaaaaa", false, map[string]string{
			"control-plane":  sha("same"),
			"reliant":        sha("old"),
			"daemon-gateway": sha("gone"), // removed: current has it, target does not
		}),
	}
	store := newMemBindingStore(map[string]release.Promotion{
		"staging": {
			Release:    "v1.3.0",
			PromotedAt: parseFixtureTime("2026-01-02T00:00:00Z"),
			Resolved: map[string]string{
				"control-plane":  sha("same"),
				"reliant":        sha("old"),
				"daemon-gateway": sha("gone"),
			},
		},
	})

	plan, err := computePromotePlan(context.Background(), promotePlanOptions{
		Env: "staging", Version: "v1.5.15", ProjectDir: t.TempDir(),
		Bindings: store, Releases: newMemReleaseLedger(releases...),
		Git: allCommitsPresent("aaaaaaaaaaaa", "cccccccccccc"),
	})
	if err != nil {
		t.Fatalf("compute plan: %v", err)
	}

	if got := changeFor(t, plan, "control-plane").Change; got != promoteImageUnchanged {
		t.Errorf("control-plane: same digest on both sides must be unchanged, got %s", got)
	}

	// CHANGED must carry BOTH digests. A row that reported only the target
	// would leave a reviewer unable to answer "from what?", which is half
	// the question.
	changed := changeFor(t, plan, "reliant")
	if changed.Change != promoteImageChanged {
		t.Errorf("reliant: differing digests must be changed, got %s", changed.Change)
	}
	if changed.CurrentDigest != sha("old") || changed.TargetDigest != sha("new") {
		t.Errorf("a changed image must carry both digests, got from=%q to=%q", changed.CurrentDigest, changed.TargetDigest)
	}

	added := changeFor(t, plan, "internal-console")
	if added.Change != promoteImageAdded {
		t.Errorf("internal-console: in target only must be added, got %s", added.Change)
	}
	if added.CurrentDigest != "" || added.TargetDigest != sha("addme") {
		t.Errorf("an added image has no current digest, got from=%q to=%q", added.CurrentDigest, added.TargetDigest)
	}

	removed := changeFor(t, plan, "daemon-gateway")
	if removed.Change != promoteImageRemoved {
		t.Errorf("daemon-gateway: in current only must be removed, got %s", removed.Change)
	}
	if removed.CurrentDigest != sha("gone") || removed.TargetDigest != "" {
		t.Errorf("a removed image has no target digest, got from=%q to=%q", removed.CurrentDigest, removed.TargetDigest)
	}

	want := promoteImageTally{Unchanged: 1, Changed: 1, Added: 1, Removed: 1}
	if plan.Tally != want {
		t.Errorf("tally = %+v, want %+v", plan.Tally, want)
	}
	if !plan.Changed {
		t.Error("a promote that changes, adds and removes images must report changed=true")
	}
}

// TestPromotePlan_FirstPromote: an env with no binding. Every image is ADDED
// (there is nothing to compare against), the direction is `initial`, and the
// commit range says first-promote rather than pretending to be empty.
func TestPromotePlan_FirstPromote(t *testing.T) {
	releases := []release.Release{rel("v1.4.0", "2026-02-01T00:00:00Z", "bbbbbbbbbbbb", false,
		map[string]string{"control-plane": sha("a")})}

	plan, err := computePromotePlan(context.Background(), promotePlanOptions{
		Env: "staging", Version: "v1.4.0", ProjectDir: t.TempDir(),
		Bindings: newMemBindingStore(nil), Releases: newMemReleaseLedger(releases...),
		Git: allCommitsPresent("bbbbbbbbbbbb"),
	})
	if err != nil {
		t.Fatalf("a first promote must not be an error: %v", err)
	}

	if plan.Current.Bound {
		t.Error("current.bound must be false for an env that was never promoted")
	}
	if plan.Direction != promoteDirectionInitial {
		t.Errorf("direction = %s, want initial", plan.Direction)
	}
	if got := changeFor(t, plan, "control-plane").Change; got != promoteImageAdded {
		t.Errorf("every image on a first promote is added, got %s", got)
	}
	if plan.Commits.State != promoteRangeFirstPromote {
		t.Errorf("commit range state = %s, want first_promote", plan.Commits.State)
	}
	// The distinction that matters: this is "no range", not "zero commits".
	if plan.Commits.Count != 0 || plan.Commits.Detail == "" {
		t.Errorf("a first promote must explain the absent range, got count=%d detail=%q", plan.Commits.Count, plan.Commits.Detail)
	}
}

// ─── Direction ───────────────────────────────────────────────────────────────

// TestPromotePlan_BackwardsPromoteReportsRollback is the most consequential
// single fact in the plan. Promoting an OLDER release is legitimate — it is how
// a rollback is spelled — but a reviewer who reads it as a forward move has
// misread the whole screen.
func TestPromotePlan_BackwardsPromoteReportsRollback(t *testing.T) {
	releases := []release.Release{
		rel("v1.5.15", "2026-03-01T00:00:00Z", "cccccccccccc", false, map[string]string{"reliant": sha("new")}),
		rel("v1.4.0", "2026-02-01T00:00:00Z", "bbbbbbbbbbbb", false, map[string]string{"reliant": sha("mid")}),
		rel("v1.3.0", "2026-01-01T00:00:00Z", "aaaaaaaaaaaa", false, map[string]string{"reliant": sha("old")}),
	}
	store := newMemBindingStore(map[string]release.Promotion{
		"prod": {Release: "v1.5.15", Resolved: map[string]string{"reliant": sha("new")}},
	})
	git := allCommitsPresent("aaaaaaaaaaaa", "cccccccccccc")
	// History runs oldest → newest, so a rollback's range is target..current.
	git.commits["aaaaaaaaaaaa..cccccccccccc"] = []string{"ccc1 later work", "bbb1 earlier work"}

	plan, err := computePromotePlan(context.Background(), promotePlanOptions{
		Env: "prod", Version: "v1.3.0", ProjectDir: t.TempDir(),
		Bindings: store, Releases: newMemReleaseLedger(releases...), Git: git,
	})
	if err != nil {
		t.Fatalf("a rollback must not be an error: %v", err)
	}

	if plan.Direction != promoteDirectionBehind {
		t.Fatalf("direction = %s, want behind (v1.3.0 is older than v1.5.15)", plan.Direction)
	}
	if plan.ReleasesBetween != 2 {
		t.Errorf("releases_between = %d, want 2 (v1.4.0 and v1.3.0 below v1.5.15)", plan.ReleasesBetween)
	}
	// The word must be in the human line, not just implied by the enum: the
	// text report is what an operator reads at 2am.
	if !strings.Contains(strings.ToUpper(plan.DirectionDetail), "ROLLBACK") {
		t.Errorf("direction_detail must name the rollback, got %q", plan.DirectionDetail)
	}
	// And the commits are being taken AWAY, which a consumer must not paint
	// as incoming changes.
	if !plan.Commits.Reverts {
		t.Error("a backwards promote must set commits.reverts so the listed commits are read as REMOVED, not added")
	}
	if plan.Commits.State != promoteRangeComputed || plan.Commits.Count != 2 {
		t.Errorf("range should still compute for a rollback, got %s count=%d", plan.Commits.State, plan.Commits.Count)
	}
	// Asked in oldest→newest order, or git log returns nothing at all.
	if len(git.rangesAsked) != 1 || git.rangesAsked[0] != "aaaaaaaaaaaa..cccccccccccc" {
		t.Errorf("a rollback range must be read target..current, asked %v", git.rangesAsked)
	}
}

// TestPromotePlan_ForwardPromoteReportsAhead is the other half. Without it a
// direction function hardcoded to "behind" would pass the rollback test.
func TestPromotePlan_ForwardPromoteReportsAhead(t *testing.T) {
	releases := []release.Release{
		rel("v1.5.15", "2026-03-01T00:00:00Z", "cccccccccccc", false, map[string]string{"reliant": sha("new")}),
		rel("v1.3.0", "2026-01-01T00:00:00Z", "aaaaaaaaaaaa", false, map[string]string{"reliant": sha("old")}),
	}
	store := newMemBindingStore(map[string]release.Promotion{
		"staging": {Release: "v1.3.0", Resolved: map[string]string{"reliant": sha("old")}},
	})
	git := allCommitsPresent("aaaaaaaaaaaa", "cccccccccccc")
	git.commits["aaaaaaaaaaaa..cccccccccccc"] = []string{"ccc1 a feature"}

	plan, err := computePromotePlan(context.Background(), promotePlanOptions{
		Env: "staging", Version: "v1.5.15", ProjectDir: t.TempDir(),
		Bindings: store, Releases: newMemReleaseLedger(releases...), Git: git,
	})
	if err != nil {
		t.Fatalf("compute plan: %v", err)
	}
	if plan.Direction != promoteDirectionAhead {
		t.Errorf("direction = %s, want ahead", plan.Direction)
	}
	if plan.Commits.Reverts {
		t.Error("a forward promote must not mark its commits as reverted")
	}
	if plan.Commits.Count != 1 {
		t.Errorf("count = %d, want 1", plan.Commits.Count)
	}
}

// TestPromotePlan_SameReleaseIsNoMove: re-promoting the bound release. The
// direction is `same` and the range is empty BY DEFINITION, which is a
// different claim from a measured zero.
func TestPromotePlan_SameReleaseIsNoMove(t *testing.T) {
	releases := []release.Release{rel("v1.4.0", "2026-02-01T00:00:00Z", "bbbbbbbbbbbb", false,
		map[string]string{"reliant": sha("a")})}
	store := newMemBindingStore(map[string]release.Promotion{
		"prod": {Release: "v1.4.0", Resolved: map[string]string{"reliant": sha("a")}},
	})

	plan, err := computePromotePlan(context.Background(), promotePlanOptions{
		Env: "prod", Version: "v1.4.0", ProjectDir: t.TempDir(),
		Bindings: store, Releases: newMemReleaseLedger(releases...), Git: allCommitsPresent("bbbbbbbbbbbb"),
	})
	if err != nil {
		t.Fatalf("compute plan: %v", err)
	}
	if plan.Direction != promoteDirectionSame {
		t.Errorf("direction = %s, want same", plan.Direction)
	}
	if plan.Commits.State != promoteRangeSameRelease {
		t.Errorf("range state = %s, want same_release", plan.Commits.State)
	}
	if plan.Changed {
		t.Error("re-promoting the same release with identical digests must report changed=false")
	}
}

// ─── Commit-range states that are NOT errors ─────────────────────────────────

// TestPromotePlan_CurrentLedgerMissingDoesNotFail: the env is bound to a
// release whose ledger is not in this checkout (cut on another branch). The
// binding is still real and still says what the env runs, so the plan stands —
// only the provenance and the range are unavailable.
func TestPromotePlan_CurrentLedgerMissingDoesNotFail(t *testing.T) {
	releases := []release.Release{rel("v1.5.15", "2026-03-01T00:00:00Z", "cccccccccccc", false,
		map[string]string{"reliant": sha("new")})}
	store := newMemBindingStore(map[string]release.Promotion{
		"prod": {Release: "v9.9.9-branchonly", Resolved: map[string]string{"reliant": sha("old")}},
	})

	plan, err := computePromotePlan(context.Background(), promotePlanOptions{
		Env: "prod", Version: "v1.5.15", ProjectDir: t.TempDir(),
		Bindings: store, Releases: newMemReleaseLedger(releases...), Git: allCommitsPresent("cccccccccccc"),
	})
	if err != nil {
		t.Fatalf("a missing CURRENT ledger must not fail the plan: %v", err)
	}

	if !plan.Current.Bound {
		t.Error("the binding is real even when its ledger is absent — bound must stay true")
	}
	if plan.Current.ReleaseKnown {
		t.Error("release_known must be false when the ledger is not in this checkout")
	}
	if plan.Current.Note == "" {
		t.Error("a missing ledger must be explained on the row, not left as blank fields")
	}
	if plan.Commits.State != promoteRangeLedgerMissing {
		t.Errorf("range state = %s, want ledger_missing", plan.Commits.State)
	}
	// The digest diff is still fully correct — that is the point of not
	// failing.
	if got := changeFor(t, plan, "reliant").Change; got != promoteImageChanged {
		t.Errorf("the digest diff must still be computed, got %s", got)
	}
	// And the direction cannot be known, rather than being guessed.
	if plan.Direction != promoteDirectionUnknown {
		t.Errorf("direction = %s, want unknown when one release is not in the ordering", plan.Direction)
	}
}

// TestPromotePlan_TargetLedgerMissingIsAnError is the deliberate asymmetry.
// Without the target ledger there are no digests to resolve, so there is no
// change set to preview — and if --plan rendered a plan here while the real
// promote errored, the two modes' exit codes would disagree, which is the one
// property this design holds onto.
func TestPromotePlan_TargetLedgerMissingIsAnError(t *testing.T) {
	dir := t.TempDir()
	_, err := computePromotePlan(context.Background(), promotePlanOptions{
		Env: "staging", Version: "v9.9.9", ProjectDir: dir,
		Bindings: newMemBindingStore(nil), Releases: newMemReleaseLedger(), Git: allCommitsPresent(),
	})
	if err == nil {
		t.Fatal("a missing TARGET release must be an error — there are no digests to preview")
	}
	if !strings.Contains(err.Error(), "v9.9.9") {
		t.Errorf("the error must name the release, got %v", err)
	}

	// And the same is true through the command, in BOTH modes: the exit
	// behaviour must not depend on --plan or --json.
	for _, opts := range []promoteOptions{
		{ProjectDir: dir, Bindings: newMemBindingStore(nil), Releases: newMemReleaseLedger(), Git: allCommitsPresent()},
		{DryRun: true, ProjectDir: dir, Bindings: newMemBindingStore(nil), Releases: newMemReleaseLedger(), Git: allCommitsPresent()},
		{DryRun: true, JSON: true, ProjectDir: dir, Bindings: newMemBindingStore(nil), Releases: newMemReleaseLedger(), Git: allCommitsPresent()},
	} {
		if err := runPromote(context.Background(), "v9.9.9", "staging", opts); err == nil {
			t.Errorf("runPromote(%+v) must fail on a missing target release", opts)
		}
	}
}

// TestPromotePlan_DirtyTargetRangeIsLabelledMeaningless.
//
// A dirty release's recorded commit is a REAL commit that does not describe
// the bytes that shipped. So the range would compute successfully and be
// wrong, which is worse than not computing it: the number looks checkable and
// is not. It must be refused and labelled, and — asserted here — git must not
// even be consulted, since any answer it gave would be misleading.
func TestPromotePlan_DirtyTargetRangeIsLabelledMeaningless(t *testing.T) {
	releases := []release.Release{
		rel("v1.5.1", "2026-03-01T00:00:00Z", "cccccccccccc", true, map[string]string{"reliant": sha("dirty")}),
		rel("v1.3.0", "2026-01-01T00:00:00Z", "aaaaaaaaaaaa", false, map[string]string{"reliant": sha("old")}),
	}
	store := newMemBindingStore(map[string]release.Promotion{
		"staging": {Release: "v1.3.0", Resolved: map[string]string{"reliant": sha("old")}},
	})
	git := allCommitsPresent("aaaaaaaaaaaa", "cccccccccccc")
	// If the range were silently computed, it would find this.
	git.commits["aaaaaaaaaaaa..cccccccccccc"] = []string{"ccc1 looks legitimate"}

	plan, err := computePromotePlan(context.Background(), promotePlanOptions{
		Env: "staging", Version: "v1.5.1", ProjectDir: t.TempDir(),
		Bindings: store, Releases: newMemReleaseLedger(releases...), Git: git,
	})
	if err != nil {
		t.Fatalf("a dirty release must not fail the plan: %v", err)
	}

	if plan.Commits.State != promoteRangeDirtyRelease {
		t.Fatalf("range state = %s, want dirty_release", plan.Commits.State)
	}
	if plan.Commits.Count != 0 || len(plan.Commits.Commits) != 0 {
		t.Errorf("a meaningless range must carry NO commits, got count=%d %v", plan.Commits.Count, plan.Commits.Commits)
	}
	if len(git.rangesAsked) != 0 {
		t.Errorf("git must not be consulted for a dirty release's range, asked %v", git.rangesAsked)
	}
	if !strings.Contains(strings.ToUpper(plan.Commits.Detail), "MEANINGLESS") {
		t.Errorf("the detail must say the range is meaningless, got %q", plan.Commits.Detail)
	}
	// The dirty flag must also survive onto the target so the text report
	// and a UI can badge it independently of the range.
	if plan.Target.Git == nil || !plan.Target.Git.Dirty {
		t.Error("target.git.dirty must be carried through to the plan")
	}
}

// TestPromotePlan_CommitAbsentFromCheckoutDoesNotFail: the release was cut on
// a branch this working copy does not have. Routine when promoting from a
// machine that only tracks main. The digests, the image set and the direction
// are all still exactly right, so the plan stands and only the range is out.
func TestPromotePlan_CommitAbsentFromCheckoutDoesNotFail(t *testing.T) {
	releases := []release.Release{
		rel("v1.5.15", "2026-03-01T00:00:00Z", "deadbeefdead", false, map[string]string{"reliant": sha("new")}),
		rel("v1.3.0", "2026-01-01T00:00:00Z", "aaaaaaaaaaaa", false, map[string]string{"reliant": sha("old")}),
	}
	store := newMemBindingStore(map[string]release.Promotion{
		"staging": {Release: "v1.3.0", Resolved: map[string]string{"reliant": sha("old")}},
	})
	// Only the OLD commit is in the checkout; the target's is not.
	git := allCommitsPresent("aaaaaaaaaaaa")

	plan, err := computePromotePlan(context.Background(), promotePlanOptions{
		Env: "staging", Version: "v1.5.15", ProjectDir: t.TempDir(),
		Bindings: store, Releases: newMemReleaseLedger(releases...), Git: git,
	})
	if err != nil {
		t.Fatalf("a commit absent from the checkout must NOT fail the plan: %v", err)
	}

	if plan.Commits.State != promoteRangeCommitNotFound {
		t.Fatalf("range state = %s, want commit_not_found", plan.Commits.State)
	}
	if !strings.Contains(plan.Commits.Detail, "deadbeefdead") {
		t.Errorf("the detail must name the missing commit, got %q", plan.Commits.Detail)
	}
	if len(git.rangesAsked) != 0 {
		t.Errorf("no range should be requested for a commit that is not present, asked %v", git.rangesAsked)
	}
	// Everything else is intact — the whole reason this is not an error.
	if plan.Direction != promoteDirectionAhead {
		t.Errorf("direction = %s, want ahead (the ordering does not need git)", plan.Direction)
	}
	if got := changeFor(t, plan, "reliant").Change; got != promoteImageChanged {
		t.Errorf("the digest diff must still be computed, got %s", got)
	}
	if !plan.OK {
		t.Error("an unavailable commit range is not a failure — ok must stay true")
	}
}

// TestPromotePlan_NoCommitRecorded: a ledger cut from a non-git tree, or by a
// forge too old to record provenance. Distinguished from "not in the checkout"
// because the fixes differ — one is a fetch, the other is unfixable after the
// fact.
func TestPromotePlan_NoCommitRecorded(t *testing.T) {
	releases := []release.Release{
		rel("v1.5.15", "2026-03-01T00:00:00Z", "", false, map[string]string{"reliant": sha("new")}),
		rel("v1.3.0", "2026-01-01T00:00:00Z", "aaaaaaaaaaaa", false, map[string]string{"reliant": sha("old")}),
	}
	store := newMemBindingStore(map[string]release.Promotion{
		"staging": {Release: "v1.3.0", Resolved: map[string]string{"reliant": sha("old")}},
	})

	plan, err := computePromotePlan(context.Background(), promotePlanOptions{
		Env: "staging", Version: "v1.5.15", ProjectDir: t.TempDir(),
		Bindings: store, Releases: newMemReleaseLedger(releases...), Git: allCommitsPresent("aaaaaaaaaaaa"),
	})
	if err != nil {
		t.Fatalf("a ledger with no commit must not fail the plan: %v", err)
	}
	if plan.Commits.State != promoteRangeNoCommit {
		t.Errorf("range state = %s, want no_commit", plan.Commits.State)
	}
}

// TestPromotePlan_GitUnavailableDoesNotFail: git itself could not be read.
func TestPromotePlan_GitUnavailableDoesNotFail(t *testing.T) {
	releases := []release.Release{
		rel("v1.5.15", "2026-03-01T00:00:00Z", "cccccccccccc", false, map[string]string{"reliant": sha("new")}),
		rel("v1.3.0", "2026-01-01T00:00:00Z", "aaaaaaaaaaaa", false, map[string]string{"reliant": sha("old")}),
	}
	store := newMemBindingStore(map[string]release.Promotion{
		"staging": {Release: "v1.3.0", Resolved: map[string]string{"reliant": sha("old")}},
	})
	git := allCommitsPresent("aaaaaaaaaaaa", "cccccccccccc")
	git.err = os.ErrPermission

	plan, err := computePromotePlan(context.Background(), promotePlanOptions{
		Env: "staging", Version: "v1.5.15", ProjectDir: t.TempDir(),
		Bindings: store, Releases: newMemReleaseLedger(releases...), Git: git,
	})
	if err != nil {
		t.Fatalf("an unreadable git must not fail the plan: %v", err)
	}
	if plan.Commits.State != promoteRangeGitUnavailable {
		t.Errorf("range state = %s, want git_unavailable", plan.Commits.State)
	}
}

// TestPromotePlan_LongRangeIsTruncatedButCountedExactly. control-plane's
// staging sits 20 releases behind prod, which is hundreds of commits; a JSON
// document that grew without bound is one a UI cannot render. The COUNT stays
// exact so the truncation never understates the change.
func TestPromotePlan_LongRangeIsTruncatedButCountedExactly(t *testing.T) {
	releases := []release.Release{
		rel("v1.5.15", "2026-03-01T00:00:00Z", "cccccccccccc", false, map[string]string{"reliant": sha("new")}),
		rel("v1.3.0", "2026-01-01T00:00:00Z", "aaaaaaaaaaaa", false, map[string]string{"reliant": sha("old")}),
	}
	store := newMemBindingStore(map[string]release.Promotion{
		"staging": {Release: "v1.3.0", Resolved: map[string]string{"reliant": sha("old")}},
	})
	git := allCommitsPresent("aaaaaaaaaaaa", "cccccccccccc")
	total := promoteCommitRangeLimit + 37
	lines := make([]string, 0, total)
	for i := 0; i < total; i++ {
		lines = append(lines, "abc123 commit")
	}
	git.commits["aaaaaaaaaaaa..cccccccccccc"] = lines

	plan, err := computePromotePlan(context.Background(), promotePlanOptions{
		Env: "staging", Version: "v1.5.15", ProjectDir: t.TempDir(),
		Bindings: store, Releases: newMemReleaseLedger(releases...), Git: git,
	})
	if err != nil {
		t.Fatalf("compute plan: %v", err)
	}
	if plan.Commits.Count != total {
		t.Errorf("count = %d, want the EXACT total %d even when truncated", plan.Commits.Count, total)
	}
	if len(plan.Commits.Commits) != promoteCommitRangeLimit {
		t.Errorf("carried %d commits, want the cap %d", len(plan.Commits.Commits), promoteCommitRangeLimit)
	}
	if !plan.Commits.Truncated {
		t.Error("truncated must be true when the list is shorter than the count")
	}
}

// ─── --plan writes nothing ───────────────────────────────────────────────────

// TestRunPromotePlan_WritesNothing is the load-bearing safety test, and it
// asserts the strongest available form: the ledger file is BYTE-IDENTICAL
// before and after.
//
// Weaker checks would pass while the guarantee was broken. "Still bound to the
// old release" survives a rewrite that reformatted the file or re-stamped
// promoted_at; "the file exists" survives almost anything. A dry run must
// leave the bytes alone, because a UI's whole promise is that previewing is
// safe to do against real state — control-plane's ledger is a committed file
// in a live repository.
func TestRunPromotePlan_WritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// A REAL file store, not the in-memory fake: the thing being protected
	// is a file on disk, so the test has to exercise the file backend.
	if err := WriteRelease(dir, rel("v1.3.0", "2026-01-01T00:00:00Z", "aaaaaaaaaaaa", false,
		map[string]string{"reliant": sha("old")})); err != nil {
		t.Fatalf("write release: %v", err)
	}
	if err := WriteRelease(dir, rel("v1.5.15", "2026-03-01T00:00:00Z", "cccccccccccc", false,
		map[string]string{"reliant": sha("new"), "internal-console": sha("added")})); err != nil {
		t.Fatalf("write release: %v", err)
	}
	// Seed a real binding so there is something a write would append to.
	if _, err := newFileBindingStore(dir).Append(context.Background(), release.Promotion{
		Env: "staging", Release: "v1.3.0", Kind: release.KindPromote,
		Resolved: map[string]string{"reliant": sha("old")},
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	ledger := promotionLogPath(dir, "staging")
	before, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	// The dry run. Note ProjectDir/Bindings are left nil so this goes
	// through exactly the production path — the file store the command
	// constructs for itself.
	out := captureStdout(t, func() {
		if err := runPromote(context.Background(), "v1.5.15", "staging", promoteOptions{
			DryRun: true,
			Git:    allCommitsPresent("aaaaaaaaaaaa", "cccccccccccc"),
		}); err != nil {
			t.Fatalf("--plan must exit 0: %v", err)
		}
	})

	after, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("read ledger after: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("--plan WROTE to the binding ledger.\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// And it must actually have produced the change set, or "wrote
	// nothing" would be satisfied by doing nothing at all.
	if !strings.Contains(out, "internal-console") || !strings.Contains(strings.ToUpper(out), "ADDED") {
		t.Errorf("--plan must report the change set it declined to write, got:\n%s", out)
	}
	if !strings.Contains(strings.ToUpper(out), "NOTHING WRITTEN") {
		t.Errorf("--plan must say plainly that nothing was written, got:\n%s", out)
	}
}

// TestRunPromote_AppliesAndSaysSo is the counterpart: without --plan the
// binding really moves. Without this, a runPromote that never wrote anything
// would pass the test above.
func TestRunPromote_AppliesAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := WriteRelease(dir, rel("v1.4.0", "2026-02-01T00:00:00Z", "bbbbbbbbbbbb", false,
		map[string]string{"reliant": sha("a")})); err != nil {
		t.Fatalf("write release: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runPromote(context.Background(), "v1.4.0", "staging", promoteOptions{
			Git: allCommitsPresent("bbbbbbbbbbbb"),
		}); err != nil {
			t.Fatalf("promote: %v", err)
		}
	})

	binding, bound, err := newFileBindingStore(dir).Current(context.Background(), "staging")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bound || binding.Release != "v1.4.0" || binding.Resolved["reliant"] != sha("a") {
		t.Fatalf("a real promote must write the binding, got bound=%v %+v", bound, binding)
	}
	if binding.PromotedAt.IsZero() {
		t.Error("promoted_at must be stamped at the time of the WRITE")
	}
	if !strings.Contains(out, "Promoted env") {
		t.Errorf("a real promote must report that it promoted, got:\n%s", out)
	}
}

// ─── --plan and the real promote agree ───────────────────────────────────────

// TestPromotePlan_PlanAndApplyAgree pins the design constraint this whole file
// exists for: the preview and the write are the SAME change set.
//
// It is asserted structurally rather than by comparing two rendered outputs.
// The dry run's plan and the applied promote's plan are compared field by
// field — images, tally, direction, commit range — with only the fields that
// MUST differ (applied, dry_run, generated_at) excluded. A future refactor
// that gave the real promote its own faster path would have to make that path
// produce an identical change set, or this fails.
func TestPromotePlan_PlanAndApplyAgree(t *testing.T) {
	releases := []release.Release{
		rel("v1.5.15", "2026-03-01T00:00:00Z", "cccccccccccc", false, map[string]string{
			"reliant":          sha("new"),
			"internal-console": sha("added"),
			"control-plane":    sha("same"),
		}),
		rel("v1.3.0", "2026-01-01T00:00:00Z", "aaaaaaaaaaaa", false, map[string]string{
			"reliant":       sha("old"),
			"control-plane": sha("same"),
		}),
	}
	seed := func() *memBindingStore {
		return newMemBindingStore(map[string]release.Promotion{
			"staging": {
				Release:    "v1.3.0",
				PromotedAt: parseFixtureTime("2026-01-02T00:00:00Z"),
				Resolved:   map[string]string{"reliant": sha("old"), "control-plane": sha("same"), "daemon-gateway": sha("gone")},
			},
		})
	}
	newGit := func() *fakeGit {
		g := allCommitsPresent("aaaaaaaaaaaa", "cccccccccccc")
		g.commits["aaaaaaaaaaaa..cccccccccccc"] = []string{"ccc1 work"}
		return g
	}

	// Both modes go through runPromote, so this compares what the COMMAND
	// does, not just what the helper returns.
	planStore, applyStore := seed(), seed()
	planJSON := captureStdout(t, func() {
		if err := runPromote(context.Background(), "v1.5.15", "staging", promoteOptions{
			DryRun: true, JSON: true, ProjectDir: t.TempDir(),
			Bindings: planStore, Releases: newMemReleaseLedger(releases...), Git: newGit(),
		}); err != nil {
			t.Fatalf("plan: %v", err)
		}
	})
	applyJSON := captureStdout(t, func() {
		if err := runPromote(context.Background(), "v1.5.15", "staging", promoteOptions{
			JSON: true, ProjectDir: t.TempDir(),
			Bindings: applyStore, Releases: newMemReleaseLedger(releases...), Git: newGit(),
		}); err != nil {
			t.Fatalf("apply: %v", err)
		}
	})

	var planned, applied promotePlan
	if err := json.Unmarshal([]byte(planJSON), &planned); err != nil {
		t.Fatalf("decode plan JSON: %v\n%s", err, planJSON)
	}
	if err := json.Unmarshal([]byte(applyJSON), &applied); err != nil {
		t.Fatalf("decode applied JSON: %v\n%s", err, applyJSON)
	}

	// The fields that MUST differ, and nothing else may.
	if planned.Applied || !applied.Applied {
		t.Errorf("applied must be false for --plan and true for a real promote, got %v / %v", planned.Applied, applied.Applied)
	}
	if !planned.DryRun || applied.DryRun {
		t.Errorf("dry_run must be true for --plan and false otherwise, got %v / %v", planned.DryRun, applied.DryRun)
	}

	// Normalise the three legitimately-varying fields, then require exact
	// equality of the rest via the encoded document — which catches a field
	// added to the contract that only one path populates.
	planned.Applied, applied.Applied = false, false
	planned.DryRun, applied.DryRun = false, false
	planned.GeneratedAt, applied.GeneratedAt = "", ""
	// Recorded is the entry the write produced, so only the apply has one.
	if planned.Recorded != nil || applied.Recorded == nil {
		t.Errorf("recorded must be absent under --plan and present after apply, got %v / %v", planned.Recorded, applied.Recorded)
	}
	planned.Recorded, applied.Recorded = nil, nil
	a, err := json.Marshal(planned)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	b, err := json.Marshal(applied)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("--plan and the real promote disagree about the change set.\nplan:  %s\napply: %s", a, b)
	}

	// Sanity: the change set was non-trivial, so equality means something.
	want := promoteImageTally{Unchanged: 1, Changed: 1, Added: 1, Removed: 1}
	if planned.Tally != want {
		t.Errorf("tally = %+v, want %+v — the agreement test must cover a real diff", planned.Tally, want)
	}
	// And the write really happened on the apply side only.
	if cur, _, _ := planStore.Current(context.Background(), "staging"); cur.Release != "v1.3.0" {
		t.Error("--plan must leave the store's binding untouched")
	}
	if cur, _, _ := applyStore.Current(context.Background(), "staging"); cur.Release != "v1.5.15" {
		t.Error("a real promote must move the store's binding")
	}
}

// ─── The JSON contract ───────────────────────────────────────────────────────

// TestPromotePlanJSON_ShapeAndFollowThrough pins the parts of the document a
// UI depends on, including the follow-through fact: promote ships nothing.
func TestPromotePlanJSON_ShapeAndFollowThrough(t *testing.T) {
	releases := []release.Release{
		rel("v1.5.15", "2026-03-01T00:00:00Z", "cccccccccccc", false, map[string]string{"reliant": sha("new")}),
		rel("v1.3.0", "2026-01-01T00:00:00Z", "aaaaaaaaaaaa", false, map[string]string{"reliant": sha("old")}),
	}
	store := newMemBindingStore(map[string]release.Promotion{
		"staging": {Release: "v1.3.0", PromotedAt: parseFixtureTime("2026-01-02T00:00:00Z"), Resolved: map[string]string{"reliant": sha("old")}},
	})
	git := allCommitsPresent("aaaaaaaaaaaa", "cccccccccccc")
	git.commits["aaaaaaaaaaaa..cccccccccccc"] = []string{"ccc1 work"}

	out := captureStdout(t, func() {
		if err := runPromote(context.Background(), "v1.5.15", "staging", promoteOptions{
			DryRun: true, JSON: true, ProjectDir: t.TempDir(),
			Bindings: store, Releases: newMemReleaseLedger(releases...), Git: git,
		}); err != nil {
			t.Fatalf("plan --json: %v", err)
		}
	})

	// Decoded generically first: this asserts the WIRE field names and the
	// lowercase enum strings a consumer actually reads, which a typed
	// decode into our own struct would hide.
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	for _, key := range []string{
		"env", "ledger", "generated_at", "dry_run", "applied", "current", "target",
		"direction", "releases_between", "images", "tally", "commits", "changed",
		"ships_nothing", "next_step", "ok",
	} {
		if _, ok := doc[key]; !ok {
			t.Errorf("missing contract field %q in:\n%s", key, out)
		}
	}
	if doc["direction"] != "ahead" {
		t.Errorf("direction must marshal as a lowercase string, got %v", doc["direction"])
	}
	images, _ := doc["images"].([]any)
	if len(images) != 1 {
		t.Fatalf("images = %v", doc["images"])
	}
	first, _ := images[0].(map[string]any)
	if first["change"] != "changed" {
		t.Errorf("change must marshal as a lowercase string, got %v", first["change"])
	}
	commits, _ := doc["commits"].(map[string]any)
	if commits["state"] != "computed" {
		t.Errorf("commits.state must marshal as a lowercase string, got %v", commits["state"])
	}

	// THE FOLLOW-THROUGH FACT. Promote moves a pointer; nothing reaches a
	// cluster until deploy runs. It is in the contract so a UI does not
	// have to know it.
	if doc["ships_nothing"] != true {
		t.Error("ships_nothing must be true — promote writes a pointer and ships nothing")
	}
	if doc["next_step"] != "forge env deploy staging" {
		t.Errorf("next_step must name the deploy that actually ships, got %v", doc["next_step"])
	}
	if doc["applied"] != false || doc["dry_run"] != true {
		t.Errorf("a --plan document must report applied=false dry_run=true, got applied=%v dry_run=%v", doc["applied"], doc["dry_run"])
	}
}

// TestPromoteEnums_RejectUnknownStrings. A lenient decoder that defaulted an
// unrecognised value would turn a state this binary does not understand into
// one it does — and for the image classification, the default would be
// "unchanged", which under-reports a real change. Every enum in this document
// must refuse instead.
func TestPromoteEnums_RejectUnknownStrings(t *testing.T) {
	var change promoteImageChangeKind
	if err := change.UnmarshalJSON([]byte(`"rebuilt"`)); err == nil {
		t.Error("an unknown image change must be rejected, not defaulted to unchanged")
	}
	var dir promoteDirection
	if err := dir.UnmarshalJSON([]byte(`"sideways"`)); err == nil {
		t.Error("an unknown direction must be rejected, not defaulted")
	}
	var rangeState promoteRangeState
	if err := rangeState.UnmarshalJSON([]byte(`"estimated"`)); err == nil {
		t.Error("an unknown range state must be rejected, not defaulted")
	}

	// And the round trip works for every value forge actually emits, so
	// strictness has not made the contract undecodable.
	for _, k := range []promoteImageChangeKind{
		promoteImageUnknown, promoteImageUnchanged, promoteImageChanged, promoteImageAdded, promoteImageRemoved,
	} {
		encoded, err := json.Marshal(k)
		if err != nil {
			t.Fatalf("marshal %v: %v", k, err)
		}
		var back promoteImageChangeKind
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Errorf("round trip %s: %v", encoded, err)
		} else if back != k {
			t.Errorf("round trip %s decoded to %s", encoded, back)
		}
	}
	for _, s := range []promoteRangeState{
		promoteRangeUnknown, promoteRangeComputed, promoteRangeFirstPromote, promoteRangeSameRelease,
		promoteRangeLedgerMissing, promoteRangeNoCommit, promoteRangeCommitNotFound,
		promoteRangeDirtyRelease, promoteRangeGitUnavailable,
	} {
		encoded, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %v: %v", s, err)
		}
		var back promoteRangeState
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Errorf("round trip %s: %v", encoded, err)
		} else if back != s {
			t.Errorf("round trip %s decoded to %s", encoded, back)
		}
	}
	for _, d := range []promoteDirection{
		promoteDirectionUnknown, promoteDirectionInitial, promoteDirectionAhead,
		promoteDirectionBehind, promoteDirectionSame,
	} {
		encoded, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal %v: %v", d, err)
		}
		var back promoteDirection
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Errorf("round trip %s: %v", encoded, err)
		} else if back != d {
			t.Errorf("round trip %s decoded to %s", encoded, back)
		}
	}

	// The zero values are the CONSERVATIVE ones. An unpopulated struct must
	// not read as "nothing changed" or "same release".
	if (promoteImageChangeKind(0)) != promoteImageUnknown {
		t.Error("the zero image change must be unknown, never unchanged")
	}
	if (promoteDirection(0)) != promoteDirectionUnknown {
		t.Error("the zero direction must be unknown, never same")
	}
	if (promoteRangeState(0)) != promoteRangeUnknown {
		t.Error("the zero range state must be unknown, never computed")
	}
}
