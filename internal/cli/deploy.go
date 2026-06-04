package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/config"
)

func newDeployCmd() *cobra.Command {
	var (
		imageTag        string
		dryRun          bool
		namespace       string
		contextOverride string
		explain         bool
		targetArch      string
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
the environment's expected cluster (configured under environments[].cluster
in forge.yaml; defaults to k3d-<project> for dev). The check ALSO runs under
--dry-run so wrong-context mistakes surface before the strict apply.

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
			return runDeploy(cmd.Context(), args[0], deployOptions{
				imageTag:        imageTag,
				dryRun:          dryRun,
				namespace:       namespace,
				contextOverride: contextOverride,
				targetArch:      targetArch,
			})
		},
	}

	cmd.Flags().StringVar(&imageTag, "image-tag", "", "Image tag (default: git short SHA)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print manifests without applying (env-cluster guard still runs)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Override namespace from environment config")
	cmd.Flags().StringVar(&contextOverride, "context", "", "Override expected kubectl context (skips the env-cluster guard)")
	cmd.Flags().BoolVar(&explain, "explain", false, "Print the env-cluster guard decision (expected/current/verdict) and exit")
	cmd.Flags().StringVar(&targetArch, "target-arch", "", "Override target GOARCH for cross-compilation (default: forge.yaml deploy.target_arch, then amd64)")

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
	expected := expectedClusterForEnv(cfg, envName)
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
		fmt.Printf("  hint:             declare `environments[%s].cluster: <context>` in forge.yaml to enable the guard\n", envName)
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

// printDeployExplainHostSkip surfaces the dev-only host-mode service
// list under `forge deploy <env> --explain` so users can see, without
// running an apply, which services would be skipped from rollout wait
// + prune. No-op for non-dev envs and for projects with no host-mode
// services, so it composes safely with the verdict-only path.
func printDeployExplainHostSkip(cfg *config.ProjectConfig, envName string) error {
	if envName != "dev" {
		return nil
	}
	hosts := hostDevTargetServices(cfg)
	if len(hosts) == 0 {
		return nil
	}
	fmt.Printf("  host-mode (dev_target: host): %s — run via `forge run <name>` (rollout wait skipped)\n",
		strings.Join(hosts, ", "))
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
}

func runDeploy(ctx context.Context, envName string, opts deployOptions) error {
	imageTag := opts.imageTag
	dryRun := opts.dryRun
	namespace := opts.namespace
	contextOverride := opts.contextOverride
	targetArchFlag := opts.targetArch
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

	// Resolve image tag.
	if imageTag == "" {
		tag, err := gitShortSHA(ctx)
		if err != nil {
			return fmt.Errorf("failed to determine image tag: %w\nUse --image-tag to specify one manually", err)
		}
		imageTag = tag
	}

	// Resolve namespace.
	if namespace == "" {
		if env := findEnvironment(cfg, envName); env != nil && env.Namespace != "" {
			namespace = env.Namespace
		} else {
			namespace = cfg.Name + "-" + envName
		}
	}

	fmt.Printf("Deploying project: %s\n", cfg.Name)
	fmt.Printf("  Environment: %s\n", envName)
	fmt.Printf("  Image tag:   %s\n", imageTag)
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
	projectDir := "."
	if cfgPath, perr := findProjectConfigFile(); perr == nil {
		projectDir = filepath.Dir(cfgPath)
	}
	if envConfig, lerr := config.LoadEnvironmentConfig(cfg, projectDir, envName); lerr == nil {
		for k, v := range envConfig {
			if s, ok := v.(string); ok && strings.HasPrefix(strings.TrimSpace(s), "${") {
				continue // secret refs handled by config_gen.k
			}
			envCfgKV[k] = fmt.Sprint(v)
		}
	}

	// Generate manifests with KCL.
	fmt.Printf("Generating manifests from %s...\n", mainK)
	manifests, err := runKCL(ctx, mainK, imageTag, namespace, envCfgKV)
	if err != nil {
		return fmt.Errorf("KCL manifest generation failed: %w", err)
	}

	if dryRun {
		fmt.Println("\n--- Generated Manifests (dry-run) ---")
		fmt.Println(manifests)
		fmt.Println("--- End Manifests ---")
		fmt.Println("\nDry run complete. No changes applied.")
		return nil
	}

	// Apply manifests.
	fmt.Println("Applying manifests...")
	if err := kubectlApply(ctx, manifests); err != nil {
		return fmt.Errorf("kubectl apply failed: %w", err)
	}

	// Wait for rollouts. Discover the actually-applied Deployment names
	// from the cluster rather than guess from cfg.Services — the schema
	// prefixes shared-binary deployments with `<project>-<svc>` and KCL
	// renders may add suffixes for component types (operator/worker).
	//
	// Dev-only host-mode filter: when env=dev, services with
	// `dev_target: host` are excluded from the rollout wait so the
	// 120s/service kubectl rollout-status budget isn't burned waiting on
	// Deployments that don't exist (or that the deploy intentionally
	// pruned). For staging/prod every Deployment is awaited unchanged.
	fmt.Println("Waiting for rollouts...")
	deployments, err := listDeployments(ctx, namespace)
	if err != nil {
		fmt.Printf("  Warning: list deployments: %v\n", err)
	} else {
		hostSkip := hostDeploymentSkipSet(cfg, envName)
		var skipped []string
		for _, dep := range deployments {
			if _, skip := hostSkip[dep]; skip {
				skipped = append(skipped, dep)
				continue
			}
			if err := waitForRollout(ctx, dep, namespace); err != nil {
				fmt.Printf("  Warning: rollout for %s: %v\n", dep, err)
			} else {
				fmt.Printf("  %s: ready\n", dep)
			}
		}
		if len(skipped) > 0 {
			fmt.Printf("Skipped rollout wait for %d service(s) with dev_target: host: %s\n",
				len(skipped), strings.Join(skipped, ", "))
		}
	}

	fmt.Printf("\nDeploy completed in %s.\n", time.Since(start).Truncate(time.Millisecond))
	return nil
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
	if env := findEnvironment(cfg, "dev"); env != nil && env.Registry != "" {
		registry = env.Registry
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

func runKCL(ctx context.Context, mainK, imageTag, namespace string, envCfg map[string]string) (string, error) {
	var out bytes.Buffer
	args := []string{"run", mainK,
		"-D", "image_tag=" + imageTag,
		"-D", "namespace=" + namespace,
	}
	// Stable ordering for reproducible output / easier diffing.
	keys := make([]string, 0, len(envCfg))
	for k := range envCfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-D", k+"="+envCfg[k])
	}
	cmd := exec.CommandContext(ctx, "kcl", args...)
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", err
	}
	return extractManifests(out.Bytes())
}

