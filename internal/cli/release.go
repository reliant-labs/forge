package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/buildtarget"
	"github.com/reliant-labs/forge/internal/gitsource"
	"github.com/reliant-labs/forge/internal/statefile"
	"github.com/reliant-labs/forge/pkg/release"
)

// The release ledger + env→release binding are the "build once → promote"
// spine described in the artifact-pipeline design. They sit ABOVE the
// per-image digest foundation that already shipped (build_state.go captures
// a digest per pushed image; deploy.go resolveDeployImageDigests pins each
// service's manifest to its own `@sha256:...`). This file adds the layer
// that lets a SINGLE build feed MANY envs:
//
//	forge build --release v1.4.0   → builds the env-agnostic images ONCE,
//	                                  records each image's digest in a
//	                                  Release ledger (.forge/releases/<v>.json).
//	forge env promote v1.4.0 --to prod → appends a promotion of prod → v1.4.0 to
//	                                  the env's append-only ledger
//	                                  (.forge/promotions/prod.jsonl, or the
//	                                  hosted control plane). No rebuild.
//	forge env deploy prod              → if prod is bound, pins the SAME digests
//	                                  the promotion froze (build once, promote);
//	                                  else falls back to the per-env build state.
//
// Scope: forge's build produces "shared" images (one digest, every env) and
// "source" frontends (a pinned commit). release.ModeVariant is representable
// and validated, but nothing here produces or deploys one yet.

// releasesDirRel is where release ledgers live, relative to the project root.
// Distinct from .forge/state (ephemeral build/deploy handoff): a release is a
// durable, human-legible artifact a team may choose to commit so the digest
// set that shipped `v1.4.0` is recoverable. One file per release version.
const releasesDirRel = ".forge/releases"

// The release TYPES live in forge/pkg/release, not here. This file is the
// FILE backend's encoding of them (where a release lives on disk, how it is
// read and written) plus the build-time harvest that produces one. The
// hosted backend (hosted_ledger.go) reads and writes the same types over the
// control plane's DeployService, so "what forge cut" and "what the control
// plane stores" are one vocabulary rather than two structs kept in step by
// comments.

// releaseFileStem maps a release version label to its on-disk ledger
// filename stem (without the .json extension). It is the SINGLE mapping
// build (write), promote (read), and deploy (read) all funnel through via
// releasePath, so they always agree on which file a version names.
//
// Unlike statefile.SafeSegment — which flattens EVERY non-[A-Za-z0-9_-]
// byte to '_', turning the common semver "v1.0.0" into the surprising
// "v1_0_0.json" a user never looks for — this preserves the dot, because a
// dot IS filesystem-safe and keeping it makes the literal version the
// filename ("v1.0.0" → "v1.0.0.json"): least surprise. Path separators
// ('/', '\') and any other unsafe byte still flatten to '_' so the write
// can never escape .forge/releases. The lone exception is the traversal
// token "." / ".." (or a name that is only dots): a dot-only stem is a
// directory reference, not a release, so it flattens to underscores. In
// practice version labels never hit that case — it's a pure safety guard.
func releaseFileStem(version string) string {
	out := make([]byte, 0, len(version))
	allDots := true
	for i := 0; i < len(version); i++ {
		c := version[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-' || c == '_':
			out = append(out, c)
			allDots = false
		case c == '.':
			out = append(out, c)
		default:
			out = append(out, '_')
			allDots = false
		}
	}
	if len(out) == 0 {
		return "_"
	}
	// A stem composed only of dots (".", "..", "...") is a path reference,
	// not a usable filename stem — neutralize it.
	if allDots {
		return strings.Repeat("_", len(out))
	}
	return string(out)
}

// releasePath returns the absolute path to a release ledger file. Build,
// promote, and deploy all resolve the version→file mapping HERE so they
// never disagree on a release's filename.
func releasePath(projectDir, version string) string {
	return filepath.Join(projectDir, releasesDirRel, releaseFileStem(version)+".json")
}

