// Package cli — zero-touch activation of the machine storage policy.
//
// The storage machinery under internal/storage was complete and did nothing,
// because activation was imperative: `forge storage register` and `forge
// storage install` had to be run by hand, in order, and never were. A
// protection that must be switched on is a protection that is off.
//
// This file closes that. Every command that creates or uses a local cache
// already knows the facts the policy wants, so each one CONVERGES the policy as
// a side effect of the work it was doing anyway:
//
//	ensureDeclaredCluster  → contexts + registry container + aliases (cluster phase)
//	registerBuildStorage   → repositories + ledger pins + project (forge build)
//
// Neither is allowed to fail the user's command. A policy that cannot be
// written is a missed cleanup opportunity, not a reason a cluster does not come
// up, so every path here warns and returns.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/modfile"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/storage"
)

// convergeStorageFn is the seam the touch points call, so a test can observe
// the facts forge derived without a policy file on disk.
var convergeStorageFn = convergeStorage

// convergeStorage upserts what this command learned into the machine policy.
// Warn-never-fail: the caller is in the middle of creating a cluster or
// running a build.
func convergeStorage(facts storage.Facts) {
	path, err := storage.DefaultPath()
	if err == nil {
		err = storage.Converge(path, facts)
	}
	// Under `go test` the machine policy is deliberately unwritable; a test
	// has nothing real to register, so that is a skip, not a warning.
	if err != nil && !errors.Is(err, storage.ErrMachinePolicyUnderTest) {
		fmt.Fprintf(os.Stderr, "storage retention registration: %v\n", err)
	}
}

// convergeClusterStorage records what the CLUSTER phase knows: the contexts
// this env declares, and the local registry its k3d config references with the
// host aliases from that config's containerd mirror block.
//
// Called for every declared cluster, on warm runs too, because the facts can
// change without the cluster being recreated (a new env adds a context, a
// mirror key is added). Converge is a no-op when they have not.
func convergeClusterStorage(c ClusterEntity, declared []ClusterEntity, projectDir string) {
	facts := storage.Facts{Project: projectDir, Contexts: localClusterContexts(declared), Repos: projectRepos(projectDir)}
	if len(facts.Contexts) == 0 {
		facts.Contexts = localClusterContexts([]ClusterEntity{c})
	}
	if c.Config != "" {
		data, err := os.ReadFile(c.Config)
		if err == nil {
			container, aliases, parseErr := registryFactsFromConfig(data)
			if parseErr == nil {
				facts.Registry, facts.Aliases = container, aliases
			}
		}
	}
	// Ledger pins come from the project, not the cluster, but this is the
	// earliest touch point in an `up` — importing them here means a registry
	// that completes during THIS run already has its release pins protected
	// rather than acquiring them one build later.
	if projectDir != "" {
		if pins, err := storage.LedgerPins(filepath.Join(projectDir, ".forge", "releases")); err == nil {
			facts.Pins = pins
		}
	}
	convergeStorageFn(facts)
}

// localClusterContexts is the kubectl context of every declared k3d
// cluster. A cluster's Context is projected by the render; the `k3d-<name>`
// fallback matches what k3d itself names the context, and is what a
// hand-constructed entity (or an older render) carries.
func localClusterContexts(declared []ClusterEntity) []string {
	var contexts []string
	for _, c := range declared {
		if c.Provider != "" && c.Provider != "k3d" {
			continue
		}
		ctxName := c.Context
		if ctxName == "" {
			ctxName = "k3d-" + c.Name
		}
		contexts = append(contexts, ctxName)
	}
	return contexts
}

// opportunisticGCInterval is how long the opportunistic pass waits after its
// own last ATTEMPT before trying again. One day matches the installed
// schedule's period.
const opportunisticGCInterval = 24 * time.Hour

// opportunisticGCBudget is the WHOLE pass's wall time (15m: the pass is a
// detached process nothing waits on, and a first Go-cache trim of hundreds of
// GB of small files needs the room; a cut-off layer stops cleanly and resumes
// next pass): its context bounds the
// docker calls, each layer's lsof snapshot and each layer's walk over entries,
// and a layer cut off by it removes nothing more (Runner.Ctx). Generous enough
// for a builder prune and a log expiry; what does not fit is reclaimed by the
// next pass or the scheduled job.
const opportunisticGCBudget = 15 * time.Minute

