package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// O-15: `forge env deploy <env>` with no version does ALL the deployment
// bits. These pin the ORDER and the write discipline, which is what the owner
// hit on hounders prod: the command that should ship the checkout instead
// printed a three-step runbook, one step of which cut a release with no
// images in it.

// deployAllProject is a self-managed (file-ledger) project with one env. The
// file ledger is deliberate: it makes "was a release cut" and "was a
// promotion written" two separate things a test can read off disk, with no
// server to interpret them.
func deployAllProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("forge.yaml", "name: deployall\nmodule_path: github.com/example/deployall\nversion: \"0.1.0\"\n")
	markServiceProject(t, dir)
	return dir
}

// deployTestTree is the tree hash these tests build "from". Stated, because
// a t.TempDir() is not a git repository and the real capture would report no
// tree at all — which is F-16's never-reuse path, the opposite of what the
// reuse tests exercise.
var deployTestTree = strings.Repeat("a", 40)

// stubDeployBuild replaces the build with a recorder that cuts the release
// the deploy asked for, exactly as the real `--release` build does, and
// states the checkout's provenance. It returns a pointer to the call log.
func stubDeployBuild(t *testing.T, dir string, artifacts map[string]string) *[]buildOptions {
	t.Helper()
	prevProv := deployProvenance
	deployProvenance = func(context.Context, string) release.Provenance {
		return release.Provenance{Commit: strings.Repeat("c", 40), Tree: deployTestTree}
	}
	t.Cleanup(func() { deployProvenance = prevProv })
	var calls []buildOptions
	prev := runDeployBuild
	runDeployBuild = func(_ context.Context, opts buildOptions) error {
		calls = append(calls, opts)
		if opts.release == "" {
			// A reused version: the build still runs and still pushes,
			// but nothing is cut. Mirrors the real path.
			return nil
		}
		rel := ociRelease(opts.release, artifacts)
		rel.Provenance = &release.Provenance{
			Commit: strings.Repeat("c", 40),
			Tree:   deployTestTree,
		}
		if _, err := (fileReleaseLedger{projectDir: dir}).Cut(context.Background(), rel); err != nil {
			t.Fatalf("stub build: cut %s: %v", opts.release, err)
		}
		return nil
	}
	t.Cleanup(func() { runDeployBuild = prev })
	return &calls
}

// TestDeployNoVersion_BuildsCutsThenPromotes is the O-15 happy path: build,
// cut the auto version, and promote — IN THAT ORDER.
//
// The order is the assertion, not an implementation detail. A promotion
// written before the images are pushed names digests that do not exist yet,
// and with the converger on, the promotion IS the deploy — so it would roll
// out a release whose bytes had not landed.
func TestDeployNoVersion_BuildsCutsThenPromotes(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the deploy path end to end; skipped in -short")
	}
	dir := deployAllProject(t)
	t.Chdir(dir)
	calls := stubDeployBuild(t, dir, map[string]string{"api": sha("1")})

	ledger := fileLedger(dir)
	prov := release.Provenance{Commit: strings.Repeat("c", 40), Tree: deployTestTree}
	cut, err := buildAndCutForDeploy(context.Background(), dir, "prod", deployCmdFlags{}, ledger)
	if err != nil {
		t.Fatalf("buildAndCutForDeploy: %v", err)
	}

	// 1. The build ran, and it ran with --push and --release <auto version>.
	if len(*calls) != 1 {
		t.Fatalf("build ran %d times, want once", len(*calls))
	}
	got := (*calls)[0]
	// pushIfDeclared, not push: both publish every declared reference, and
	// they differ only on an env that declares NONE — where --push is a
	// usage error and this verb must still proceed to the cut (a hosted env
	// whose images CI pushes is perfectly deployable).
	if !got.pushIfDeclared {
		t.Error("the build did not push: a release pins digests, which require a registry")
	}
	if got.push {
		t.Error("the build used --push, which refuses an env with nothing of its own to push")
	}
	if got.release != cut.Version {
		t.Errorf("build cut %q but the deploy returned %q", got.release, cut.Version)
	}
	if got.env != "prod" {
		t.Errorf("build env = %q, want prod", got.env)
	}

	// 2. The version is the auto-version shape, naming this tree.
	if want := autoVersionFor(prov, time.Now()); cut.Version[:8] != want[:8] {
		t.Errorf("version %q does not carry today's UTC date like %q", cut.Version, want)
	}
	if !strings.HasSuffix(cut.Version, "-"+strings.Repeat("a", 12)) {
		t.Errorf("version %q does not end in the tree's first 12 hex", cut.Version)
	}
	if cut.Reused {
		t.Error("the first deploy of a tree must CUT, not reuse")
	}

	// 3. The release is in the ledger and nothing is promoted yet — the
	// promotion is runPromote's job, behind the confirmation gate.
	rel, err := ledger.Releases.Get(context.Background(), cut.Version)
	if err != nil || rel == nil {
		t.Fatalf("release %s was not recorded: %v", cut.Version, err)
	}
	if _, bound, _ := ledger.Bindings.Current(context.Background(), "prod"); bound {
		t.Error("a promotion was written by the build-and-cut half, before the plan was even computed")
	}
}

