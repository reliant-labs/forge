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
//	forge env promote v1.4.0 --to prod → records env prod → release v1.4.0 in the
//	                                  binding ledger (.forge/env-releases.json).
//	                                  Pure pointer move, no rebuild.
//	forge env deploy prod              → if prod is bound, pins the SAME digests
//	                                  the release captured (build once, promote);
//	                                  else falls back to today's per-env build
//	                                  state (full backward compat).
//
// MVP scope (see Release.Mode): all images are "shared" — built once, one
// digest each, promoted byte-identical to every env. The `variant` mode
// (genuinely-different per-env builds — NEXT_PUBLIC_* baked at build, compile
// flags) and the multi-arch index / cross-cloud-region matrix are DEFERRED;
// the shapes leave room for them (per-image digest map keyed by variant) but
// the MVP only ever writes the "*" shared key.

// releasesDirRel is where release ledgers live, relative to the project root.
// Distinct from .forge/state (ephemeral build/deploy handoff): a release is a
// durable, human-legible artifact a team may choose to commit so the digest
// set that shipped `v1.4.0` is recoverable. One file per release version.
const releasesDirRel = ".forge/releases"

// envReleasesRel is the single env→release binding ledger, relative to the
// project root. One file (not one-per-env) so the whole promotion state of the
// project — which release each env runs — is legible at a glance.
const envReleasesRel = ".forge/env-releases.json"

// sharedVariantKey is the digest-map key used for a `shared` artifact: ONE
// digest serves every env. `variant` mode (deferred) would key by variant
// name ("staging", "prod"); the map shape is forward-compatible.
const sharedVariantKey = "*"

// Artifact kinds. A release spans more than container images, and the kind is
// what lets a verifier know HOW to check a given entry exists.
//
// WHY THIS EXISTS. Until this field, a Release could only describe OCI images,
// so every other artifact a release actually ships was invisible to it. That
// is not a theoretical gap: forge's own v0.1.12 release tagged
// `web-runtime/v0.3.1`, never published it to npm, and nothing noticed until a
// scaffolded project failed to install — because the release model had no way
// to say "this release also contains an npm package, at this version, with
// this integrity hash". A ledger that cannot NAME an artifact cannot verify it,
// and an unverifiable artifact is one that silently drifts.
//
// The kinds are open by design: an unknown kind round-trips through the ledger
// untouched, so an older forge reading a newer release does not corrupt it.
const (
	// ArtifactKindOCI is a container image in a registry, addressed by digest.
	ArtifactKindOCI = "oci"
	// ArtifactKindNPM is a package on an npm registry, addressed by version
	// plus the registry's integrity hash.
	ArtifactKindNPM = "npm"
	// ArtifactKindGoModule is a module on a Go module proxy, addressed by
	// version plus the go.sum hash.
	ArtifactKindGoModule = "gomod"
	// ArtifactKindFile is a file published to object storage or a CDN,
	// addressed by URL plus a content hash.
	ArtifactKindFile = "file"
)

// ReleaseArtifact is one artifact's resolved identity within a release.
//
// Two addressing shapes live here, discriminated by Kind, because the things a
// release ships are not all content-addressed the same way:
//
//   - OCI images are addressed by DIGEST alone — the digest IS the name, so
//     Digests carries it and Version/Integrity stay empty.
//   - Everything else (npm, Go modules, published files) is addressed by a
//     COORDINATE plus a hash: the version is how a consumer asks for it, and
//     the hash is how they know they got the right bytes.
//
// Kind is empty on ledgers written before kinds existed. Those are OCI by
// construction — it was the only thing a release could hold — so EffectiveKind
// reports them as such rather than forcing a migration of files on disk.
//
// MODE IS A SEPARATE AXIS FROM KIND and both are load-bearing. Kind says WHAT
// the artifact is (an image, an npm package, a file); Mode says how its
// identity is pinned — "shared" (one registry digest promoted byte-identical
// to every env) or "source" (a frontend built from a pinned COMMIT at deploy
// time, with no registry digest because there is no registry). An OCI artifact
// is Mode shared; a GitSource frontend is Mode source. Collapsing the two
// would make a source-built frontend unrepresentable, which is exactly how one
// gets left behind while every image around it advances.
const (
	// artifactModeShared is a container image: one registry digest,
	// promoted byte-identical to every env. Consumed by the deploy path
	// as `<image>@sha256:...`.
	artifactModeShared = "shared"
	// artifactModeSource is a frontend built FROM SOURCE at deploy time
	// rather than pulled as an image — a Firebase Hosting SPA declared
	// via forge.GitSource. There is no registry digest to pin because
	// there is no registry: the artifact's content-addressed identity is
	// the COMMIT the declared ref resolved to.
	artifactModeSource = "source"
)

