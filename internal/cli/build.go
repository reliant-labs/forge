package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
)

// sortedKeys returns map keys in deterministic order. Used so docker
// build args are stable across runs (relevant for layer caching).
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resolveBuildContext normalises a forge.yaml `docker.build_contexts`
// value into the exact string `docker buildx --build-context name=…`
// expects.
//
// Scheme-bearing values (anything containing `://`, e.g.
// `docker-image://my-base:latest`, `oci-layout://./layout`,
// `https://example.com/foo.tar`) are passed through verbatim — buildkit
// owns the interpretation and forge has no business rewriting it.
//
// Absolute paths are also passed through unchanged.
//
// Relative paths are resolved against the project root (the directory
// holding forge.yaml — the build commands run with cwd at the project
// root, so an empty projectRoot is treated as "."). Resolving to an
// absolute path means downstream `docker build` invocations work
// regardless of which subdirectory the user actually launched forge
// from and survives the working-directory churn inside test harnesses.
func resolveBuildContext(value, projectRoot string) string {
	if strings.Contains(value, "://") {
		return value
	}
	if filepath.IsAbs(value) {
		return value
	}
	if projectRoot == "" {
		projectRoot = "."
	}
	return filepath.Join(projectRoot, value)
}

// appendBuildContexts extends dockerArgs with one `--build-context
// name=value` pair per entry in cfg.Docker.BuildContexts, in a
// deterministic order. Each value is run through resolveBuildContext so
// relative paths land as absolute paths against projectRoot and
// scheme-bearing values pass through unchanged. A per-context log line
// is emitted so users can confirm what buildx will see — useful when
// debugging a Dockerfile that fails to find a `COPY --from=name`.
//
// No-ops when cfg.Docker.BuildContexts is empty, so existing projects
// see no change in behaviour or output.
func appendBuildContexts(dockerArgs []string, cfg *config.ProjectConfig, projectRoot string) []string {
	if len(cfg.Docker.BuildContexts) == 0 {
		return dockerArgs
	}
	for _, name := range sortedKeys(cfg.Docker.BuildContexts) {
		value := resolveBuildContext(cfg.Docker.BuildContexts[name], projectRoot)
		dockerArgs = append(dockerArgs, "--build-context", name+"="+value)
		fmt.Printf("[build] docker build-context %s=%s\n", name, value)
	}
	return dockerArgs
}

// buildOptions holds the flag values for the build command.
type buildOptions struct {
	outputDir   string
	buildTarget string
	parallel    bool
	buildDocker bool
	debug       bool
	// pushRegistry, when non-empty, retags built docker images to
	// <registry>/<name>:<tag> and pushes them after build. Implies
	// --docker so users don't have to pass both flags.
	pushRegistry string
	// targetArch overrides the GOARCH used for the Go binary build
	// AND the docker buildx --platform when --docker / --push is set.
	// Empty means "use host arch for plain go build; use forge.yaml
	// deploy.target_arch (default amd64) for docker builds". See
	// resolveBuildArch.
	targetArch string
	// env, when set, scopes the build to a specific deploy environment.
	// Reads `deploy/kcl/<env>/` to determine which services run as host
	// processes (deploy: "host" in the rendered KCL) and excludes them
	// from the docker build/push. The Go binary itself still compiles
	// every service — the host/cluster split is a runtime placement
	// decision, not a code one. Empty (the default) means "build
	// everything", preserving the pre-orchestration behaviour so CI
	// builds for staging/prod aren't affected.
	env string
	// skipFrontends, when true, drops every frontend from the build set
	// regardless of deploy type. Set by `forge up`'s build phase because
	// up dev-serves frontends via `npm run dev` (in upFrontends) and
	// never consumes the `npm run build` prod artifact. Saves the entire
	// Next.js prod build time on every `forge up` cycle. Direct
	// `forge build` callers leave this false to preserve prod-build
	// behaviour. Independent of the Frontend.deploy-discriminator
	// filter (which is a no-op until forge.Frontend gets a deploy field).
	skipFrontends bool
	// tag, when set, overrides the git-derived image tag computed by
	// resolveImageTag. CI pipelines that pin the image to a release
	// number (e.g. `--tag v1.2.3`) use this. Empty (the default) means
	// "compute from git" — the same resolution `forge deploy` falls
	// back to when no build-state file is present.
	tag string
	// skipGenerate disables the pre-build "ensure generated code" step
	// (--no-generate). The default (false) auto-runs `forge generate`
	// when gen/ is missing or proto is newer than the generated tree, so
	// a fresh checkout doesn't fail with the go.work "cannot load module
	// gen" error. Set it when the generated tree is known-good and the
	// caller wants to skip the staleness scan (e.g. a CI lane that runs
	// generate as its own step). See ensureGeneratedCode.
	skipGenerate bool
}

