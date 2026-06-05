// Package cli — `forge dev` command tree.
//
// The `dev` subtree consolidates the universal local-cluster lifecycle
// mechanics every k8s-targeting forge project would otherwise hand-write
// in bash:
//
//   - cluster up/down/status/reset/reload    (k3d cluster lifecycle)
//   - urls                                   (ingress URL table for the env)
//   - logs / status / info / instances       (per-namespace introspection)
//
// kubectl port-forward isn't wrapped here — the Gateway API ingress
// path (forge dev urls) is the canonical "reach a service from the
// host" entry point. Ad-hoc port-forwards for stateful workloads
// (database shells, debug metrics endpoints) are `kubectl
// port-forward` directly.
//
// Project-specific orchestration (sibling-repo deploys, helm bootstraps,
// Stripe webhook listeners, per-tenant seeds) is NOT owned by forge —
// projects keep those in scripts/ and Taskfile.yml, composed with the
// forge dev primitives. See the `dev` skill for the boundary doc.
package cli

import (
	"github.com/spf13/cobra"
)

// newDevCmd builds the `forge dev` parent command. Subcommands are
// registered here so `forge dev --help` lists them in a stable order.
func newDevCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "Local-cluster dev loop primitives",
		Long: `Local-cluster dev loop primitives.

forge dev owns the universal mechanics every k8s-targeting forge project
needs: cluster lifecycle, ingress URLs, status, logs. Project-specific
orchestration (sibling-repo deploys, helm chart bootstraps, webhook
listeners) lives in your scripts/ and Taskfile.yml — composed with the
forge dev primitives.

Examples:
  forge dev cluster up          # create k3d cluster from deploy/k3d.yaml
  forge dev cluster reload      # re-render KCL + kubectl apply + wait rollout
  forge dev urls                # print the ingress URL table for the env
  forge dev status              # cluster + pods + ingress URLs
  forge dev logs --service api  # kubectl logs -f for a service
  forge dev instances           # list every cp-forge dev namespace on the host`,
	}

	cmd.AddCommand(newDevClusterCmd())
	cmd.AddCommand(newDevStatusCmd())
	cmd.AddCommand(newDevLogsCmd())
	cmd.AddCommand(newDevInfoCmd())
	cmd.AddCommand(newDevUrlsCmd())
	cmd.AddCommand(newDevInstancesCmd())

	return cmd
}
