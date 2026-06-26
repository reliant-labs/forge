package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/reliant-labs/forge/internal/baseimage"
	"github.com/reliant-labs/forge/internal/config"
)

// baseImageDigestResolver is the resolver `forge build --repin-bases` uses to
// look up each declared base's index digest THROUGH the mirror. It defaults to
// the docker-buildx-imagetools resolver (registry read, multi-arch index
// digest); a package var only so tests can substitute a deterministic fake
// without a docker daemon. Production never reassigns it.
var baseImageDigestResolver baseimage.DigestResolver = baseimage.DockerImagetoolsResolver{}

// repinBaseImages re-resolves every declared base image's index digest through
// the mirror and writes .forge/base-images.lock.json. Driven by
// `forge build --repin-bases`. Fail-fast: a tag that won't resolve aborts the
// re-pin (the resolver names it) rather than writing a partial lock. No-op
// (with a hint) when no base_images are declared.
func repinBaseImages(ctx context.Context, cfg *config.ProjectConfig, projectDir string) error {
	d := cfg.Docker.BaseImages.Declared()
	if len(d.Tags) == 0 {
		fmt.Println("[build] --repin-bases: no docker.base_images declared in forge.yaml; nothing to pin.")
		return nil
	}
	fmt.Printf("[build] --repin-bases: resolving %d base image(s) through mirror %s\n", len(d.Tags), d.MirrorPrefix)
	lk, err := baseimage.Repin(ctx, d, baseImageDigestResolver)
	if err != nil {
		return fmt.Errorf("repin base images: %w", err)
	}
	path, werr := baseimage.WriteLock(projectDir, lk)
	if werr != nil {
		return werr
	}
	for _, e := range lk.Entries {
		fmt.Printf("[build]   %-28s → %s\n", e.Tag, e.Ref)
	}
	fmt.Printf("[build] --repin-bases: wrote %s (commit it)\n", path)
	return nil
}

// errStaleBaseImages is returned by enforceBaseImagesFresh when the committed
// base-image lock is out of sync with forge.yaml docker.base_images and the
// build was not invoked with --force-stale-bases. Sentinel so tests can assert
// the failure class with errors.Is, independent of the (intentionally verbose)
// message text.
var errStaleBaseImages = errors.New("base-images lock out of sync with forge.yaml")

// enforceBaseImagesFresh gates a build on the committed base-image lock being
// in sync with forge.yaml docker.base_images. It runs ONCE, up front in
// runBuild, before any build work.
//
// When base_images are declared, a lock IS present, and its tag set / mirror
// no longer match the declaration, the build would inject STALE pins — images
// shipped off base refs that may have pending security or bug-fix updates. By
// default that's a HARD error directing the user to `forge build --repin-bases`
// (or --force-stale-bases to ship anyway), because a warning printed mid-build
// is easily scrolled past and the stale ship lands silently.
//
// It deliberately does NOT error on:
//   - no base_images declared (feature off),
//   - a MISSING lock (not yet pinned — builds fall back to the Dockerfile ARG
//     defaults, which are themselves pinned and correct, just not centrally
//     overridden). This case prints a one-time hint, matching the prior
//     behaviour; it is not drift, it's "feature declared but not yet pinned".
//
// With force=true the same drift only prints a loud warning and proceeds, so
// the escape hatch is observable in the build log.
func enforceBaseImagesFresh(cfg *config.ProjectConfig, projectDir string, force bool) error {
	d := cfg.Docker.BaseImages.Declared()
	if len(d.Tags) == 0 {
		return nil
	}
	lk, err := baseimage.ReadLock(projectDir)
	if err != nil {
		return err
	}
	if lk == nil {
		fmt.Printf("[build]   Note: docker.base_images declared but %s is missing; "+
			"builds use the Dockerfile ARG defaults. Run `forge build --repin-bases` to pin.\n",
			baseimage.LockRel)
		return nil
	}
	if lk.LockMatchesDeclared(d) {
		return nil
	}
	if force {
		fmt.Printf("[build]   WARNING: %s is out of sync with forge.yaml docker.base_images "+
			"(declared tag set changed or mirror moved); --force-stale-bases set, building with STALE pins. "+
			"Run `forge build --repin-bases` to refresh.\n", baseimage.LockRel)
		return nil
	}
	return fmt.Errorf("%w: %s is out of sync with forge.yaml docker.base_images "+
		"(declared tag set changed or mirror moved) — run `forge build --repin-bases` to update, "+
		"or pass --force-stale-bases to proceed with stale pins", errStaleBaseImages, baseimage.LockRel)
}