// extractManifests pulls the `manifests` list out of KCL's YAML output and
// emits each item as its own YAML document, separated by `---`.
//
// KCL's `kcl run` always emits the program's top-level variables wrapped in
// a YAML object (e.g. `manifests:\n- apiVersion: ...`), but `kubectl apply`
// expects a plain document stream. Convention is that the canonical
// rendering returns a list of resources from `render.render_environment` and
// assigns it to a top-level `manifests` variable; this helper unwraps that
// assignment and re-emits the items with `---` separators.
//
// All other top-level KCL variables (image_tag, registry, etc.) MUST be
// declared as private (underscore-prefix) so they don't pollute the
// document stream — the unwrapping logic only handles the `manifests`
// key. Stable: anything else under the wrapper is dropped with a warning,
// not silently included.
func extractManifests(kclOutput []byte) (string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(kclOutput, &doc); err != nil {
		return "", fmt.Errorf("parse kcl output: %w", err)
	}
	raw, ok := doc["manifests"]
	if !ok {
		return "", fmt.Errorf("kcl output has no top-level `manifests` key; main.k must end with `manifests = render.render_environment(...)` and other top-level vars must be private (underscore-prefix)")
	}
	items, ok := raw.([]any)
	if !ok {
		return "", fmt.Errorf("`manifests` is not a list (got %T)", raw)
	}
	for k := range doc {
		if k != "manifests" {
			fmt.Fprintf(os.Stderr, "warning: ignoring extra top-level KCL var %q (mark as private with `_%s = ...` to suppress)\n", k, k)
		}
	}

	var sb strings.Builder
	for i, it := range items {
		if i > 0 {
			sb.WriteString("---\n")
		}
		b, err := yaml.Marshal(it)
		if err != nil {
			return "", fmt.Errorf("marshal manifest item %d: %w", i, err)
		}
		sb.Write(b)
	}
	return sb.String(), nil
}

func kubectlApply(ctx context.Context, manifests string) error {
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "--server-side", "-f", "-")
	cmd.Stdin = strings.NewReader(manifests)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func waitForRollout(ctx context.Context, name, namespace string) error {
	cmd := exec.CommandContext(ctx, "kubectl", "rollout", "status",
		"deployment/"+name,
		"-n", namespace,
		"--timeout=120s",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// listDeployments returns the names of all Deployments in the given
// namespace that forge owns (managed-by=forge label). This is the
// authoritative list for rollout watching — it covers shared-binary
// `<project>-<svc>` names, per-service `<svc>` names, operator and
// worker deployments, and anything packs add — without forge having to
// guess naming schemes per scaffold mode.
func listDeployments(ctx context.Context, namespace string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "get", "deployments",
		"-n", namespace,
		"-l", "app.kubernetes.io/managed-by=forge",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	names := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}

// hostDeploymentSkipSet returns the set of Deployment names that the
// dev-only host-mode filter should skip when waiting on rollouts (or
// pruning stale resources). For env != "dev" the set is empty — the
// filter only applies to the local dev loop.
//
// Each host-mode service name expands to two keys:
//   - the bare name ("admin-server"), matching per-service-binary mode
//   - the project-prefixed name ("<project>-admin-server"), matching
//     shared-binary mode where KCL renders `<project>-<svc>` Deployments
//
// Returning both is cheap and lets the caller iterate over the actually-
// applied Deployment names without re-deriving the project-prefix rule.
func hostDeploymentSkipSet(cfg *config.ProjectConfig, envName string) map[string]struct{} {
	out := map[string]struct{}{}
	if cfg == nil || envName != "dev" {
		return out
	}
	for _, name := range hostDevTargetServices(cfg) {
		out[name] = struct{}{}
		out[cfg.Name+"-"+name] = struct{}{}
	}
	return out
}

// expectedClusterForEnv returns the expected kubectl context name for
// an environment. Resolution priority:
//  1. environments[<env>].cluster from forge.yaml
//  2. For dev: k3d-<project-name>
//  3. Empty string — no expectation declared (skip the guard)
func expectedClusterForEnv(cfg *config.ProjectConfig, envName string) string {
	if env := findEnvironment(cfg, envName); env != nil && env.Cluster != "" {
		return env.Cluster
	}
	if envName == "dev" {
		// Dev's default is the k3d cluster forge deploy dev creates.
		return "k3d-" + cfg.Name
	}
	return ""
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

	expected := expectedClusterForEnv(cfg, envName)
	if expected == "" {
		// No expectation declared for this env. Print a one-liner
		// reminder so users know they can lock it down, but don't
		// block the deploy (backwards-compatible default).
		fmt.Printf("Note: no environments[%s].cluster declared in forge.yaml — kubectl-context guard skipped.\n", envName)
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
