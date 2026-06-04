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
			// --push implies --docker so users don't have to pass both.
			if opts.pushRegistry != "" {
				opts.buildDocker = true
			}
			return runBuild(cmd.Context(), opts)
		},
	}

	cmd.Flags().StringVarP(&opts.outputDir, "output", "o", "bin", "Output directory for binaries")
	cmd.Flags().StringVarP(&opts.buildTarget, "target", "t", "all", "Build target (all, or a specific service/frontend name)")
	cmd.Flags().BoolVar(&opts.parallel, "parallel", true, "Build services in parallel")
	cmd.Flags().BoolVar(&opts.buildDocker, "docker", false, "Build Docker images for all services")
	cmd.Flags().BoolVar(&opts.debug, "debug", false, "Build with debug symbols for Delve")
	cmd.Flags().StringVar(&opts.pushRegistry, "push", "", "Push docker images to this registry after build (implies --docker)")
	cmd.Flags().StringVar(&opts.targetArch, "target-arch", "", "Override target GOARCH for cross-compilation (default: forge.yaml deploy.target_arch, then amd64 for docker builds)")
	cmd.Flags().StringVar(&opts.env, "env", "", "Deploy environment (e.g. dev, staging, prod). When set, services declared `deploy: host` in deploy/kcl/<env>/ are excluded from docker build/push (the Go binary still includes their code).")

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
	cfg, err := loadProjectConfig()
	if err != nil {
		return err
	}

	fmt.Printf("[build] Building project: %s\n", cfg.Name)
	fmt.Printf("[build]   Output:   %s\n", opts.outputDir)
	fmt.Printf("[build]   Target:   %s\n", opts.buildTarget)
	fmt.Printf("[build]   Parallel: %v\n", opts.parallel)
	fmt.Printf("[build]   Docker:   %v\n", opts.buildDocker)
	if opts.env != "" {
		fmt.Printf("[build]   Env:      %s\n", opts.env)
	}
	// Per-env host-mode notice is re-implemented in the KCL-orchestration
	// batch (deliverable 3): when --env is set, the build reads the
	// rendered KCL for services declared deploy: "host" and skips the
	// docker build/push for them. Hooked up in a follow-up commit.
	fmt.Println()

	// Create output directory
	if err := os.MkdirAll(opts.outputDir, 0o755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Filter targets — the Go binary is always built (single binary).
	// The target flag only filters frontends now.
	frontends := cfg.Frontends
	buildBinary := true

	if opts.buildTarget != "all" {
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

	start := time.Now()
	var results []buildResult

	if opts.parallel {
		results = buildParallel(ctx, cfg, frontends, buildBinary, opts)
	} else {
		results = buildSequential(ctx, cfg, frontends, buildBinary, opts)
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

func buildParallel(ctx context.Context, cfg *config.ProjectConfig, frontends []config.FrontendConfig, buildBinary bool, opts buildOptions) []buildResult {
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
			r := buildGoBinary(ctx, cfg, opts.outputDir, opts.debug, resolveBuildArch(cfg.Deploy.TargetArch, opts.targetArch, false))
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
		dockerArch := resolveBuildArch(cfg.Deploy.TargetArch, opts.targetArch, true)
		if buildBinary {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := dockerBuildProject(ctx, cfg, opts.pushRegistry, dockerArch)
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}()
		}
		for _, fe := range frontends {
			wg.Add(1)
			go func(f config.FrontendConfig) {
				defer wg.Done()
				r := dockerBuild(ctx, cfg, f.Name, f.Path, opts.pushRegistry, dockerArch)
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}(fe)
		}
		wg.Wait()
	}

	return results
}

func buildSequential(ctx context.Context, cfg *config.ProjectConfig, frontends []config.FrontendConfig, buildBinary bool, opts buildOptions) []buildResult {
	var results []buildResult

	if buildBinary {
		r := buildGoBinary(ctx, cfg, opts.outputDir, opts.debug, resolveBuildArch(cfg.Deploy.TargetArch, opts.targetArch, false))
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
		dockerArch := resolveBuildArch(cfg.Deploy.TargetArch, opts.targetArch, true)
		if buildBinary {
			r := dockerBuildProject(ctx, cfg, opts.pushRegistry, dockerArch)
			results = append(results, r)
			if r.err != nil {
				return results
			}
		}
		for _, fe := range frontends {
			r := dockerBuild(ctx, cfg, fe.Name, fe.Path, opts.pushRegistry, dockerArch)
			results = append(results, r)
			if r.err != nil {
				return results
			}
		}
	}

	return results
}

func buildGoBinary(ctx context.Context, cfg *config.ProjectConfig, outputDir string, debug bool, crossArch string) buildResult {
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
		versionInfo := gitVersionInfo(ctx)
		ldflags := fmt.Sprintf("-s -w -X main.version=%s -X main.commit=%s -X main.date=%s",
			versionInfo.version, versionInfo.commit, versionInfo.date)
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
// built binaries via -ldflags. Fields fall back to "dev"/"none"/"unknown"
// when the project is not a git repo.
type versionInfo struct {
	version string
	commit  string
	date    string
}

// gitVersionInfo returns version metadata derived from git, falling back to
// safe defaults when git commands fail (e.g. not a git repo).
func gitVersionInfo(ctx context.Context) versionInfo {
	info := versionInfo{
		version: "dev",
		commit:  "none",
		date:    "unknown",
	}

	if out, err := exec.CommandContext(ctx, "git", "describe", "--tags", "--always", "--dirty").Output(); err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			info.version = v
		}
	}
	if out, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output(); err == nil {
		if c := strings.TrimSpace(string(out)); c != "" {
			info.commit = c
		}
	}
	info.date = time.Now().UTC().Format(time.RFC3339)
	return info
}

