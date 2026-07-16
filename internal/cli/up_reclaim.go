package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Ownership markers stamped onto every host process / frontend `forge up`
// spawns (see stampForgeOwnership). They ride the child's environment, so
// they PROPAGATE to every descendant — air's re-exec'd server, npm's
// next/vite grandchild — regardless of pid churn or a stale/absent ledger.
// This makes process ownership a property of the LIVE process, discoverable
// by inspecting whoever actually holds a wanted port, rather than a fact
// that lives only in the drift-prone per-env .pids file.
const (
	forgeUpEnvVar     = "FORGE_UP_ENV"
	forgeUpServiceVar = "FORGE_UP_SERVICE"
)

// procFacts is the process-inspection seam the ownership resolver reads:
// a pid's environment and its parent. The real implementation reads the OS
// (sysctl KERN_PROCARGS2 on darwin, /proc on linux, `ps` for the ppid
// table); unit tests inject a map-backed fake so the ancestry logic runs
// without spawning real processes.
type procFacts interface {
	// environ returns pid's environment as KEY=VALUE strings. ok is false
	// when the env is unreadable (dead pid, permission, SIP-redacted system
	// binary) — an unreadable holder is treated as NOT forge-owned.
	environ(pid int) (env []string, ok bool)
	// parent returns pid's parent pid. ok is false when unknown.
	parent(pid int) (ppid int, ok bool)
}

// osProcFacts is the production procFacts: it reads real process env via
// the platform readProcEnviron and answers parent() from a single ppid
// snapshot taken at construction (one `ps` call, then pure map lookups for
// the ancestry walk).
type osProcFacts struct {
	ppids map[int]int
}

func newOSProcFacts() *osProcFacts {
	return &osProcFacts{ppids: ppidMap()}
}

func (o *osProcFacts) environ(pid int) ([]string, bool) {
	return readProcEnviron(pid)
}

func (o *osProcFacts) parent(pid int) (int, bool) {
	ppid, ok := o.ppids[pid]
	return ppid, ok
}

// markerEnvName extracts the FORGE_UP_ENV value from a process's environment
// (empty when absent). The environ slice is KEY=VALUE strings; a later
// duplicate wins, matching exec's last-wins env semantics.
func markerEnvName(env []string) string {
	v, _ := markerFields(env)
	return v
}

// markerFields extracts (FORGE_UP_ENV, FORGE_UP_SERVICE) from a process's
// environment. Either may be empty when the marker is absent.
func markerFields(env []string) (envName, service string) {
	prefixEnv := forgeUpEnvVar + "="
	prefixSvc := forgeUpServiceVar + "="
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, prefixEnv):
			envName = kv[len(prefixEnv):]
		case strings.HasPrefix(kv, prefixSvc):
			service = kv[len(prefixSvc):]
		}
	}
	return envName, service
}

// maxAncestryDepth bounds the parent walk so a pathological ppid cycle (or a
// deeply nested tree) can't spin. Real forge chains are shallow
// (forge-up → go/air/npm → server/next), so a handful of levels is ample.
const maxAncestryDepth = 8

// forgeOwnerOfPID reports whether pid — or any ancestor up to init (pid 1) —
// carries FORGE_UP_ENV==env, i.e. whether the process is one THIS env's
// `forge up` spawned. Returns the matching FORGE_UP_SERVICE marker too.
//
// Walking ancestry is the grandchild-reparented-to-launchd safeguard: even
// if the port-holder's own env were unreadable, a marked ancestor still
// identifies it as ours. It cannot produce a false positive — a process not
// descended from forge has no forge-marked ancestor — so a genuinely foreign
// holder is never misclassified as reclaimable. The walk stops at pid 1
// (launchd/init, everyone's ancestor) which is never inspected.
func forgeOwnerOfPID(pid int, envName string, f procFacts) (service string, owned bool) {
	for depth := 0; pid > 1 && depth < maxAncestryDepth; depth++ {
		if env, ok := f.environ(pid); ok {
			if name, svc := markerFields(env); name == envName {
				return svc, true
			}
		}
		ppid, ok := f.parent(pid)
		if !ok || ppid == pid || ppid <= 1 {
			break
		}
		pid = ppid
	}
	return "", false
}

// topmostForgeOwnedAncestor returns the HIGHEST ancestor of pid (including
// pid itself) still carrying FORGE_UP_ENV==env, or -1 when the process is
// not forge-owned for env. Tree-killing this pid tears down the whole
// orphaned subtree in one shot — e.g. from a leaf `next` grandchild it
// climbs to the `npm` forge actually launched, so killing it also reaps
// `next`, instead of orphaning npm to respawn it.
func topmostForgeOwnedAncestor(pid int, envName string, f procFacts) int {
	top := -1
	for depth := 0; pid > 1 && depth < maxAncestryDepth; depth++ {
		if env, ok := f.environ(pid); ok {
			if name, _ := markerFields(env); name == envName {
				top = pid
			}
		}
		ppid, ok := f.parent(pid)
		if !ok || ppid == pid || ppid <= 1 {
			break
		}
		pid = ppid
	}
	return top
}

