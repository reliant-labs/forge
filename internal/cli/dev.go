// Package cli — `forge cluster` command tree.
//
// The `cluster` subtree consolidates the universal local-cluster
// lifecycle mechanics every k8s-targeting forge project would otherwise
// hand-write in bash, plus the dev-state introspection commands:
//
//   - up/down/reset/reload                   (k3d cluster lifecycle)
//   - status                                 (dynamic: cluster up/down, pods, ingress URLs)
//   - urls                                   (ingress URL table for the env)
//   - logs / info / instances                (per-namespace introspection)
//
// kubectl port-forward isn't wrapped here — the Gateway API ingress
// path (forge cluster urls) is the canonical "reach a service from the
// host" entry point. Ad-hoc port-forwards for stateful workloads
// (database shells, debug metrics endpoints) are `kubectl
// port-forward` directly.
//
// Project-specific orchestration (sibling-repo deploys, helm bootstraps,
// Stripe webhook listeners, per-tenant seeds) is NOT owned by forge —
// projects keep those in scripts/ and Taskfile.yml, composed with the
// forge cluster primitives. See the `dev` skill for the boundary doc.
package cli

import (
	"github.com/spf13/cobra"
)

// newClusterCmd builds the `forge cluster` parent command. The k3d
// lifecycle children (up/down/reset/reload) plus the dev-state
// introspection commands (status/logs/info/urls/instances) are
// registered here as a single flat namespace so `forge cluster --help`
// lists them in a stable order.
func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Manage the local k3d cluster and inspect dev state",
		Long: `Manage the local k3d development cluster and inspect dev-loop state.

forge cluster owns the universal mechanics every k8s-targeting forge
project needs: k3d cluster lifecycle, ingress URLs, status, logs.
Project-specific orchestration (sibling-repo deploys, helm chart
bootstraps, webhook listeners) lives in your scripts/ and Taskfile.yml —
composed with the forge cluster primitives.

The cluster config is read from deploy/k3d.yaml (override via --config).
Lifecycle subcommands pin the kubectl context to k3d-<cluster-name> as a
guardrail against accidental prod-context leaks.

Examples:
  forge cluster up             # create k3d cluster from deploy/k3d.yaml
  forge cluster reload         # re-render KCL + kubectl apply + wait rollout
  forge cluster urls           # print the ingress URL table for the env
  forge cluster status         # cluster + pods + ingress URLs
  forge cluster logs --service api  # kubectl logs -f for a service
  forge cluster instances      # list every forge dev namespace on the host`,
	}

	// k3d lifecycle children, promoted flat from the old `dev cluster`
	// nested namespace.
	cmd.AddCommand(newDevClusterUpCmd())
	cmd.AddCommand(newDevClusterDownCmd())
	cmd.AddCommand(newDevClusterResetCmd())
	cmd.AddCommand(newDevClusterReloadCmd())

	// `status` is the dev_status.go implementation (a superset of the old
	// nested `dev cluster status`): it shows cluster up/down + current
	// kubectl context + config path + pods + ingress URLs + siblings.
	cmd.AddCommand(newDevStatusCmd())

	// Dev-state introspection, promoted unchanged.
	cmd.AddCommand(newDevLogsCmd())
	cmd.AddCommand(newDevInfoCmd())
	cmd.AddCommand(newDevUrlsCmd())
	cmd.AddCommand(newDevInstancesCmd())

	return cmd
}
