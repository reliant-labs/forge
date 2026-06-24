// build_external.go wires KCL Services that declare `build_cmd` into
// the `forge build` loop. Mirrors the deploytarget/External provider
// on the build side — same `sh -c` shape, same ${X} substitution,
// same skip-when-cwd-missing contract. See internal/buildtarget for
// the runner + spec types.
//
// The dispatcher's responsibilities are deliberately narrow:
//
//   - Iterate KCL services whose Service.BuildCmd is non-empty.
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
)

// kclHasExternalBuildService reports whether the KCL entity set
// contains any service with a non-empty BuildCmd. Used by runBuild
// to decide whether to invoke the external-build dispatcher at all,
// and by `--target external` validation to fail loudly when no
// external-build services are declared.
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
// Service.BuildCmd is non-empty. Convenience wrapper so the dispatcher
// loop stays one-liner; also used by the `--target external` filter.
//
// Returns nil (not an empty slice) when no external-build services
// are declared so the parallel/sequential dispatch loops can use the
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
func buildExternalServices(ctx context.Context, services []ServiceEntity, opts buildOptions, registry, tag, projectDir, targetArch string, entities *KCLEntities) []buildResult {
	if len(services) == 0 {
		return nil
	}
	runner := buildtarget.NewRunner()
	resultCh := make(chan buildResult, len(services))

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
			// EffectiveBuildCmd resolves Service.build_cmd (generic
			// escape hatch) before falling back to External.build_cmd
			// (the build-side mirror of deploy_cmd). EffectiveBuildEnv
			// mirrors that precedence so an External target's `env` map
			// feeds the substitution + process env when build_cmd lives
			// on the External block.
			BuildCmd: svc.EffectiveBuildCmd(),
			BuildCwd: svc.BuildCwd,
			BuildEnv: svc.EffectiveBuildEnv(),
		}
		fmt.Printf("[build] %s: external build_cmd (tag %s)\n", svc.Name, svcTag)
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

		// Success path: persist the per-service state file so
		// `forge deploy <env>` reads the exact tag forge build just
		// pushed. Non-fatal: a failed state write logs a warning but
		// the build itself stays successful (a future deploy will
		// fall back to git-derived tag resolution).
		state := buildtarget.State{
			Service:  svc.Name,
			Image:    svc.Image,
			Tag:      svcTag,
			Registry: registry,
			PushedAt: nowRFC3339(),
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