// TestDeployNoVersion_RetryOfTheSameTreeReusesTheRelease: a second deploy of
// an unchanged checkout cuts NOTHING.
//
// Without this, every retry — a declined confirmation, a failed rollout, a CI
// re-run — names a new version for bytes that already have one, and the
// ledger fills with near-duplicates distinguished only by the minute they
// were named.
func TestDeployNoVersion_RetryOfTheSameTreeReusesTheRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the deploy path end to end; skipped in -short")
	}
	dir := deployAllProject(t)
	t.Chdir(dir)
	calls := stubDeployBuild(t, dir, map[string]string{"api": sha("1")})
	ledger := fileLedger(dir)

	first, err := buildAndCutForDeploy(context.Background(), dir, "prod", deployCmdFlags{}, ledger)
	if err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	second, err := buildAndCutForDeploy(context.Background(), dir, "prod", deployCmdFlags{}, ledger)
	if err != nil {
		t.Fatalf("second deploy: %v", err)
	}

	if second.Version != first.Version {
		t.Errorf("the retry cut %q, want the existing %q — one tree is cut once", second.Version, first.Version)
	}
	if !second.Reused {
		t.Error("the retry did not report the release as reused")
	}
	// The build still RAN (the registry may have expired the images, and a
	// re-push of identical bytes is a content-addressed no-op), but it was
	// asked to cut nothing.
	if len(*calls) != 2 {
		t.Fatalf("build ran %d times, want twice — the retry still builds", len(*calls))
	}
	if (*calls)[1].release != "" {
		t.Errorf("the retry asked the build to cut %q; a reused version must not be re-cut",
			(*calls)[1].release)
	}
	// Exactly one release exists.
	all, err := ledger.Releases.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("%d releases in the ledger, want 1", len(all))
	}
}

// TestAutoVersion covers the naming rule, including F-16's unhashed tree.
func TestAutoVersion(t *testing.T) {
	at := time.Date(2026, 10, 2, 17, 4, 5, 0, time.UTC)
	tree := strings.Repeat("a", 40)
	commit := strings.Repeat("c", 40)

	if got, want := autoVersionFor(release.Provenance{Tree: tree, Commit: commit}, at),
		"20261002.170405-"+strings.Repeat("a", 12); got != want {
		t.Errorf("tree version = %q, want %q", got, want)
	}
	// F-16: an unhashed tree falls back to the commit. The name still means
	// something, and reusableReleaseForTree refuses to reuse it.
	if got, want := autoVersionFor(release.Provenance{Commit: commit}, at),
		"20261002.170405-"+strings.Repeat("c", 12); got != want {
		t.Errorf("commit fallback = %q, want %q", got, want)
	}
	// Not a git tree at all: the timestamp is still unique per project and
	// still sorts, which is all the ledger needs.
	if got, want := autoVersionFor(release.Provenance{}, at), "20261002.170405"; got != want {
		t.Errorf("bare version = %q, want %q", got, want)
	}
	// It is UTC, not local: two machines in two zones must not name the
	// same tree differently.
	zone := time.FixedZone("UTC+9", 9*3600)
	if got := autoVersionFor(release.Provenance{Tree: tree}, at.In(zone)); !strings.HasPrefix(got, "20261002.170405") {
		t.Errorf("version %q is not UTC-normalized", got)
	}
}

// An UNHASHED tree is never reused (F-16): forge does not know what the
// content was, so two builds sharing a commit may differ. Reusing on the
// commit would pin a release to bytes it was not cut from.
func TestReusableReleaseForTree_UnhashedTreeIsNeverReused(t *testing.T) {
	dir := t.TempDir()
	store := fileReleaseLedger{projectDir: dir}
	rel := ociRelease("v1", map[string]string{"api": sha("1")})
	rel.Provenance = &release.Provenance{Commit: strings.Repeat("c", 40)} // no Tree
	if _, err := store.Cut(context.Background(), rel); err != nil {
		t.Fatal(err)
	}
	// An empty tree finds nothing, even though a release with the same
	// (empty) tree is sitting in the ledger.
	got, err := reusableReleaseForTree(context.Background(), store, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("an unhashed tree reused release %s", got.Version)
	}
}