// WriteRelease persists a Release ledger. The directory is created lazily.
//
// A RELEASE IS IMMUTABLE, and this is where the file backend enforces it —
// the same rule the hosted ledger enforces with a unique index and an
// append-only trigger. Re-cutting a version that already names the SAME
// artifact set is an idempotent retry and rewrites nothing; re-cutting it
// with a DIFFERENT set is release.ErrReleaseConflict, because one version
// label meaning two digest sets would void every guarantee promotion rests
// on. release.CheckRecut is the one implementation of that rule both
// backends call.
func WriteRelease(projectDir string, r release.Release) error {
	if err := r.Validate(); err != nil {
		return err
	}
	existing, err := ReadRelease(projectDir, r.Version)
	if err != nil {
		return err
	}
	if existing != nil {
		return release.CheckRecut(*existing, r)
	}
	return statefile.Write(releasePath(projectDir, r.Version), "release", r)
}

// ReadRelease loads a Release ledger by version. Returns (nil, nil) when the
// file is missing — the caller decides whether that's an error (a deploy
// referencing an absent release) or a fall-through.
//
// A ledger that does not satisfy release.Validate is an ERROR, not a
// best-effort read: the closed Kind/Mode enums exist so that an artifact
// nobody can classify is never read as an image.
func ReadRelease(projectDir, version string) (*release.Release, error) {
	path := releasePath(projectDir, version)
	rel, err := statefile.Read[release.Release](path, "release")
	if err != nil || rel == nil {
		return rel, err
	}
	if err := rel.Validate(); err != nil {
		return nil, fmt.Errorf("release ledger %s: %w", path, err)
	}
	return rel, nil
}

// harvestReleaseArtifacts collects the per-image digests captured by the build
// that just ran, from the SAME build-state sources resolveDeployImageDigests
// reads at deploy time:
//
//   - the aggregate .forge/state/build-<env>.json (and the env-agnostic
//     "default" record a plain `forge build` writes) — the project image's
//     digest.
//   - every per-service .forge/state/build-<env>-<service>.json the external-
//     build dispatcher writes — each external image's own digest.
//
// Reusing the existing digest capture is the whole point: the release ledger is
// a DURABLE projection of the ephemeral build state, not a parallel capture
// mechanism. Every harvested image becomes a "shared" artifact (one digest,
// promoted to all envs) — the MVP scope.
//
// Returns an image-name → ReleaseArtifact map. An image with no captured digest
// is omitted (a release records only what was content-addressed); the caller
// errors if the map is empty so a release is never cut with zero digests.
func harvestReleaseArtifacts(projectDir, envName string) map[string]release.Artifact {
	out := map[string]release.Artifact{}

	add := func(image, digest, registry string, platforms []string) {
		if image == "" || digest == "" {
			return
		}
		out[image] = release.Artifact{
			Kind:    release.KindOCI,
			Mode:    release.ModeShared,
			Digests: map[string]string{release.SharedVariant: digest},
			// The registry the build pushed to. Recorded because a digest
			// alone is not an ADDRESS: `sha256:…` says what the bytes are
			// but not which host serves them, so a ledger without this can
			// name an image it cannot prove exists. `forge release verify`
			// reads it to fetch the manifest. Empty for a local/compose
			// build that pushed nowhere, which verification then reports as
			// unverifiable rather than passing it silently.
			URI:       registry,
			Platforms: platforms,
		}
	}

	// Aggregate project-image build state(s): env-specific, then the
	// env-agnostic default a plain `forge build --release` (no --env) writes.
	for _, key := range buildStateLookupEnvs(envName) {
		st, err := ReadBuildState(projectDir, key)
		if err != nil || st == nil {
			continue
		}
		add(st.Image, st.Digest, st.Registry, st.Platforms)
	}

	// Per-service external-build states: build-<env>-<service>.json. Glob the
	// state dir for each lookup env's per-service files and read each typed
	// record. A `forge build --release` runs env-agnostic (env ""), so the
	// per-service external builds land under the "default" key — iterating the
	// same buildStateLookupEnvs fallback the aggregate uses keeps the two
	// sources symmetric (and a release built with an explicit --env still picks
	// up its env-specific per-service files).
	stateDir := filepath.Join(projectDir, statefile.DirRel)
	for _, key := range buildStateLookupEnvs(envName) {
		prefix := "build-" + statefile.SafeSegment(key) + "-"
		matches, _ := filepath.Glob(filepath.Join(stateDir, prefix+"*.json"))
		for _, path := range matches {
			base := filepath.Base(path)
			if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, ".json") {
				continue
			}
			service := strings.TrimSuffix(strings.TrimPrefix(base, prefix), ".json")
			st, err := buildtarget.ReadState(projectDir, key, service)
			if err != nil || st == nil {
				continue
			}
			add(st.Image, st.Digest, st.Registry, st.Platforms)
		}
	}

	return out
}