// gitVersionTag returns the git-describe version if this is a git repo,
// or the empty string otherwise. Used to add a version tag to docker images.
func gitVersionTag(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "git", "describe", "--tags", "--always", "--dirty").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

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
func dockerBuildProject(ctx context.Context, cfg *config.ProjectConfig, pushRegistry, crossArch string) buildResult {
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
	if v := gitVersionTag(ctx); v != "" {
		versionTag = fmt.Sprintf("%s/%s:%s", registry, cfg.Name, v)
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
		if v := gitVersionTag(ctx); v != "" {
			pushVersion := fmt.Sprintf("%s/%s:%s", reg, cfg.Name, v)
			dockerArgs = append(dockerArgs, "-t", pushVersion)
			if i == 0 {
				pushTags = append(pushTags, pushVersion)
			}
		}
	}
	// Additional build contexts from forge.yaml's docker.build_contexts.
	// Each becomes a `--build-context name=path` arg, letting the
	// Dockerfile pull files from outside the normal context via
	// `COPY --from=name`. Typical use: sibling-checkout local replace
	// directives where the replaced module lives outside the project tree.
	for _, name := range sortedKeys(cfg.Docker.BuildContexts) {
		path := cfg.Docker.BuildContexts[name]
		dockerArgs = append(dockerArgs, "--build-context", name+"="+path)
	}
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
func dockerBuild(ctx context.Context, cfg *config.ProjectConfig, name, path, pushRegistry, crossArch string) buildResult {
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
	if v := gitVersionTag(ctx); v != "" {
		versionTag = fmt.Sprintf("%s/%s:%s", registry, name, v)
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
		if v := gitVersionTag(ctx); v != "" {
			pushVersion := fmt.Sprintf("%s/%s:%s", reg, name, v)
			dockerArgs = append(dockerArgs, "-t", pushVersion)
			if i == 0 {
				pushTags = append(pushTags, pushVersion)
			}
		}
	}
	// Additional build contexts from forge.yaml. Same semantics as
	// dockerBuildProject — useful when the frontend Dockerfile needs
	// to reference paths outside its own subtree.
	for _, k := range sortedKeys(cfg.Docker.BuildContexts) {
		dockerArgs = append(dockerArgs, "--build-context", k+"="+cfg.Docker.BuildContexts[k])
	}
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

func filterFrontends(frontends []config.FrontendConfig, target string) []config.FrontendConfig {
	for _, f := range frontends {
		if f.Name == target {
			return []config.FrontendConfig{f}
		}
	}
	return nil
}