func newBuildCmd() *cobra.Command {
	var opts buildOptions

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build the project binary and frontends",
		Long: `Build the project binary and frontends.

This command will:
- Build the single Go binary from ./cmd (CGO_ENABLED=0, stripped)
- Build Next.js frontends (npm run build)
- Optionally build Docker images (--docker)
- Output binaries to the specified output directory

Examples:
  forge build                                # Build everything
  forge build -t web                         # Build only the "web" frontend
  forge build -o bin                         # Output binaries to bin/
  forge build --docker                       # Also build Docker images
  forge build --debug                        # Build with debug symbols for Delve
  forge build --push ghcr.io/acme            # Build + retag + docker push to a registry
  forge build --push localhost:5051          # k3d: auto-mirrors to registry.localhost:5051

For k3d clusters, --push localhost:<port> also tags the image as
registry.localhost:<port>/<name> (LOCAL alias only — the host can't
DNS-resolve registry.localhost, so we don't push it; the containerd
mirror config inside k3d resolves the manifest reference at pull time).
This lets deployed manifests reference the in-cluster-resolvable name
without forcing the user to add /etc/hosts entries on the host.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := requireFeature(config.FeatureBuild); err != nil {
				return err
			}
			// --push implies --docker so users don't have to pass both.
			if opts.pushRegistry != "" {
				opts.buildDocker = true
			}
			return runBuild(cmd.Context(), opts)
		},
	}

	cmd.Flags().StringVarP(&opts.outputDir, "output", "o", "bin", "Output directory for binaries")
	cmd.Flags().StringVarP(&opts.buildTarget, "target", "t", "all", "Build target (all | external | a specific service/frontend name). `external` builds only the KCL services declaring build_cmd; requires --env.")
	cmd.Flags().BoolVar(&opts.parallel, "parallel", true, "Build services in parallel")
	cmd.Flags().BoolVar(&opts.buildDocker, "docker", false, "Build Docker images for all services")
	cmd.Flags().BoolVar(&opts.debug, "debug", false, "Build with debug symbols for Delve")
	cmd.Flags().StringVar(&opts.pushRegistry, "push", "", "Push docker images to this registry after build (implies --docker)")
	cmd.Flags().StringVar(&opts.targetArch, "target-arch", "", "Override target GOARCH for cross-compilation (default: forge.yaml deploy.target_arch, then amd64 for docker builds)")
	cmd.Flags().StringVar(&opts.env, "env", "", "Deploy environment (e.g. dev, staging, prod). When set, services declared `deploy: host` in deploy/kcl/<env>/ are excluded from docker build/push (the Go binary still includes their code).")
	cmd.Flags().StringVar(&opts.tag, "tag", "", "Override the image tag (default: git describe --tags --always --dirty). Persisted to .forge/state/build-<env>.json when --push succeeds so forge deploy uses the same value.")
	cmd.Flags().BoolVar(&opts.skipGenerate, "no-generate", false, "Skip the pre-build code-generation check. By default `forge build` runs `forge generate` when gen/ is missing or proto sources are newer than the generated tree.")

	return cmd
}

// resolveBuildArch chooses the GOARCH for `go build`. The arg-shaped
// signature decouples the three knobs that compose the answer:
//
//   - cfgArch: forge.yaml deploy.target_arch (project-level pin)
//   - flagArch: --target-arch (per-invocation override)
//   - dockerCtx: whether the caller is building a docker image (in
//     which case we always cross-compile to the deploy-target arch
//     since the image is destined for a k8s node, not the dev host)
//
// Returns the empty string when no cross-compile is needed (i.e.
// the resolved target equals runtime.GOARCH). The empty return is
// the signal buildGoBinary uses to skip the GOOS/GOARCH/CGO_ENABLED
// env override.
//
// Rule of thumb: a plain `forge build` (no docker) defaults to host
// arch — the user wants a runnable local binary. `forge build
// --docker` (or --push) defaults to forge.yaml deploy.target_arch
// (or "amd64" when unset) since the resulting image will be pulled
// by kubelet on a node whose arch is fixed at cluster-build time.
func resolveBuildArch(cfgArch, flagArch string, dockerCtx bool) string {
	target := flagArch
	if target == "" && dockerCtx {
		target = cfgArch
		if target == "" {
			target = "amd64"
		}
	}
	if target == "" || target == runtime.GOARCH {
		return ""
	}
	return target
}

type buildResult struct {
	name     string
	kind     string // "service", "frontend", or "docker"
	duration time.Duration
	err      error
}

func runBuild(ctx context.Context, opts buildOptions) error {
	store, err := loadProjectStore()
	if err != nil {
		return err
	}
	cfg := store.Config()

	// Ensure generated code exists / is fresh before any `go build`.
	// Missing gen/ (gitignored, or freshly cleaned) otherwise fails with
	// the cryptic "cannot load module gen listed in go.work" error. Gated
	// on staleness so the steady-state loop pays nothing; --no-generate
	// opts out. See ensureGeneratedCode.
	if err := ensureGeneratedCode(projectDirForKCL(), opts.skipGenerate); err != nil {
		return err
	}

	// Resolve the docker image tag once, up front. Both the docker
	// build/push path below and the post-push build-state write consume
	// this; resolving once guarantees the tag the user sees printed
	// equals the tag that lands in .forge/state/build-<env>.json and the
	// tag that subsequent `forge deploy` reads back. Override priority
	// matches the deploy side: --tag flag > resolveImageTag from git.
	resolvedTag := opts.tag
	if resolvedTag == "" && opts.buildDocker {
		// Only resolve when we'll actually use a tag — avoids surfacing
		// "not a git repo" errors on a plain `forge build` (no docker).
		t, terr := resolveImageTag(ctx, opts.env)
		if terr != nil {
			return fmt.Errorf("resolve image tag: %w (pass --tag to override)", terr)
		}
		resolvedTag = t
	}

	// Resolve the EMBEDDED build version once, up front, so the host
	// binary and the docker image stamp the identical version for this
	// build. Override priority: --tag > forge.yaml build.version > "".
	// This is distinct from resolvedTag (the IMAGE tag) — the embedded
	// version follows build.version, the image tag follows resolveImageTag.
	// They coincide when --tag is set (--tag pins both) but diverge when
	// the tag is git-derived and build.version is unset.
	versionOverride := opts.tag
	if versionOverride == "" {
		versionOverride = cfg.Build.Version
	}
	resolvedVersion := resolveBuildVersion(ctx, versionOverride)

	fmt.Printf("[build] Building project: %s\n", cfg.Name)
	fmt.Printf("[build]   Output:   %s\n", opts.outputDir)
	fmt.Printf("[build]   Target:   %s\n", opts.buildTarget)
	fmt.Printf("[build]   Parallel: %v\n", opts.parallel)
	fmt.Printf("[build]   Docker:   %v\n", opts.buildDocker)
	if opts.env != "" {
		fmt.Printf("[build]   Env:      %s\n", opts.env)
	}
	if opts.buildDocker {
		fmt.Printf("[build]   Tag:      %s\n", resolvedTag)
	}

	// When --env is set, read the rendered KCL to drive the docker-skip
	// set, the per-service platform override, and the build-only variant
	// builds. Missing KCL render is logged and treated as "no env filter"
	// so projects that haven't migrated to the deploy module yet keep
	// working unchanged.
	var entities *KCLEntities
	if opts.env != "" {
		projectDir := projectDirForKCL()
		ents, kerr := RenderKCL(ctx, projectDir, opts.env)
		if kerr != nil {
			fmt.Printf("[build]   Note: skipping KCL filter (%v)\n", kerr)
		} else {
			entities = ents
			summarizeKCLBuildPlan(entities)
		}
	}
	fmt.Println()

	// Create output directory
	if err := os.MkdirAll(opts.outputDir, 0o755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Filter targets — the Go binary is always built (single binary).
	// The target flag only filters frontends now.
	frontends := cfg.Frontends
	buildBinary := true

	// `stack.frontend.framework: none` means the project has no frontend
	// build toolchain forge should drive — drop every declared frontend
	// from the build set BEFORE anything runs `npm run build`. Without
	// this, a project that set framework:none (often because deps aren't
	// installed / the frontend builds out-of-band) still had forge run
	// `npm run build`, and a failure there (e.g. `next: command not
	// found`) failed the WHOLE build, blocking an unrelated deployable Go
	// service that compiled fine (fr-cc10bfab0c). Logged, not silent, so
	// the user can see why their frontend wasn't built. The frontends stay
	// in cfg.Frontends for non-build commands (generate, up's dev serve).
	if frontendsSkippedByFramework(cfg) {
		fmt.Printf("[build]   Skipping %d frontend(s): stack.frontend.framework is \"none\"\n", len(frontends))
		frontends = nil
	}

	// `--target external` is the explicit "build ONLY the KCL services
	// with build_cmd" filter. Useful for the cp-forge pattern where the
	// sibling-repo binary changes faster than the project binary, so the
	// user wants to iterate the external-build leg without rebuilding
	// the whole project image / frontends. Requires --env so we have a
	// rendered KCL set to filter against.
	if opts.buildTarget == "external" {
		// No experimental gate here: `build_cmd` is the build-side mirror
		// of External's `deploy_cmd`, and `forge deploy` of an External
		// target needs no opt-in. Gating build behind
		// features.experimental.external_builds while deploy ran free left
		// the build/deploy pair of the SAME target with mismatched maturity
		// gates (fr-da9a6614fb) — you could deploy an external target but
		// not build it. The gates are unified by retiring the build-side
		// one. The config key is still accepted (back-compat) but no longer
		// governs whether build_cmd runs.
		if opts.env == "" {
			return fmt.Errorf("--target external requires --env to know which KCL services to build")
		}
		if !kclHasExternalBuildService(entities) {
			return fmt.Errorf("--target external: no KCL services declare build_cmd in env %q", opts.env)
		}
		// Skip everything else — only the external dispatcher runs.
		frontends = nil
		buildBinary = false
		opts.skipFrontends = true
	} else if opts.buildTarget != "all" {
		frontends = filterFrontends(frontends, opts.buildTarget)
		if len(frontends) == 0 {
			// Not a frontend name — check if it matches the project name (binary)
			if opts.buildTarget != cfg.Name {
				return fmt.Errorf("target %q not found in project config", opts.buildTarget)
			}
		} else {
			// Target is a frontend, skip binary build
			buildBinary = false
		}
	}

	// `forge up` skips frontend prod builds entirely. Its frontend phase
	// (upFrontends) dev-serves via `npm run dev` and never consumes the
	// prod artifact. Set explicitly by upBuildCluster.
	if opts.skipFrontends {
		if len(frontends) > 0 {
			fmt.Printf("[build]   Skipping %d frontend(s): forge up dev-serves frontends\n", len(frontends))
		}
		frontends = nil
	}

	// KCL-driven prod-build skip for host-mode frontends. Host-mode
	// frontends only ever run via `npm run dev` (the dev loop in
	// `forge up`); they never consume the `npm run build` artifact, so
	// running the full Next.js prod build is wasted minutes. Skip them
	// from `frontends` (the input to buildFrontend → `npm run build`)
	// while keeping their entry in cfg.Frontends so other commands
	// (forge generate, forge up's frontend phase) see them unchanged.
	//
	// Frontends without a Deploy block (legacy KCL that doesn't emit
	// frontend deploy yet) fall through to "build" — preserving the
	// pre-discriminator behaviour so projects upgrade lazily.
	if entities != nil {
		frontends = filterFrontendsForBuild(frontends, entities)
	}

	// KCL-driven docker skip: with --env set, frontends that ship as
	// images (cluster / external / compose deploy) still need a docker
	// build; host-mode frontends and the legacy "no deploy block" case
	// stay docker-free. Also skip the project docker build when every
	// declared service is host or build-only (no cluster service in
	// this env → no image to push to the cluster).
	dockerFrontends := frontends
	skipProjectDocker := false
	if entities != nil {
		dockerFrontends = nil
		if !kclHasClusterService(entities) {
			skipProjectDocker = true
		}
	}

	// Per-env platform override from KCL: use the first cluster
	// service's deploy.Cluster.Platform when set. KCL renders all cluster
	// services in one env onto the same node arch in practice (one
	// project image, one Application set), so picking the first non-empty
	// platform is a clean default. Falls back to forge.yaml's
	// deploy.target_arch otherwise.
	cfgArchForDocker := cfg.Deploy.TargetArch
	if entities != nil {
		if p := kclFirstClusterPlatform(entities); p != "" {
			cfgArchForDocker = p
		}
	}

	start := time.Now()
	var results []buildResult

	if opts.parallel {
		results = buildParallel(ctx, cfg, frontends, dockerFrontends, buildBinary, skipProjectDocker, cfgArchForDocker, resolvedTag, resolvedVersion, opts)
	} else {
		results = buildSequential(ctx, cfg, frontends, dockerFrontends, buildBinary, skipProjectDocker, cfgArchForDocker, resolvedTag, resolvedVersion, opts)
	}

	// Build-only variants from KCL: each declared variant produces one
	// `bin/<service>-<variant>` binary with the variant's ldflags + build
	// tags. No docker build; this is the lane for sidecar binaries
	// shipped in a release artifact, not container images.
	if entities != nil {
		results = append(results, buildKCLBuildOnlyVariants(ctx, entities, opts.outputDir)...)
	}

	// External-build dispatcher: services whose KCL declares
	// `build_cmd` get their image constructed by a user-supplied shell
	// command rather than forge's built-in Go-build + docker-build
	// pipeline. Mirrors the deploytarget/External provider on the build
	// side. Runs after Go/docker/variant builds so a failing project
	// build doesn't waste time on the (likely orthogonal) external
	// services — but does NOT short-circuit on docker failures because
	// the external builds may target a different registry / pipeline.
	//
	// Skip-with-warn semantics live in the runner: a missing build_cwd
	// produces a "skipped: …" log line and an external-skip result that
	// the summary surfaces but doesn't count as a failure.
	if entities != nil {
		externalSvcs := externalBuildServices(entities)
		// No experimental gate: build_cmd is the build-side mirror of
		// External's deploy_cmd (which needs no opt-in), so a service that
		// declares build_cmd just builds. See the --target external branch
		// above for the rationale (fr-da9a6614fb).
		if len(externalSvcs) > 0 {
			externalRegistry := opts.pushRegistry
			if externalRegistry == "" {
				externalRegistry = cfg.Docker.Registry
			}
			externalTag := resolvedTag
			if externalTag == "" {
				// External-build dispatchers need a stable tag even when the
				// caller didn't pass --tag (the user's command interpolates
				// ${TAG} into `docker push <reg>/<img>:${TAG}` and an empty
				// tag would push :latest accidentally). Resolve the same
				// git-describe tag the docker path would have used.
				t, terr := resolveImageTag(ctx, opts.env)
				if terr != nil {
					return fmt.Errorf("external build: resolve image tag: %w (pass --tag to override)", terr)
				}
				externalTag = t
			}
			externalArch := resolveExternalBuildTargetArch(cfgArchForDocker, opts.targetArch)
			projDir := projectDirForKCL()
			results = append(results, buildExternalServices(ctx, externalSvcs, opts, externalRegistry, externalTag, projDir, externalArch)...)
		}
	}

	// Check for errors
	var failed []buildResult
	var succeeded []buildResult
	for _, r := range results {
		if r.err != nil {
			failed = append(failed, r)
		} else {
			succeeded = append(succeeded, r)
		}
	}

	// Persist build state on ANY successful project docker build — not
	// just --push. The state file is the build→deploy tag handoff, which
	// every transport needs (scp/compose deploy a local image just as much
	// as a registry deploy). Pushing only adds the registry coordinates;
	// it is not what makes the handoff worth recording. Skipped only when
	// no docker image was built (host-only env / no --docker / no
	// Dockerfile) or the docker build failed.
	if opts.buildDocker && resolvedTag != "" && !skipProjectDocker {
		projectDockerSucceeded := false
		for _, r := range succeeded {
			if r.kind == "docker" && r.name == cfg.Name+" (docker)" {
				projectDockerSucceeded = true
				break
			}
		}
		if projectDockerSucceeded {
			commit, gitTag, dirty := gitBuildProvenance(ctx)
			state := BuildState{
				Image:    cfg.Name,
				Tag:      resolvedTag,
				Registry: opts.pushRegistry,
				Pushed:   opts.pushRegistry != "",
				Commit:   commit,
				GitTag:   gitTag,
				Dirty:    dirty,
				PushedAt: nowRFC3339(),
			}
			if werr := WriteBuildState(projectDirForKCL(), opts.env, state); werr != nil {
				// Non-fatal: the build succeeded; recording the state is
				// a convenience for the downstream deploy. Print a
				// warning so the user knows deploy may fall back to git.
				fmt.Printf("[build]   Warning: failed to write build-state file: %v\n", werr)
			} else {
				fmt.Printf("[build]   Wrote build state: %s\n", buildStatePath(projectDirForKCL(), opts.env))
			}
		}
	}

	// Print summary
	fmt.Println()
	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("[build] Summary (%s)\n", time.Since(start).Truncate(time.Millisecond))
	fmt.Println(strings.Repeat("-", 50))

	for _, r := range succeeded {
		fmt.Printf("  OK   %-20s %-8s (%s)\n", r.name, r.kind, r.duration.Truncate(time.Millisecond))
	}
	for _, r := range failed {
		fmt.Printf("  FAIL %-20s %-8s %v\n", r.name, r.kind, r.err)
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d of %d builds failed", len(failed), len(results))
	}

	fmt.Printf("\n[build] All %d builds succeeded.\n", len(results))
	fmt.Printf("[build] Binaries available in %s/\n", opts.outputDir)
	return nil
}

func buildParallel(ctx context.Context, cfg *config.ProjectConfig, frontends, dockerFrontends []config.FrontendConfig, buildBinary, skipProjectDocker bool, cfgArchForDocker, resolvedTag string, resolvedVersion versionInfo, opts buildOptions) []buildResult {
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results []buildResult
	)

	// Build single Go binary and frontends in parallel
	if buildBinary {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := buildGoBinary(ctx, cfg, opts.outputDir, opts.debug, resolveBuildArch(cfg.Deploy.TargetArch, opts.targetArch, false), resolvedVersion)
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}()
	}

	for _, fe := range frontends {
		wg.Add(1)
		go func(f config.FrontendConfig) {
			defer wg.Done()
			r := buildFrontend(ctx, f)
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}(fe)
	}

	wg.Wait()

	// Check if any builds failed before attempting Docker
	hasBuildFailure := false
	for _, r := range results {
		if r.err != nil {
			hasBuildFailure = true
			break
		}
	}

	// Docker builds after binary builds succeed (only if --docker flag is set).
	// dockerArch is resolved with dockerCtx=true so cross-compile kicks in
	// whenever the deploy-target arch differs from the host — even if the
	// preceding go build above happened to use the host arch.
	if opts.buildDocker && !hasBuildFailure {
		dockerArch := resolveBuildArch(cfgArchForDocker, opts.targetArch, true)
		if buildBinary && !skipProjectDocker {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := dockerBuildProject(ctx, cfg, opts.pushRegistry, dockerArch, resolvedTag, resolvedVersion)
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}()
		}
		for _, fe := range dockerFrontends {
			wg.Add(1)
			go func(f config.FrontendConfig) {
				defer wg.Done()
				r := dockerBuild(ctx, cfg, f.Name, f.Path, opts.pushRegistry, dockerArch, resolvedTag)
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}(fe)
		}
		wg.Wait()
	}

	return results
}

