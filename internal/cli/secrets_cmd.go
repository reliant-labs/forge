package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// newSecretsCmd is the `forge secrets` command group. Today it exposes a
// single `sync` subcommand that materializes the k8s Secrets an env's
// dotenv secret_provider implies — the standalone primitive CI lanes need
// so they can provision cluster secrets WITHOUT a full `forge deploy`
// (which would also bring up compose/host targets). External-provider
// envs are a no-op (forge never renders their values).
func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Project the env's secret_provider into the cluster",
		Long: `Work with the secrets an environment's bundle secret_provider implies.

A secret is declared once as a reference (EnvVar.secret_ref) and its value
comes from the env's bundle secret_provider. For a DotenvSecrets provider,
forge can render the declared cluster secret_refs into k8s Secret objects
and apply them. ExternalSecrets envs are a no-op (their values are
provisioned out-of-band).`,
	}
	cmd.AddCommand(newSecretsSyncCmd())
	return cmd
}

// newSecretsSyncCmd renders + applies the k8s Secrets for an env whose
// bundle declares a DotenvSecrets provider. It is the same projection the
// deploy phase runs (applyK8sSecretsFromProvider) lifted into a standalone
// command, so a CI lane can do `forge secrets sync --env dev` before its
// `kcl run | kubectl apply` and retire a hand-rolled secret-bootstrap
// script. Guarded to LOCAL clusters (it renders plaintext); fail-fasts on
// a declared ref the dotenv can't supply.
func newSecretsSyncCmd() *cobra.Command {
	var env string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Render + apply the k8s Secrets for an env's dotenv secret_provider (local clusters only)",
		Long: `Render the k8s Secret objects implied by an environment's bundle
secret_provider and apply them to the current kubectl context.

Only DotenvSecrets providers produce output — forge reads the gitignored
dotenv (keyed by env-var name), builds a Secret per declared cluster
secret_ref, and applies it. ExternalSecrets / no provider are a no-op.
The dotenv renders PLAINTEXT, so this refuses any non-local cluster.

Use it in a CI test lane (k3d/kind) to provision cluster secrets the
forge-native way instead of a bespoke create-secret script:

  forge secrets sync --env dev
  kcl run deploy/kcl/dev/main.k -D image_tag=ci | kubectl apply -f -`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if env == "" {
				return fmt.Errorf("--env is required (e.g. --env=dev)")
			}
			return runSecretsSync(cmd.Context(), env, dryRun)
		},
	}
	cmd.Flags().StringVar(&env, "env", "", "Environment whose secret_provider to sync (e.g. dev) — required")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the Secret manifests instead of applying them")
	return cmd
}

// runSecretsSync renders the env's KCL, derives its namespace + deploy
// groups (the groups drive the local-cluster guard), and delegates to the
// shared applyK8sSecretsFromProvider projection.
func runSecretsSync(ctx context.Context, envName string, dryRun bool) error {
	store, err := loadProjectStore()
	if err != nil {
		return err
	}
	projectDir := projectDirForKCL()

	entities, err := RenderKCL(ctx, projectDir, envName)
	if err != nil {
		return fmt.Errorf("render KCL: %w", err)
	}

	// Namespace resolution mirrors runDeploy: per-env cluster namespace,
	// falling back to "<project>-<env>".
	namespace := k8sClusterNamespaceForEnv(ctx, envName)
	if namespace == "" {
		namespace = store.Meta().Name + "-" + envName
	}

	groups, gerr := buildDeployGroups(envName, entities, namespace)
	if gerr != nil {
		return fmt.Errorf("group services: %w", gerr)
	}

	// secrets sync is local-cluster-only and targets the current kubectl
	// context (no --context flag), so the context override is empty.
	return applyK8sSecretsFromProvider(ctx, entities, groups, namespace, "", envName, dryRun)
}