func envNameOr(env string) string {
	if env == "" {
		return "<env>"
	}
	return env
}

// addFrontendSourceArtifacts records every source-pinned frontend the env
// declares as a source-mode artifact, keyed by frontend name.
//
// WHY THIS IS NOT OPTIONAL. A release is meant to be the whole environment at
// a version. Container images are only the containerized half of one: a
// Firebase Hosting SPA declared via forge.GitSource is built from a pinned
// commit and uploaded to a CDN, so it produces no image, writes no build
// state, and — before this — appeared nowhere in the ledger. The consequence
// is not cosmetic. Such a frontend has no promotion record, no staleness
// check, and no entry a deploy could verify; its only pin is a ref string in
// KCL that nothing reconciles, so it drifts out of step with the API it talks
// to while every image around it advances correctly. Recording it here makes
// the release cover the environment rather than a subset of it.
//
// The pin is resolved through the SAME gitsource resolver the build and
// deploy paths use, so the commit recorded is the commit those paths would
// build. A local source-override (a developer pointing at a working tree) is
// deliberately NOT recorded: an override is "build what is in front of me",
// which by definition is not a reproducible pin and must never be frozen into
// a release as though it were one.
//
// Errors are collected rather than swallowed. A frontend that cannot be
// resolved is a release that cannot honestly claim to contain it.
func addFrontendSourceArtifacts(ctx context.Context, projectDir string, entities *KCLEntities, out map[string]release.Artifact) error {
	if entities == nil {
		return nil
	}
	if !hasPinnedFrontend(entities) {
		return nil
	}
	resolver, err := frontendSourceResolver(projectDir)
	if err != nil {
		return err
	}
	return addFrontendSourceArtifactsWith(ctx, resolver, entities, out)
}

// hasPinnedFrontend reports whether any frontend declares a cross-repo pin.
// Checked before constructing a resolver so a project with no such frontend
// never touches the source cache.
func hasPinnedFrontend(entities *KCLEntities) bool {
	for _, fe := range entities.Frontends {
		if fe.Source != nil && fe.Source.Repo != "" {
			return true
		}
	}
	return false
}

// pinResolver is the one thing the release path needs from the source
// resolver: turn a declared pin into a resolution. Declared HERE, at the
// consumer, rather than exported from internal/gitsource — the release path
// does not care about caching, overrides files, or fetching, and a narrow
// local interface is what lets this be tested without a network.
type pinResolver interface {
	Resolve(ctx context.Context, src gitsource.Source) (gitsource.Resolution, error)
}