func buildSequential(ctx context.Context, cfg *config.ProjectConfig, frontends, dockerFrontends []config.FrontendConfig, buildBinary, skipProjectDocker bool, cfgArchForDocker, resolvedTag string, resolvedVersion versionInfo, opts buildOptions) []buildResult {
	var results []buildResult

	if buildBinary {
		r := buildGoBinary(ctx, cfg, opts.outputDir, opts.debug, resolveBuildArch(cfg.Deploy.TargetArch, opts.targetArch, false), resolvedVersion)
		results = append(results, r)
		if r.err != nil {
			return results // Stop on first failure in sequential mode
		}
	}
	for _, fe := range frontends {
		r := buildFrontend(ctx, fe)
		results = append(results, r)
		if r.err != nil {
			return results
		}
	}

	// Docker builds only if --docker flag is set
	if opts.buildDocker {
		dockerArch := resolveBuildArch(cfgArchForDocker, opts.targetArch, true)
		if buildBinary && !skipProjectDocker {
			r := dockerBuildProject(ctx, cfg, opts.pushRegistry, dockerArch, resolvedTag, resolvedVersion)
			results = append(results, r)
			if r.err != nil {
				return results
			}
		}
		for _, fe := range dockerFrontends {
			r := dockerBuild(ctx, cfg, fe.Name, fe.Path, opts.pushRegistry, dockerArch, resolvedTag)
			results = append(results, r)
			if r.err != nil {
				return results
			}
		}
	}

	return results
}

