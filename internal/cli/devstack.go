// Package cli — `forge devstack` command tree.
//
// The parallel-dev-stack primitives (ADR 0003) live in internal/devstack:
// the raw git facts pushed into KCL as options, and the memoized
// forge.allocate_port(base, key) block allocator. Those primitives are
// resolved INSIDE a KCL render (under the up/deploy activation path). But a
// host launcher — a Taskfile target, a bootstrap script — needs the SAME
// allocated host port BEFORE `forge up` renders the KCL, so it can start the
// host `reliant` process LISTENING on exactly the port the in-cluster
// controller will dial.
//
// `forge devstack port <base>` is that single source of truth: it resolves
// the current worktree key (devstack.Worktree) and returns
// allocate_port(base, key) — base + block(key)*100 — through the SAME
// lock-guarded block registry (.forge/blocks.json) the KCL builtin uses, so
// the launcher and the render can never disagree on the port. On the PRIMARY
// checkout the key is "" ⇒ block 0 ⇒ the base is returned unchanged (no
// registry/lock touch), so the default dev loop is byte-identical to today.
package cli

import (
	"fmt"
	"strconv"

	"github.com/reliant-labs/forge/internal/devstack"
	"github.com/spf13/cobra"
)

// newDevStackCmd builds the `forge devstack` parent command — the host-side
// surface of the parallel-dev-stack primitives. The KCL-side surface is the
// option("worktree")/option("branch") seam + the forge.allocate_port builtin;
// this command lets a launcher resolve the SAME values without a render.
func newDevStackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devstack",
		Short: "Parallel-dev-stack host helpers (worktree key + port allocation)",
		Long: `Host-side helpers for forge's parallel-dev-stack primitives (ADR 0003).

A launcher (Taskfile target, bootstrap script) that starts a host process
BEFORE 'forge up' renders the KCL needs the SAME host port the render will
allocate. 'forge devstack port' resolves it through the same lock-guarded
block registry (.forge/blocks.json) the forge.allocate_port KCL builtin uses,
so the launcher and the render can never drift.

On the PRIMARY checkout the worktree key is "" so every port is returned
unchanged (block 0) — the default dev loop is byte-identical to today. A
linked git worktree gets its own stable 100-port block.

Examples:
  forge devstack port 3091     # the reliant-api host port for this worktree
  forge devstack key           # the worktree key ("" on the primary checkout)`,
	}
	cmd.AddCommand(newDevStackPortCmd())
	cmd.AddCommand(newDevStackKeyCmd())
	cmd.AddCommand(newDevStackListCmd())
	return cmd
}

// newDevStackListCmd: `forge devstack list` → the registered worktree keys,
// one per line, sorted by block index. This is the source a DECLARATIVE
// per-stack config generator reads to enumerate the active stacks WITHOUT
// re-implementing worktree detection or the registry format.
//
// The DEFAULT stack (key "") is implicit — it is never stored in the registry
// and is NOT printed here. A generator MUST always emit the default's config
// itself; this command lists only the NAMED worktree stacks layered on top.
//
// Empty output (no named worktrees registered yet, or a missing registry) is
// the normal primary-checkout-only case and exits 0.
func newDevStackListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered worktree keys, one per line (default stack \"\" is implicit, not printed)",
		Long: `Print the registered worktree keys (one per line, sorted by block index)
from the lock-guarded block registry (.forge/blocks.json).

This is the source a DECLARATIVE per-stack config generator reads to enumerate
the active named stacks — e.g. the dev NATS-account generator renders one
account per key plus the implicit default. The keys are the EXACT values
option("worktree") renders to in KCL, so a generator's per-key derivation
(NATS user/password, DB name, …) can be made byte-identical to the KCL's.

The DEFAULT stack (the primary checkout, key "") is never stored and is NOT
printed — a generator always emits the default's config itself. No named
worktrees (or no registry yet) prints nothing and exits 0.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			blocks, err := devstack.List(projectDirForKCL())
			if err != nil {
				return fmt.Errorf("read block registry: %w", err)
			}
			for _, b := range blocks {
				fmt.Fprintln(cmd.OutOrStdout(), b.Key)
			}
			return nil
		},
	}
}

// newDevStackPortCmd: `forge devstack port <base>` → base + block(key)*100,
// keyed on the current worktree, allocating the block on first use. This is
// the exact value forge.allocate_port(base, option("worktree")) renders to,
// so a launcher can match the host listen port to the rendered contract port.
func newDevStackPortCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "port <base>",
		Short: "Resolve the worktree-allocated host port for a base port",
		Long: `Print base + block(worktree)*100 — the host port forge.allocate_port(base,
option("worktree")) renders to for the CURRENT worktree.

The block is read from (or allocated into) .forge/blocks.json under the same
file lock the KCL builtin uses, so the printed port is identical to what
'forge up'/'forge deploy' renders for this worktree. On the primary checkout
the key is "" so <base> is returned unchanged.

Used by the dev launcher to start the host 'reliant' process listening on the
exact port the in-cluster workspace-controller will dial.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("base port %q is not an integer: %w", args[0], err)
			}
			projectDir := projectDirForKCL()
			key := devstack.Worktree(projectDir)
			port, err := devstack.AllocatePort(projectDir, base, key)
			if err != nil {
				return fmt.Errorf("allocate port for worktree %q: %w", key, err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), port)
			return nil
		},
	}
}

// newDevStackKeyCmd: `forge devstack key` → the current worktree key ("" on
// the primary checkout). Lets a launcher derive the namespace suffix / DB
// suffix without re-implementing the worktree detection.
func newDevStackKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "key",
		Short: "Print the current worktree key (\"\" on the primary checkout)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), devstack.Worktree(projectDirForKCL()))
			return nil
		},
	}
}
