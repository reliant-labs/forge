// build_external_release_reuse.go lets `forge env build <env> --release` reuse
// an earlier release's image for a ShellBuild whose declared inputs did not
// change (forge#2326).
//
// Every release used to run every ShellBuild. A build that is not
// byte-for-byte reproducible (apt mirrors, `go install`, a remote builder)
// then pushed a new digest for unchanged inputs, and every workload pinned to
// that digest restarted although nothing it runs had changed.
//
// The rule, for a ShellBuild that declares `inputs`:
//
//  1. Fingerprint the inputs the way a release compares them
//     (releaseInputsFingerprint): every selected file's content plus the
//     step's own declaration — command, cwd, env, built_images, platform,
//     destination repository — with the release TAG and the checkout's
//     LOCATION masked out, since both differ on every cut while the bytes the
//     command reads do not.
//  2. Look for that fingerprint on the same image in the release the env runs
//     now, then in the newest release of the project (releaseReuseBase). An
//     older release, or a record without the fingerprint, means rebuild.
//  3. Confirm the registry still serves that digest, then point the new
//     release's tag at it (a manifest copy: no layers move), so the registry
//     looks exactly as if the command had pushed it.
//  4. Record the digest — and the fingerprint and the release it came from —
//     in the build state the cut harvests (inputs_digest, carried_from).
//
// Anything forge cannot prove runs the command and says why. `--rebuild`
// forces every command; a step with no `inputs` runs as it always has.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/reliant-labs/forge/internal/buildtarget"
	"github.com/reliant-labs/forge/pkg/release"
)

// releaseInputsScheme names the layout of the recipe releaseInputsFingerprint
// hashes. Bump it whenever that layout changes, so a digest an older forge
// recorded can never match one a newer forge computes under other rules.
const releaseInputsScheme = "forge-shellbuild-release-inputs/v1"

// Sentinels the masked values are replaced with. Each holds a NUL, which a
// rendered command, an env value and a path can never contain, so masking
// cannot make two different declarations equal unless they differ ONLY in
// the masked values.
const (
	maskTag       = "\x00forge:release-tag\x00"
	maskProject   = "\x00forge:project-dir\x00"
	maskWorkspace = "\x00forge:workspace-dir\x00"
)

// externalImageRetag points dst (`<repository>:<tag>`) at src
// (`<repository>@<digest>`) in the registry. A package var so tests can
// substitute a fake registry; production never reassigns it.
var externalImageRetag = retagImage

// releaseReuseCandidatesFn reads the releases a --release build may reuse an
// image from, in the order they are tried. A package var so tests can
// substitute a ledger; production never reassigns it.
var releaseReuseCandidatesFn = loadReleaseReuseCandidates

// retagImage copies src's manifest to dst with `docker buildx imagetools
// create`. --prefer-index=false keeps a single-platform manifest a carbon
// copy (the default would wrap it in a new index with a new digest). A buildx
// too old for the flag fails here, and the build runs instead.
func retagImage(ctx context.Context, src, dst string) error {
	out, err := exec.CommandContext(ctx, "docker", "buildx", "imagetools", "create",
		"--prefer-index=false", "--tag", dst, src).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker buildx imagetools create --tag %s %s: %w: %s", dst, src, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// releaseBuildCanCarry reports whether this build may reuse an earlier
// release's image: a --release build outside snapshot composition (which
// carries an unchanged workload by leaving it unselected instead).
func releaseBuildCanCarry(opts buildOptions) bool {
	return opts.release != "" && opts.composition == nil
}

// releaseCarry is the earlier release's image a ShellBuild reuses.
type releaseCarry struct {
	// from is the version of the release the image was recorded in.
	from string
	// repository and digest name the image; platforms is what its manifest
	// advertises.
	repository, digest string
	platforms          []string
	// tagged is the new release's tag, now pointing at digest.
	tagged string
}

// releaseReuseBase is the releases a --release build compares against, read
// from the env's ledger once per build and only if a ShellBuild needs it.
type releaseReuseBase struct {
	once     sync.Once
	load     func() ([]release.Release, error)
	releases []release.Release
	err      error
}

// newReleaseReuseBase returns the lazily-read comparison base for a build,
// or nil when the build cannot carry anything (not a release, or --rebuild).
func newReleaseReuseBase(ctx context.Context, projectDir string, opts buildOptions) *releaseReuseBase {
	if !releaseBuildCanCarry(opts) || opts.rebuild {
		return nil
	}
	return &releaseReuseBase{load: func() ([]release.Release, error) {
		return releaseReuseCandidatesFn(ctx, projectDir, opts.env)
	}}
}

func (b *releaseReuseBase) get() ([]release.Release, error) {
	if b == nil {
		return nil, nil
	}
	b.once.Do(func() { b.releases, b.err = b.load() })
	return b.releases, b.err
}

// loadReleaseReuseCandidates is the release env runs now, then the newest
// release recorded for the project, without repeats. Those are "the previous
// release": the one whose digests the env's workloads are pinned to, and the
// one a re-cut would otherwise supersede. Reusing from the bound one first is
// what keeps a consumer from rolling when two releases carry equal inputs.
func loadReleaseReuseCandidates(ctx context.Context, projectDir, env string) ([]release.Release, error) {
	ledger, err := ledgerFor(ctx, projectDir, env)
	if err != nil {
		return nil, err
	}
	var out []release.Release
	seen := map[string]bool{}
	if p, ok, err := ledger.Bindings.Current(ctx, env); err != nil {
		return nil, err
	} else if ok && p.Release != "" {
		r, err := ledger.Releases.Get(ctx, p.Release)
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, *r)
			seen[r.Version] = true
		}
	}
	all, err := ledger.Releases.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(all) > 0 && !seen[all[0].Version] {
		out = append(out, all[0])
	}
	return out, nil
}

