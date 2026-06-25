package cli

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/buildtarget"
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
//	forge promote v1.4.0 --to prod → records env prod → release v1.4.0 in the
//	                                  binding ledger (.forge/env-releases.json).
//	                                  Pure pointer move, no rebuild.
//	forge deploy prod              → if prod is bound, pins the SAME digests
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

// ReleaseArtifact is one image's resolved content-addressed identity within a
// release. For the MVP every artifact is Mode "shared": Digests has exactly
// one entry under sharedVariantKey, promoted to every env. The map shape (not
// a bare string) is the seam for the deferred `variant` mode, where each env's
// variant_key maps to its own digest.
type ReleaseArtifact struct {
	// Mode is "shared" (build once, one digest, all envs) for the MVP.
	// "variant" (per-env digests) is a documented follow-up, not yet produced.
	Mode string `json:"mode"`
	// Digests maps a variant key → canonical `sha256:...` digest. For a
	// shared artifact the only key is sharedVariantKey ("*").
	Digests map[string]string `json:"digests"`
	// Platforms is the OS/arch set the captured manifest advertises
	// (e.g. ["linux/amd64"]). Informational for the MVP (single-arch amd64);
	// the deploy preflight inspects the live image's arch independently.
	Platforms []string `json:"platforms,omitempty"`
}

// SharedDigest returns the digest of a shared artifact (the only mode the MVP
// produces) and whether it was present. A variant artifact (deferred) returns
// ("", false) here — its resolution is keyed by the target's variant_key, a
// follow-up.
func (a ReleaseArtifact) SharedDigest() (string, bool) {
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

// EnvReleases is the env→release binding ledger written by `forge promote`. A
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
	// PromotedAt is RFC3339 wall-clock — when this binding was written.
	PromotedAt string `json:"promoted_at"`
}

// releasePath returns the absolute path to a release ledger file.
func releasePath(projectDir, version string) string {
	return filepath.Join(projectDir, releasesDirRel, statefile.SafeSegment(version)+".json")
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
// env has no binding. This is the gate `forge deploy <env>` uses to choose
// between the release-pinned digest path and today's per-env build-state path.
func boundReleaseForEnv(projectDir, envName string) (EnvBinding, bool, error) {
	er, err := ReadEnvReleases(projectDir)
	if err != nil {
		return EnvBinding{}, false, err
	}
	b, ok := er.Bindings[envName]
	return b, ok, nil
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
func harvestReleaseArtifacts(projectDir, envName string) map[string]ReleaseArtifact {
	out := map[string]ReleaseArtifact{}

	add := func(image, digest string, platforms []string) {
		if image == "" || digest == "" {
			return
		}
		out[image] = ReleaseArtifact{
			Mode:      "shared",
			Digests:   map[string]string{sharedVariantKey: digest},
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
		add(st.Image, st.Digest, st.Platforms)
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
			add(st.Image, st.Digest, st.Platforms)
		}
	}

	return out
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
		return nil, fmt.Errorf("release %q carries no shared image digests to pin", r.Version)
	}
	return out, nil
}
