// Package cli — `forge env devstack` command tree.
//
// The parallel-dev-stack primitives (ADR 0003) live in internal/devstack:
// the raw git facts pushed into KCL as options, and the memoized
// forge.allocate_port(base, key) block allocator. Those primitives are
// resolved INSIDE a KCL render (under the up/deploy activation path). But a
// host launcher — a Taskfile target, a bootstrap script — needs the SAME
// allocated host port BEFORE `forge env up` renders the KCL, so it can start the
// host `reliant` process LISTENING on exactly the port the in-cluster
// controller will dial.
//
// `forge env devstack port <base>` is that single source of truth: it resolves
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

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/devstack"
)

// newDevStackCmd builds the `forge env devstack` parent command — the host-side
// surface of the parallel-dev-stack primitives. The KCL-side surface is the
// option("worktree")/option("branch") seam + the forge.allocate_port builtin;
// this command lets a launcher resolve the SAME values without a render.
func newDevStackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devstack",
		Short: "Parallel-dev-stack host helpers (worktree key + port allocation)",
		Long: `Host-side helpers for forge's parallel-dev-stack primitives (ADR 0003).

A launcher (Taskfile target, bootstrap script) that starts a host process
BEFORE 'forge env up' renders the KCL needs the SAME host port the render will
allocate. 'forge env devstack port' resolves it through the same lock-guarded
block registry (.forge/blocks.json) the forge.allocate_port KCL builtin uses,
so the launcher and the render can never drift.

On the PRIMARY checkout the worktree key is "" so every port is returned
unchanged (block 0) — the default dev loop is byte-identical to today. A
linked git worktree gets its own stable 100-port block.

Examples:
  forge env devstack port 3091     # the reliant-api host port for this worktree
  forge env devstack key           # the worktree key ("" on the primary checkout)`,
	}
	cmd.AddCommand(newDevStackPortCmd())
	cmd.AddCommand(newDevStackKeyCmd())
	cmd.AddCommand(newDevStackListCmd())
	cmd.AddCommand(newDevStackPruneCmd())
	return cmdutil.StrictGroup(cmd)
}

// newDevStackListCmd: `forge env devstack list` → the registered worktree keys,
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
		Short: "List registered dev-stack (worktree) keys, one per line (default stack \"\" is implicit, not printed)",
		Long: `Print the registered DEV-STACK keys (one per line, sorted by block index)
from the lock-guarded block registry (.forge/blocks.json).

Only worktree stacks are listed. The registry also memoizes plain port-block
keys — any KCL expression that just wants a non-colliding host port, e.g. a
prod frontend's dev-server port keyed "prod" — and those are NOT stacks and are
NOT printed. Treating one as a stack is a real bug with a real blast radius: a
per-stack config generator that enumerated the raw registry emitted a dev NATS
account for a prod web port into a tracked config file.

The keys printed are the EXACT values option("worktree") renders to in KCL, so
a generator's per-key derivation (NATS user/password, DB name, …) can be made
byte-identical to the KCL's.

The DEFAULT stack (the primary checkout, key "") is never stored and is NOT
printed — a generator always emits the default's config itself. No named
worktrees (or no registry yet) prints nothing and exits 0.

Inside KCL, prefer the fp.dev_stacks() builtin over shelling out to this
command: it returns the same roster during the render, and it deliberately
returns EMPTY on a read-only render (forge generate / forge ci) so a file
generated from it stays byte-identical across machines.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			keys, err := devstack.ListStacks(projectDirForKCL())
			if err != nil {
				return fmt.Errorf("read block registry: %w", err)
			}
			for _, key := range keys {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), key)
			}
			return nil
		},
	}
}

// newDevStackPortCmd: `forge env devstack port <base>` → base + block(key)*100,
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
'forge env up'/'forge env deploy' renders for this worktree. On the primary checkout
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
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), port)
			return nil
		},
	}
}

// newDevStackKeyCmd: `forge env devstack key` → the current worktree key ("" on
// the primary checkout). Lets a launcher derive the namespace suffix / DB
// suffix without re-implementing the worktree detection.
func newDevStackKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "key",
		Short: "Print the current worktree key (\"\" on the primary checkout)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), devstack.Worktree(projectDirForKCL()))
			return nil
		},
	}
}

// newDevStackPruneCmd: `forge env devstack prune` → reclaim dev-stack blocks
// whose worktree is gone from disk, so the dense block range (see
// dev_stack.max_stacks) doesn't fill up with leaked entries from deleted
// worktrees.
func newDevStackPruneCmd() *cobra.Command {
	var apply bool

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Reclaim dev-stack port blocks for worktrees that no longer exist on disk",
		Long: `Reclaim entries in the block registry (.forge/blocks.json) for git
worktrees that have been removed from disk.

Nothing is ever removed from this registry automatically — a deleted
worktree's block stays held forever unless something reclaims it. Blocks are
DENSE and the reachable range is finite (see dev_stack.max_stacks in
forge.yaml, default 8), so leaked entries from old worktrees are exactly how
a project runs out of dev stacks.

What gets reclaimed: a DEV-STACK key (one registered per git worktree, e.g.
"wt-feature-x") whose worktree no longer appears in
'git worktree list --porcelain'.

What is NEVER reclaimed, no matter how "dead" it looks:
  - The default key "" (block 0) — the primary checkout's implicit block.
  - A PLAIN PORT-BLOCK key — e.g. "prod", the key prod's reliant-web
    dev-server port allocates under. These are not tied to any worktree at
    all, so they can never legitimately look dead; reclaiming one would
    silently move a live, running stack's port. This is the single most
    important correctness property of this command.

If enumerating live worktrees fails for any reason (git missing, this isn't
a git checkout, the git command errors), prune reclaims NOTHING and reports
the failure — deleting a block that might actually still be live would move
a running stack's ports.

Freeing a block lets the NEXT new worktree take it (blocks are filled
densely, lowest free index first), so pruning is what keeps the block range
from being exhausted by worktrees nobody remembers to clean up.

By default this only PRINTS what would be reclaimed and changes nothing —
pass --apply to actually rewrite the registry. This mutates machine-local
state that other running dev stacks depend on (a concurrent 'forge env up'
is briefly locked out while the rewrite happens), so making it opt-in to
apply is the safer default for a command most people will run interactively
to see what's accumulated.

Examples:
  forge env devstack prune            # show what would be reclaimed
  forge env devstack prune --apply    # actually reclaim it`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dryRun := !apply
			pruned, err := devstack.Prune(projectDirForKCL(), dryRun)
			if err != nil {
				return fmt.Errorf("prune block registry: %w", err)
			}
			if len(pruned) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "nothing to prune — no dev-stack key's worktree is gone")
				return nil
			}
			verb := "would reclaim"
			if apply {
				verb = "reclaimed"
			}
			for _, p := range pruned {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (block %d)\n", verb, p.Key, p.Index)
			}
			if dryRun {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\n%d block(s) would be freed. Re-run with --apply to reclaim them.\n", len(pruned))
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\n%d block(s) freed.\n", len(pruned))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "Actually rewrite the registry (default is dry-run: print what would be reclaimed)")
	return cmd
}
