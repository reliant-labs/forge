package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

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

	// Dev environment: ensure k3d cluster and push images locally.
	if envName == "dev" {
		if err := ensureDevCluster(); err != nil {
			return err
		}

		if err := buildAndPushLocal(cfg, imageTag); err != nil {
			return err
		}
	}

	// Generate manifests with KCL.
	fmt.Printf("Generating manifests from %s...\n", mainK)
	manifests, err := runKCL(mainK, imageTag, namespace)
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

	// Wait for rollouts.
	fmt.Println("Waiting for rollouts...")
	for _, svc := range cfg.Services {
		if err := waitForRollout(svc.Name, namespace); err != nil {
			fmt.Printf("  Warning: rollout for %s: %v\n", svc.Name, err)
		} else {
			fmt.Printf("  %s: ready\n", svc.Name)
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
			createCmd = exec.Command("k3d", "cluster", "create", "dev",
				"--registry-create", "dev-registry:0.0.0.0:5050",
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
	}
	return nil
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

func runKCL(mainK, imageTag, namespace string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("kcl", "run", mainK,
		"-D", "image_tag="+imageTag,
		"-D", "namespace="+namespace,
	)
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
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