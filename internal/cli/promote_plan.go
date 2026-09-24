package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// The promote CHANGE SET — computed once, rendered twice, applied optionally.
//
// WHY THIS FILE EXISTS. `forge env promote` is the one write verb in the
// release model with genuinely reviewable semantics: forge builds once and
// then binds digests, so promote moves a POINTER and rebuilds nothing. That
// makes it cheap and, in principle, fully previewable — you can know the
// entire effect before anything is written. Until this file you could not:
// promote read the ledger and called SetBinding immediately, and the only way
// to find out what it would do was to let it do it.
//
// ONE FUNCTION, BOTH PATHS, AND THAT IS THE LOAD-BEARING CONSTRAINT.
// computePromotePlan produces the whole change set; `--plan` renders it and
// stops, and a real promote renders the SAME value and applies it. Two code
// paths — a preview that describes the write and a write that performs it —
// would be free to disagree, and a preview that disagrees with the write is
// worse than no preview at all: it converts "I did not look" into "I looked
// and was told the wrong thing". TestPromotePlan_PlanAndApplyAgree pins this
// so a later refactor cannot quietly split them.
//
// WHAT A REVIEWER NEEDS, AND WHY EACH PIECE IS MODELLED EXPLICITLY.
//
//   - The image SET can change, not just the digests. control-plane's prod
//     line carries `internal-console` and its staging/preprod line does not,
//     so promoting between those two release lines ADDS or REMOVES an image.
//     A digest map diff leaves that for the caller to notice; an explicit
//     added/removed classification cannot be missed.
//   - DIRECTION is the single most consequential thing a reader can misread.
//     A backwards promote is a legitimate operation (it is how a rollback is
//     spelled), but "staging moves forward 20 releases" and "staging moves
//     back 20 releases" look identical in a digest diff.
//   - The COMMIT RANGE is what makes the digests mean something. Every way it
//     can be unavailable is a first-class state rather than an error, because
//     a plan that refuses to render because a release was cut on another
//     branch is a plan nobody can use on the day they need it.
//   - Promote SHIPS NOTHING. `forge env verify` exists because that gap was
//     invisible; the plan states it rather than assuming the reader knows.

// promoteCommitRangeLimit caps how many commit subjects the range carries.
// A 20-release gap can be hundreds of commits, and a JSON document that grows
// without bound is one a UI cannot render — the count is always exact, and
// Truncated says the list is not.
const promoteCommitRangeLimit = 50

// promoteGitTimeout bounds each read-only git invocation. Generous for a cold
// object store on a large repository, short enough that a wedged git cannot
// hang a plan whose whole promise is that it is cheap.
const promoteGitTimeout = 20 * time.Second

// ─── Enums. Lowercase strings out, strict decode in ──────────────────────────

// promoteImageChangeKind classifies what a promote does to ONE image.
//
// THE ZERO VALUE IS "unknown", DELIBERATELY. The tempting zero value is
// "unchanged", and it is exactly wrong: an unpopulated struct, or a value
// decoded by a binary that does not recognise a newer classification, would
// then read as "nothing happens to this image" — the one answer that makes a
// real change invisible. Unknown is the conservative default, and
// UnmarshalJSON refuses a string it does not recognise for the same reason.
type promoteImageChangeKind int

const (
	// promoteImageUnknown: not classified. Never produced by
	// computePromotePlan; it exists so an uninitialised or
	// forward-incompatible value cannot masquerade as "unchanged".
	promoteImageUnknown promoteImageChangeKind = iota
	// promoteImageUnchanged: present in both bindings, same digest. The
	// env already runs these bytes for this image.
	promoteImageUnchanged
	// promoteImageChanged: present in both, DIFFERENT digest. Both digests
	// are carried, because the reviewer's question is "from what, to what".
	promoteImageChanged
	// promoteImageAdded: in the target release, absent from the current
	// binding. The env gains an image it was not running.
	promoteImageAdded
	// promoteImageRemoved: in the current binding, absent from the target
	// release. The env's binding will stop declaring this image — nothing
	// is deleted from any cluster by promote, but the next deploy will no
	// longer pin it.
	promoteImageRemoved
)

// String renders the fixed-width label used in the text report.
func (k promoteImageChangeKind) String() string {
	switch k {
	case promoteImageUnchanged:
		return "UNCHANGED"
	case promoteImageChanged:
		return "CHANGED"
	case promoteImageAdded:
		return "ADDED"
	case promoteImageRemoved:
		return "REMOVED"
	default:
		return "UNKNOWN"
	}
}

// MarshalJSON emits the lowercase form, derived from String() so the text
// column and the JSON value cannot disagree about a cell.
func (k promoteImageChangeKind) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strings.ToLower(k.String()) + `"`), nil
}

// UnmarshalJSON reads the string form back, REJECTING anything it does not
// know. A lenient decoder that fell back to "unchanged" would under-report a
// real change, which is the single failure this whole file exists to prevent.
func (k *promoteImageChangeKind) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return fmt.Errorf("promote image change must be a string: %w", err)
	}
	switch strings.ToLower(name) {
	case "unknown":
		*k = promoteImageUnknown
	case "unchanged":
		*k = promoteImageUnchanged
	case "changed":
		*k = promoteImageChanged
	case "added":
		*k = promoteImageAdded
	case "removed":
		*k = promoteImageRemoved
	default:
		return fmt.Errorf("unknown promote image change %q (expected unchanged, changed, added or removed) — "+
			"refusing to decode it as a default, which would report a real change as unchanged", name)
	}
	return nil
}