// storageAutoOptOut is the environment variable that turns the opportunistic
// pass off: FORGE_STORAGE_AUTO=0. The pass runs behind the user's back, so it
// must be possible to say no without editing a policy file.
const storageAutoOptOut = "FORGE_STORAGE_AUTO"

// nonDisruptiveGCFn is the pass itself, seamed so the GATING decisions (is it
// stale, what is recorded, what is printed) are testable without a Docker
// daemon — the real pass shells out to docker and takes tens of seconds.
var nonDisruptiveGCFn = func(ctx context.Context, r storage.Runner) error {
	return r.NonDisruptiveGC(ctx, true)
}

// startAutoGCFn starts the opportunistic pass in the background and returns
// where its output goes. Seamed so a test never spawns a process.
var startAutoGCFn = startAutoGC

// maybeOpportunisticGC is the other half of zero-touch: on a machine where
// nobody ran `forge storage install`, nothing ever reclaims anything. At the end
// of a successful `forge env up`, if this pass has not been attempted within
// opportunisticGCInterval, start the NON-DISRUPTIVE layers — rotated-log
// expiry, builder-cache prune, temp sweep, source eviction — in the
// background, and return.
//
// Explicitly NOT run here: registry GC (it takes the registry offline, and `up`
// is typically the command that just pushed to it) and node reconfiguration
// (it restarts kubelet). Those stay with the scheduled pass and the explicit
// commands. That restriction, not the time budget, is what makes this safe to
// do behind the user's back.
//
// BACKGROUND, not inline. The pass used to run synchronously, so `env up` did
// not return until it finished — and its budget did not even bound the host
// layers, so a first run could delete a hundred-odd GB of temp scratch before
// `up` came back. A goroutine is not the answer either: in its detaching
// shape `up` exits within milliseconds, killing it mid-prune. So the pass is a
// separate, detached `forge storage auto-gc` process with its own lock, its
// own bounded context, and its own log.
//
// Because it never runs the registry layer, this pass records only its own
// ATTEMPT (last-auto-gc.json), which is its rate limit. It never writes the
// full-GC record: an earlier version stamped one shared "last GC" marker here,
// even on failure, and `forge doctor` read it as "GC scheduled, last ran 2h
// ago" on a machine where registry retention had never once run.
//
// It also prints at most ONE line about storage health: that no schedule is
// installed, or else that the last full GC failed, is stale, or never ran.
func maybeOpportunisticGC(_ context.Context, out io.Writer) {
	path, err := storage.DefaultPath()
	if err != nil {
		return
	}
	policy, err := storage.Load(path)
	if err != nil {
		return
	}
	if len(policy.Registries) > 0 && !storageScheduleInstalledFn() {
		fmt.Fprintf(out, "[up] storage: %d local registr(ies) registered but no maintenance schedule is installed — run `forge storage install` to reclaim registry versions daily.\n", len(policy.Registries))
	} else {
		last, ok := storage.LastFullGC(path)
		if problem := storage.FullGCProblem(policy, last, ok, time.Now()); problem != "" {
			fmt.Fprintf(out, "[up] storage: %s, so registry images are not being reclaimed — run `forge storage gc --dry-run` to see why.\n", problem)
		}
	}
	if os.Getenv(storageAutoOptOut) == "0" {
		return
	}
	if attempt, ok := storage.LastAutoGC(path); ok && time.Since(attempt.At) < opportunisticGCInterval {
		return
	}
	logPath, err := startAutoGCFn(path)
	if err != nil {
		fmt.Fprintf(out, "[up] storage: could not start background cleanup (%v); `forge storage gc --apply` runs it by hand.\n", err)
		return
	}
	fmt.Fprintf(out, "[up] storage: pruning build cache, temp scratch and rotated logs in the background (log: %s; %s=0 disables)\n", logPath, storageAutoOptOut)
}

