// build_external.go is the SINGLE dispatcher for KCL Services whose
// effective build is a ShellBuild (`build = forge.ShellBuild { cmd, cwd,
// env }`) — the one shell escape hatch. Mirrors the deploytarget/External
// provider on the build side — same `sh -c` shape, same ${X} substitution,
// same fail-when-cwd-missing contract. See internal/buildtarget for the
// runner + spec types.
//
// The dispatcher's responsibilities are deliberately narrow:
//
//   - Iterate KCL services whose effective build is a ShellBuild
//     (EffectiveBuildCmd != "").
//   - Build a Spec from KCL fields + the build-loop's resolved tag /
//     registry / target arch.
//   - Run Spec through buildtarget.Runner.Build.
//   - Persist per-service state when the build succeeded so a
//     subsequent `forge deploy <env>` can pin the same tag.
//
// Build-side ownership boundary: the user's BuildCmd owns BOTH the
// build AND the push (the user composes
// `docker build … && docker push …` into one string). Forge does NOT
// run docker push afterwards. This matches the External (deploy)
// provider's "user owns the CLI" contract — one mental model across
// both escape hatches.
package cli

import (
	"context"
	"fmt"
	"runtime"
	"sync"

	"github.com/reliant-labs/forge/internal/buildtarget"
	"github.com/reliant-labs/forge/internal/config"
)

// externalImageDigestResolver resolves the content-addressed manifest digest
// (+ platforms) of a pushed image ref by querying the REGISTRY ONLY
// (imageRepoDigest → `docker buildx imagetools inspect`). This is load-bearing
// for the external / ShellBuild / remote-built path: there the user's
// build_cmd owns build AND push and the image may have been built on a remote
// builder (so the local docker daemon never holds it) OR the local daemon may
// hold a STALE image carrying the same `:tag` from an earlier, never-pushed
// build. A local-daemon RepoDigest read in that situation captures a digest
// that does NOT match what was pushed, and deploy would then pin
// `<image>@<stale-digest>` — shipping the wrong image or one the registry
// doesn't have (ImagePullBackOff, surfacing downstream as a deploy exit 1).
// There is therefore deliberately NO local-cache fallback and NO fall-through
// to a previously captured build-state digest: a registry miss records NO
// digest and deploy falls back to the mutable tag.
//
// It's a package var purely so tests can substitute a deterministic fake
// without shelling out to docker; production code never reassigns it.
var externalImageDigestResolver = imageRepoDigest

// kclHasExternalBuildService reports whether the KCL entity set
// contains any service whose effective build is a ShellBuild. Used by
// runBuild to decide whether to invoke the external-build dispatcher at
// all, and by `--target external` validation to fail loudly when no
// shell-build services are declared.
func kclHasExternalBuildService(e *KCLEntities) bool {
	if e == nil {
		return false
	}
	for _, s := range e.Services {
		if s.EffectiveBuildCmd() != "" {
			return true
		}
	}
	return false
}

// externalBuildServices returns the subset of KCL services whose
// effective build is a ShellBuild (EffectiveBuildCmd non-empty).
// Convenience wrapper so the dispatcher loop stays one-liner; also used
// by the `--target external` filter.
//
// Returns nil (not an empty slice) when no shell-build services are
// declared so the parallel/sequential dispatch loops can use the
// idiomatic `len(s) > 0` guard.
func externalBuildServices(e *KCLEntities) []ServiceEntity {
	if e == nil {
		return nil
	}
	var out []ServiceEntity
	for _, s := range e.Services {
		if s.EffectiveBuildCmd() != "" {
			out = append(out, s)
		}
	}
	return out
}