func buildGoBinary(ctx context.Context, cfg *config.ProjectConfig, outputDir string, debug bool, crossArch string, versionInfo versionInfo) buildResult {
	start := time.Now()
	binaryPath := filepath.Join(outputDir, cfg.Name)

	if debug {
		fmt.Printf("[build] %s: go build (debug) -> %s\n", cfg.Name, binaryPath)
	} else {
		fmt.Printf("[build] %s: go build -> %s\n", cfg.Name, binaryPath)
	}
	if crossArch != "" {
		fmt.Printf("[build] cross-compiling for linux/%s (host: %s/%s)\n",
			crossArch, runtime.GOOS, runtime.GOARCH)
	}

	args := []string{"build", "-o", binaryPath}
	if debug {
		args = append(args, "-gcflags=all=-N -l")
	} else {
		ldflags := fmt.Sprintf("-s -w -X main.version=%s -X main.commit=%s -X main.date=%s",
			versionInfo.version, versionInfo.commit, versionInfo.date)
		// Stamp an additional project-chosen version target (forge.yaml
		// build.version_var) with the SAME resolved version, for runtime
		// code that can't import package main.
		if cfg.Build.VersionVar != "" {
			ldflags += fmt.Sprintf(" -X %s=%s", cfg.Build.VersionVar, versionInfo.version)
		}
		args = append(args, "-ldflags", ldflags)
	}
	args = append(args, "./cmd")
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if crossArch != "" {
		// Force linux/<crossArch>. We do not let CGO leak through here:
		// linux containers built on a mac-arm64 host with CGO enabled
		// would need a cross-cc toolchain (clang+aarch64-linux-gnu).
		// Forge's contract is pure-Go binaries, so CGO=0 stays.
		cmd.Env = append(cmd.Env, "GOOS=linux", "GOARCH="+crossArch)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	return buildResult{
		name:     cfg.Name,
		kind:     "service",
		duration: time.Since(start),
		err:      err,
	}
}

// versionInfo captures the source-of-truth version/commit/date injected into
// built binaries via -ldflags. Fields fall back to a time-based dev version /
// "none" / the current timestamp when the project is not a git repo.
type versionInfo struct {
	version string
	commit  string
	date    string
}

// resolveBuildVersion is the ONE version resolver shared by the host
// binary build and the docker image build, so both embed the identical
// version for a given build. The previous split — gitVersionInfo on the
// host side, an in-container `git describe` on the docker side — was the
// root cause of the "every image is main.version=dev" bug: .dockerignore
// excludes .git, so the in-container describe always failed.
//
// version policy, in order:
//
//	a. override (non-empty) — `--tag`, else forge.yaml build.version.
//	b. `git describe --tags --always --dirty` — semver when tagged,
//	   commit-ish otherwise.
//	c. `git rev-parse --short HEAD` — commit fallback for shallow / no-
//	   describe repos.
//	d. fmt.Sprintf("0.0.0-dev.%d", <unix seconds>) — time-based dev
//	   fallback when there is no git at all.
//
// commit: `git rev-parse HEAD`, else "none". date: now in RFC3339 UTC.
func resolveBuildVersion(ctx context.Context, override string) versionInfo {
	info := versionInfo{
		commit: "none",
		date:   time.Now().UTC().Format(time.RFC3339),
	}

	switch {
	case override != "":
		info.version = override
	default:
		if out, err := exec.CommandContext(ctx, "git", "describe", "--tags", "--always", "--dirty").Output(); err == nil {
			if v := strings.TrimSpace(string(out)); v != "" {
				info.version = v
			}
		}
		if info.version == "" {
			if out, err := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output(); err == nil {
				if v := strings.TrimSpace(string(out)); v != "" {
					info.version = v
				}
			}
		}
		if info.version == "" {
			info.version = fmt.Sprintf("0.0.0-dev.%d", time.Now().Unix())
		}
	}

	if out, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output(); err == nil {
		if c := strings.TrimSpace(string(out)); c != "" {
			info.commit = c
		}
	}
	return info
}

// (gitVersionTag was removed when build/deploy converged on the single
// `resolveImageTag` helper — see internal/cli/image_tag.go. The same
// `git describe --tags --always --dirty` shape now lives there as the
// shared source of truth both `forge build` and `forge deploy` consume.)

func buildFrontend(ctx context.Context, fe config.FrontendConfig) buildResult {
	start := time.Now()
	fmt.Printf("[build] %s: NODE_ENV=production npm run build in %s\n", fe.Name, fe.Path)

	cmd := exec.CommandContext(ctx, "npm", "run", "build")
	cmd.Dir = fe.Path
	cmd.Env = withForcedEnv(os.Environ(), "NODE_ENV", "production")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	return buildResult{
		name:     fe.Name,
		kind:     "frontend",
		duration: time.Since(start),
		err:      err,
	}
}

func withForcedEnv(env []string, key, value string) []string {
	prefix := key + "="
	rewritten := make([]string, 0, len(env)+1)
	replaced := false

	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if !replaced {
				rewritten = append(rewritten, prefix+value)
				replaced = true
			}
			continue
		}
		rewritten = append(rewritten, entry)
	}

	if !replaced {
		rewritten = append(rewritten, prefix+value)
	}

	return rewritten
}