// promoteDirection is where the target release sits relative to what the env
// runs now, in the project's release ordering.
//
// Zero value is "unknown": a direction nobody computed must not read as
// "same", which would tell a reviewer a rollback was a no-op.
type promoteDirection int

const (
	// promoteDirectionUnknown: the ordering could not place both releases.
	promoteDirectionUnknown promoteDirection = iota
	// promoteDirectionInitial: the env has never been promoted, so there
	// is no "from" to have a direction relative to.
	promoteDirectionInitial
	// promoteDirectionAhead: the target was cut AFTER the release the env
	// currently runs — moving forward.
	promoteDirectionAhead
	// promoteDirectionBehind: the target was cut BEFORE the current
	// release. A ROLLBACK. Legitimate, and the thing a reviewer most needs
	// told to them explicitly.
	promoteDirectionBehind
	// promoteDirectionSame: the env is already bound to this release.
	promoteDirectionSame
)

func (d promoteDirection) String() string {
	switch d {
	case promoteDirectionInitial:
		return "INITIAL"
	case promoteDirectionAhead:
		return "AHEAD"
	case promoteDirectionBehind:
		return "BEHIND"
	case promoteDirectionSame:
		return "SAME"
	default:
		return "UNKNOWN"
	}
}

func (d promoteDirection) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strings.ToLower(d.String()) + `"`), nil
}

func (d *promoteDirection) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return fmt.Errorf("promote direction must be a string: %w", err)
	}
	switch strings.ToLower(name) {
	case "unknown":
		*d = promoteDirectionUnknown
	case "initial":
		*d = promoteDirectionInitial
	case "ahead":
		*d = promoteDirectionAhead
	case "behind":
		*d = promoteDirectionBehind
	case "same":
		*d = promoteDirectionSame
	default:
		return fmt.Errorf("unknown promote direction %q (expected initial, ahead, behind or same) — "+
			"refusing to decode it as a default, which would render a rollback as a forward promote", name)
	}
	return nil
}

// promoteRangeState says whether the commit range was computed, and when it
// was not, WHY — as a value, not an error.
//
// Every one of these is a state a real project reaches, and none of them is a
// reason to refuse to print a plan. Zero value is "unknown" so an uncomputed
// range never reads as an empty-but-computed one: "0 commits" and "we could
// not tell" are completely different claims to put in front of a reviewer.
type promoteRangeState int

const (
	// promoteRangeUnknown: nothing was attempted or the attempt's outcome
	// was not recorded. Never produced deliberately.
	promoteRangeUnknown promoteRangeState = iota
	// promoteRangeComputed: the range was read from this checkout's git
	// history. Count is exact.
	promoteRangeComputed
	// promoteRangeFirstPromote: the env has no current binding, so there
	// is no "from" commit and therefore no range.
	promoteRangeFirstPromote
	// promoteRangeSameRelease: current and target are the same release —
	// the range is empty by definition, not by measurement.
	promoteRangeSameRelease
	// promoteRangeLedgerMissing: the release ledger for the CURRENT
	// binding is not in this checkout (it was cut on another branch), so
	// its commit is unknown. Not an error — the binding is still real and
	// still says what the env runs.
	promoteRangeLedgerMissing
	// promoteRangeNoCommit: a ledger exists but records no git commit
	// (cut from a non-git tree, or by an older forge).
	promoteRangeNoCommit
	// promoteRangeCommitNotFound: a commit is named but is not present in
	// THIS checkout — the release was cut on a branch this working copy
	// does not have. Reported plainly; the plan stands.
	promoteRangeCommitNotFound
	// promoteRangeDirtyRelease: an endpoint release was cut from a DIRTY
	// tree. Those bytes correspond to no reviewable commit, so a range
	// computed against the recorded commit would describe something other
	// than what shipped. Labelled meaningless rather than silently
	// computed — a plausible-looking wrong range is worse than none.
	promoteRangeDirtyRelease
	// promoteRangeGitUnavailable: git could not be read (not a repository,
	// no git binary, a failed invocation).
	promoteRangeGitUnavailable
)

func (s promoteRangeState) String() string {
	switch s {
	case promoteRangeComputed:
		return "COMPUTED"
	case promoteRangeFirstPromote:
		return "FIRST-PROMOTE"
	case promoteRangeSameRelease:
		return "SAME-RELEASE"
	case promoteRangeLedgerMissing:
		return "LEDGER-MISSING"
	case promoteRangeNoCommit:
		return "NO-COMMIT"
	case promoteRangeCommitNotFound:
		return "COMMIT-NOT-FOUND"
	case promoteRangeDirtyRelease:
		return "DIRTY-RELEASE"
	case promoteRangeGitUnavailable:
		return "GIT-UNAVAILABLE"
	default:
		return "UNKNOWN"
	}
}

func (s promoteRangeState) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strings.ReplaceAll(strings.ToLower(s.String()), "-", "_") + `"`), nil
}

func (s *promoteRangeState) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return fmt.Errorf("promote commit range state must be a string: %w", err)
	}
	switch strings.ToLower(name) {
	case "unknown":
		*s = promoteRangeUnknown
	case "computed":
		*s = promoteRangeComputed
	case "first_promote":
		*s = promoteRangeFirstPromote
	case "same_release":
		*s = promoteRangeSameRelease
	case "ledger_missing":
		*s = promoteRangeLedgerMissing
	case "no_commit":
		*s = promoteRangeNoCommit
	case "commit_not_found":
		*s = promoteRangeCommitNotFound
	case "dirty_release":
		*s = promoteRangeDirtyRelease
	case "git_unavailable":
		*s = promoteRangeGitUnavailable
	default:
		return fmt.Errorf("unknown promote commit range state %q — "+
			"refusing to decode it as a default, which would present an uncomputed range as a measured one", name)
	}
	return nil
}

