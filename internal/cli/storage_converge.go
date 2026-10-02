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
	"path/filepath"
	"time"

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
	facts := storage.Facts{Project: projectDir, Contexts: localClusterContexts(declared)}
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

// nonDisruptiveGCFn is the pass itself, seamed so the GATING decisions (is it
// stale, what is recorded, what is printed) are testable without a Docker
// daemon — the real pass shells out to docker and takes tens of seconds.
var nonDisruptiveGCFn = func(ctx context.Context, r storage.Runner) error {
	return r.NonDisruptiveGC(ctx, true)
}

// opportunisticGCBudget caps the pass. It is generous enough for a builder
// prune and a log expiry and short enough that nothing waits on it long.
const opportunisticGCBudget = 2 * time.Minute

// maybeOpportunisticGC is the other half of zero-touch: on a machine where
// nobody ran `forge storage install`, nothing ever reclaims anything. At the end
// of a successful `forge env up`, if this pass has not been attempted within
// opportunisticGCInterval, run the NON-DISRUPTIVE layers — rotated-log expiry,
// builder-cache prune, temp sweep, source eviction.
//
// Explicitly NOT run here: registry GC (it takes the registry offline, and `up`
// is typically the command that just pushed to it) and node reconfiguration
// (it restarts kubelet). Those stay with the scheduled pass and the explicit
// commands. That restriction, not the time budget, is what makes this safe to
// do behind the user's back.
//
// Because it never runs the registry layer, this pass records only its own
// ATTEMPT (last-auto-gc.json), which is its rate limit. It never writes the
// full-GC record: an earlier version stamped one shared "last GC" marker here,
// even on failure, and `forge doctor` read it as "GC scheduled, last ran 2h
// ago" on a machine where registry retention had never once run.
//
// It also prints at most ONE line about storage health: that no schedule is
// installed, or else that the last full GC failed, is stale, or never ran.
func maybeOpportunisticGC(ctx context.Context, out io.Writer) {
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
	if attempt, ok := storage.LastAutoGC(path); ok && time.Since(attempt.At) < opportunisticGCInterval {
		return
	}
	fmt.Fprintf(out, "[up] storage: last cleanup is over %s old — pruning build cache and rotated logs (registry cleanup is left to `forge storage gc`)...\n", opportunisticGCInterval)

	ctx, cancel := context.WithTimeout(ctx, opportunisticGCBudget)
	defer cancel()
	// The lock is held for the pass, so this never races the scheduled
	// daemon or a concurrent `forge storage gc`.
	runErr := storage.WithLock(path, func() error {
		gcErr := nonDisruptiveGCFn(ctx, maintenanceRunner(policy, path, out))
		// The attempt is recorded whether or not it succeeded: the interval
		// rate-limits ATTEMPTS, so a machine where this reliably fails (no
		// Docker daemon, a vanished builder) does not retry and re-print on
		// every `forge env up`. It is recorded as what it was, a failure, in
		// the opportunistic pass's own record and nowhere else.
		if err := storage.RecordAutoGC(path, storage.NewGCResult(time.Now(), gcErr)); err != nil && gcErr == nil {
			return err
		}
		return gcErr
	})
	if runErr != nil {
		fmt.Fprintf(out, "[up] storage: cleanup pass incomplete (retrying in %s; run `forge storage gc` to see why): %v\n",
			opportunisticGCInterval, runErr)
	}
}