// ReleaseArtifact is ONE thing a release froze, in the form the ledger
// records it on disk. It is the unit promotion moves: `forge env promote`
// copies these verbatim into an env binding, so whatever identity is
// captured here is exactly what that env deploys — nothing re-resolves a
// tag or a ref later.
//
// The identity lives in a different field per Kind, which is why Digests,
// Version/Integrity and Source coexist and are each individually optional.
// See the Kind and Mode doc blocks above for which axis means what; the
// per-field comments below say which kinds populate them.
type ReleaseArtifact struct {
	// Kind is one of the ArtifactKind* constants. Empty means OCI, for
	// ledgers cut before this field existed.
	Kind string `json:"kind,omitempty"`
	// Mode is "shared" (build once, one digest, all envs) for the MVP.
	// "variant" (per-env digests) is a documented follow-up, not yet produced.
	Mode string `json:"mode"`
	// Digests maps a variant key → canonical `sha256:...` digest. For a
	// shared artifact the only key is sharedVariantKey ("*"). OCI only.
	Digests map[string]string `json:"digests,omitempty"`
	// Platforms is the OS/arch set the captured manifest advertises
	// (e.g. ["linux/amd64"]). Informational for the MVP (single-arch amd64);
	// the deploy preflight inspects the live image's arch independently.
	Platforms []string `json:"platforms,omitempty"`
	// Source is the resolved cross-repo pin for a source-mode artifact:
	// the repo, the ref as DECLARED, and the commit that ref resolved to
	// at cut time. Nil for an image artifact.
	//
	// Both the ref and the commit are recorded because they answer
	// different questions. The ref is what the KCL declares and what a
	// human edits; the commit is what actually shipped. A ref alone is
	// not content-addressed — a moved tag silently changes what a release
	// means — so the commit is the half that makes the release auditable.
	Source *ReleaseSource `json:"source,omitempty"`

	// ── Non-OCI coordinates ──────────────────────────────────────────────
	// Version is how a consumer ASKS for this artifact: an npm version
	// ("0.3.1"), a Go module version ("v0.1.15"), a release label.
	Version string `json:"version,omitempty"`
	// Integrity is how a consumer KNOWS they got the right bytes, in the
	// hash format native to that ecosystem — npm's "sha512-…" integrity
	// string, a go.sum "h1:…" hash, or "sha256:…" for a published file.
	// Stored verbatim rather than normalised: a verifier compares it against
	// what the registry reports, and re-encoding it would break that compare.
	Integrity string `json:"integrity,omitempty"`
	// URI is the artifact's location when it is not implied by name+version:
	// a download URL for a file, or a non-default registry. Empty means the
	// ecosystem's default registry.
	URI string `json:"uri,omitempty"`
}

// ReleaseSource is the content-addressed identity of a source-built
// artifact: the git coordinates its bytes came from. It is the
// non-container analogue of an image digest.
type ReleaseSource struct {
	// Repo is the repository the source was fetched from.
	Repo string `json:"repo"`
	// Ref is the tag/branch/sha as DECLARED in KCL.
	Ref string `json:"ref"`
	// Subdir is the path within the repo the component builds from
	// ("web" for a repo whose SPA lives there). Empty means the root.
	Subdir string `json:"subdir,omitempty"`
	// Commit is the sha Ref resolved to when the release was cut. This is
	// the actual content address. Empty only when the fetcher could not
	// report one, which costs auditability rather than correctness.
	Commit string `json:"commit,omitempty"`
}

// SharedDigest returns the digest of a shared (container image) artifact and
// whether it was present. A source-mode artifact returns ("", false) — it is
// pinned by commit, not by registry digest, and has no image reference for a
// manifest to carry. A variant artifact (deferred) returns ("", false) too;
// its resolution is keyed by the target's variant_key, a follow-up.

// EffectiveKind reports the artifact's kind, treating an empty Kind as OCI.
//
// The default is not a guess: before Kind existed a Release could only hold
// image digests, so every artifact in a pre-kind ledger IS an OCI image. This
// keeps those files readable without rewriting them, which matters because a
// release ledger is immutable — migrating one in place would violate the
// property the whole model rests on.
func (a ReleaseArtifact) EffectiveKind() string {
	if a.Kind == "" {
		return ArtifactKindOCI
	}
	return a.Kind
}