// baseImageBuildArgs loads the committed base-image lock and returns the
// `--build-arg`-shaped strings forge build injects so every image build pulls
// the pinned, mirrored base. Staleness is enforced up front by
// enforceBaseImagesFresh (a build that reaches here past a stale lock did so
// only via --force-stale-bases), so this function no longer re-warns. The
// injection itself is always driven by the LOCK (the committed source of
// truth), never by re-resolving during a normal build — a build must never
// depend on registry reachability for digests it already pinned.
//
// Returns nil args when no base_images are declared (feature off) or when the
// lock is absent (not yet pinned) — in both cases builds fall back to the
// Dockerfile ARG defaults, which are themselves pinned, so the build is still
// correct, just not centrally overridden.
func baseImageBuildArgs(cfg *config.ProjectConfig, projectDir string) ([]string, error) {
	d := cfg.Docker.BaseImages.Declared()
	if len(d.Tags) == 0 {
		return nil, nil
	}
	lk, err := baseimage.ReadLock(projectDir)
	if err != nil {
		return nil, err
	}
	if lk == nil {
		return nil, nil
	}
	args := lk.BuildArgs()
	if len(args) > 0 {
		fmt.Printf("[build]   Base images: pinning %d base(s) from lock via --build-arg\n", len(args))
	}
	return args, nil
}

// baseArgsCache memoizes baseImageBuildArgs per projectDir so the lock is read
// (and its staleness Note printed) ONCE per build even though every docker
// invocation — project image, each frontend, each KCL DockerBuild service —
// calls appendBaseImageBuildArgs. The build runs docker invocations
// concurrently (buildParallel), so the cache is mutex-guarded. Keyed by
// projectDir; in practice every call in one build passes the same root.
var (
	baseArgsMu    sync.Mutex
	baseArgsCache = map[string][]string{}
)

// appendBaseImageBuildArgs extends dockerArgs with one `--build-arg
// BASE_<slug>=<mirror-ref>@<digest>` per pinned base in the committed lock, so
// every `docker build` forge runs overrides the Dockerfile's ARG default with
// the centrally-pinned ref (single source of truth). It mirrors
// appendBuildContexts: same call shape, called right beside it at every docker
// invocation, so the project image, frontends, and KCL DockerBuild services
// are all pinned uniformly.
//
// No-ops (returns dockerArgs unchanged) when no base_images are declared or
// the lock is absent — the Dockerfile ARG defaults (themselves pinned) carry
// the build. A lock read error is logged and treated as "no injection" rather
// than failing the build: a malformed lock should be a loud warning a re-pin
// fixes, not a hard build stop. projectRoot follows appendBuildContexts'
// convention ("" means the cwd, which is the project root by construction).
func appendBaseImageBuildArgs(dockerArgs []string, cfg *config.ProjectConfig, projectRoot string) []string {
	if projectRoot == "" {
		projectRoot = "."
	}
	baseArgsMu.Lock()
	defer baseArgsMu.Unlock()
	args, ok := baseArgsCache[projectRoot]
	if !ok {
		var err error
		args, err = baseImageBuildArgs(cfg, projectRoot)
		if err != nil {
			fmt.Printf("[build]   Note: could not load base-image lock (%v); using Dockerfile ARG defaults. "+
				"Run `forge build --repin-bases` to refresh.\n", err)
			args = nil
		}
		baseArgsCache[projectRoot] = args
	}
	for _, a := range args {
		dockerArgs = append(dockerArgs, "--build-arg", a)
	}
	return dockerArgs
}

// baseImageBuildEnv returns the pinned base-image refs as a BASE_<slug> → ref
// map for injection into an external build_cmd's environment + substitution
// tokens (buildtarget merges BuildEnv into both). Shares the memoized lock
// read with appendBaseImageBuildArgs so the staleness Note prints once.
// Returns nil when no base_images are declared or no lock is present.
func baseImageBuildEnv(cfg *config.ProjectConfig, projectDir string) map[string]string {
	if projectDir == "" {
		projectDir = "."
	}
	baseArgsMu.Lock()
	args, ok := baseArgsCache[projectDir]
	if !ok {
		var err error
		args, err = baseImageBuildArgs(cfg, projectDir)
		if err != nil {
			fmt.Printf("[build]   Note: could not load base-image lock (%v); external build_cmds "+
				"see no BASE_* tokens. Run `forge build --repin-bases` to refresh.\n", err)
			args = nil
		}
		baseArgsCache[projectDir] = args
	}
	baseArgsMu.Unlock()
	if len(args) == 0 {
		return nil
	}
	env := make(map[string]string, len(args))
	for _, kv := range args {
		// args are "KEY=VALUE"; split on the FIRST '=' (values are
		// registry refs with no '=' but be defensive).
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				env[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return env
}

// mergeBuildEnv overlays `over` onto `base`, returning a new map where keys in
// `over` win. Used to layer a service's explicit BuildEnv on top of the
// injected BASE_<slug> refs. Returns nil only when BOTH are empty so the
// runner's `len(env) > 0` guard stays meaningful.
func mergeBuildEnv(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}