// A HASHED tree finds its release, whatever the version is called — including
// a hand-named one, so `forge env deploy <env>` after `forge env build <env>
// --release v1.4.0` deploys v1.4.0 rather than cutting a duplicate.
func TestReusableReleaseForTree_FindsAHandNamedRelease(t *testing.T) {
	dir := t.TempDir()
	store := fileReleaseLedger{projectDir: dir}
	tree := strings.Repeat("b", 40)
	rel := ociRelease("v1.4.0", map[string]string{"api": sha("1")})
	rel.Provenance = &release.Provenance{Commit: strings.Repeat("c", 40), Tree: tree}
	if _, err := store.Cut(context.Background(), rel); err != nil {
		t.Fatal(err)
	}
	got, err := reusableReleaseForTree(context.Background(), store, tree)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Version != "v1.4.0" {
		t.Fatalf("got %v, want the hand-named v1.4.0", got)
	}
}

// An unknown version's fix is `forge env deploy <env>`, NEVER --no-build.
// This is the exact text the owner was given on hounders prod.
func TestDeployUnknownVersion_FixIsTheDeployVerbNotNoBuild(t *testing.T) {
	dir := deployAllProject(t)
	t.Chdir(dir)
	err := runPromote(context.Background(), "v9.9.9", "prod", promoteOptions{
		Ledger: fileLedger(dir), ProjectDir: dir,
	})
	if err == nil {
		t.Fatal("deploying a version nobody cut must fail")
	}
	got := err.Error()
	if strings.Contains(got, "--no-build") {
		t.Errorf("the fix still recommends --no-build, which cuts a release with no images:\n%s", got)
	}
	for _, want := range []string{"was never cut", "forge env deploy prod", "builds, pushes, cuts"} {
		if !strings.Contains(got, want) {
			t.Errorf("error is missing %q:\n%s", want, got)
		}
	}
}

// ─── The confirmation gate (O-13, §13 F-18) ─────────────────────────────────

// No TTY and no --yes: exit 5, plan_unconfirmed, with the release CUT and NO
// promotion written.
//
// The two halves both matter. Exit 5 is what a pipeline branches on. "Release
// cut, promotion absent" is what makes the refusal cheap to recover from: the
// images are pushed and the version exists, so approving it needs no rebuild
// — which is why the message names the exact command.
func TestDeployConfirm_NoTTYNoYesExits5AndWritesNoPromotion(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the deploy path end to end; skipped in -short")
	}
	dir := deployAllProject(t)
	t.Chdir(dir)
	stubDeployBuild(t, dir, map[string]string{"api": sha("1")})
	ledger := fileLedger(dir)

	cut, err := buildAndCutForDeploy(context.Background(), dir, "prod", deployCmdFlags{}, ledger)
	if err != nil {
		t.Fatalf("build and cut: %v", err)
	}
	err = runPromote(context.Background(), cut.Version, "prod", promoteOptions{
		Ledger: ledger, ProjectDir: dir,
		Confirm: &deployConfirm{AutoVersion: cut.Version, Interactive: false},
	})
	if err == nil {
		t.Fatal("a non-interactive deploy with no --yes must refuse")
	}
	var coded exitCodeError
	if !errors.As(err, &coded) {
		t.Fatalf("err = %v (%T), want an exitCodeError carrying 5", err, err)
	}
	// Both: the named constant, and the literal a pipeline branches on.
	if coded.ExitCode() != exitPlanUnconfirmed || coded.ExitCode() != 5 {
		t.Errorf("exit code = %d, want 5 (plan_unconfirmed)", coded.ExitCode())
	}
	for _, want := range []string{"plan_unconfirmed", "NO promotion was written", "--yes", cut.Version} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q:\n%v", want, err)
		}
	}
	// THE RELEASE IS CUT.
	if rel, gerr := ledger.Releases.Get(context.Background(), cut.Version); gerr != nil || rel == nil {
		t.Errorf("release %s is absent; the refusal must keep what the build produced", cut.Version)
	}
	// AND NO PROMOTION WAS WRITTEN. Asserted against the ledger, not
	// against the message: a promotion IS the deploy, so this is the
	// property the whole gate exists for.
	if _, bound, berr := ledger.Bindings.Current(context.Background(), "prod"); berr != nil || bound {
		t.Error("a promotion was written despite the plan never being confirmed")
	}
}