// ─── The plan document ───────────────────────────────────────────────────────

// promoteImageChange is one image's row in the change set.
type promoteImageChange struct {
	// Image is the bare image name, as it keys both the binding's Resolved
	// map and the release's Artifacts map.
	Image string `json:"image"`
	// Change is the classification. Read this rather than diffing the two
	// digest fields: `added` and `removed` are the cases a digest compare
	// silently turns into "one side is empty".
	Change promoteImageChangeKind `json:"change"`
	// CurrentDigest is what the env's existing binding declares. Empty for
	// an added image.
	CurrentDigest string `json:"current_digest,omitempty"`
	// TargetDigest is what the target release declares. Empty for a
	// removed image.
	TargetDigest string `json:"target_digest,omitempty"`
}

// promoteImageTally counts the classifications, so a consumer can badge the
// change set without walking it.
type promoteImageTally struct {
	Unchanged int `json:"unchanged"`
	Changed   int `json:"changed"`
	Added     int `json:"added"`
	Removed   int `json:"removed"`
}

// promoteCommitRange is the source change between the current and target
// releases: what a reviewer is actually approving.
type promoteCommitRange struct {
	// State says whether Count and Commits mean anything. Check it FIRST —
	// a zero Count with a non-computed state is "unknown", not "nothing".
	State promoteRangeState `json:"state"`
	// Detail explains a non-computed state in one line, naming the release
	// and the commit involved.
	Detail string `json:"detail,omitempty"`
	// FromCommit is the CURRENT release's recorded commit, ToCommit the
	// TARGET's. They are the ledgers' values verbatim, regardless of
	// direction, so a consumer can always tell which release each belongs
	// to. For a rollback, history runs from ToCommit to FromCommit — see
	// Reverts.
	FromCommit string `json:"from_commit,omitempty"`
	ToCommit   string `json:"to_commit,omitempty"`
	// Reverts is true when the target is BEHIND the current release. The
	// listed commits are then the ones being taken AWAY, not added, and a
	// consumer rendering them as "incoming changes" has inverted the most
	// consequential fact on the screen.
	Reverts bool `json:"reverts,omitempty"`
	// Count is the exact number of commits in the range. Exact even when
	// Commits is truncated.
	Count int `json:"count"`
	// Commits is "<short-sha> <subject>", newest first, capped at
	// promoteCommitRangeLimit. Always non-nil so consumers see [].
	Commits []string `json:"commits"`
	// Truncated is true when Commits holds fewer entries than Count.
	Truncated bool `json:"truncated,omitempty"`
}

// promotePlanBinding describes the env's CURRENT state — what it is bound to
// before this promote.
type promotePlanBinding struct {
	// Bound is false for an env that has never been promoted. A separate
	// field from an empty Release so "never promoted" is distinguishable
	// from a binding carrying a blank version.
	Bound bool `json:"bound"`
	// Release is the version the env runs now. Empty when unbound.
	Release string `json:"release,omitempty"`
	// PromotedAt is RFC3339 for when this binding was WRITTEN — not when
	// it was deployed. Promote moves a pointer; deploy moves bytes.
	PromotedAt string `json:"promoted_at,omitempty"`
	// ReleaseKnown is true when the ledger for Release exists in this
	// checkout. FALSE IS NOT AN ERROR: a release cut on another branch
	// leaves a perfectly real binding pointing at a file this checkout has
	// never seen, so provenance is omitted rather than guessed.
	ReleaseKnown bool `json:"release_known"`
	// Git is the current release's provenance. Nil when the ledger is
	// absent.
	Git *release.Git `json:"git,omitempty"`
	// ReleaseCreatedAt is RFC3339 for when the current release was CUT.
	ReleaseCreatedAt string `json:"release_created_at,omitempty"`
	// Note explains a state that would otherwise look like missing data.
	Note string `json:"note,omitempty"`
}

// promotePlanTarget describes the release being promoted TO.
type promotePlanTarget struct {
	// Release is the version label.
	Release string `json:"release"`
	// CreatedAt is RFC3339 for when it was cut.
	CreatedAt string `json:"created_at,omitempty"`
	// Git is its provenance. Read `dirty`: a release cut from a tree with
	// uncommitted changes ships bytes matching no reviewable commit, and
	// promoting one means nobody can say what is in it.
	Git *release.Git `json:"git,omitempty"`
	// Images is how many images the release resolves digests for.
	Images int `json:"images"`
}

