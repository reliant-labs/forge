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
	if err != nil {
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

// opportunisticGCInterval is how stale the last maintenance pass must be before
// `forge env up` runs the non-disruptive layers itself. One day matches the
// installed schedule's period, and `forge storage gc --apply` stamps the same
// marker, so a machine WITH the schedule installed never does redundant work
// here.
const opportunisticGCInterval = 24 * time.Hour

// nonDisruptiveGCFn is the pass itself, seamed so the GATING decisions (is it
// stale, does the stamp advance, is the notice printed) are testable without a
// Docker daemon — the real pass shells out to docker and takes tens of seconds.
var nonDisruptiveGCFn = func(ctx context.Context, r storage.Runner) error {
	return r.NonDisruptiveGC(ctx, true)
}

// opportunisticGCBudget caps the pass. It is generous enough for a builder
// prune and a log expiry and short enough that nothing waits on it long.
const opportunisticGCBudget = 2 * time.Minute

// maybeOpportunisticGC is the other half of zero-touch: on a machine where
// nobody ran `forge storage install`, nothing ever reclaims anything. At the end
// of a successful `forge env up`, if maintenance has not completed within
// opportunisticGCInterval, run the NON-DISRUPTIVE layers — rotated-log expiry,
// builder-cache prune, temp sweep — and record the completion.
//
// Explicitly NOT run here: registry GC (it takes the registry offline, and `up`
// is typically the command that just pushed to it) and node reconfiguration
// (it restarts kubelet). Those stay with the scheduled pass and the explicit
// commands. That restriction, not the time budget, is what makes this safe to
// do behind the user's back.
//
// It runs SYNCHRONOUSLY under a 2-minute context, rather than in a detached
// goroutine. `forge env up` has two exit shapes — it holds the foreground, or
// it detaches and RETURNS — and in the detaching shape the process exits within
// milliseconds of this call, so a background goroutine would be killed
// mid-prune most times it mattered. A synchronous pass bounded at two minutes
// is honest about the cost and actually completes; the alternative is work that
// appears to be scheduled and silently is not.
//
// Also prints the one-line notice when the policy has registries and no
// schedule is installed — the registry layer is the one that reclaims the most
// and the one this pass will never run, so a machine with registered registries
// genuinely needs the LaunchAgent.
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
	}
	if since := time.Since(storage.LastGC(path)); since < opportunisticGCInterval {
		return
	}
	fmt.Fprintf(out, "[up] storage: last cleanup is over %s old — pruning build cache and rotated logs (registry cleanup is left to `forge storage gc`)...\n", opportunisticGCInterval)

	ctx, cancel := context.WithTimeout(ctx, opportunisticGCBudget)
	defer cancel()
	// The lock is held for the pass, so this never races the scheduled
	// daemon or a concurrent `forge storage gc`. A busy lock is not an error
	// worth reporting: somebody else is already doing this work.
	runErr := storage.WithLock(path, func() error {
		gcErr := nonDisruptiveGCFn(ctx, maintenanceRunner(policy, out))
		// The stamp advances even when the pass failed, which is deliberate: an
		// ATTEMPT is what the interval rate-limits. A stamp written only on
		// success would mean a machine where this reliably fails — no Docker
		// daemon, a remote DOCKER_HOST, a builder that no longer exists —
		// re-attempts and re-prints the failure on EVERY `forge env up`, which
		// is noise the user cannot act on from here. The real reclaim path is
		// the installed schedule, and the notice above is what points at it.
		if err := storage.RecordGC(path, time.Now()); err != nil && gcErr == nil {
			return err
		}
		return gcErr
	})
	if runErr != nil {
		fmt.Fprintf(out, "[up] storage: cleanup pass incomplete (retrying in %s; run `forge storage gc` to see why): %v\n",
			opportunisticGCInterval, runErr)
	}
}