// addFrontendSourceArtifactsWith is the resolver-injected half of
// addFrontendSourceArtifacts. Split so the capture logic — including the
// override rejection, which is a correctness rule and not an implementation
// detail — is testable against a stub.
func addFrontendSourceArtifactsWith(ctx context.Context, resolver pinResolver, entities *KCLEntities, out map[string]release.Artifact) error {
	var pinned []FrontendEntity
	for _, fe := range entities.Frontends {
		if fe.Source != nil && fe.Source.Repo != "" {
			pinned = append(pinned, fe)
		}
	}
	for _, fe := range pinned {
		src := toGitSource(fe.Source)
		res, rerr := resolver.Resolve(ctx, src)
		if rerr != nil {
			return fmt.Errorf("frontend %q: resolve pinned source %s: %w", fe.Name, src, rerr)
		}
		if res.Overridden {
			return fmt.Errorf("frontend %q is served by a LOCAL SOURCE OVERRIDE (%s), not its declared pin %s.\n"+
				"  A release must be reproducible, and an override is by definition not — freezing one into a\n"+
				"  release ledger would record a version that no other machine can rebuild.\n"+
				"  Remove the entry from %s and re-cut",
				fe.Name, res.Dir, src, filepath.Join(gitsource.OverridesDirName, gitsource.OverridesFileName))
		}
		out[fe.Name] = release.Artifact{
			Kind: release.KindGit,
			Mode: release.ModeSource,
			Source: &release.Source{
				Repo:   src.Repo,
				Ref:    src.Ref,
				Subdir: src.Subdir,
				Commit: res.Commit,
			},
		}
	}
	return nil
}

