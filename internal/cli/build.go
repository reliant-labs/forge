package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
)

// buildOptions holds the flag values for the build command.
type buildOptions struct {
	outputDir   string
	buildTarget string
	parallel    bool
	buildDocker bool
	debug       bool
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
  forge build                    # Build everything
  forge build -t web             # Build only the "web" frontend
  forge build -o bin             # Output binaries to bin/
  forge build --docker           # Also build Docker images
  forge build --debug           # Build with debug symbols for Delve`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuild(opts)
		},
	}

	cmd.Flags().StringVarP(&opts.outputDir, "output", "o", "bin", "Output directory for binaries")
	cmd.Flags().StringVarP(&opts.buildTarget, "target", "t", "all", "Build target (all, or a specific service/frontend name)")
	cmd.Flags().BoolVar(&opts.parallel, "parallel", true, "Build services in parallel")
	cmd.Flags().BoolVar(&opts.buildDocker, "docker", false, "Build Docker images for all services")
	cmd.Flags().BoolVar(&opts.debug, "debug", false, "Build with debug symbols for Delve")

	return cmd
}

type buildResult struct {
	name     string
	kind     string // "service", "frontend", or "docker"
	duration time.Duration
	err      error
}

func runBuild(opts buildOptions) error {
	cfg, err := loadProjectConfig()
	if err != nil {
		return err
	}

	fmt.Printf("[build] Building project: %s\n", cfg.Name)
	fmt.Printf("[build]   Output:   %s\n", opts.outputDir)
	fmt.Printf("[build]   Target:   %s\n", opts.buildTarget)
	fmt.Printf("[build]   Parallel: %v\n", opts.parallel)
	fmt.Printf("[build]   Docker:   %v\n", opts.buildDocker)
	fmt.Println()

	// Create output directory
	if err := os.MkdirAll(opts.outputDir, 0755); err != nil {
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
		results = buildParallel(cfg, frontends, buildBinary, opts)
	} else {
		results = buildSequential(cfg, frontends, buildBinary, opts)
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

func buildParallel(cfg *config.ProjectConfig, frontends []config.FrontendConfig, buildBinary bool, opts buildOptions) []buildResult {
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
			r := buildGoBinary(cfg, opts.outputDir, opts.debug)
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}()
	}

	for _, fe := range frontends {
		wg.Add(1)
		go func(f config.FrontendConfig) {
			defer wg.Done()
			r := buildFrontend(f)
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

	// Docker builds after binary builds succeed (only if --docker flag is set)
	if opts.buildDocker && !hasBuildFailure {
		if buildBinary {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := dockerBuildProject(cfg)
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}()
		}
		for _, fe := range frontends {
			wg.Add(1)
			go func(f config.FrontendConfig) {
				defer wg.Done()
				r := dockerBuild(cfg, f.Name, f.Path)
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}(fe)
		}
		wg.Wait()
	}

	return results
}

func buildSequential(cfg *config.ProjectConfig, frontends []config.FrontendConfig, buildBinary bool, opts buildOptions) []buildResult {
	var results []buildResult

	if buildBinary {
		r := buildGoBinary(cfg, opts.outputDir, opts.debug)
		results = append(results, r)
		if r.err != nil {
			return results // Stop on first failure in sequential mode
		}
	}
	for _, fe := range frontends {
		r := buildFrontend(fe)
		results = append(results, r)
		if r.err != nil {
			return results
		}
	}

	// Docker builds only if --docker flag is set
	if opts.buildDocker {
		if buildBinary {
			r := dockerBuildProject(cfg)
			results = append(results, r)
			if r.err != nil {
				return results
			}
		}
		for _, fe := range frontends {
			r := dockerBuild(cfg, fe.Name, fe.Path)
			results = append(results, r)
			if r.err != nil {
				return results
			}
		}
	}

	return results
}

func buildGoBinary(cfg *config.ProjectConfig, outputDir string, debug bool) buildResult {
	start := time.Now()
	binaryPath := filepath.Join(outputDir, cfg.Name)

	if debug {
		fmt.Printf("[build] %s: go build (debug) -> %s\n", cfg.Name, binaryPath)
	} else {
		fmt.Printf("[build] %s: go build -> %s\n", cfg.Name, binaryPath)
	}

	args := []string{"build", "-o", binaryPath}
	if debug {
		args = append(args, "-gcflags=all=-N -l")
	} else {
		versionInfo := gitVersionInfo()
		ldflags := fmt.Sprintf("-s -w -X main.version=%s -X main.commit=%s -X main.date=%s",
			versionInfo.version, versionInfo.commit, versionInfo.date)
		args = append(args, "-ldflags", ldflags)
	}
	args = append(args, "./cmd")
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
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
func gitVersionInfo() versionInfo {
	info := versionInfo{
		version: "dev",
		commit:  "none",
		date:    "unknown",
	}

	if out, err := exec.Command("git", "describe", "--tags", "--always", "--dirty").Output(); err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			info.version = v
		}
	}
	if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
		if c := strings.TrimSpace(string(out)); c != "" {
			info.commit = c
		}
	}
	info.date = time.Now().UTC().Format(time.RFC3339)
	return info
}

// gitVersionTag returns the git-describe version if this is a git repo,
// or the empty string otherwise. Used to add a version tag to docker images.
func gitVersionTag() string {
	out, err := exec.Command("git", "describe", "--tags", "--always", "--dirty").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func buildFrontend(fe config.FrontendConfig) buildResult {
	start := time.Now()
	fmt.Printf("[build] %s: NODE_ENV=production npm run build in %s\n", fe.Name, fe.Path)

	cmd := exec.Command("npm", "run", "build")
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

// dockerBuildProject builds the single project Docker image from the root Dockerfile.
func dockerBuildProject(cfg *config.ProjectConfig) buildResult {
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
	if v := gitVersionTag(); v != "" {
		versionTag = fmt.Sprintf("%s/%s:%s", registry, cfg.Name, v)
	}

	dockerArgs := []string{"build", "-t", latestTag}
	if versionTag != "" {
		dockerArgs = append(dockerArgs, "-t", versionTag)
		fmt.Printf("[build] %s: docker build -t %s -t %s\n", cfg.Name, latestTag, versionTag)
	} else {
		fmt.Printf("[build] %s: docker build -t %s\n", cfg.Name, latestTag)
	}
	dockerArgs = append(dockerArgs, "-f", dockerfile, ".")

	cmd := exec.Command("docker", dockerArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	return buildResult{
		name:     cfg.Name + " (docker)",
		kind:     "docker",
		duration: time.Since(start),
		err:      err,
	}
}

// dockerBuild builds a Docker image for a frontend from its own Dockerfile.
func dockerBuild(cfg *config.ProjectConfig, name, path string) buildResult {
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
	if v := gitVersionTag(); v != "" {
		versionTag = fmt.Sprintf("%s/%s:%s", registry, name, v)
	}

	dockerArgs := []string{"build", "-t", latestTag}
	if versionTag != "" {
		dockerArgs = append(dockerArgs, "-t", versionTag)
		fmt.Printf("[build] %s: docker build -t %s -t %s\n", name, latestTag, versionTag)
	} else {
		fmt.Printf("[build] %s: docker build -t %s\n", name, latestTag)
	}
	dockerArgs = append(dockerArgs, "-f", dockerfile, path)

	cmd := exec.Command("docker", dockerArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	return buildResult{
		name:     name + " (docker)",
		kind:     "docker",
		duration: time.Since(start),
		err:      err,
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