// buildExternalServices runs every KCL service with a non-empty
// BuildCmd through buildtarget.Runner. Returns one buildResult per
// service so the build-loop summary surfaces them alongside the Go
// binary / docker / frontend results.
//
// Concurrency: external builds run in parallel when opts.parallel is
// true, sequentially otherwise. Each service's BuildCmd is a separate
// `sh -c` invocation so there's no shared mutable state to coordinate.
//
// Skipped builds (build_cwd missing on disk) are recorded as a
// successful buildResult with kind "external-skip" so the summary
// shows them but they don't trip the failed/succeeded fan-out.
//
// State-file write: per the Phase 2 brief, every successful build
// writes .forge/state/build-<env>-<service>.json. The file is the
// single source of truth a subsequent `forge deploy <env>` reads to
// pin the image tag — eliminating the build/deploy tag divergence
// the External (deploy) provider already closes for the deploy side.
func buildExternalServices(ctx context.Context, cfg *config.ProjectConfig, services []ServiceEntity, opts buildOptions, registry, tag, projectDir, targetArch string, entities *KCLEntities) []buildResult {
	if len(services) == 0 {
		return nil
	}
	runner := buildtarget.NewRunner()
	resultCh := make(chan buildResult, len(services))

	// Pinned, mirrored base images from .forge/base-images.lock.json, exposed
	// to each external build_cmd as both ${BASE_<slug>} substitution tokens
	// and BASE_<slug> env vars (buildtarget merges BuildEnv into both). A
	// build_cmd that builds a Dockerfile pins its base via
	// `docker build --build-arg BASE_<slug>=${BASE_<slug>} …` — the same
	// single-source-of-truth override the in-forge docker paths get. Empty
	// when no base_images are declared / no lock; the build_cmd then relies on
	// the Dockerfile ARG defaults exactly as before.
	baseEnv := baseImageBuildEnv(cfg, projectDir)

	dispatch := func(svc ServiceEntity) {
		// Per-service tag: honor an explicit KCL per-service pin
		// (Service.image_tag, e.g. e2e's reliant_image_tag="e2e" or the
		// workspace-base "dev-per-daemon" build-only pin) first, then the
		// env's resolved tag for THIS service's image off the rendered
		// manifests, then the env-wide build-loop tag. This keeps the
		// ${TAG} a build_cmd interpolates equal to the tag the env's
		// deploy manifests reference for the SAME image — so the external
		// build pushes exactly what deploy pulls, even when different
		// external images carry different env tags.
		svcTag := tag
		if svc.ImageTag != "" {
			svcTag = svc.ImageTag
		} else if envTag := envImageTagFor(entities, svc.Image); envTag != "" {
			svcTag = envTag
		}
		spec := buildtarget.Spec{
			Service:    svc.Name,
			Image:      svc.Image,
			Tag:        svcTag,
			TargetArch: targetArch,
			Registry:   registry,
			ProjectDir: projectDir,
			Env:        opts.env,
			// The single shell hatch: EffectiveBuildCmd/Cwd/Env all read
			// off the service's effective ShellBuild (build = forge.
			// ShellBuild { cmd, cwd, env }). One source, one contract.
			BuildCmd: svc.EffectiveBuildCmd(),
			BuildCwd: svc.EffectiveBuildCwd(),
			// Merge the pinned base-image refs UNDER the service's own build
			// env so an explicit per-service BuildEnv key wins on conflict.
			BuildEnv: mergeBuildEnv(baseEnv, svc.EffectiveBuildEnv()),
		}
		fmt.Printf("[build] %s: ShellBuild (tag %s)\n", svc.Name, svcTag)
		res := runner.Build(ctx, spec)

		// Skip-with-warn: the runner returns Skipped=true when the
		// build_cwd is missing on disk. Surface a clear "skipped: X"
		// log line so the user sees why their build finished early
		// without a failure cluttering the summary.
		if res.Skipped {
			fmt.Printf("[build] %s: skipped (%s)\n", svc.Name, res.SkipMsg)
			resultCh <- buildResult{
				name:     svc.Name + " (external)",
				kind:     "external-skip",
				duration: res.Duration,
				err:      nil,
			}
			return
		}

		// Real failure — return as-is so the build summary's failed
		// list catches it and the outer runBuild returns non-zero.
		if res.Err != nil {
			resultCh <- buildResult{
				name:     svc.Name + " (external)",
				kind:     "external",
				duration: res.Duration,
				err:      res.Err,
			}
			return
		}

		// Capture the pushed image's content-addressed digest so deploy can
		// pin `<image>@sha256:...` (immutable, node-cache-proof) instead of the
		// mutable env tag — closing the external-build half of the digest gap:
		// the user's build_cmd owns build AND push, so forge resolves the
		// digest AFTER the command by querying the registry for the exact ref
		// it pushed (${REGISTRY}/${IMAGE}:${TAG}). Best-effort, same contract as
		// the docker PROJECT path: any lookup failure (local-only ref with no
		// registry manifest — the e2e workspace-base/reliant case — or an
		// unreachable registry) records no digest and deploy falls back to the
		// tag exactly as before. NEVER fails the build.
		pushedRef := externalPushedRef(registry, svc.Image, svcTag)
		digest, platforms := "", []string(nil)
		if d, p, derr := externalImageDigestResolver(ctx, pushedRef); derr == nil {
			digest, platforms = d, p
			fmt.Printf("[build] %s: pushed digest %s\n", svc.Name, digest)
		} else {
			fmt.Printf("[build]   Note: could not capture image digest for %s (%v); deploy will use the tag\n", pushedRef, derr)
		}

		// Success path: persist the per-service state file so
		// `forge deploy <env>` reads the exact tag forge build just
		// pushed. Non-fatal: a failed state write logs a warning but
		// the build itself stays successful (a future deploy will
		// fall back to git-derived tag resolution).
		state := buildtarget.State{
			Service:   svc.Name,
			Image:     svc.Image,
			Tag:       svcTag,
			Registry:  registry,
			PushedAt:  nowRFC3339(),
			Digest:    digest,
			Platforms: platforms,
		}
		if werr := buildtarget.WriteState(projectDir, opts.env, state); werr != nil {
			fmt.Printf("[build] %s: warning: failed to write build-state file: %v\n", svc.Name, werr)
		} else {
			fmt.Printf("[build] %s: wrote build state: %s\n", svc.Name, buildtarget.StatePath(projectDir, opts.env, svc.Name))
		}

		// Also write the deploy-side build-<env>.json so a subsequent
		// `forge deploy <env>` reuses this exact tag without --tag. The
		// per-service file above is consumed only by forge audit/doctor;
		// deploy's tag-resolution reads THIS single-per-env file
		// (build_state.go::buildStatePath, deploy.go:892). Closing this
		// gap is the external-build half of fr-e6dbce2a01 (build-state
		// was only written on --push, leaving external builds with no
		// deploy-readable tag). Non-fatal — deploy falls back to git
		// describe on a missing file. Last successful service wins; this
		// matches the single-file-per-env shape the --push path uses.
		deployState := BuildState{
			Image:    svc.Image,
			Tag:      svcTag,
			Registry: registry,
			// The user's build_cmd owns build AND push; we record the
			// registry coordinates but can't prove a push happened, so
			// Pushed stays false (the deploy-side tag read doesn't gate
			// on it — see BuildState.Pushed).
			PushedAt: nowRFC3339(),
			// Carry the digest into the deploy-readable aggregate too — this
			// is the file resolveDeployImageTag actually reads, so without it
			// the per-service capture above would never reach deploy and the
			// reliant/workspace-base images would still pin the mutable tag.
			Digest:    digest,
			Platforms: platforms,
		}
		if werr := WriteBuildState(projectDir, opts.env, deployState); werr != nil {
			fmt.Printf("[build] %s: warning: failed to write deploy build-state file: %v\n", svc.Name, werr)
		} else {
			fmt.Printf("[build] %s: wrote deploy build state: %s\n", svc.Name, buildStatePath(projectDir, opts.env))
		}
		resultCh <- buildResult{
			name:     svc.Name + " (external)",
			kind:     "external",
			duration: res.Duration,
			err:      nil,
		}
	}

	if opts.parallel {
		var wg sync.WaitGroup
		for _, svc := range services {
			wg.Add(1)
			go func(s ServiceEntity) {
				defer wg.Done()
				dispatch(s)
			}(svc)
		}
		wg.Wait()
	} else {
		for _, svc := range services {
			dispatch(svc)
		}
	}
	close(resultCh)
	results := make([]buildResult, 0, len(services))
	for r := range resultCh {
		results = append(results, r)
	}
	return results
}

