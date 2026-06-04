package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/deploytarget"
)

func newDeployCmd() *cobra.Command {
	var (
		imageTag        string
		tag             string
		dryRun          bool
		namespace       string
		contextOverride string
		explain         bool
		targetArch      string
		prune           bool
	)

	cmd := &cobra.Command{
		Use:   "deploy <environment>",
		Short: "Deploy services to a Kubernetes environment (Kubernetes-only)",
		Long: `Note: forge deploy currently targets Kubernetes only — compose, docker-run, and bare-binary deploys are out of scope for this command.

Deploy services to the specified Kubernetes environment using KCL manifests.

The environment must correspond to a directory under deploy/kcl/<env>/.

For dev environments, the command ensures a k3d cluster exists and pushes images
to the local registry at localhost:5050.

Safety: before applying, forge verifies the current kubectl context matches
the environment's expected cluster (read from the rendered KCL's
forge.K8sCluster.cluster; defaults to k3d-<project> for dev). The check
ALSO runs under --dry-run so wrong-context mistakes surface before the
strict apply.

Use --context to override when a single CI deploy-bot context legitimately
targets multiple environments. Use --explain to print the resolved guard
decision (expected / current / verdict) without applying anything.

Examples:
  forge deploy dev                          # Deploy to dev (local k3d)
  forge deploy staging --image-tag v1.2     # Deploy to staging with specific tag
  forge deploy prod --dry-run               # Preview prod manifests (guard runs)
  forge deploy prod --explain               # Show the env-cluster guard verdict
  forge deploy dev --namespace custom-ns    # Override namespace
  forge deploy prod --context deploy-bot    # Override the expected kubectl context`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if explain {
				return runDeployExplain(cmd.Context(), args[0], contextOverride)
			}
			// --tag and --image-tag are interchangeable; --tag is the
			// canonical name (it matches `forge build --tag`) and
			// --image-tag is retained for backwards compat with
			// pre-converged scripts. When both are set, --tag wins.
			effectiveTag := tag
			if effectiveTag == "" {
				effectiveTag = imageTag
			}
			return runDeploy(cmd.Context(), args[0], deployOptions{
				imageTag:        effectiveTag,
				dryRun:          dryRun,
				namespace:       namespace,
				contextOverride: contextOverride,
				targetArch:      targetArch,
				prune:           prune,
			})
		},
	}

	cmd.Flags().StringVar(&imageTag, "image-tag", "", "Image tag (deprecated alias for --tag; default: build-state file, then git describe --tags --always --dirty)")
	cmd.Flags().StringVar(&tag, "tag", "", "Override the image tag (priority: --tag > .forge/state/build-<env>.json > git describe --tags --always --dirty)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print manifests without applying (env-cluster guard still runs)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Override namespace from environment config")
	cmd.Flags().StringVar(&contextOverride, "context", "", "Override expected kubectl context (skips the env-cluster guard)")
	cmd.Flags().BoolVar(&explain, "explain", false, "Print the env-cluster guard decision (expected/current/verdict) and exit")
	cmd.Flags().StringVar(&targetArch, "target-arch", "", "Override target GOARCH for cross-compilation (default: forge.yaml deploy.target_arch, then amd64)")
	cmd.Flags().BoolVar(&prune, "prune", false, "Delete forge-managed Deployments in the namespace that the current KCL render no longer produces (opt-in)")

	return cmd
}

// runDeployExplain prints the resolved kubectl-context guard decision
// for an environment without doing anything destructive. Useful when
// debugging why `forge deploy staging` refuses to apply or what context
// staging is expected to live in.
func runDeployExplain(ctx context.Context, envName, override string) error {
	cfg, err := loadProjectConfig()
	if err != nil {
		return err
	}
	expected := expectedClusterForEnv(ctx, cfg, envName)
	current := strings.TrimSpace(currentKubectlContext(ctx))

	fmt.Printf("forge deploy %s — env-cluster guard\n", envName)
	fmt.Printf("  expected context: %s\n", emptyAs(expected, "(not declared)"))
	fmt.Printf("  current context:  %s\n", emptyAs(current, "(none — kubectl not configured)"))

	if override != "" {
		fmt.Printf("  override:         --context %s (guard skipped, kubectl context will switch)\n", override)
		fmt.Println("  verdict: ALLOW (override active)")
		return nil
	}
	if expected == "" {
		fmt.Printf("  hint:             declare `forge.K8sCluster.cluster` in deploy/kcl/%s/main.k to enable the guard\n", envName)
		fmt.Println("  verdict: ALLOW (no expectation declared — guard skipped)")
		return printDeployExplainHostSkip(cfg, envName)
	}
	if current == expected {
		fmt.Println("  verdict: ALLOW (current matches expected)")
		return printDeployExplainHostSkip(cfg, envName)
	}
	fmt.Printf("  fix:              kubectl config use-context %s\n", expected)
	fmt.Println("  verdict: REFUSE (context mismatch)")
	return nil
}