// promotePlan is the full change set, and the `--json` output contract for
// both `--plan` and a real promote.
//
// Extensions are ADDITIVE: new fields may be added; no field is renamed or
// repurposed, so a consumer reading `images[].change` keeps working.
type promotePlan struct {
	// Env is the environment being promoted.
	Env string `json:"env"`
	// Kind is what this entry records: "promote", or "rollback" when
	// --rollback was passed. A rollback must name a release the env has
	// already run; the ledger refuses one that does not.
	Kind release.PromotionKind `json:"kind"`
	// Ledger names where the binding is recorded, as the binding store
	// reports it — a path today, a URL for a hosted backend. Opaque, for
	// DISPLAY only; do not join it or open it.
	Ledger string `json:"ledger"`
	// GeneratedAt is RFC3339 for when the plan was computed.
	GeneratedAt string `json:"generated_at"`
	// DryRun is true when --plan was passed.
	DryRun bool `json:"dry_run"`
	// Applied says whether the binding was actually WRITTEN. This is the
	// field that keeps `--plan` and a real promote one document: the shape
	// is identical and this is how a consumer tells them apart. Under
	// --plan it is always false, and nothing was written.
	Applied bool `json:"applied"`
	// Current is what the env runs now.
	Current promotePlanBinding `json:"current"`
	// Target is the release being promoted to.
	Target promotePlanTarget `json:"target"`
	// Direction is where the target sits relative to the current release.
	// `behind` is a ROLLBACK — legitimate, and the single most
	// consequential thing on this screen to misread.
	Direction promoteDirection `json:"direction"`
	// DirectionDetail says the same thing in one human sentence.
	DirectionDetail string `json:"direction_detail,omitempty"`
	// ReleasesBetween is how many releases separate the two in the
	// project's ordering, regardless of direction. Zero when the direction
	// is not known or the releases are the same.
	ReleasesBetween int `json:"releases_between"`
	// Images is one row per image in the union of the current binding and
	// the target release, sorted by name. Always non-nil.
	Images []promoteImageChange `json:"images"`
	// Tally counts the classifications.
	Tally promoteImageTally `json:"tally"`
	// Commits is the source change between the two releases.
	Commits promoteCommitRange `json:"commits"`
	// Changed is false only when this promote would alter nothing at all —
	// same release, same digest set. A caller can use it to skip a no-op.
	Changed bool `json:"changed"`
	// ShipsNothing is always TRUE, and it is in the contract on purpose.
	// Promote writes a pointer; not one byte reaches any cluster until
	// `forge env deploy` runs. `forge env verify` exists precisely because
	// that gap used to be invisible, so the plan states it rather than
	// relying on the reader to know it.
	ShipsNothing bool `json:"ships_nothing"`
	// NextStep is the command that actually ships these digests.
	NextStep string `json:"next_step"`
	// Note is the human phrasing of ShipsNothing.
	Note string `json:"note,omitempty"`
	// OK is false exactly when text mode exits non-zero. Computing or
	// applying a plan either succeeds (true) or returns an error, so a
	// rendered plan is always true — a rollback is not a failure.
	OK bool `json:"ok"`
	// Recorded is the ledger entry the env now resolves to, after an
	// apply: the new entry, or — for a retry of the env's current state —
	// the existing one, unchanged. Nil under --plan.
	Recorded *release.Promotion `json:"recorded,omitempty"`

	// targetSources is the source-built frontend snapshot the binding will be
	// written with, the non-container half of targetResolved. Carried on the
	// plan for the same reason the digests are: the value APPLIED must be the
	// value PREVIEWED, and resolving it a second time at write would let the
	// two drift. Without it a promotion records only images and a frontend
	// silently stays on whatever ref is in KCL — see release.Promotion.Sources.
	targetSources map[string]release.Source

	// targetResolved is the digest map the binding will be written with.
	// Unexported so it cannot leak into the JSON contract as a second,
	// redundant encoding of Images — and so applyPromotePlan writes
	// exactly the digests the plan reported, which is what makes the
	// preview and the write the same thing.
	targetResolved map[string]string
}

// ─── The git seam ────────────────────────────────────────────────────────────

// promoteGitReader reads commit facts out of a checkout. READ-ONLY: the two
// methods run `git cat-file -e` and `git log`, neither of which mutates a
// repository. That matters beyond hygiene — this code runs in shared
// checkouts, and a plan that touched the index would be a plan nobody could
// run.
//
// A seam rather than a direct call because every interesting case is a
// property of a repository's history: a commit absent from this checkout, a
// git that cannot run at all. Staging real repositories to imply those states
// makes the tests slow and the premises implicit; a fake states them.
// Declared at the consumer, per the package-boundary rule the rest of this
// package follows for clusterImageLister and bindingStore.
type promoteGitReader interface {
	// HasCommit reports whether commit resolves to a commit object in dir.
	// False means "not in this checkout", which is a reportable state, not
	// an error.
	HasCommit(ctx context.Context, dir, commit string) bool
	// CommitsBetween lists commits reachable from `to` but not `from`,
	// newest first, as "<short-sha> <subject>".
	CommitsBetween(ctx context.Context, dir, from, to string) ([]string, error)
}

// gitCommitReader is the production reader: plain read-only git invocations,
// scoped to the project directory rather than the process CWD so the answer
// does not depend on where forge was launched from.
type gitCommitReader struct{}

func (gitCommitReader) HasCommit(ctx context.Context, dir, commit string) bool {
	if commit == "" {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, promoteGitTimeout)
	defer cancel()
	// `^{commit}` makes this reject a blob or tree that happens to share
	// the prefix: the question is whether this is a commit we can log from.
	cmd := exec.CommandContext(cctx, "git", "cat-file", "-e", commit+"^{commit}")
	cmd.Dir = dir
	return cmd.Run() == nil
}

func (gitCommitReader) CommitsBetween(ctx context.Context, dir, from, to string) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, promoteGitTimeout)
	defer cancel()
	// Merges are INCLUDED. A release range that hid merge commits would
	// under-count exactly the history that arrives by pull request, which
	// is most of it.
	cmd := exec.CommandContext(cctx, "git", "log", "--pretty=format:%h %s", from+".."+to)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var commits []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			commits = append(commits, line)
		}
	}
	return commits, nil
}

// ─── Computation ─────────────────────────────────────────────────────────────