// releaseInputsFingerprint fingerprints a ShellBuild's declared inputs the way
// a release compares them: the files' content, plus every field of the
// artifact identity (externalArtifactIdentity — the command, cwd, platform,
// inputs_file, env, built_images, source and patterns) and what only an
// execution knows (the effective env with the built_images overlay, the
// destination repository, unit, output name and inputs_file content).
//
// Two things are masked before hashing, because both differ on every cut
// while the bytes the command reads do not:
//
//   - the release tag, wherever it appears in the command or an env value
//     (the render composes it into the command: `TAG=${tag}`);
//   - the checkout's location: the project directory and the directory that
//     holds it (a command composed with `${file.workdir()}`), and the cwd,
//     which is keyed relative to the project.
//
// So a command that bakes the tag into the image itself (a version stamp)
// would be carried with the earlier release's stamp: such a build should
// declare no inputs, or take its version from a file it declares as one.
func releaseInputsFingerprint(svc WorkloadEntity, spec buildtarget.Spec, cwd, projectDir string, opts buildOptions) (buildtarget.Fingerprint, error) {
	canonical, err := canonicalDir(cwd)
	if err != nil {
		return buildtarget.Fingerprint{}, err
	}
	project, err := canonicalDir(projectDir)
	if err != nil {
		return buildtarget.Fingerprint{}, err
	}
	mask := releaseInputsMasker(spec.Tag, projectDir, project)
	identity := externalBuildIdentity(svc, canonical, opts)
	identity.Cmd = mask.Replace(identity.Cmd)
	identity.Env = maskValues(mask, identity.Env)
	identity.Cwd = releaseInputsCwd(project, canonical, identity.Source != nil)
	inputsFileDigest, err := shellBuildInputsFileDigest(svc, projectDir)
	if err != nil {
		return buildtarget.Fingerprint{}, err
	}
	recipe, err := json.Marshal(struct {
		Scheme           string                   `json:"scheme"`
		Identity         externalArtifactIdentity `json:"identity"`
		Repository       string                   `json:"repository"`
		Env              map[string]string        `json:"env"`
		Unit             string                   `json:"unit"`
		OutputName       string                   `json:"output_name"`
		InputsFileDigest string                   `json:"inputs_file_digest"`
	}{releaseInputsScheme, identity, externalPushedRepository(svc, opts.pushPlan.pushBase), maskValues(mask, spec.BuildEnv),
		svc.Build.Unit, shellBuildOutputName(svc), inputsFileDigest})
	if err != nil {
		return buildtarget.Fingerprint{}, err
	}
	return buildtarget.FingerprintInputs(shellBuildInputScope(canonical, projectDir), externalBuildInputs(svc), recipe)
}

func canonicalDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// releaseInputsMasker replaces the release tag and the checkout's location
// (the project directory and the one holding it, both as written and as
// resolved) with fixed sentinels. Longer strings are listed first, so a path
// is masked whole before any shorter value inside it.
func releaseInputsMasker(tag, projectDir, canonicalProject string) *strings.Replacer {
	type pair struct{ old, new string }
	var pairs []pair
	add := func(old, new string, minLen int) {
		if len(old) >= minLen {
			pairs = append(pairs, pair{old, new})
		}
	}
	for _, p := range distinctNonEmpty(absOr(projectDir), canonicalProject) {
		add(p, maskProject, 2)
		// The directory holding the project is where its sibling checkouts
		// live (`cwd = "../reliant"`). A filesystem root is never masked: it
		// would rewrite every separator.
		if parent := filepath.Dir(p); parent != p && parent != string(filepath.Separator) {
			add(parent, maskWorkspace, 2)
		}
	}
	add(tag, maskTag, 1)
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i].old) > len(pairs[j].old) })
	args := make([]string, 0, 2*len(pairs))
	seen := map[string]bool{}
	for _, p := range pairs {
		if !seen[p.old] {
			seen[p.old] = true
			args = append(args, p.old, p.new)
		}
	}
	return strings.NewReplacer(args...)
}