// printDeployExplainHostSkip is a placeholder for the post-orchestration
// shape: once the KCL-side `deploy: "host"` filter lands (deliverable 4)
// this helper renders the host-mode service list for `forge deploy <env>
// --explain`. Kept as a stub so the call site keeps compiling while the
// re-wire is in progress.
func printDeployExplainHostSkip(_ *config.ProjectConfig, _ string) error {
	return nil
}

// emptyAs returns alt when s is empty, otherwise s. Cheap helper for
// rendering "not declared" / "(none)" placeholders.
func emptyAs(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// deployOptions bundles the flag values for `forge deploy`. The
// runDeploy function previously took six discrete parameters; growing
// it to seven tipped revive's argument-limit lint. The struct form
// makes the call site self-documenting and keeps the per-flag default
// (e.g. dryRun=false) co-located with the field declaration.
type deployOptions struct {
	imageTag        string
	dryRun          bool
	namespace       string
	contextOverride string
	targetArch      string
	// prune, when true, deletes forge-managed Deployments in the
	// namespace that the just-applied KCL render no longer produces.
	// Opt-in to start: pruning is destructive (deletes resources the
	// user didn't ask to remove) and surprising behaviour during an
	// in-progress KCL refactor would be costly to roll back. The dev
	// loop benefits most — `forge deploy dev` after a host-mode
	// refactor leaves stale Deployments behind otherwise.
	prune bool
}

func runDeploy(ctx context.Context, envName string, opts deployOptions) error {
	imageTag := opts.imageTag
	dryRun := opts.dryRun
	namespace := opts.namespace
	contextOverride := opts.contextOverride
	targetArchFlag := opts.targetArch
	prune := opts.prune
	cfg, err := loadProjectConfig()
	if err != nil {
		return err
	}

	if !cfg.Features.DeployEnabled() {
		fmt.Println("deploy feature is disabled in forge.yaml")
		return nil
	}

	// Resolve KCL directory.
	kclDir := cfg.K8s.KCLDir
	if kclDir == "" {
		kclDir = "deploy/kcl"
	}
	envDir := filepath.Join(kclDir, envName)
	mainK := filepath.Join(envDir, "main.k")

	// Validate environment exists.
	if _, err := os.Stat(mainK); os.IsNotExist(err) {
		return fmt.Errorf("environment %q not found: %s does not exist\nAvailable environments can be found under %s/", envName, mainK, kclDir)
	}

	// Resolve image tag via the three-tier precedence chain. Split into
	// its own helper so the precedence logic is testable without
	// stubbing the whole deploy pipeline.
	projectDir := projectDirForKCL()
	tag, tagSource, terr := resolveDeployImageTag(ctx, projectDir, envName, imageTag)
	if terr != nil {
		return terr
	}
	imageTag = tag

	// Resolve namespace.
	if namespace == "" {
		if ns := k8sClusterNamespaceForEnv(ctx, envName); ns != "" {
			namespace = ns
		} else {
			namespace = cfg.Name + "-" + envName
		}
	}

	fmt.Printf("Deploying project: %s\n", cfg.Name)
	fmt.Printf("  Environment: %s\n", envName)
	fmt.Printf("  Image tag:   %s  (source: %s)\n", imageTag, tagSource)
	fmt.Printf("  Namespace:   %s\n", namespace)
	fmt.Printf("  Dry run:     %v\n", dryRun)
	fmt.Println()

	// kubectl-context guard: verify the current context matches the
	// env's expected cluster before doing anything destructive. Runs
	// under --dry-run too: dry-run is for surfacing mistakes (wrong
	// context!) before they ship, not for papering over them. The guard
	// is skipped when --context is passed (CI scenarios where a single
	// deploy-bot context legitimately targets multiple envs).
	if err := verifyKubectlContext(ctx, cfg, envName, contextOverride); err != nil {
		return err
	}

	start := time.Now()

	// Dev environment: ensure k3d cluster and push images locally. Skip
	// the cluster bootstrap and docker build/push when --dry-run is set —
	// dry-run only renders manifests and never touches the cluster or
	// registry, so the slow image step is dead weight.
	if envName == "dev" && !dryRun {
		if err := ensureDevCluster(ctx); err != nil {
			return err
		}

		if err := buildAndPushLocal(ctx, cfg, imageTag, targetArchFlag); err != nil {
			return err
		}
	}

	// Resolve per-env config. Non-sensitive scalars are passed to KCL via
	// `-D key=value` so they bind to top-level identifiers in main.k.
	// Sensitive fields are emitted as secret refs by the deploy gen
	// pipeline (deploy/kcl/<env>/config_gen.k) and aren't piped through
	// here. Missing per-env config is non-fatal.
	envCfgKV := map[string]string{}
	if envConfig, lerr := config.LoadEnvironmentConfig(projectDir, envName); lerr == nil {
		for k, v := range envConfig {
			if s, ok := v.(string); ok && strings.HasPrefix(strings.TrimSpace(s), "${") {
				continue // secret refs handled by config_gen.k
			}
			envCfgKV[k] = fmt.Sprint(v)
		}
	}

	// Read rendered KCL to drive the host-mode rollout skip and the
	// per-Job waiter for one-shot CronJobs. Missing KCL render is logged
	// and treated as "no filter" (every Deployment in the namespace is
	// awaited), preserving the pre-orchestration behaviour for projects
	// that haven't migrated to the deploy module yet.
	//
	// Note: the warning prints earlier in the deploy sequence than it did
	// pre-extraction (before "Generating manifests..." rather than after
	// "Applying manifests..."). Strictly informational, and only fires on
	// an edge case (kcl JSON parse fails after the YAML render succeeds).
	entities, kerr := RenderKCL(ctx, projectDir, envName)
	if kerr != nil {
		fmt.Printf("Note: KCL entity read skipped (%v) — waiting on every Deployment in namespace.\n", kerr)
	}

	fmt.Printf("Generating manifests from %s...\n", mainK)

	// Build deploy groups from the rendered entities. Services bucket
	// by deploy target type: K8sCluster groups by (cluster, ns,
	// registry); VMDocker by ssh_host; Compose by compose_file. Host
	// / build-only services are skipped (those are forge run /
	// forge build territory).
	groups, gerr := buildDeployGroups(envName, entities, namespace)
	if gerr != nil {
		return fmt.Errorf("group services for deploy: %w", gerr)
	}

	// When no K8sCluster groups are present, the rendered set carries
	// only vm-docker / compose / host / build-only — nothing to apply
	// via the cluster pipeline. Skip the check above (no namespace)
	// and let dispatchDeployGroups handle the stub paths or no-op
	// trivially.
	if len(groups) == 0 {
		// Nothing to dispatch — historical behaviour was to still run
		// cluster.Apply against the env's main.k in case host-only
		// entities still produced manifests (CronJobs etc.). Preserve
		// that with one direct call.
		if err := cluster.Apply(ctx, cluster.ApplyOpts{
			MainK:        mainK,
			ImageTag:     imageTag,
			Namespace:    namespace,
			EnvConfigKV:  envCfgKV,
			DryRun:       dryRun,
			DryRunFramed: true,
			Prune:        prune,
			HostSkip:     hostDeploymentSkipSetFromKCL(cfg, entities),
			OneShotJobs:  oneShotJobNamesFromKCL(entities),
		}); err != nil {
			return err
		}
	} else {
		// Dispatch each group through its provider. The K8sCluster
		// provider wraps cluster.Apply via the builder closure so the
		// per-call envelope (mainK / image tag / env config / dry-run
		// / prune / host-skip / one-shot jobs) flows through verbatim.
		hostSkip := hostDeploymentSkipSetFromKCL(cfg, entities)
		oneShotJobs := oneShotJobNamesFromKCL(entities)
		builder := applyOptsBuilderFromContext(mainK, imageTag, namespace, envCfgKV, dryRun, prune, hostSkip, oneShotJobs)
		registry := deploytarget.NewRegistry()
		registry.Register(deploytarget.K8sClusterProvider{ApplyOptsBuilder: builder})
		if err := dispatchDeployGroups(ctx, registry, groups, ""); err != nil {
			return err
		}
	}

	if dryRun {
		return nil
	}

	fmt.Printf("\nDeploy completed in %s.\n", time.Since(start).Truncate(time.Millisecond))
	return nil
}

// oneShotJobNamesFromKCL returns the names of every CronJob entity with
// an empty Schedule — these render as one-shot Jobs and the deploy
// waits on `condition=complete` before returning. Scheduled CronJobs
// (non-empty Schedule) are excluded; they run on their own cadence.
func oneShotJobNamesFromKCL(e *KCLEntities) []string {
	if e == nil {
		return nil
	}
	var out []string
	for _, cj := range e.CronJobs {
		if cj.Schedule == "" {
			out = append(out, cj.Name)
		}
	}
	return out
}

// hostDeploymentSkipSetFromKCL returns the set of Deployment names that
// the deploy's rollout wait should skip — services declared `deploy: host`
// in the rendered KCL. Each host service name expands to two keys:
//
//   - the bare name ("admin-server"), matching per-service-binary mode
//   - the project-prefixed name ("<project>-admin-server"), matching
//     shared-binary mode where KCL renders `<project>-<svc>` Deployments
//
// Returning both is cheap and lets the caller iterate over actually-
// applied Deployment names without re-deriving the project-prefix rule.
// Empty entity set → empty skip set (legacy behaviour preserved).
func hostDeploymentSkipSetFromKCL(cfg *config.ProjectConfig, e *KCLEntities) map[string]struct{} {
	out := map[string]struct{}{}
	if cfg == nil || e == nil {
		return out
	}
	for _, name := range e.HostServiceNames() {
		out[name] = struct{}{}
		out[cfg.Name+"-"+name] = struct{}{}
	}
	return out
}

// resolveDeployImageTag is the precedence chain `forge deploy <env>`
// runs to pick the image tag for the KCL manifest render. Returns the
// resolved tag and a human-readable description of where it came from
// (printed in the deploy summary so users can debug a surprising
// choice without re-deriving the priority order in their head).
//
// Priority:
//
//  1. flagOverride — `--tag` (or its `--image-tag` alias) on the CLI.
//     CI pipelines that pin a release number land here.
//  2. .forge/state/build-<env>.json — what `forge build --push` last
//     pushed for this env. This is the load-bearing path that closes
//     the build/deploy tag divergence: build records the exact tag it
//     pushed, deploy reads it back, the working-tree state between the
//     two phases stops mattering.
//  3. resolveImageTag — git-derived fallback (`git describe --tags
//     --always --dirty`). The standalone-deploy path: no preceding
//     build, no override — recompute the tag the way build would.
//
// Errors:
//
//   - flagOverride bypasses every error path (the user told us
//     exactly what to use).
//   - A present-but-unreadable state file is a hard error — silently
//     falling back would mask a real bug.
//   - A missing state file is fine; the function falls through to git.
//   - A git-derivation failure on the fallback path is wrapped with a
//     "pass --tag to override" hint so users have an escape hatch.
func resolveDeployImageTag(ctx context.Context, projectDir, envName, flagOverride string) (string, string, error) {
	if flagOverride != "" {
		return flagOverride, "explicit --tag flag", nil
	}
	st, berr := ReadBuildState(projectDir, envName)
	if berr != nil {
		return "", "", fmt.Errorf("read build state: %w (delete .forge/state/build-%s.json to recompute from git)", berr, envName)
	}
	if st != nil {
		return st.Tag, fmt.Sprintf(".forge/state/build-%s.json (pushed %s)", envName, st.PushedAt), nil
	}
	t, terr := resolveImageTag(ctx, envName)
	if terr != nil {
		return "", "", fmt.Errorf("failed to determine image tag: %w\nUse --tag to specify one manually", terr)
	}
	return t, "git describe --tags --always --dirty", nil
}

func gitShortSHA(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func ensureDevCluster(ctx context.Context) error {
	fmt.Println("Checking k3d cluster...")
	out, err := exec.CommandContext(ctx, "k3d", "cluster", "list", "-o", "json").Output()
	if err != nil {
		return fmt.Errorf("k3d not available: %w\nInstall k3d: https://k3d.io", err)
	}

	// If no clusters exist or our cluster isn't found, the user needs to create one.
	if len(out) == 0 || string(out) == "[]" || string(out) == "[]\n" {
		fmt.Println("No k3d clusters found. Creating dev cluster...")
		k3dConfig := filepath.Join("deploy", "k3d.yaml")
		var createCmd *exec.Cmd
		if _, err := os.Stat(k3dConfig); err == nil {
			createCmd = exec.CommandContext(ctx, "k3d", "cluster", "create", "--config", k3dConfig)
		} else {
			// Fallback create path (no project-level deploy/k3d.yaml).
			// Write a temp registries.yaml that mirrors the canonical
			// `localhost:5050 → registry.localhost:5000` mapping, so
			// in-cluster pulls succeed for images pushed to the
			// host-visible `localhost:5050`. Without this, `docker push
			// localhost:5050/<image>` lands in the registry but pods
			// ImagePullBackOff because `localhost:5050` doesn't resolve
			// from inside the node container. The project-templated
			// `deploy/k3d.yaml` carries the same mirrors inline via the
			// k3d Simple config's `registries.config` block — see
			// internal/templates/deploy/k3d.yaml.tmpl.
			regsPath, regsErr := writeFallbackRegistriesYAML()
			if regsErr != nil {
				return fmt.Errorf("write fallback registries.yaml: %w", regsErr)
			}
			defer func() { _ = os.Remove(regsPath) }()
			createCmd = exec.CommandContext(ctx, "k3d", "cluster", "create", "dev",
				"--registry-create", "dev-registry:0.0.0.0:5050",
				"--registry-config", regsPath,
				"--servers", "1",
				"--no-lb",
			)
		}
		createCmd.Stdout = os.Stdout
		createCmd.Stderr = os.Stderr
		if err := createCmd.Run(); err != nil {
			return fmt.Errorf("failed to create k3d cluster: %w", err)
		}
	} else {
		fmt.Println("  k3d cluster found.")
		fmt.Println("  Tip: if in-cluster image pulls fail with `localhost:5050`,")
		fmt.Println("       the cluster pre-dates forge's auto-mirror config. See")
		fmt.Println("       the deploy skill ('Pre-existing k3d cluster mirror fix').")
	}
	return nil
}

// fallbackRegistriesYAML is the canonical containerd mirror config
// written when forge creates a k3d cluster without a project-level
// `deploy/k3d.yaml` to drive it. Kept as a top-level const so the
// content is reviewable in one place.
const fallbackRegistriesYAML = `mirrors:
  "registry.localhost:5000":
    endpoint:
      - http://registry.localhost:5000
  "registry.localhost:5050":
    endpoint:
      - http://registry.localhost:5000
  "localhost:5050":
    endpoint:
      - http://registry.localhost:5000
`

// writeFallbackRegistriesYAML writes the canonical mirror config to a
// temp file and returns the path. Caller is responsible for removing
// it after `k3d cluster create` returns.
func writeFallbackRegistriesYAML() (string, error) {
	f, err := os.CreateTemp("", "forge-k3d-registries-*.yaml")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(fallbackRegistriesYAML); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func buildAndPushLocal(ctx context.Context, cfg *config.ProjectConfig, tag, targetArchFlag string) error {
	registry := "localhost:5050"
	if reg := k8sClusterRegistryForEnv(ctx, "dev"); reg != "" {
		registry = reg
	}

	// Build and push the single project image from root Dockerfile.
	dockerfile := "Dockerfile"
	if _, err := os.Stat(dockerfile); os.IsNotExist(err) {
		fmt.Printf("  Skipping %s (no Dockerfile)\n", cfg.Name)
		return nil
	}

	imageRef := fmt.Sprintf("%s/%s:%s", registry, cfg.Name, tag)

	// Skip the rebuild if the image is already present at the tag (e.g.
	// the user just ran `forge build --push` against the same registry).
	// `docker manifest inspect` is cheap (HEAD against the registry) and
	// avoids an O(minutes) docker build + push on the hot path.
	if imageExistsInRegistry(ctx, imageRef) {
		fmt.Printf("  %s already present — skipping rebuild.\n", imageRef)
		return nil
	}

	fmt.Printf("  Building and pushing %s...\n", imageRef)

	// Resolve cross-compile target: --target-arch flag > forge.yaml
	// deploy.target_arch > "amd64" (k8s host default). When the resolved
	// target equals the host arch, no --platform flag is emitted.
	crossArch := resolveDeployArch(cfg.Deploy.TargetArch, targetArchFlag)

	buildArgs := []string{"build"}
	if crossArch != "" {
		buildArgs = append(buildArgs, "--platform=linux/"+crossArch)
		fmt.Printf("  [build] cross-compiling for linux/%s (host: %s/%s)\n",
			crossArch, runtime.GOOS, runtime.GOARCH)
	}
	buildArgs = append(buildArgs, "-t", imageRef)
	// Apply docker.build_contexts from forge.yaml so sibling-checkout
	// replace directives resolve in the deploy-time rebuild too.
	for _, k := range sortedKeys(cfg.Docker.BuildContexts) {
		buildArgs = append(buildArgs, "--build-context", k+"="+cfg.Docker.BuildContexts[k])
	}
	buildArgs = append(buildArgs, "-f", dockerfile, ".")
	buildCmd := exec.CommandContext(ctx, "docker", buildArgs...)
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("docker build for %s failed: %w", cfg.Name, err)
	}

	pushCmd := exec.CommandContext(ctx, "docker", "push", imageRef)
	pushCmd.Stdout = os.Stdout
	pushCmd.Stderr = os.Stderr
	if err := pushCmd.Run(); err != nil {
		return fmt.Errorf("docker push for %s failed: %w", cfg.Name, err)
	}

	return nil
}

// resolveDeployArch picks the target GOARCH for a deploy build. The
// dispatch order is: explicit --target-arch override, then forge.yaml's
// deploy.target_arch, then the "amd64" default (which reflects the
// empirical reality that most k8s nodes are amd64). Returns the empty
// string when the resolved target equals runtime.GOARCH — the empty
// return signals callers that no --platform flag is needed.
//
// Unlike resolveBuildArch in build.go, this function always falls back
// to amd64 (i.e. deploy is treated as the docker-context case in
// build.go). `forge deploy` always builds an image destined for a
// cluster node, so the "no cross-compile, use host arch" outcome
// happens only when host == target.
func resolveDeployArch(cfgArch, flagArch string) string {
	target := flagArch
	if target == "" {
		target = cfgArch
	}
	if target == "" {
		target = "amd64"
	}
	if target == runtime.GOARCH {
		return ""
	}
	return target
}

// imageExistsInRegistry returns true when `docker manifest inspect` can
// resolve the given image:tag, i.e. it's already in the registry. Used
// by buildAndPushLocal to short-circuit the redundant deploy-time build
// when `forge build --push` has already pushed the same tag. Any error
// (manifest absent, registry unreachable, manifest API disabled) yields
// false so we fall through to the normal build+push path.
//
// For local/HTTP registries we use --insecure so the check works against
// the dev k3d registry (localhost:5051), which doesn't speak TLS.
func imageExistsInRegistry(ctx context.Context, ref string) bool {
	args := []string{"manifest", "inspect"}
	if isInsecureRegistry(ref) {
		args = append(args, "--insecure")
	}
	args = append(args, ref)
	cmd := exec.CommandContext(ctx, "docker", args...)
	return cmd.Run() == nil
}

// isInsecureRegistry reports whether the image ref points at a registry
// that should be treated as HTTP. We treat localhost / 127.0.0.1 /
// registry.localhost as insecure — these are the dev-cluster k3d
// registries forge sets up. Anything else (ghcr.io, gcr.io, AR…) is
// HTTPS by default.
func isInsecureRegistry(ref string) bool {
	host, _, _ := strings.Cut(ref, "/")
	hostOnly, _, _ := strings.Cut(host, ":")
	switch hostOnly {
	case "localhost", "127.0.0.1", "registry.localhost":
		return true
	}
	return false
}

// expectedClusterForEnv returns the expected kubectl context name for
// an environment. Resolution priority:
//  1. The rendered KCL's first K8sCluster.cluster for env <envName>
//  2. For dev: k3d-<project-name>
//  3. Empty string — no expectation declared (skip the guard)
//
// Reads the rendered KCL via RenderKCL using a background context so
// the lookup remains usable from the explain path where we don't carry
// a request context. Failures fall through to the dev default / empty
// — the env-cluster guard is a recommendation, not a hard dependency.
func expectedClusterForEnv(ctx context.Context, cfg *config.ProjectConfig, envName string) string {
	if cluster := firstK8sClusterField(ctx, envName, "cluster"); cluster != "" {
		return cluster
	}
	if envName == "dev" && cfg != nil {
		// Dev's default is the k3d cluster forge deploy dev creates.
		return "k3d-" + cfg.Name
	}
	return ""
}

// firstK8sClusterField reads the rendered KCL for env and returns the
// requested field ("cluster" / "namespace" / "registry" / "domain")
// from the first service whose Deploy is K8sCluster-shaped. Returns ""
// when KCL can't be rendered, no service is cluster-shaped, or the
// requested field is empty across every service.
func firstK8sClusterField(ctx context.Context, envName, field string) string {
	if envName == "" {
		return ""
	}
	entities, err := RenderKCL(ctx, projectDirForKCL(), envName)
	if err != nil || entities == nil {
		return ""
	}
	for _, svc := range entities.Services {
		if svc.Deploy.Type != "cluster" || svc.Deploy.Cluster == nil {
			continue
		}
		c := svc.Deploy.Cluster
		switch field {
		case "cluster":
			if c.Cluster != "" {
				return c.Cluster
			}
		case "namespace":
			if c.Namespace != "" {
				return c.Namespace
			}
		case "registry":
			if c.Registry != "" {
				return c.Registry
			}
		case "domain":
			if c.Domain != "" {
				return c.Domain
			}
		}
	}
	return ""
}

// k8sClusterNamespaceForEnv reads the rendered KCL and returns the
// first K8sCluster.namespace declared for env. Returns "" when no
// cluster-shaped service is declared or the field is unset.
func k8sClusterNamespaceForEnv(ctx context.Context, envName string) string {
	return firstK8sClusterField(ctx, envName, "namespace")
}

// k8sClusterRegistryForEnv reads the rendered KCL and returns the
// first K8sCluster.registry declared for env. Returns "" when no
// cluster-shaped service is declared or the field is unset.
func k8sClusterRegistryForEnv(ctx context.Context, envName string) string {
	return firstK8sClusterField(ctx, envName, "registry")
}

// verifyKubectlContext refuses the deploy when the current kubectl
// context doesn't match the env's expected cluster. An empty expected
// value (no declaration in forge.yaml for non-dev envs) skips the
// guard — projects opt in by declaring environments[<env>].cluster.
// An explicit --context override also skips the guard, but emits a
// notice so the override is visible in the deploy log.
func verifyKubectlContext(ctx context.Context, cfg *config.ProjectConfig, envName, override string) error {
	if override != "" {
		fmt.Printf("kubectl context override: %s (env-cluster guard skipped)\n", override)
		// Switch the context so the apply lands in the right place.
		switchCmd := exec.CommandContext(ctx, "kubectl", "config", "use-context", override)
		switchCmd.Stdout = os.Stdout
		switchCmd.Stderr = os.Stderr
		if err := switchCmd.Run(); err != nil {
			return fmt.Errorf("kubectl config use-context %s: %w", override, err)
		}
		return nil
	}

	expected := expectedClusterForEnv(ctx, cfg, envName)
	if expected == "" {
		// No expectation declared for this env. Print a one-liner
		// reminder so users know they can lock it down, but don't
		// block the deploy (backwards-compatible default).
		fmt.Printf("Note: no forge.K8sCluster.cluster declared in deploy/kcl/%s/main.k — kubectl-context guard skipped.\n", envName)
		return nil
	}

	currentCmd := exec.CommandContext(ctx, "kubectl", "config", "current-context")
	out, err := currentCmd.Output()
	if err != nil {
		return fmt.Errorf("kubectl config current-context: %w (is kubectl installed and configured?)", err)
	}
	current := strings.TrimSpace(string(out))
	if err := kubectlContextGuardVerdict(envName, expected, current); err != nil {
		return err
	}
	fmt.Printf("kubectl context: %s (matches env %s)\n", current, envName)
	return nil
}

// kubectlContextGuardVerdict is the pure comparison core of the
// kubectl-context guard. Lifted out so unit tests can exercise the
// mismatch path without shelling to kubectl. Returns nil when current
// matches expected (or when expected is empty — guard skipped), and
// the user-facing refusal message otherwise.
func kubectlContextGuardVerdict(envName, expected, current string) error {
	if expected == "" || current == expected {
		return nil
	}
	return fmt.Errorf(
		"kubectl context mismatch for env %q:\n"+
			"  expected: %s\n"+
			"  current:  %s\n"+
			"\n"+
			"refusing to deploy. Fix with one of:\n"+
			"  kubectl config use-context %s\n"+
			"  forge deploy %s --context %s   (CI/legitimate override)",
		envName, expected, current, expected, envName, current)
}