// SharedDigest returns the digest of a shared OCI artifact (the only mode the
// MVP produces) and whether it was present. A variant artifact (deferred)
// returns ("", false) here — its resolution is keyed by the target's
// variant_key, a follow-up.
//
// A NON-OCI artifact always returns ("", false), and that is the guard that
// keeps kinds from leaking into the image path: an npm package has no digest
// to pin a container spec with, so a caller asking for one must get "no"
// rather than an empty string it might write into a pod. Callers pinning
// images therefore skip non-OCI artifacts for free, without every call site
// having to remember to check Kind.
func (a ReleaseArtifact) SharedDigest() (string, bool) {
	if a.EffectiveKind() != ArtifactKindOCI {
		return "", false
	}
	d, ok := a.Digests[sharedVariantKey]
	return d, ok && d != ""
}

// Release is the ledger written by `forge build --release <version>`. It is the
// unit of truth for "what bytes are v1.4.0": a version label on top, a
// content-addressed digest per image underneath. Immutable once cut — promotion
// advances it across envs BY REFERENCE (a binding), never by rebuild.
type Release struct {
	// Version is the human-readable label (semver, "v1.4.0") and the ledger
	// filename stem.
	Version string `json:"release"`
	// Git captures the source provenance of the build so a reviewer can tie a
	// release back to a commit. Best-effort (empty on a non-git tree).
	Git ReleaseGit `json:"git"`
	// CreatedAt is RFC3339 wall-clock. Informational across invocations.
	CreatedAt string `json:"created_at"`
	// Artifacts maps the bare image name (the key services match against
	// `svc.image`, e.g. "control-plane", "reliant") → its resolved identity.
	Artifacts map[string]ReleaseArtifact `json:"artifacts"`
}

// ReleaseGit is the source provenance recorded in a Release.
type ReleaseGit struct {
	Commit string `json:"commit,omitempty"`
	Tag    string `json:"tag,omitempty"`
	Dirty  bool   `json:"dirty,omitempty"`
}

// EnvReleases is the env→release binding ledger written by `forge env promote`. A
// binding is a pure pointer: env `<name>` runs release `<version>`. The
// resolved per-image digests are snapshotted alongside the version so a deploy
// can pin them without re-reading the (possibly moved/edited) release file, and
// so the binding is self-describing when a human peeks at it.
type EnvReleases struct {
	// Bindings maps env name → the release bound to it.
	Bindings map[string]EnvBinding `json:"bindings"`
}

// EnvBinding records that an env runs a specific release, with the per-image
// digests resolved at promote time. Resolving at promote (not deploy) time is
// what makes "the bytes that passed staging ARE the bytes in prod" a checkable
// invariant: the digests are frozen into the binding the moment the env is
// promoted.
type EnvBinding struct {
	// Release is the version label bound to this env (e.g. "v1.4.0").
	Release string `json:"release"`
	// Resolved maps the bare image name → the canonical `sha256:...` digest
	// this env will deploy. A snapshot of the release's shared digests at
	// promote time.
	Resolved map[string]string `json:"resolved"`
	// Sources maps a source-built frontend's name → the commit this env
	// will build it from. The non-container half of the same snapshot:
	// without it a promotion records only the images and the frontend
	// silently stays on whatever ref happens to be in KCL, which is how a
	// frontend gets left behind by a promotion that looked complete.
	Sources map[string]ReleaseSource `json:"sources,omitempty"`
	// PromotedAt is RFC3339 wall-clock — when this binding was written.
	PromotedAt string `json:"promoted_at"`
}

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

// envReleasesPath returns the absolute path to the env→release binding ledger.
func envReleasesPath(projectDir string) string {
	return filepath.Join(projectDir, envReleasesRel)
}

// WriteRelease persists a Release ledger. The directory is created lazily.
func WriteRelease(projectDir string, r Release) error {
	return statefile.Write(releasePath(projectDir, r.Version), "release", r)
}

// ReadRelease loads a Release ledger by version. Returns (nil, nil) when the
// file is missing — the caller decides whether that's an error (a deploy
// referencing an absent release) or a fall-through.
func ReadRelease(projectDir, version string) (*Release, error) {
	return statefile.Read[Release](releasePath(projectDir, version), "release")
}