// promotePlanOptions carries the inputs and the injected seams.
type promotePlanOptions struct {
	// Env is the environment to bind; Version the release to bind it to.
	Env     string
	Version string
	// ProjectDir is the checkout the ledgers and the git history are read
	// from. Empty falls back to projectDirForKCL().
	ProjectDir string
	// Kind is promote (the default) or rollback.
	Kind release.PromotionKind
	// Bindings is the promotion ledger. Nil falls back to the env's
	// store. Injected so a test can STATE the env's current binding
	// instead of staging a file to imply it.
	Bindings bindingStore
	// Releases is the release ledger the target is read from. Nil falls
	// back to the env's.
	Releases releaseLedger
	// Git reads the commit range. Nil uses real git.
	Git promoteGitReader
}

// computePromotePlan builds the whole change set WITHOUT writing anything.
//
// It returns an error only for conditions that make a promote impossible at
// all: an unreadable binding ledger, a target release that does not exist, a
// release carrying no resolvable digests. Everything else — a missing current
// ledger, a commit this checkout does not have, a dirty release, a rollback —
// is a VALUE in the returned plan. That split is deliberate: the states a
// reviewer most needs described are exactly the ones an error would refuse to
// describe.
//
// Both --plan and a real promote call this. The real promote then calls
// applyPromotePlan with the result, so the write is defined by the same value
// that was rendered.
func computePromotePlan(ctx context.Context, opts promotePlanOptions) (promotePlan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	projectDir := opts.ProjectDir
	if projectDir == "" {
		projectDir = projectDirForKCL()
	}
	bindings, releaseStore := opts.Bindings, opts.Releases
	if bindings == nil || releaseStore == nil {
		l, err := ledgerFor(ctx, projectDir, opts.Env)
		if err != nil {
			return promotePlan{}, err
		}
		if bindings == nil {
			bindings = l.Bindings
		}
		if releaseStore == nil {
			releaseStore = l.Releases
		}
	}
	kind := opts.Kind
	if kind == "" {
		kind = release.KindPromote
	}
	git := opts.Git
	if git == nil {
		git = gitCommitReader{}
	}
	// Newest-first, with the ordering env topology already established
	// (semver first, created_at as the tie-break so a skewed CI clock
	// cannot reorder history). Both backends return this ordering, so a
	// promote's DIRECTION has one answer whichever holds the ledger.
	releases, err := releaseStore.List(ctx)
	if err != nil {
		return promotePlan{}, fmt.Errorf("list releases from %s: %w", releaseStore.Location(), err)
	}
	byVersion := map[string]release.Release{}
	for _, rel := range releases {
		byVersion[rel.Version] = rel
	}

	plan := promotePlan{
		Env:          opts.Env,
		Kind:         kind,
		Ledger:       bindings.Location(),
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Images:       []promoteImageChange{},
		Commits:      promoteCommitRange{Commits: []string{}},
		ShipsNothing: true,
		NextStep:     fmt.Sprintf("forge env deploy %s", opts.Env),
		Note: fmt.Sprintf("promote writes a POINTER and ships nothing — no image reaches %s until `forge env deploy %s` runs, "+
			"and `forge env verify %s` proves it arrived", opts.Env, opts.Env, opts.Env),
		OK: true,
	}

	// THE TARGET RELEASE IS REQUIRED, and its absence stays an error in
	// BOTH modes rather than becoming a plan state. Without the ledger
	// there are no digests to resolve, so there is no change set to
	// preview — and reporting it as a renderable plan under --plan while a
	// real promote errored would make the two modes' exit codes disagree,
	// which is the one property this design is holding onto.
	target, ok := byVersion[opts.Version]
	if !ok {
		// Fall back to the direct read: a list is bounded, and a release
		// older than the page is still a legitimate promote target.
		rel, err := releaseStore.Get(ctx, opts.Version)
		if err != nil {
			return promotePlan{}, fmt.Errorf("read release %q: %w", opts.Version, err)
		}
		if rel == nil {
			return promotePlan{}, fmt.Errorf("release %q not found in %s.\n"+
				"  Cut it first with: forge build --release %s --push <registry>",
				opts.Version, releaseStore.Location(), opts.Version)
		}
		target = *rel
	}

	resolved, err := resolveReleaseDigests(target)
	if err != nil {
		return promotePlan{}, err
	}
	plan.targetResolved = resolved
	plan.targetSources = target.Sources()
	plan.Target = promotePlanTarget{
		Release:   opts.Version,
		CreatedAt: formatLedgerTime(target.CreatedAt),
		Images:    len(resolved),
	}
	targetGit := target.Git
	plan.Target.Git = &targetGit

	prev, hadPrev, err := bindings.Current(ctx, opts.Env)
	if err != nil {
		return promotePlan{}, fmt.Errorf("read the promotion ledger for %s: %w", opts.Env, err)
	}
	plan.Current = promotePlanBinding{Bound: hadPrev}
	var currentRel release.Release
	var currentKnown bool
	if hadPrev {
		plan.Current.Release = prev.Release
		plan.Current.PromotedAt = formatLedgerTime(prev.PromotedAt)
		if rel, found := byVersion[prev.Release]; found {
			currentRel, currentKnown = rel, true
			plan.Current.ReleaseKnown = true
			g := rel.Git
			plan.Current.Git = &g
			plan.Current.ReleaseCreatedAt = formatLedgerTime(rel.CreatedAt)
		} else {
			plan.Current.Note = fmt.Sprintf(
				"release %s is not in %s — its provenance and the commit range are unknown",
				prev.Release, releaseStore.Location())
		}
	} else {
		plan.Current.Note = fmt.Sprintf("never promoted — no release is bound to %s, so this is its first promote", opts.Env)
	}

	plan.Images = classifyPromoteImages(prev.Resolved, resolved)
	plan.Tally = tallyPromoteImages(plan.Images)
	plan.Changed = plan.Tally.Changed > 0 || plan.Tally.Added > 0 || plan.Tally.Removed > 0 ||
		!hadPrev || prev.Release != opts.Version

	plan.Direction, plan.ReleasesBetween, plan.DirectionDetail =
		promoteDirectionFor(releases, hadPrev, prev.Release, opts.Version)

	plan.Commits = computePromoteCommitRange(ctx, git, projectDir, promoteRangeInput{
		HadPrev:      hadPrev,
		CurrentRel:   currentRel,
		CurrentKnown: currentKnown,
		CurrentName:  prev.Release,
		Target:       target,
		TargetName:   opts.Version,
		Reverts:      plan.Direction == promoteDirectionBehind,
	})

	return plan, nil
}