// checkReleaseCoversEnv fails the cut when the env DECLARES an artifact the
// ledger does not contain.
//
// WHY A CUT SHOULD FAIL RATHER THAN WARN. A release names a version, and
// everything downstream treats that name as covering the environment: promote
// binds it wholesale, deploy pins from it, and a human reviewing "v1.6.0"
// reasonably assumes v1.6.0 is all of v1.6.0. A ledger missing one declared
// artifact violates that silently — the deploy falls back to a mutable tag (or
// to whatever the previous binding held) for the missing piece and ships a mix
// of two releases under one version. Nothing goes red; the environment is just
// quietly wrong, and it stays wrong until someone notices behaviour that does
// not match the version they think is deployed.
//
// The check is DERIVED from the rendered env, never from a list. That is the
// whole point: the artifact set is discovered from deploy/kcl/<env>/main.k, so
// declaring a new service or frontend puts it in the release automatically and
// a hand-maintained enumeration cannot fall behind the declaration.
func checkReleaseCoversEnv(entities *KCLEntities, artifacts map[string]release.Artifact, opts buildOptions) error {
	if entities == nil {
		// No render (no --env) means nothing to compare against. --release
		// requires an env argument, so this is unreachable in practice; a
		// nil check here is cheaper than an assumption.
		return nil
	}

	// Deduplicate by IMAGE, not by service. Several services routinely share
	// one image (in this project five run `control-plane` and three run
	// `reliant`), and listing the same missing image once per service turns
	// one fact into nine lines and reads like nine separate problems.
	missingImages := map[string][]string{}
	var imageOrder []string
	for _, s := range entities.Services {
		if s.Image == "" {
			continue
		}
		if _, ok := artifacts[s.Image]; ok {
			continue
		}
		if _, seen := missingImages[s.Image]; !seen {
			imageOrder = append(imageOrder, s.Image)
		}
		missingImages[s.Image] = append(missingImages[s.Image], s.Name)
	}

	var missing []string
	for _, image := range imageOrder {
		svcs := missingImages[image]
		sort.Strings(svcs)
		missing = append(missing, fmt.Sprintf("%s (image, used by %s)", image, strings.Join(svcs, ", ")))
	}
	for _, fe := range entities.Frontends {
		// A hosted StaticSite ships as an OCI release artifact keyed by the
		// frontend name (buildHostedStaticSites); the hosted deploy pins it
		// as liveDigest, so a release without it cannot deploy the site.
		if entities.ControlPlane != nil && fe.Deploy != nil && fe.Deploy.Type == frontendDeployStaticSite {
			if _, ok := artifacts[fe.Name]; !ok {
				missing = append(missing, fmt.Sprintf("%s (hosted static site: forge build %s --push <image push base>)", fe.Name, envNameOr(opts.env)))
			}
			continue
		}
		// Cluster frontends ship as images and are covered by the image
		// sweep above under their image name; source-pinned frontends are
		// keyed by frontend name. Only the latter are checked here.
		if fe.Source == nil || fe.Source.Repo == "" {
			continue
		}
		if _, ok := artifacts[fe.Name]; !ok {
			missing = append(missing, fmt.Sprintf("%s (frontend)", fe.Name))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)

	envName := opts.env
	if envName == "" {
		envName = "<env>"
	}
	return fmt.Errorf("--release %s: the release does not cover everything %s declares.\n"+
		"  Missing from the ledger:\n    %s\n"+
		"  These are declared in deploy/kcl/%s/main.k but no digest or commit was captured for them,\n"+
		"  so promoting this release would deploy them from a mutable tag (or leave them on whatever the\n"+
		"  previous binding pinned) while every other artifact advanced — one version, two releases.\n"+
		"  Build the full set (drop --target, and pass --push <registry> so images are digest-addressable),\n"+
		"  or remove what the environment no longer ships",
		opts.release, envName, strings.Join(missing, "\n    "), envName)
}

// countOCIArtifacts returns how many of a release's artifacts are container
// images. Used to tell "this release ships no images" apart from "this
// release's images are all variant-mode", which are different problems with
// different answers.
func countOCIArtifacts(r release.Release) int {
	n := 0
	for _, art := range r.Artifacts {
		if art.Kind == release.KindOCI {
			n++
		}
	}
	return n
}

// releaseArtifactKinds returns the sorted distinct kinds present in a release,
// for error messages that name what a release actually holds rather than what
// it lacks.
func releaseArtifactKinds(r release.Release) []string {
	seen := map[string]bool{}
	for _, art := range r.Artifacts {
		seen[string(art.Kind)] = true
	}
	kinds := make([]string, 0, len(seen))
	for k := range seen {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// releaseImageNames returns the sorted image names in a release, for legible
// summary/log output.
func releaseImageNames(r release.Release) []string {
	return r.ArtifactNames()
}

// resolveReleaseDigests flattens a release's artifacts into the bare
// image-name → digest map the deploy/promote paths consume. Only shared
// artifacts resolve here (the MVP); a variant artifact is skipped with no
// error (its per-target resolution is a deferred follow-up). Returns an error
// only if the release carries no resolvable digests at all — a release that
// can't pin anything is a bug, not a silent fall-through.
func resolveReleaseDigests(r release.Release) (map[string]string, error) {
	out := r.SharedDigests()
	if len(out) == 0 {
		// A release that pins nothing is a dead end for promote/deploy: the
		// whole point is to advance content-addressed digests by reference.
		// The single most common way to reach here is a release cut with an
		// EMPTY artifact map — `forge build --release X` ran but captured no
		// digests (forgot --push, no services built, external builds skipped
		// for a missing build_cwd). The previous message ("carries no shared
		// image digests to pin") read like an internal invariant and left the
		// user with no next step, so distinguish the two cases and, for the
		// empty-artifacts case, spell out exactly why a release ends up empty
		// and what to do about it.
		if len(r.Artifacts) == 0 {
			return nil, fmt.Errorf(
				"release %q was cut with no image digests. This can happen if:\n"+
					"  (1) no docker images were built (check --env and that KCL declares services),\n"+
					"  (2) digests were not captured (re-run the build with --push <registry>), or\n"+
					"  (3) all external builds were skipped due to a missing build_cwd.\n"+
					"Inspect the release file with `forge project audit` to see what was recorded",
				r.Version)
		}
		// Artifacts exist but none resolved a shared OCI digest. Two very
		// different causes, and saying "variant-mode" for both would send a
		// reader looking for a feature flag when the real answer is that this
		// release ships no images at all.
		if n := countOCIArtifacts(r); n == 0 {
			return nil, fmt.Errorf(
				"release %q contains no container images to deploy — it has %d artifact(s), all non-OCI (%s). "+
					"A release of only packages or files can be verified with `forge release verify`, but there is nothing for an environment to run",
				r.Version, len(r.Artifacts), strings.Join(releaseArtifactKinds(r), ", "))
		}
		return nil, fmt.Errorf("release %q carries only variant-mode artifacts; shared image digests are required to promote/deploy (variant promotion is not yet supported)", r.Version)
	}
	return out, nil
}