// ReadEnvReleases loads the binding ledger. Returns a zero-value (non-nil)
// EnvReleases with an empty Bindings map when the file is missing, so callers
// can range/lookup without a nil guard.
func ReadEnvReleases(projectDir string) (*EnvReleases, error) {
	er, err := statefile.Read[EnvReleases](envReleasesPath(projectDir), "env-releases")
	if err != nil {
		return nil, err
	}
	if er == nil {
		er = &EnvReleases{}
	}
	if er.Bindings == nil {
		er.Bindings = map[string]EnvBinding{}
	}
	return er, nil
}

// WriteEnvReleases persists the binding ledger.
func WriteEnvReleases(projectDir string, er EnvReleases) error {
	return statefile.Write(envReleasesPath(projectDir), "env-releases", er)
}

// boundReleaseForEnv returns the release bound to env, or ("", false) when the
// env has no binding. This is the gate `forge env deploy <env>` uses to choose
// between the release-pinned digest path and today's per-env build-state path.
func boundReleaseForEnv(projectDir, envName string) (EnvBinding, bool, error) {
	er, err := ReadEnvReleases(projectDir)
	if err != nil {
		return EnvBinding{}, false, err
	}
	b, ok := er.Bindings[envName]
	return b, ok, nil
}

// The env→release lookup that used to live here (boundReleaseForEnv) is now
// bindingStore.Binding — see binding_store.go. Consumers ask a store, not a
// project directory, so the ledger's backing can change without touching them.
//
// ReadEnvReleases/WriteEnvReleases remain as the FILE backend's encoding: they
// are the frozen on-disk contract for .forge/env-releases.json, and
// fileBindingStore is their only production caller.

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
func harvestReleaseArtifacts(projectDir, envName string) map[string]ReleaseArtifact {
	out := map[string]ReleaseArtifact{}

	add := func(image, digest, registry string, platforms []string) {
		if image == "" || digest == "" {
			return
		}
		out[image] = ReleaseArtifact{
			// Stamped explicitly rather than left to EffectiveKind's default:
			// a ledger cut today should SAY what it holds, so a reader never
			// has to know that an absent kind once meant OCI.
			Kind:    ArtifactKindOCI,
			Mode:    "shared",
			Digests: map[string]string{sharedVariantKey: digest},
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
func addFrontendSourceArtifacts(ctx context.Context, projectDir string, entities *KCLEntities, out map[string]ReleaseArtifact) error {
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
func addFrontendSourceArtifactsWith(ctx context.Context, resolver pinResolver, entities *KCLEntities, out map[string]ReleaseArtifact) error {
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
		out[fe.Name] = ReleaseArtifact{
			Mode: artifactModeSource,
			Source: &ReleaseSource{
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
func checkReleaseCoversEnv(entities *KCLEntities, artifacts map[string]ReleaseArtifact, opts buildOptions) error {
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

// resolveReleaseSources flattens a release's source-mode artifacts into the
// frontend-name → pin map a promotion snapshots. Empty (not an error) for a
// release of a project that declares no cross-repo frontend.
func resolveReleaseSources(r Release) map[string]ReleaseSource {
	out := map[string]ReleaseSource{}
	for name, art := range r.Artifacts {
		if art.Mode == artifactModeSource && art.Source != nil {
			out[name] = *art.Source
		}
	}
	return out
}

// countOCIArtifacts returns how many of a release's artifacts are container
// images. Used to tell "this release ships no images" apart from "this
// release's images are all variant-mode", which are different problems with
// different answers.
func countOCIArtifacts(r Release) int {
	n := 0
	for _, art := range r.Artifacts {
		if art.EffectiveKind() == ArtifactKindOCI {
			n++
		}
	}
	return n
}

// releaseArtifactKinds returns the sorted distinct kinds present in a release,
// for error messages that name what a release actually holds rather than what
// it lacks.
func releaseArtifactKinds(r Release) []string {
	seen := map[string]bool{}
	for _, art := range r.Artifacts {
		seen[art.EffectiveKind()] = true
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
func releaseImageNames(r Release) []string {
	names := make([]string, 0, len(r.Artifacts))
	for name := range r.Artifacts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resolveReleaseDigests flattens a release's artifacts into the bare
// image-name → digest map the deploy/promote paths consume. Only shared
// artifacts resolve here (the MVP); a variant artifact is skipped with no
// error (its per-target resolution is a deferred follow-up). Returns an error
// only if the release carries no resolvable digests at all — a release that
// can't pin anything is a bug, not a silent fall-through.
func resolveReleaseDigests(r Release) (map[string]string, error) {
	out := map[string]string{}
	for image, art := range r.Artifacts {
		if d, ok := art.SharedDigest(); ok {
			out[image] = d
		}
	}
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
