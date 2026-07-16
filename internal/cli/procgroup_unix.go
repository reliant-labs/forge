//go:build !windows

package cli

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// startInOwnProcessGroup makes the child the leader of a new process
// group (pgid == child pid) so the whole subtree it spawns can be
// signalled at once.
//
// The load-bearing case is `go run`: it compiles to a temp binary and
// execs it as a CHILD, and it does NOT forward signals to that child.
// SIGTERM to the `go run` parent alone leaves the real server orphaned
// (reparented to init) and still holding its port — exactly the stale-
// :3090 squatter that bumped reliant-api-server to :3091. Putting the
// parent in its own group and signalling the GROUP on shutdown takes the
// orphan-prone child down with it. Air/binary/delve benefit too (any
// runner that forks a grandchild).
func startInOwnProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalProcessGroup sends sig to the entire process group led by pid —
// a negative pid targets the group, per kill(2). Falls back to signalling
// just pid if the group send fails (e.g. the leader already reaped). A
// non-positive pid is a no-op so a stale 0/-1 in the state file can never
// fan a signal out to unrelated processes.
func signalProcessGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		return syscall.Kill(pid, sig)
	}
	return nil
}

// processAlive reports whether pid is a live process, via a signal-0 probe.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// killProcessTree signals pid, its process group, AND every transitive
// descendant. The tree walk is the robust part: a runner like Air re-execs
// its server in a NEW process group on every rebuild, so signalling only
// pid's group (signalProcessGroup) leaves the respawned child squatting its
// port. Walking ppid catches it regardless of group/session games — the
// parent/child relationship is the one stable handle. Descendants are
// collected BEFORE signalling so a dying tree's shifting ppids can't hide
// a child.
func killProcessTree(pid int, sig syscall.Signal) {
	if pid <= 0 {
		return
	}
	descendants := descendantPIDs(pid)
	_ = syscall.Kill(-pid, sig) // the group (cheap; catches in-group children)
	_ = syscall.Kill(pid, sig)  // the leader
	for _, d := range descendants {
		_ = syscall.Kill(d, sig)
	}
}

// ppidMap returns a pid→ppid snapshot of the whole process table, read
// from `ps` (works on macOS and Linux without /proc). Used by the
// ownership resolver to walk a port-holder's ancestry looking for the
// FORGE_UP_ENV marker. A nil/empty map (ps failed) makes ancestry walks a
// no-op — the caller then only trusts the holder's own env.
func ppidMap() map[int]int {
	out, err := exec.CommandContext(context.Background(), "ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	m := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil {
			continue
		}
		m[pid] = ppid
	}
	return m
}

// listPIDs returns every pid in the process table — the scan surface for
// `forge up stop`'s marker sweep (find every process carrying our
// FORGE_UP_ENV, whatever port it holds).
func listPIDs() []int {
	m := ppidMap()
	out := make([]int, 0, len(m))
	for pid := range m {
		out = append(out, pid)
	}
	return out
}

// portListenerPID returns the pid LISTENing on 127.0.0.1:<port>, or 0 when
// none is found / the lookup tool is unavailable. Uses lsof (present by
// default on macOS; commonly on Linux). A 0 return classifies the holder as
// unidentifiable → foreign, so a missing lsof degrades to today's safe
// "never reclaim an unknown process" behaviour rather than a misfire.
func portListenerPID(port int) int {
	if port <= 0 {
		return 0
	}
	out, err := exec.CommandContext(context.Background(),
		"lsof", "-nP", "-tiTCP:"+strconv.Itoa(port), "-sTCP:LISTEN").Output()
	if err != nil {
		return 0
	}
	// lsof -t prints one pid per line; take the first.
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if pid, e := strconv.Atoi(line); e == nil {
				return pid
			}
		}
	}
	return 0
}

// descendantPIDs returns every transitive child of root, reading the
// (pid, ppid) table from `ps` so it works on macOS and Linux alike without
// depending on /proc.
func descendantPIDs(root int) []int {
	out, err := exec.CommandContext(context.Background(), "ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}
	var result []int
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			if !seen[c] {
				seen[c] = true
				result = append(result, c)
				queue = append(queue, c)
			}
		}
	}
	return result
}