// applyPromotePlan writes the binding the plan describes.
//
// It takes the plan rather than a version and a digest map so the write cannot
// drift from the preview: the digests applied here are the same map
// computePromotePlan classified the Images from. PromotedAt is stamped now,
// which is the one field a plan cannot predict — it is the time of the WRITE,
// and a plan that pre-stamped it would be claiming a promote happened at the
// moment it was previewed.
//
// It APPENDS. The ledger decides (release.Decide) whether the entry is a real
// move, a retry of the state the env is already in (nothing is written, and
// the existing entry comes back), or a rollback to a release the env never
// ran (refused). Applied reports whether a NEW entry was written.
func applyPromotePlan(ctx context.Context, bindings bindingStore, plan *promotePlan, by release.Actor, note string) error {
	p := release.Promotion{
		Env:        plan.Env,
		Release:    plan.Target.Release,
		Kind:       plan.Kind,
		Resolved:   plan.targetResolved,
		Sources:    plan.targetSources,
		PromotedBy: by,
		Note:       note,
	}
	got, err := bindings.Append(ctx, p)
	if err != nil {
		return fmt.Errorf("record %s of %s → %s in %s: %w", plan.Kind, plan.Env, plan.Target.Release, bindings.Location(), err)
	}
	plan.Recorded = &got
	plan.Applied = true
	return nil
}