// dockerBuildProject builds the single project Docker image from the
// root Dockerfile. When pushRegistry is non-empty, the image is also
// tagged with <pushRegistry>/<name>:<tag> and pushed after a successful
// build (one docker build + one docker push per image in forge.yaml,
// matching the MultiServiceApplication pattern).
//
// crossArch, when non-empty, drives `docker buildx build --platform=linux/<arch>`
// so the resulting image runs on a node whose arch matches the deploy
// target rather than the build host. Empty means "let docker use the
// host arch" — appropriate when host == target.
func dockerBuildProject(ctx context.Context, cfg *config.ProjectConfig, pushRegistry, crossArch, resolvedTag string, resolvedVersion versionInfo) buildResult {
	start := time.Now()
	dockerfile := "Dockerfile"

	if _, err := os.Stat(dockerfile); os.IsNotExist(err) {
		fmt.Printf("[build] %s: skipping docker (no Dockerfile)\n", cfg.Name)
		return buildResult{
			name:     cfg.Name + " (docker)",
			kind:     "docker",
			duration: time.Since(start),
			err:      nil,
		}
	}

	registry := cfg.Docker.Registry
	if registry == "" {
		registry = cfg.Name
	}
	latestTag := fmt.Sprintf("%s/%s:latest", registry, cfg.Name)
	versionTag := ""
	if resolvedTag != "" {
		versionTag = fmt.Sprintf("%s/%s:%s", registry, cfg.Name, resolvedTag)
	}

	dockerArgs := []string{"build"}
	// Pass the resolved build version into the image build as build-args.
	// The Dockerfile bakes these into -ldflags (FORGE_VERSION/COMMIT/DATE),
	// replacing the old in-container `git describe` that always failed
	// because .dockerignore excludes .git. The VersionVar PATH is baked
	// into the Dockerfile at generate time, so only the VALUE flows here.
	dockerArgs = append(dockerArgs,
		"--build-arg", "FORGE_VERSION="+resolvedVersion.version,
		"--build-arg", "FORGE_COMMIT="+resolvedVersion.commit,
		"--build-arg", "FORGE_DATE="+resolvedVersion.date,
	)
	if crossArch != "" {
		dockerArgs = append(dockerArgs, "--platform=linux/"+crossArch)
		fmt.Printf("[build] cross-compiling for linux/%s (host: %s/%s)\n",
			crossArch, runtime.GOOS, runtime.GOARCH)
	}
	dockerArgs = append(dockerArgs, "-t", latestTag)
	if versionTag != "" {
		dockerArgs = append(dockerArgs, "-t", versionTag)
	}
	// Tag for the push registry too when requested. For localhost:<port>
	// we also tag the k3d in-cluster mirror (registry.localhost:<port>)
	// so deployed manifests can reference the in-cluster-resolvable name —
	// but we only PUSH to the user-specified registry, since the host
	// can't DNS-resolve `registry.localhost` (it's a k3d-internal name).
	// The mirror tag is a local-only alias that downstream manifest
	// references resolve via the containerd mirror config inside k3d.
	var pushTags []string
	for i, reg := range expandPushRegistries(pushRegistry) {
		pushLatest := fmt.Sprintf("%s/%s:latest", reg, cfg.Name)
		dockerArgs = append(dockerArgs, "-t", pushLatest)
		// Only the first (user-specified) registry gets pushed. The
		// auto-mirrored registry.localhost:<port> tag is local-alias-only.
		if i == 0 {
			pushTags = append(pushTags, pushLatest)
		}
		if resolvedTag != "" {
			pushVersion := fmt.Sprintf("%s/%s:%s", reg, cfg.Name, resolvedTag)
			dockerArgs = append(dockerArgs, "-t", pushVersion)
			if i == 0 {
				pushTags = append(pushTags, pushVersion)
			}
		}
	}
	// Additional build contexts from forge.yaml's docker.build_contexts.
	// Each becomes a `--build-context name=value` arg, letting the
	// Dockerfile pull files from outside the normal context via
	// `FROM name` / `COPY --from=name`. See [config.DockerConfig.BuildContexts]
	// for the supported value shapes (relative path, absolute path,
	// `docker-image://`, …). cwd is the project root by construction
	// (forge build runs alongside forge.yaml).
	dockerArgs = appendBuildContexts(dockerArgs, cfg, "")
	fmt.Printf("[build] %s: docker build (%d tags)\n", cfg.Name, countTags(dockerArgs))
	dockerArgs = append(dockerArgs, "-f", dockerfile, ".")

	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return buildResult{
			name:     cfg.Name + " (docker)",
			kind:     "docker",
			duration: time.Since(start),
			err:      err,
		}
	}

	// Push every push-registry tag if requested.
	for _, t := range pushTags {
		fmt.Printf("[build] %s: docker push %s\n", cfg.Name, t)
		pushCmd := exec.CommandContext(ctx, "docker", "push", t)
		pushCmd.Stdout = os.Stdout
		pushCmd.Stderr = os.Stderr
		if err := pushCmd.Run(); err != nil {
			return buildResult{
				name:     cfg.Name + " (docker)",
				kind:     "docker",
				duration: time.Since(start),
				err:      fmt.Errorf("docker push %s: %w", t, err),
			}
		}
	}

	return buildResult{
		name:     cfg.Name + " (docker)",
		kind:     "docker",
		duration: time.Since(start),
		err:      nil,
	}
}

