package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/storage"
	"github.com/reliant-labs/forge/pkg/release"
)

func newStorageCmd() *cobra.Command {
	var path string
	group := &cobra.Command{Use: "storage", Short: "Inspect and bound local caches; preserve persistent application data"}
	group.PersistentFlags().StringVar(&path, "policy", "", "machine storage policy (default: user config directory/forge/storage.json)")
	load := func() (storage.Policy, string, error) {
		p := path
		if p == "" {
			var err error
			p, err = storage.DefaultPath()
			if err != nil {
				return storage.Policy{}, "", err
			}
		}
		cfg, err := storage.Load(p)
		return cfg, p, err
	}
	group.AddCommand(&cobra.Command{Use: "status", Args: cobra.NoArgs, Short: "Report physical host capacity, Docker usage and node cleanup settings", RunE: func(cmd *cobra.Command, _ []string) error {
		p, _, err := load()
		if err != nil {
			return err
		}
		return (storage.Runner{Policy: p, Out: cmd.OutOrStdout()}).Status(cmd.Context())
	}})
	group.AddCommand(&cobra.Command{Use: "policy", Args: cobra.NoArgs, Short: "Print the effective storage policy", RunE: func(cmd *cobra.Command, _ []string) error {
		p, _, err := load()
		if err != nil {
			return err
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(p)
	}})
	var apply, dryRun bool
	gc := &cobra.Command{Use: "gc", Args: cobra.NoArgs, Short: "Preview cleanup; --apply deletes only eligible cache and registry versions", RunE: func(cmd *cobra.Command, _ []string) error {
		p, path, err := load()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return storage.WithLock(path, func() error { return (storage.Runner{Policy: p, Out: cmd.OutOrStdout()}).GC(ctx, apply) })
	}}
	gc.Flags().BoolVar(&apply, "apply", false, "execute the cleanup plan")
	gc.Flags().BoolVar(&dryRun, "dry-run", false, "preview only (the default)")
	gc.MarkFlagsMutuallyExclusive("apply", "dry-run")
	group.AddCommand(gc)
	var interval time.Duration
	daemon := &cobra.Command{Use: "daemon", Args: cobra.NoArgs, Short: "Run maintenance periodically, reloading policy for every pass", RunE: func(cmd *cobra.Command, _ []string) error {
		if interval < time.Hour {
			return fmt.Errorf("maintenance interval must be at least 1h")
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			p, path, err := load()
			if err == nil {
				err = storage.WithLock(path, func() error { return (storage.Runner{Policy: p, Out: cmd.OutOrStdout()}).GC(ctx, true) })
			}
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "storage maintenance: %v\n", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	}}
	daemon.Flags().DurationVar(&interval, "interval", 24*time.Hour, "time between maintenance passes")
	group.AddCommand(daemon)
	group.AddCommand(&cobra.Command{Use: "install", Args: cobra.NoArgs, Short: "Install daily maintenance using a stable copy of this executable", RunE: func(cmd *cobra.Command, _ []string) error {
		p, path, err := load()
		if err != nil {
			return err
		}
		return storage.WithLock(path, func() error { return installStorageSchedule(cmd.Context(), cmd.OutOrStdout(), path, p) })
	}})
	var contexts, repos, pins []string
	var ledgerDir string
	register := &cobra.Command{Use: "register", Args: cobra.NoArgs, Short: "Register local cluster contexts, declared repositories or release pins", RunE: func(cmd *cobra.Command, _ []string) error {
		_, path, err := load()
		if err != nil {
			return err
		}
		if err := storage.RegisterProject(path, filepath.Dir(filepath.Dir(ledgerDir))); err != nil {
			return err
		}
		historical, err := storage.LedgerPins(ledgerDir)
		if err != nil {
			return err
		}
		return storage.Register(cmd.Context(), path, contexts, repos, append(pins, historical...))
	}}
	register.Flags().StringVar(&ledgerDir, "ledger", ".forge/releases", "local release ledger directory to import before enabling cleanup")
	register.Flags().StringSliceVar(&contexts, "context", nil, "local k3d contexts that can use these images")
	register.Flags().StringSliceVar(&repos, "repository", nil, "declared local image repositories, including registry host")
	register.Flags().StringSliceVar(&pins, "pin", nil, "image references that must remain available")
	group.AddCommand(register)
	var restart bool
	nodes := &cobra.Command{Use: "configure-nodes", Args: cobra.NoArgs, Short: "Preview kubelet GC migration for existing registered nodes; --apply restarts them", RunE: func(cmd *cobra.Command, _ []string) error {
		p, path, err := load()
		if err != nil {
			return err
		}
		return storage.WithLock(path, func() error {
			return (storage.Runner{Policy: p, Out: cmd.OutOrStdout()}).ConfigureNodes(cmd.Context(), path, restart)
		})
	}}
	nodes.Flags().BoolVar(&restart, "apply", false, "install configuration and restart local k3d nodes sequentially")
	group.AddCommand(nodes)
	var repo, base string
	var worktreeAge time.Duration
	var remove bool
	trees := &cobra.Command{Use: "worktrees", Args: cobra.NoArgs, Short: "Preview old, clean worktrees whose commits are merged; --apply removes them", RunE: func(cmd *cobra.Command, _ []string) error {
		return (storage.Runner{Out: cmd.OutOrStdout()}).Worktrees(cmd.Context(), repo, base, worktreeAge, remove)
	}}
	trees.Flags().StringVar(&repo, "repo", ".", "repository whose worktrees to inspect")
	trees.Flags().StringVar(&base, "base", "origin/main", "existing integration branch (fetch it before cleanup)")
	trees.Flags().DurationVar(&worktreeAge, "older-than", 30*24*time.Hour, "minimum age of directory and HEAD commit")
	trees.Flags().BoolVar(&remove, "apply", false, "remove clean merged worktrees without forcing or deleting branches")
	group.AddCommand(trees)
	return group
}

var checkBuildStorageFn = func(project string) error {
	path, err := storage.DefaultPath()
	if err != nil {
		return err
	}
	p, err := storage.Load(path)
	if err != nil {
		return err
	}
	_, err = storage.CheckSpace(p, project)
	return err
}

// Only actual k3d creation gains this mount. Existing nodes are never restarted
// as a side effect of a build; storage status exposes their config for migration.
func addClusterStorageArgs(args []string) ([]string, error) {
	path, err := storage.DefaultPath()
	if err != nil {
		return nil, err
	}
	p, err := storage.Load(path)
	if err != nil {
		return nil, err
	}
	file, err := storage.NodeConfigPath(path, p)
	if err != nil {
		return nil, err
	}
	return append(args, "--volume", filepath.Clean(file)+":/var/lib/rancher/k3s/agent/etc/kubelet.conf.d/90-forge-storage.conf:ro@all"), nil
}

func registerBuildStorage(ctx context.Context, project string, entities *KCLEntities, plan pushPlan) {
	if entities == nil {
		return
	}
	var contexts, repositories []string
	for _, c := range entities.Clusters {
		if c.Provider == "" || c.Provider == "k3d" {
			contexts = append(contexts, "k3d-"+c.Name)
		}
	}
	for _, d := range plan.destinations {
		host := d.host()
		if strings.HasPrefix(host, "localhost:") || strings.HasPrefix(host, "127.0.0.1:") || strings.Contains(host, ".localhost:") {
			repositories = append(repositories, d.repository)
		}
	}
	if len(contexts) == 0 || len(repositories) == 0 {
		return
	}
	path, err := storage.DefaultPath()
	if err == nil {
		err = storage.RegisterProject(path, project)
	}
	if err == nil {
		pins, pinErr := storage.LedgerPins(filepath.Join(project, ".forge", "releases"))
		if pinErr != nil {
			err = pinErr
		} else {
			err = storage.Register(ctx, path, contexts, repositories, pins)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "storage retention registration: %v\n", err)
	}
}

// Pin before publishing the ledger entry. If maintenance owns the lock, cutting
// the release fails rather than racing deletion. Failed cuts can leave safe pins.
func pinStorageRelease(rel release.Release) error {
	path, err := storage.DefaultPath()
	if err != nil {
		return err
	}
	return storage.WithLock(path, func() error {
		p, err := storage.Load(path)
		if err != nil {
			return err
		}
		if len(p.Registries) == 0 {
			return nil
		}
		for _, a := range rel.Artifacts {
			for _, digest := range a.Digests {
				p.Pins = append(p.Pins, digest)
			}
		}
		seen := map[string]bool{}
		var pins []string
		for _, pin := range p.Pins {
			if !seen[pin] {
				pins = append(pins, pin)
				seen[pin] = true
			}
		}
		p.Pins = pins
		return storage.Save(path, p)
	})
}
