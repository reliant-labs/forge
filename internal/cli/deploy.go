package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/config"
)

func newDeployCmd() *cobra.Command {
	var (
		imageTag  string
		dryRun    bool
		namespace string
	)

	cmd := &cobra.Command{
		Use:   "deploy <environment>",
		Short: "Deploy services to a Kubernetes environment",
		Long: `Deploy services to the specified Kubernetes environment using KCL manifests.

The environment must correspond to a directory under deploy/kcl/<env>/.

For dev environments, the command ensures a k3d cluster exists and pushes images
to the local registry at localhost:5050.

Examples:
  forge deploy dev                       # Deploy to dev (local k3d)
  forge deploy staging --image-tag v1.2  # Deploy to staging with specific tag
  forge deploy prod --dry-run            # Preview production manifests
  forge deploy dev --namespace custom-ns # Override namespace`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeploy(args[0], imageTag, dryRun, namespace)
		},
	}

	cmd.Flags().StringVar(&imageTag, "image-tag", "", "Image tag (default: git short SHA)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print manifests without applying")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Override namespace from environment config")

	return cmd
}

func runDeploy(envName, imageTag string, dryRun bool, namespace string) error {
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
		tag, err := gitShortSHA()
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

	start := time.Now()

	// Dev environment: ensure k3d cluster and push images locally. Skip
	// the cluster bootstrap and docker build/push when --dry-run is set —
	// dry-run only renders manifests and never touches the cluster or
	// registry, so the slow image step is dead weight.
	if envName == "dev" && !dryRun {
		if err := ensureDevCluster(); err != nil {
			return err
		}

		if err := buildAndPushLocal(cfg, imageTag); err != nil {
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
	manifests, err := runKCL(mainK, imageTag, namespace, envCfgKV)
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
	if err := kubectlApply(manifests); err != nil {
		return fmt.Errorf("kubectl apply failed: %w", err)
	}

	// Wait for rollouts. Discover the actually-applied Deployment names
	// from the cluster rather than guess from cfg.Services — the schema
	// prefixes shared-binary deployments with `<project>-<svc>` and KCL
	// renders may add suffixes for component types (operator/worker).
	fmt.Println("Waiting for rollouts...")
	deployments, err := listDeployments(namespace)
	if err != nil {
		fmt.Printf("  Warning: list deployments: %v\n", err)
	} else {
		for _, dep := range deployments {
			if err := waitForRollout(dep, namespace); err != nil {
				fmt.Printf("  Warning: rollout for %s: %v\n", dep, err)
			} else {
				fmt.Printf("  %s: ready\n", dep)
			}
		}
	}

	fmt.Printf("\nDeploy completed in %s.\n", time.Since(start).Truncate(time.Millisecond))
	return nil
}

func gitShortSHA() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func ensureDevCluster() error {
	fmt.Println("Checking k3d cluster...")
	out, err := exec.Command("k3d", "cluster", "list", "-o", "json").Output()
	if err != nil {
		return fmt.Errorf("k3d not available: %w\nInstall k3d: https://k3d.io", err)
	}

	// If no clusters exist or our cluster isn't found, the user needs to create one.
	if len(out) == 0 || string(out) == "[]" || string(out) == "[]\n" {
		fmt.Println("No k3d clusters found. Creating dev cluster...")
		k3dConfig := filepath.Join("deploy", "k3d.yaml")
		var createCmd *exec.Cmd
		if _, err := os.Stat(k3dConfig); err == nil {
			createCmd = exec.Command("k3d", "cluster", "create", "--config", k3dConfig)
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
			defer os.Remove(regsPath)
			createCmd = exec.Command("k3d", "cluster", "create", "dev",
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
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func buildAndPushLocal(cfg *config.ProjectConfig, tag string) error {
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
	fmt.Printf("  Building and pushing %s...\n", imageRef)

	buildCmd := exec.Command("docker", "build",
		"-t", imageRef,
		"-f", dockerfile,
		".",
	)
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("docker build for %s failed: %w", cfg.Name, err)
	}

	pushCmd := exec.Command("docker", "push", imageRef)
	pushCmd.Stdout = os.Stdout
	pushCmd.Stderr = os.Stderr
	if err := pushCmd.Run(); err != nil {
		return fmt.Errorf("docker push for %s failed: %w", cfg.Name, err)
	}

	return nil
}

func runKCL(mainK, imageTag, namespace string, envCfg map[string]string) (string, error) {
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
	cmd := exec.Command("kcl", args...)
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

func kubectlApply(manifests string) error {
	cmd := exec.Command("kubectl", "apply", "--server-side", "-f", "-")
	cmd.Stdin = strings.NewReader(manifests)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func waitForRollout(name, namespace string) error {
	cmd := exec.Command("kubectl", "rollout", "status",
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
func listDeployments(namespace string) ([]string, error) {
	cmd := exec.Command("kubectl", "get", "deployments",
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