// expandPushRegistries returns the set of registries to tag a built
// image against. For non-localhost registries this is just the single
// pushRegistry the caller passed. For `localhost:<port>` it also adds
// `registry.localhost:<port>` — the canonical k3d pattern where the
// host pushes to localhost and kubelet inside the node container pulls
// from registry.localhost (the deploy/k3d.yaml mirrors block maps both
// to the same backend). Returns nil when pushRegistry is empty.
func expandPushRegistries(pushRegistry string) []string {
	if pushRegistry == "" {
		return nil
	}
	registries := []string{pushRegistry}
	if strings.HasPrefix(pushRegistry, "localhost:") {
		port := strings.TrimPrefix(pushRegistry, "localhost:")
		registries = append(registries, "registry.localhost:"+port)
	}
	return registries
}

// countTags counts the `-t` flags in a docker build arg list for the
// progress line. Cheap; only used for human-readable output.
func countTags(args []string) int {
	n := 0
	for _, a := range args {
		if a == "-t" {
			n++
		}
	}
	return n
}

// dockerBuild builds a Docker image for a frontend from its own
// Dockerfile. When pushRegistry is non-empty, the image is also tagged
// with <pushRegistry>/<name>:<tag> and pushed after a successful build.
//
// crossArch, when non-empty, drives `docker buildx build --platform=linux/<arch>`
// so frontends destined for the deploy-target node arch are built
// correctly even on a different host arch. Same semantics as
// dockerBuildProject.
func dockerBuild(ctx context.Context, cfg *config.ProjectConfig, name, path, pushRegistry, crossArch, resolvedTag string) buildResult {
	start := time.Now()
	dockerfile := filepath.Join(path, "Dockerfile")

	if _, err := os.Stat(dockerfile); os.IsNotExist(err) {
		fmt.Printf("[build] %s: skipping docker (no Dockerfile)\n", name)
		return buildResult{
			name:     name + " (docker)",
			kind:     "docker",
			duration: time.Since(start),
			err:      nil,
		}
	}

	registry := cfg.Docker.Registry
	if registry == "" {
		registry = cfg.Name
	}
	latestTag := fmt.Sprintf("%s/%s:latest", registry, name)
	versionTag := ""
	if resolvedTag != "" {
		versionTag = fmt.Sprintf("%s/%s:%s", registry, name, resolvedTag)
	}

	dockerArgs := []string{"build"}
	if crossArch != "" {
		dockerArgs = append(dockerArgs, "--platform=linux/"+crossArch)
		fmt.Printf("[build] cross-compiling for linux/%s (host: %s/%s)\n",
			crossArch, runtime.GOOS, runtime.GOARCH)
	}
	dockerArgs = append(dockerArgs, "-t", latestTag)
	if versionTag != "" {
		dockerArgs = append(dockerArgs, "-t", versionTag)
	}
	// For localhost:<port> we also tag the k3d in-cluster mirror
	// (registry.localhost:<port>) so deployed manifests can reference
	// the in-cluster-resolvable name. See expandPushRegistries. We only
	// PUSH to the first (user-specified) registry — the host can't
	// DNS-resolve registry.localhost, so the mirror tag is a local
	// alias that downstream manifests resolve via the containerd mirror
	// config inside k3d. Matches the dockerBuildProject behaviour.
	var pushTags []string
	for i, reg := range expandPushRegistries(pushRegistry) {
		pushLatest := fmt.Sprintf("%s/%s:latest", reg, name)
		dockerArgs = append(dockerArgs, "-t", pushLatest)
		if i == 0 {
			pushTags = append(pushTags, pushLatest)
		}
		if resolvedTag != "" {
			pushVersion := fmt.Sprintf("%s/%s:%s", reg, name, resolvedTag)
			dockerArgs = append(dockerArgs, "-t", pushVersion)
			if i == 0 {
				pushTags = append(pushTags, pushVersion)
			}
		}
	}
	// Additional build contexts from forge.yaml. Same semantics as
	// dockerBuildProject — useful when the frontend Dockerfile needs
	// to reference paths outside its own subtree.
	dockerArgs = appendBuildContexts(dockerArgs, cfg, "")
	fmt.Printf("[build] %s: docker build (%d tags)\n", name, countTags(dockerArgs))
	dockerArgs = append(dockerArgs, "-f", dockerfile, path)

	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return buildResult{
			name:     name + " (docker)",
			kind:     "docker",
			duration: time.Since(start),
			err:      err,
		}
	}

	for _, t := range pushTags {
		fmt.Printf("[build] %s: docker push %s\n", name, t)
		pushCmd := exec.CommandContext(ctx, "docker", "push", t)
		pushCmd.Stdout = os.Stdout
		pushCmd.Stderr = os.Stderr
		if err := pushCmd.Run(); err != nil {
			return buildResult{
				name:     name + " (docker)",
				kind:     "docker",
				duration: time.Since(start),
				err:      fmt.Errorf("docker push %s: %w", t, err),
			}
		}
	}

	return buildResult{
		name:     name + " (docker)",
		kind:     "docker",
		duration: time.Since(start),
		err:      nil,
	}
}