// runAutoGC is the body of `forge storage auto-gc`: one bounded,
// non-disruptive pass under the maintenance lock, recorded as an attempt.
// A busy lock means another pass is already doing this work, which is not a
// failure and not an attempt.
func runAutoGC(ctx context.Context, path string, out io.Writer) error {
	policy, err := storage.Load(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, opportunisticGCBudget)
	defer cancel()
	return storage.WithLock(path, func() error {
		gcErr := nonDisruptiveGCFn(ctx, maintenanceRunner(policy, path, out))
		// Recorded whether or not it succeeded: the interval rate-limits
		// ATTEMPTS, so a machine where this reliably fails (no Docker
		// daemon, a vanished builder) does not retry on every `forge env
		// up`. It is recorded as what it was, in this pass's own record and
		// nowhere else.
		result := storage.NewGCResult(time.Now(), gcErr)
		if err := storage.RecordAutoGC(path, result); err != nil && gcErr == nil {
			return err
		}
		if len(result.CutOffLayers) > 0 {
			fmt.Fprintf(out, "storage auto-gc: %s\n", result.Summary())
		}
		return storage.RealFailure(gcErr)
	})
}

// newStorageAutoGCCmd is the hidden `forge storage auto-gc`, the process
// `forge env up` starts in the background. It is a command, not a flag on
// `gc`, because it is a different pass: non-disruptive layers only, bounded,
// and recorded as an opportunistic attempt rather than a full GC.
func newStorageAutoGCCmd(policyPath *string) *cobra.Command {
	return &cobra.Command{
		Use:    "auto-gc",
		Short:  "Run the bounded, non-disruptive maintenance pass forge env up starts in the background",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if *policyPath == "" {
				return fmt.Errorf("auto-gc requires --policy")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runAutoGC(ctx, *policyPath, cmd.OutOrStdout())
		},
	}
}

// errAutoGCUnderTest refuses the background launch from a test binary.
var errAutoGCUnderTest = errors.New("refusing to start background storage maintenance under `go test` " +
	"(os.Executable is the test binary; stub startAutoGCFn)")

// startAutoGC starts `forge storage auto-gc` detached from this process, with
// its output in a log beside the policy, and does not wait for it.
//
// Under `go test` it refuses. It re-executes os.Executable(), which in a test
// is the test binary, so a test that reached it without stubbing
// startAutoGCFn would start a detached copy of the whole suite that outlives
// the run. This is the same guard TempRoot, SourceCacheRoot and the machine
// policy use: a hole that must be remembered to close stays open.
func startAutoGC(policyPath string) (string, error) {
	if testing.Testing() {
		return "", errAutoGCUnderTest
	}
	tokens, err := forgeExecCommand()
	if err != nil {
		return "", err
	}
	logDir := filepath.Join(filepath.Dir(policyPath), "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return "", err
	}
	logPath := filepath.Join(logDir, "auto-gc.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer func() { _ = logFile.Close() }()
	args := append(append([]string{}, tokens[1:]...), "storage", "auto-gc", "--policy", policyPath)
	cmd := exec.Command(tokens[0], args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detachFromParent(cmd)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	// Not waited for: the child outlives this command by design.
	_ = cmd.Process.Release()
	return logPath, nil
}

// projectRepos is every git repository the project builds from: its own, the
// local directories named by forge.yaml docker.build_contexts, and go.mod
// replace targets. The worktree layer asks git for each repository's
// worktrees, so only the repositories need recording, not their checkouts.
// Anything unreadable is skipped: this feeds a warn-never-fail converge.
func projectRepos(projectDir string) []string {
	if projectDir == "" {
		return nil
	}
	dirs := []string{projectDir}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(projectDir, p)
	}
	if cfg, err := config.LoadProjectDir(projectDir); err == nil && cfg != nil {
		for _, value := range cfg.Docker.BuildContexts {
			if !strings.Contains(value, "://") {
				dirs = append(dirs, resolve(value))
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(projectDir, "go.mod")); err == nil {
		mod, err := modfile.Parse("go.mod", data, nil)
		if err == nil {
			for _, rep := range mod.Replace {
				if modfile.IsDirectoryPath(rep.New.Path) {
					dirs = append(dirs, resolve(rep.New.Path))
				}
			}
		}
	}
	var repos []string
	for _, dir := range dirs {
		// The common dir's parent is the main checkout even when dir is a
		// linked worktree that may itself be removed later.
		out, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
		if err != nil {
			continue
		}
		top := filepath.Dir(strings.TrimSpace(string(out)))
		if top != "." && !slices.Contains(repos, top) {
			repos = append(repos, top)
		}
	}
	return repos
}