func absOr(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func distinctNonEmpty(ss ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range ss {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// maskValues masks every value of env; nil and empty are one environment.
func maskValues(mask *strings.Replacer, env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = mask.Replace(v)
	}
	return out
}

// releaseInputsCwd keys the build's cwd without the checkout's location:
// relative to the project when it lies in the project's workspace (the
// project itself, or a sibling checkout), "git-source" for a declared
// GitSource's cache checkout (whose repo and ref the identity already holds),
// and absolute otherwise — which ties only an absolute cwd outside the
// workspace to its path, costing a rebuild, never a stale reuse.
func releaseInputsCwd(project, cwd string, gitSource bool) string {
	if within(filepath.Dir(project), cwd) {
		if rel, err := filepath.Rel(project, cwd); err == nil {
			return filepath.ToSlash(rel)
		}
	}
	if gitSource {
		return "git-source"
	}
	return filepath.ToSlash(cwd)
}

// checkReleaseCarry finishes a --release build's verdict: it records the
// release fingerprint (so a build that runs still records it), and decides
// whether an earlier release's image stands in for running the command.
func checkReleaseCarry(ctx context.Context, svc WorkloadEntity, spec buildtarget.Spec, cwd string, opts buildOptions, projectDir string,
	base *releaseReuseBase, v shellInputsVerdict) (out shellInputsVerdict) {
	// v.hashed is the time already spent on the build's own fingerprint.
	start := time.Now().Add(-v.hashed)
	defer func() { out.took = time.Since(start) }()
	rfp, err := releaseInputsFingerprint(svc, spec, cwd, projectDir, opts)
	if err != nil {
		v.reason = fmt.Sprintf("could not fingerprint its inputs for a release (%v)", err)
		return v
	}
	v.releaseFP = &rfp
	v.hashed = time.Since(start)
	switch {
	case opts.rebuild:
		v.reason = "forced by --rebuild"
		return v
	case svc.Image == "":
		v.reason = "the workload declares no image, so there is no earlier release's image to reuse"
		return v
	}
	candidates, err := base.get()
	if err != nil {
		v.reason = fmt.Sprintf("could not read the release ledger to compare its inputs (%v)", err)
		return v
	}
	repo := externalPushedRepository(svc, opts.pushPlan.pushBase)
	carry, why := findReleaseCarry(ctx, candidates, repo, rfp.Digest, spec.Tag, externalImageDigestResolver, externalImageRetag)
	if carry == nil {
		v.reason = why
		return v
	}
	v.carry = carry
	return v
}

// findReleaseCarry picks the image a release build reuses: the first
// candidate release whose artifact for repo recorded inputsDigest and whose
// digest the registry still serves, after pointing repo:tag at it. nil and
// the reason when there is none.
func findReleaseCarry(ctx context.Context, candidates []release.Release, repo, inputsDigest, tag string,
	resolve func(context.Context, string) (string, []string, error),
	retag func(context.Context, string, string) error) (*releaseCarry, string) {
	if len(candidates) == 0 {
		return nil, "no earlier release is recorded to compare its inputs with"
	}
	var why []string
	note := func(format string, args ...any) { why = append(why, fmt.Sprintf(format, args...)) }
	for _, cand := range candidates {
		art, ok := cand.Artifacts[repo]
		digest, shared := art.SharedDigest()
		switch {
		case !ok || !shared:
			note("release %s has no image %s", cand.Version, repo)
			continue
		case art.InputsDigest == "":
			note("release %s recorded no inputs digest for %s (cut before forge recorded one, or the step declared no inputs then)", cand.Version, repo)
			continue
		case art.InputsDigest != inputsDigest:
			note("declared inputs changed since release %s (%s → %s)", cand.Version, shortFingerprint(art.InputsDigest), shortFingerprint(inputsDigest))
			continue
		}
		pinned := repo + "@" + digest
		got, platforms, err := resolve(ctx, pinned)
		if err != nil || got != digest {
			if err == nil {
				err = fmt.Errorf("the registry answered %s", got)
			}
			note("release %s's image %s is no longer in the registry (%v)", cand.Version, pinned, err)
			continue
		}
		tagged := repo + ":" + tag
		if why, ok := pointTagAt(ctx, tagged, pinned, digest, resolve, retag); !ok {
			note("%s", why)
			continue
		}
		return &releaseCarry{from: cand.Version, repository: repo, digest: digest, platforms: platforms, tagged: tagged}, ""
	}
	return nil, strings.Join(why, "; ")
}

// pointTagAt makes tagged resolve to digest: a no-op when it already does, a
// retag when the tag does not exist yet, and a refusal when it names another
// image (a release's tag is immutable). Verified by reading it back.
func pointTagAt(ctx context.Context, tagged, pinned, digest string,
	resolve func(context.Context, string) (string, []string, error),
	retag func(context.Context, string, string) error) (string, bool) {
	if got, _, err := resolve(ctx, tagged); err == nil {
		if got == digest {
			return "", true
		}
		return fmt.Sprintf("%s already names %s, not %s", tagged, got, digest), false
	}
	if err := retag(ctx, pinned, tagged); err != nil {
		return fmt.Sprintf("could not tag %s as %s (%v)", pinned, tagged, err), false
	}
	if got, _, err := resolve(ctx, tagged); err != nil || got != digest {
		if err == nil {
			err = fmt.Errorf("it resolves to %s", got)
		}
		return fmt.Sprintf("tagged %s as %s, but could not read it back (%v)", pinned, tagged, err), false
	}
	return "", true
}

// carryReleaseImage stands an earlier release's image in for running the
// command: it records the build state a run would have — the new release's
// tag, the reused digest, the checkout the inputs were read from — plus the
// fingerprint and the release it came from, for every consumer of the
// artifact.
func carryReleaseImage(ctx context.Context, svc WorkloadEntity, opts buildOptions, projectDir, cwd, tag string, aliases map[string][]WorkloadEntity, v shellInputsVerdict) buildResult {
	c := v.carry
	fmt.Printf("[build] %s: reusing %s@%s from release %s — declared inputs unchanged (%s; %s); tagged %s; not running the command (--rebuild forces it)\n",
		svc.Name, c.repository, c.digest, c.from, describeFingerprint(v.releaseFP, v.hashed), describeMatchedInputs(svc, v.releaseFP), c.tagged)
	for _, alias := range aliases[svc.Name] {
		fmt.Printf("[build] %s: sharing ShellBuild artifact with %s\n", alias.Name, svc.Name)
	}
	state := buildtarget.State{
		Service:   svc.Name,
		Image:     c.repository,
		Tag:       tag,
		PushedAt:  nowRFC3339(),
		Digest:    c.digest,
		Platforms: c.platforms,
		// The checkout whose declared inputs matched: a release's sources
		// are held to the project's pins (checkRecordedReleaseSources), and
		// these are the inputs the carried image corresponds to.
		Source:              externalSourceCapture(ctx, svc.Name, cwd, projectDir),
		ReleaseInputsDigest: v.releaseFP.Digest,
		CarriedFrom:         c.from,
	}
	if v.fp != nil {
		state.InputsFingerprint = v.fp.Digest
	}
	recordExternalBuildState(svc, opts, projectDir, aliases, state)
	return buildResult{
		name:       svc.Name + " (external)",
		kind:       "external",
		duration:   v.took,
		upToDate:   true,
		digest:     c.digest,
		platforms:  c.platforms,
		repository: c.repository,
		image:      svc.Image,
		workload:   svc.Name,
	}
}

// describeMatchedInputs names what matched: the declared patterns and the
// first few files they selected.
func describeMatchedInputs(svc WorkloadEntity, fp *buildtarget.Fingerprint) string {
	s := "inputs " + strings.Join(externalBuildInputs(svc), ", ")
	if fp == nil {
		return s
	}
	paths := fp.Paths()
	more := ""
	if len(paths) > 5 {
		more = fmt.Sprintf(" and %d more", len(paths)-5)
		paths = paths[:5]
	}
	return s + " → " + strings.Join(paths, ", ") + more
}