// frontendsSkippedByFramework reports whether `forge build` should drop
// ALL declared frontends from the build set because the project declares
// `stack.frontend.framework: none`. That setting means "forge does not own
// a frontend build toolchain here" — so forge must not run `npm run build`,
// even when the `frontends:` list is populated (a frontend that builds
// out-of-band, or one whose deps aren't installed). Honoring it keeps an
// unrelated frontend build failure from sinking a deployable Go service
// (fr-cc10bfab0c). Returns false when there are no frontends (nothing to
// skip — the log line would be noise).
func frontendsSkippedByFramework(cfg *config.ProjectConfig) bool {
	return cfg.Stack.EffectiveFrontendFramework() == "none" && len(cfg.Frontends) > 0
}

func filterFrontends(frontends []config.FrontendConfig, target string) []config.FrontendConfig {
	for _, f := range frontends {
		if f.Name == target {
			return []config.FrontendConfig{f}
		}
	}
	return nil
}

// filterFrontendsForBuild drops frontends whose KCL `deploy.type` is
// "host" — the host-mode dev server (`npm run dev` in forge up) doesn't
// consume the production build artifact, so running `npm run build`
// for it is a pure waste. Per-frontend lookup goes by name; a frontend
// in cfg.Frontends with no matching KCL entry (or whose KCL entry has
// no deploy block) falls through to "build" — preserving the
// pre-discriminator behaviour so legacy projects keep working.
//
// Prints a one-line note per skipped frontend so users can see at a
// glance why their build finished early.
func filterFrontendsForBuild(frontends []config.FrontendConfig, entities *KCLEntities) []config.FrontendConfig {
	if entities == nil {
		return frontends
	}
	kept := make([]config.FrontendConfig, 0, len(frontends))
	for _, fe := range frontends {
		mode := frontendDeployMode(entities, fe.Name)
		if mode == "host" {
			fmt.Printf("[build] skipping prod build for %s (host-mode deploy)\n", fe.Name)
			continue
		}
		kept = append(kept, fe)
	}
	return kept
}