// formatLedgerTime renders a ledger timestamp for the plan and topology
// documents, whose JSON contract is RFC3339 strings. The zero time is "".
func formatLedgerTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// classifyPromoteImages diffs the two digest maps into an explicit
// classification per image, over the UNION of both sides.
//
// The union is the whole point. Iterating only the target's images would make
// a REMOVED image vanish from the report, and iterating only the current
// binding's would hide an ADDED one — and a promote between release lines
// with different image sets (control-plane's prod carries internal-console,
// its staging line does not) is exactly when the set changes rather than the
// digests.
func classifyPromoteImages(current, target map[string]string) []promoteImageChange {
	names := make([]string, 0, len(current)+len(target))
	for name := range current {
		names = append(names, name)
	}
	for name := range target {
		if _, both := current[name]; !both {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	out := make([]promoteImageChange, 0, len(names))
	for _, name := range names {
		cur, inCur := current[name]
		tgt, inTgt := target[name]
		row := promoteImageChange{Image: name, CurrentDigest: cur, TargetDigest: tgt}
		switch {
		case inCur && inTgt && cur == tgt:
			row.Change = promoteImageUnchanged
		case inCur && inTgt:
			row.Change = promoteImageChanged
		case inTgt:
			row.Change = promoteImageAdded
		default:
			row.Change = promoteImageRemoved
		}
		out = append(out, row)
	}
	return out
}

// tallyPromoteImages counts the classifications.
func tallyPromoteImages(images []promoteImageChange) promoteImageTally {
	var t promoteImageTally
	for _, img := range images {
		switch img.Change {
		case promoteImageUnchanged:
			t.Unchanged++
		case promoteImageChanged:
			t.Changed++
		case promoteImageAdded:
			t.Added++
		case promoteImageRemoved:
			t.Removed++
		}
	}
	return t
}

// promoteDirectionFor places the target relative to the current release in the
// project's release ordering.
//
// The ordering is the newest-first list, so comparing INDEXES is what decides
// the direction — not a semver comparison done here. That is deliberate:
// sortReleasesNewestFirst already decided the order once, including the
// fallbacks for labels semver cannot parse, and a second comparison in this
// function would eventually disagree with `forge env topology` about which of
// two releases is newer. The direction of a promote is precisely the fact that
// must not have two answers.
func promoteDirectionFor(releases []release.Release, hadPrev bool, currentVersion, targetVersion string) (promoteDirection, int, string) {
	if !hadPrev {
		return promoteDirectionInitial, 0,
			"FIRST PROMOTE — this environment has never been bound to a release, so there is nothing to move from"
	}
	if currentVersion == targetVersion {
		return promoteDirectionSame, 0, fmt.Sprintf(
			"NO MOVE — already bound to %s; re-promoting re-resolves the digests from the release ledger", targetVersion)
	}
	curIdx, tgtIdx := -1, -1
	for i, rel := range releases {
		if rel.Version == currentVersion {
			curIdx = i
		}
		if rel.Version == targetVersion {
			tgtIdx = i
		}
	}
	if curIdx < 0 || tgtIdx < 0 {
		// One of the two is not in this checkout's release set, so the
		// ordering cannot place them. UNKNOWN rather than a guess: a
		// direction invented here is the one number on the screen that
		// would be believed without checking.
		missing := currentVersion
		if tgtIdx < 0 {
			missing = targetVersion
		}
		return promoteDirectionUnknown, 0, fmt.Sprintf(
			"DIRECTION UNKNOWN — release %s has no ledger in this checkout, so forge cannot tell whether %s is newer or older than %s",
			missing, targetVersion, currentVersion)
	}
	// Newest first, so a SMALLER index is a NEWER release.
	if tgtIdx < curIdx {
		return promoteDirectionAhead, curIdx - tgtIdx, fmt.Sprintf(
			"FORWARD — %s is %d release(s) NEWER than %s", targetVersion, curIdx-tgtIdx, currentVersion)
	}
	return promoteDirectionBehind, tgtIdx - curIdx, fmt.Sprintf(
		"ROLLBACK — %s is %d release(s) OLDER than %s. This moves the environment BACKWARDS",
		targetVersion, tgtIdx-curIdx, currentVersion)
}

// promoteRangeInput is what the commit-range computation needs, gathered so
// the function reads as a classification rather than a parameter list.
type promoteRangeInput struct {
	HadPrev      bool
	CurrentRel   release.Release
	CurrentKnown bool
	CurrentName  string
	Target       release.Release
	TargetName   string
	Reverts      bool
}

// computePromoteCommitRange derives the source change between the two
// releases, classifying every way it can be unavailable instead of failing.
//
// ORDER MATTERS HERE. Dirty is checked BEFORE the commits are looked up,
// because a dirty release's recorded commit is a real commit that does not
// describe the bytes that shipped — so the range would compute successfully
// and be wrong, which is the worst available outcome. A plainly-labelled
// "meaningless" beats a plausible number nobody can check.
func computePromoteCommitRange(ctx context.Context, git promoteGitReader, dir string, in promoteRangeInput) promoteCommitRange {
	out := promoteCommitRange{Commits: []string{}, Reverts: in.Reverts}

	if !in.HadPrev {
		out.State = promoteRangeFirstPromote
		out.ToCommit = in.Target.Git.Commit
		out.Detail = fmt.Sprintf("no commit range — this is the first promote of this environment, so there is no previous release to compare %s against", in.TargetName)
		return out
	}
	if in.CurrentName == in.TargetName {
		out.State = promoteRangeSameRelease
		out.FromCommit = in.Target.Git.Commit
		out.ToCommit = in.Target.Git.Commit
		out.Detail = fmt.Sprintf("no commit range — %s is already bound to %s", in.CurrentName, in.TargetName)
		return out
	}
	if !in.CurrentKnown {
		out.State = promoteRangeLedgerMissing
		out.ToCommit = in.Target.Git.Commit
		out.Detail = fmt.Sprintf("no commit range — release %s has no ledger in this checkout, so its commit is unknown (it was cut on another branch)", in.CurrentName)
		return out
	}

	out.FromCommit = in.CurrentRel.Git.Commit
	out.ToCommit = in.Target.Git.Commit

	// DIRTY FIRST. See the function comment: a dirty endpoint's commit is
	// real but does not describe what shipped, so any range computed from
	// it is confidently wrong.
	switch {
	case in.Target.Git.Dirty && in.CurrentRel.Git.Dirty:
		out.State = promoteRangeDirtyRelease
		out.Detail = fmt.Sprintf("commit range is MEANINGLESS — both %s and %s were cut from DIRTY working trees, so neither endpoint's bytes correspond to a reviewable commit", in.CurrentName, in.TargetName)
		return out
	case in.Target.Git.Dirty:
		out.State = promoteRangeDirtyRelease
		out.Detail = fmt.Sprintf("commit range is MEANINGLESS — target release %s was cut from a DIRTY working tree, so its bytes correspond to no reviewable commit (recorded commit %s does not describe them)", in.TargetName, shortSHA(in.Target.Git.Commit))
		return out
	case in.CurrentRel.Git.Dirty:
		out.State = promoteRangeDirtyRelease
		out.Detail = fmt.Sprintf("commit range is MEANINGLESS — the currently bound release %s was cut from a DIRTY working tree, so there is no reviewable commit to measure from", in.CurrentName)
		return out
	}

	if out.FromCommit == "" || out.ToCommit == "" {
		out.State = promoteRangeNoCommit
		which := in.CurrentName
		if out.ToCommit == "" {
			which = in.TargetName
		}
		out.Detail = fmt.Sprintf("no commit range — release %s's ledger records no git commit (cut from a non-git tree, or by a forge too old to record provenance)", which)
		return out
	}

	// A COMMIT ABSENT FROM THIS CHECKOUT IS NOT A FAILURE. It means the
	// release was cut on a branch this working copy does not have, which is
	// routine when promoting from a machine that only tracks main. Report
	// it and let the rest of the plan stand — the digests, the image set
	// and the direction are all still exactly right.
	missing := make([]string, 0, 2)
	if !git.HasCommit(ctx, dir, out.FromCommit) {
		missing = append(missing, fmt.Sprintf("%s (%s)", shortSHA(out.FromCommit), in.CurrentName))
	}
	if !git.HasCommit(ctx, dir, out.ToCommit) {
		missing = append(missing, fmt.Sprintf("%s (%s)", shortSHA(out.ToCommit), in.TargetName))
	}
	if len(missing) > 0 {
		out.State = promoteRangeCommitNotFound
		out.Detail = fmt.Sprintf("commit range unavailable — %s not present in this checkout; fetch the branch the release was cut from to see the range",
			strings.Join(missing, " and "))
		return out
	}

	// History runs oldest → newest. For a rollback that is target →
	// current, and the commits listed are the ones being taken AWAY.
	from, to := out.FromCommit, out.ToCommit
	if in.Reverts {
		from, to = out.ToCommit, out.FromCommit
	}
	commits, err := git.CommitsBetween(ctx, dir, from, to)
	if err != nil {
		out.State = promoteRangeGitUnavailable
		out.Detail = fmt.Sprintf("commit range unavailable — git could not be read in %s: %v", dir, err)
		return out
	}
	out.State = promoteRangeComputed
	out.Count = len(commits)
	if len(commits) > promoteCommitRangeLimit {
		out.Commits = append(out.Commits, commits[:promoteCommitRangeLimit]...)
		out.Truncated = true
	} else {
		out.Commits = append(out.Commits, commits...)
	}
	return out
}

// ─── Rendering ───────────────────────────────────────────────────────────────

// writePromotePlanJSON emits the plan to stdout, indented, per the house
// convention every other --json command in this package follows.
func writePromotePlanJSON(plan promotePlan) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(plan); err != nil {
		return fmt.Errorf("write promote plan: %w", err)
	}
	return nil
}

// renderPromotePlanText prints the human report.
//
// The same value drives this and the JSON, and the same value drove (or did
// not drive) the write — so the heading states which of those happened before
// anything else, rather than leaving a reader to infer it from the absence of
// a success line.
func renderPromotePlanText(plan promotePlan) {
	out := os.Stdout
	if plan.Applied {
		if plan.Current.Bound && plan.Current.Release != plan.Target.Release {
			fmt.Fprintf(out, "Promoted env %q: %s → %s\n", plan.Env, plan.Current.Release, plan.Target.Release)
		} else {
			fmt.Fprintf(out, "Promoted env %q → release %s\n", plan.Env, plan.Target.Release)
		}
	} else {
		fmt.Fprintf(out, "PLAN (dry run — nothing was written): promote env %q → release %s\n", plan.Env, plan.Target.Release)
	}

	if plan.Current.Bound {
		fmt.Fprintf(out, "  current   %s (promoted %s)\n", plan.Current.Release, plan.Current.PromotedAt)
	} else {
		fmt.Fprintf(out, "  current   (never promoted)\n")
	}
	fmt.Fprintf(out, "  target    %s", plan.Target.Release)
	if plan.Target.CreatedAt != "" {
		fmt.Fprintf(out, " (cut %s)", plan.Target.CreatedAt)
	}
	if plan.Target.Git != nil && plan.Target.Git.Dirty {
		fmt.Fprintf(out, "  [DIRTY TREE — these bytes match no reviewable commit]")
	}
	fmt.Fprintln(out)
	// The direction line is printed with the state's own word in it, not
	// as a symbol: a reader skimming for "am I going forwards" must not
	// have to decode an arrow.
	fmt.Fprintf(out, "  direction %s  %s\n", plan.Direction, plan.DirectionDetail)
	if plan.Current.Note != "" {
		fmt.Fprintf(out, "  note      %s\n", plan.Current.Note)
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "Images (%d unchanged, %d changed, %d added, %d removed)\n",
		plan.Tally.Unchanged, plan.Tally.Changed, plan.Tally.Added, plan.Tally.Removed)
	for _, img := range plan.Images {
		fmt.Fprintf(out, "  %-10s %-22s", img.Change, img.Image)
		switch img.Change {
		case promoteImageChanged:
			// Both digests, on their own labelled lines. This is the
			// case the reviewer came for and the values get copied
			// out; a single-line diff makes that harder for no gain.
			fmt.Fprintln(out)
			fmt.Fprintf(out, "      from  %s\n", img.CurrentDigest)
			fmt.Fprintf(out, "      to    %s\n", img.TargetDigest)
		case promoteImageRemoved:
			fmt.Fprintf(out, " %s\n", img.CurrentDigest)
			fmt.Fprintf(out, "      no longer declared by %s — the next deploy will not pin it\n", plan.Target.Release)
		default:
			fmt.Fprintf(out, " %s\n", img.TargetDigest)
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "Commits  %s\n", plan.Commits.State)
	switch plan.Commits.State {
	case promoteRangeComputed:
		verb := "arriving in"
		if plan.Commits.Reverts {
			verb = "REVERTED OUT OF"
		}
		fmt.Fprintf(out, "  %d commit(s) %s %s (%s..%s)\n",
			plan.Commits.Count, verb, plan.Env, shortSHA(plan.Commits.FromCommit), shortSHA(plan.Commits.ToCommit))
		for _, line := range plan.Commits.Commits {
			fmt.Fprintf(out, "    %s\n", line)
		}
		if plan.Commits.Truncated {
			fmt.Fprintf(out, "    … %d more (showing the newest %d)\n",
				plan.Commits.Count-len(plan.Commits.Commits), len(plan.Commits.Commits))
		}
	default:
		fmt.Fprintf(out, "  %s\n", plan.Commits.Detail)
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Binding:  %s\n", plan.Ledger)
	// Stated on every invocation, applied or not. Promote's effect is a
	// pointer move, and the gap between "promoted" and "running" is the
	// thing `forge env verify` had to be written to expose.
	if plan.Applied {
		fmt.Fprintf(out, "  SHIPS NOTHING: the binding moved; no image reaches %s until you run the deploy below.\n", plan.Env)
	} else {
		fmt.Fprintf(out, "  NOTHING WRITTEN: re-run without --plan to record this binding. Even then, no image ships until the deploy below.\n")
	}
	fmt.Fprintf(out, "  Deploy:   forge env deploy %s\n", plan.Env)
	fmt.Fprintf(out, "  Verify:   forge env verify %s\n", plan.Env)
}