// externalPushedRef reconstructs the image ref the external build_cmd was
// handed via ${REGISTRY}/${IMAGE}:${TAG} — the exact ref the user's command
// pushed — so the post-build digest lookup queries the same manifest. When
// registry is empty (a local build_cmd that tags `${IMAGE}:${TAG}` with no
// registry prefix, e.g. the e2e workspace-base/reliant images) we drop the
// `<registry>/` segment, matching the deploy-side External ${IMAGE}:${TAG}
// substitution. A local-only ref simply won't resolve a registry digest, so
// the best-effort lookup returns empty and deploy stays on the tag.
func externalPushedRef(registry, image, tag string) string {
	if registry == "" {
		return image + ":" + tag
	}
	return registry + "/" + image + ":" + tag
}

// resolveExternalBuildTargetArch picks the GOARCH for the ${TARGETARCH}
// substitution token used by external build_cmd scripts. Distinct
// helper from resolveBuildArch (which controls the Go-build env)
// because external builds delegate cross-compilation to the user's
// command — forge just hands them the target arch, the user's
// `docker buildx --platform=linux/${TARGETARCH}` does the work.
//
// Precedence (highest to lowest):
//
//  1. opts.targetArch (--target-arch flag)
//  2. cfgArch (resolved from KCL deploy.Cluster.Platform / forge.yaml deploy.target_arch)
//  3. runtime.GOARCH fallback
//
// Empty return is never useful for external builds — the substitution
// would expand to `--platform=linux/` which buildx rejects. So the
// fallback to runtime.GOARCH keeps the token always-resolvable.
func resolveExternalBuildTargetArch(cfgArch, flagArch string) string {
	if flagArch != "" {
		return flagArch
	}
	if cfgArch != "" {
		return cfgArch
	}
	return runtime.GOARCH
}