// frontendDeployMode returns the deploy.type for the named frontend in
// the rendered KCL, or "" when the frontend isn't found or has no
// deploy block. Lower-cased for case-insensitive comparison.
func frontendDeployMode(entities *KCLEntities, name string) string {
	if entities == nil {
		return ""
	}
	for _, fe := range entities.Frontends {
		if fe.Name != name {
			continue
		}
		if fe.Deploy == nil {
			return ""
		}
		return strings.ToLower(strings.TrimSpace(fe.Deploy.Type))
	}
	return ""
}

// projectDirForKCL resolves the project root directory used as the
// argument to RenderKCL. Falls back to "." when forge.yaml isn't found
// (the kcl shell-out will still surface the error with a useful path).
func projectDirForKCL() string {
	if cfgPath, perr := findProjectConfigFile(); perr == nil {
		return filepath.Dir(cfgPath)
	}
	return "."
}

// summarizeKCLBuildPlan prints the per-deploy.type split so users see,
// in one glance, which services this `forge build --env=<env>` will
// docker-build vs skip vs treat as build-only-variants. The skip set
// matches the runtime behaviour wired in runBuild — host and build-only
// services are excluded from the docker layer; cluster services drive
// it.
func summarizeKCLBuildPlan(e *KCLEntities) {
	if e == nil {
		return
	}
	if hosts := e.HostServiceNames(); len(hosts) > 0 {
		fmt.Printf("[build]   Host-mode (skip docker): %s\n", strings.Join(hosts, ", "))
	}
	if cluster := e.ClusterServiceNames(); len(cluster) > 0 {
		fmt.Printf("[build]   Cluster-mode (docker):   %s\n", strings.Join(cluster, ", "))
	}
	if bo := e.BuildOnlyServiceNames(); len(bo) > 0 {
		fmt.Printf("[build]   Build-only (binary):     %s\n", strings.Join(bo, ", "))
	}
	if len(e.Frontends) > 0 {
		names := make([]string, 0, len(e.Frontends))
		for _, f := range e.Frontends {
			names = append(names, f.Name)
		}
		fmt.Printf("[build]   Frontends (skip docker): %s\n", strings.Join(names, ", "))
	}
}

// kclHasClusterService reports whether the entity set contains at least
// one service with deploy.Type == "cluster". When false the project
// docker build is skipped: there's no in-cluster Application to ship.
func kclHasClusterService(e *KCLEntities) bool {
	for _, s := range e.Services {
		if s.Deploy.Type == "cluster" {
			return true
		}
	}
	return false
}

// kclFirstClusterPlatform returns the platform (GOARCH) of the first
// cluster service whose deploy.Cluster.Platform is non-empty. KCL renders
// all cluster services in one env onto the same node arch in practice,
// so the first hit is the env-wide default. Returns "" when no cluster
// service declares a platform — callers fall back to forge.yaml's
// deploy.target_arch.
func kclFirstClusterPlatform(e *KCLEntities) string {
	for _, s := range e.Services {
		if s.Deploy.Cluster != nil && s.Deploy.Cluster.Platform != "" {
			return s.Deploy.Cluster.Platform
		}
	}
	return ""
}

// buildKCLBuildOnlyVariants compiles each declared build-only variant
// into bin/<service>-<variant> with the variant's ldflags and build
// tags. Each variant is a separate `go build` invocation; failures are
// captured in the returned buildResult slice rather than short-circuited
// so users see the full list of failures from one run.
func buildKCLBuildOnlyVariants(ctx context.Context, e *KCLEntities, outputDir string) []buildResult {
	var out []buildResult
	for _, svc := range e.Services {
		if svc.Deploy.Type != "build-only" || svc.Deploy.BuildOnly == nil {
			continue
		}
		for _, v := range svc.Deploy.BuildOnly.BuildVariants {
			out = append(out, buildVariant(ctx, svc.Name, v, outputDir))
		}
	}
	return out
}

// buildVariant builds one binary for a build-only service variant.
// The output name is <service>-<variant> unless v.OutputName overrides
// it. ldflags and -tags are appended to the go-build args; env_at_build
// pairs join CGO_ENABLED=0 on the subprocess env.
func buildVariant(ctx context.Context, svcName string, v BuildVariant, outputDir string) buildResult {
	start := time.Now()
	outName := v.OutputName
	if outName == "" {
		outName = svcName + "-" + v.Name
	}
	binPath := filepath.Join(outputDir, outName)
	fmt.Printf("[build] %s (variant %s): go build -> %s\n", svcName, v.Name, binPath)

	args := []string{"build", "-o", binPath}
	if len(v.Ldflags) > 0 {
		args = append(args, "-ldflags", strings.Join(v.Ldflags, " "))
	}
	if len(v.BuildTags) > 0 {
		args = append(args, "-tags", strings.Join(v.BuildTags, ","))
	}
	args = append(args, "./cmd")

	cmd := exec.CommandContext(ctx, "go", args...)
	env := append(os.Environ(), "CGO_ENABLED=0")
	for k, val := range v.EnvAtBuild {
		env = append(env, k+"="+val)
	}
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	return buildResult{
		name:     svcName + ":" + v.Name,
		kind:     "variant",
		duration: time.Since(start),
		err:      err,
	}
}