// classifyPortConflicts splits the pre-flight port conflicts into the ones
// held by a forge-owned process for env (OURS — reclaimable) and the ones
// held by anything else (FOREIGN — never touched). The holder pid is
// resolved via resolvePID (lsof); a holder that can't be resolved, or whose
// ancestry carries no marker, lands in foreign — the conservative default
// that preserves the "never reclaim an unidentifiable process" safety
// property.
func classifyPortConflicts(envName string, conflicts []portConflict, resolvePID func(int) int, f procFacts) (owned, foreign []portConflict) {
	for _, c := range conflicts {
		pid := resolvePID(c.port)
		if pid > 0 {
			if _, ok := forgeOwnerOfPID(pid, envName, f); ok {
				owned = append(owned, c)
				continue
			}
		}
		foreign = append(foreign, c)
	}
	return owned, foreign
}

// reclaimMarkedOrphans tree-kills the forge-owned orphan holding each
// conflicting port (from the topmost marked ancestor down) and waits for the
// listeners to actually free. Foreign / unidentifiable holders are left
// untouched. Used by the `--restart` guard path to reclaim orphans the
// ledger never recorded (crash, air re-exec, grandchild reparent).
func reclaimMarkedOrphans(envName string, conflicts []portConflict, resolvePID func(int) int, f procFacts) {
	killTreesAndWait(markedOrphanRootsForPorts(envName, conflicts, resolvePID, f))
}

// markedOrphanRootsForPorts is the pure selection core of
// reclaimMarkedOrphans: it maps each conflicting port to the topmost
// forge-owned ancestor of its holder, deduping shared roots. Split out so
// the selection is unit-testable without signalling real processes.
func markedOrphanRootsForPorts(envName string, conflicts []portConflict, resolvePID func(int) int, f procFacts) []int {
	var roots []int
	seen := map[int]bool{}
	for _, c := range conflicts {
		pid := resolvePID(c.port)
		if pid <= 0 {
			continue
		}
		top := topmostForgeOwnedAncestor(pid, envName, f)
		if top <= 1 || seen[top] {
			continue
		}
		seen[top] = true
		fmt.Printf("[up] reclaiming orphaned %s on :%d (pid %d + tree)\n", c.name, c.port, top)
		roots = append(roots, top)
	}
	return roots
}

// reclaimAllMarkedOrphans sweeps the WHOLE process table for processes
// carrying FORGE_UP_ENV==env and tree-kills them — the port-independent
// unblock `forge up stop` offers so a user can always clear a wedged env,
// even with no ledger and whatever port drift occurred. Returns the count of
// subtrees signalled. Only processes carrying the exact env marker are ever
// touched; unmarked processes are never signalled.
func reclaimAllMarkedOrphans(envName string, f procFacts) int {
	roots := markedOrphanRoots(envName, listPIDs(), f)
	killTreesAndWait(roots)
	return len(roots)
}

// markedOrphanRoots is the pure selection core of reclaimAllMarkedOrphans:
// from a candidate pid set it keeps every process carrying FORGE_UP_ENV==env
// whose marked ancestors are NOT also in the set (the subtree roots), so a
// parent+child pair collapses to a single tree-kill. Split out so the sweep
// is unit-testable without a real process table.
func markedOrphanRoots(envName string, pids []int, f procFacts) []int {
	marked := map[int]bool{}
	for _, pid := range pids {
		if pid <= 1 {
			continue
		}
		if env, ok := f.environ(pid); ok && markerEnvName(env) == envName {
			marked[pid] = true
		}
	}
	var roots []int
	for pid := range marked {
		if !hasMarkedAncestor(pid, marked, f) {
			roots = append(roots, pid)
		}
	}
	return roots
}

// hasMarkedAncestor reports whether any ancestor of pid is in the marked
// set — used to reduce the marker sweep to subtree roots.
func hasMarkedAncestor(pid int, marked map[int]bool, f procFacts) bool {
	for depth := 0; depth < maxAncestryDepth; depth++ {
		ppid, ok := f.parent(pid)
		if !ok || ppid <= 1 || ppid == pid {
			return false
		}
		if marked[ppid] {
			return true
		}
		pid = ppid
	}
	return false
}

// killTreesAndWait SIGTERMs each pid's process tree, polls for exit up to a
// bounded grace, then SIGKILLs any straggler — the same escalation
// runUpStop uses, so a caller like `--restart` knows the ports are released
// on return. No-op on an empty list.
func killTreesAndWait(pids []int) {
	if len(pids) == 0 {
		return
	}
	for _, pid := range pids {
		if pid > 1 {
			killProcessTree(pid, syscall.SIGTERM)
		}
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		anyAlive := false
		for _, pid := range pids {
			if pid > 1 && processAlive(pid) {
				anyAlive = true
				break
			}
		}
		if !anyAlive || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range pids {
		if pid > 1 && processAlive(pid) {
			killProcessTree(pid, syscall.SIGKILL)
		}
	}
}

// stampForgeOwnership marks cmd's child (and, via env inheritance, every
// descendant) as forge-`up`-owned for envName/service. withForcedEnv dedups,
// so a re-stamp / an inherited marker from a nested forge-up is overwritten
// rather than duplicated. A nil cmd.Env is seeded from the current process
// env so the child doesn't lose its inherited environment.
func stampForgeOwnership(cmd *exec.Cmd, envName, service string) {
	base := cmd.Env
	if base == nil {
		base = os.Environ()
	}
	base = withForcedEnv(base, forgeUpEnvVar, envName)
	base = withForcedEnv(base, forgeUpServiceVar, service)
	cmd.Env = base
}