// --plan-only writes no promotion and exits 0: it is the first stage of a
// two-stage pipeline, and a stage that failed would make the pipeline red for
// doing exactly what it was asked.
func TestDeployConfirm_PlanOnlyWritesNoPromotionAndSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the deploy path end to end; skipped in -short")
	}
	dir := deployAllProject(t)
	t.Chdir(dir)
	stubDeployBuild(t, dir, map[string]string{"api": sha("1")})
	ledger := fileLedger(dir)

	cut, err := buildAndCutForDeploy(context.Background(), dir, "prod", deployCmdFlags{}, ledger)
	if err != nil {
		t.Fatalf("build and cut: %v", err)
	}
	if err := runPromote(context.Background(), cut.Version, "prod", promoteOptions{
		Ledger: ledger, ProjectDir: dir,
		Confirm: &deployConfirm{PlanOnly: true, AutoVersion: cut.Version, Interactive: false},
	}); err != nil {
		t.Fatalf("--plan-only must exit 0, got: %v", err)
	}
	if _, bound, _ := ledger.Bindings.Current(context.Background(), "prod"); bound {
		t.Error("--plan-only wrote a promotion")
	}
	// --plan-only beats --yes: a caller that passes both asked for the
	// preview, and a preview that deployed would be the one command nobody
	// could run safely.
	if err := runPromote(context.Background(), cut.Version, "prod", promoteOptions{
		Ledger: ledger, ProjectDir: dir,
		Confirm: &deployConfirm{PlanOnly: true, Yes: true, AutoVersion: cut.Version, Interactive: false},
	}); err != nil {
		t.Fatalf("--plan-only --yes must exit 0, got: %v", err)
	}
	if _, bound, _ := ledger.Bindings.Current(context.Background(), "prod"); bound {
		t.Error("--plan-only --yes wrote a promotion; --plan-only must win")
	}
}

// --yes proceeds, and an interactive NO does not. Both end states are read
// off the ledger.
func TestDeployConfirm_YesWritesAndDeclineDoesNot(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the deploy path end to end; skipped in -short")
	}
	dir := deployAllProject(t)
	t.Chdir(dir)
	stubDeployBuild(t, dir, map[string]string{"api": sha("1")})
	ledger := fileLedger(dir)
	cut, err := buildAndCutForDeploy(context.Background(), dir, "prod", deployCmdFlags{}, ledger)
	if err != nil {
		t.Fatalf("build and cut: %v", err)
	}

	// An interactive "no" is a SUCCESS that writes nothing: the caller
	// asked what would happen and found out.
	if err := runPromote(context.Background(), cut.Version, "prod", promoteOptions{
		Ledger: ledger, ProjectDir: dir,
		Confirm: &deployConfirm{
			AutoVersion: cut.Version,
			Interactive: true,
			prompt:      func(string) (bool, error) { return false, nil },
		},
	}); err != nil {
		t.Fatalf("a declined confirmation must exit 0, got: %v", err)
	}
	if _, bound, _ := ledger.Bindings.Current(context.Background(), "prod"); bound {
		t.Fatal("answering no wrote a promotion — this is the v1.7.13 regression")
	}

	// --yes writes it. Follow is nil, so this is the ledger write alone.
	if err := runPromote(context.Background(), cut.Version, "prod", promoteOptions{
		Ledger: ledger, ProjectDir: dir,
		Confirm: &deployConfirm{Yes: true, AutoVersion: cut.Version, Interactive: false},
	}); err != nil {
		t.Fatalf("--yes must deploy, got: %v", err)
	}
	bound, ok, err := ledger.Bindings.Current(context.Background(), "prod")
	if err != nil || !ok {
		t.Fatalf("--yes wrote no promotion: ok=%v err=%v", ok, err)
	}
	if bound.Release != cut.Version {
		t.Errorf("promoted %q, want %q", bound.Release, cut.Version)
	}
}

// An interactive prompt defaults to NO: a bare Enter must not ship a release.
func TestDeployConfirm_PromptDefaultsToNo(t *testing.T) {
	plan := promotePlan{Env: "prod", Target: promotePlanTarget{Release: "v1"}}
	var asked string
	out := confirmDeployPlan("prod", plan, deployConfirm{
		Interactive: true,
		prompt: func(q string) (bool, error) {
			asked = q
			// What promptYesNo returns for a bare Enter.
			return false, nil
		},
		out: os.Stderr,
	})
	if out.Confirmed {
		t.Error("a bare Enter confirmed the deploy")
	}
	if out.Err != nil {
		t.Errorf("declining is not an error: %v", out.Err)
	}
	if !strings.Contains(asked, "prod") || !strings.Contains(asked, "v1") {
		t.Errorf("the prompt must name the env and the release, got %q", asked)
	}
}
