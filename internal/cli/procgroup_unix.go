//go:build !windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/reliant-labs/forge/internal/procguard"
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
// fan a signal out to unrelated processes. A member of this command's own
// lineage is refused, and a group holding one is narrowed to pid alone (see
// procguard).
func signalProcessGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	self := procguard.Self()
	if self.Contains(pid) {
		return &procguard.LineageError{PID: pid, Command: commandLabel(readProcArgv(pid))}
	}
	if self.ContainsGroup(pid) {
		return syscall.Kill(pid, sig)
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
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// killProcessTree signals pid, its process group, every transitive
// descendant, AND every process group a descendant leads. The tree walk is
// the robust part: a runner like Air starts its server in a NEW process group
// (Setpgid) on every rebuild, so signalling only pid's group
// (signalProcessGroup) leaves that server squatting its port. Walking ppid
// catches it regardless of group/session games — the parent/child
// relationship is the one stable handle — and signalling the group it leads
// catches what IT forked (a `sh -c` wrapper's children). Descendants are
// collected BEFORE signalling so a dying tree's shifting ppids can't hide a
// child.
//
// The walk only sees what is still attached: a runner that has already died
// left its children with init. Teardown therefore keeps every runner alive
// until it is signalled here (procRegistry.start) and sweeps the run's
// ownership markers for what escaped anyway (procRegistry.shutdown).
//
// It is also the last line of the lineage guard (procguard): the teardown
// planners already set aside every root that hosts this command, and this
// refuses one that reaches here anyway, skips a group-wide signal to a group
// this command belongs to, and never signals a lineage member found among the
// descendants.
func killProcessTree(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	self := procguard.Self()
	if self.Contains(pid) {
		return &procguard.LineageError{PID: pid, Command: commandLabel(readProcArgv(pid))}
	}
	targets := treeSignalTargets(pid, processTable(), self.Contains, self.ContainsGroup)
	var failures []error
	// A missing group is normal after its leader exits. Permission failures
	// are not proof of shutdown, including when a direct leader signal works.
	for _, target := range targets {
		if err := syscall.Kill(target, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			failures = append(failures, fmt.Errorf("signal %s to pid/group %d: %w", sig, target, err))
		}
	}
	return errors.Join(failures...)
}

// treeSignalTargets is killProcessTree's pure decision: the kill(2) targets
// for root's tree, read from one process-table snapshot. Negative entries are
// process groups. It names root's group, root, every descendant, and the group
// of every descendant that leads one (pgid == pid). A group is only ever
// signalled when its leader is in the tree, so no group a stranger leads is
// reached; and nothing in this command's lineage, nor a group it belongs to,
// is ever a target.
func treeSignalTargets(root int, table map[int]procRow, inLineage, lineageGroup func(int) bool) []int {
	descendants := descendantsIn(root, table)
	targets := make([]int, 0, 2*len(descendants)+2)
	if !lineageGroup(root) {
		targets = append(targets, -root)
	}
	targets = append(targets, root)
	for _, d := range descendants {
		if inLineage(d) {
			continue
		}
		targets = append(targets, d)
		if row, ok := table[d]; ok && row.pgid == d && !lineageGroup(d) {
			targets = append(targets, -d)
		}
	}
	return targets
}

// procRow is one process-table entry: a pid's parent and process group.
type procRow struct {
	ppid int
	pgid int
}

// processTable returns a pid→(ppid, pgid) snapshot of the whole process table,
// read from `ps` (works on macOS and Linux without /proc). Nil when ps fails:
// tree walks then see no descendants, and a caller signals the root alone.
func processTable() map[int]procRow {
	out, err := exec.CommandContext(context.Background(), "ps", "-axo", "pid=,ppid=,pgid=").Output()
	if err != nil {
		return nil
	}
	return parseProcessTable(string(out))
}

// parseProcessTable parses `ps -axo pid=,ppid=,pgid=` output. Lines that are
// not three integers are skipped.
func parseProcessTable(out string) map[int]procRow {
	m := map[int]procRow{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		pgid, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		m[pid] = procRow{ppid: ppid, pgid: pgid}
	}
	return m
}

// descendantsIn returns every transitive child of root in table, breadth
// first. Cycle-safe.
func descendantsIn(root int, table map[int]procRow) []int {
	children := map[int][]int{}
	for pid, row := range table {
		children[row.ppid] = append(children[row.ppid], pid)
	}
	for _, c := range children {
		sort.Ints(c)
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

// procStartTimes returns the wall-clock start time of each requested pid, read
// from `ps -o lstart=` (works on macOS and Linux without /proc) in ONE call so
// the whole set costs a single fork. Used by `forge env status` build-freshness
// to tell a freshly-launched server from a straggler running an older build
// (its start predates the on-disk binary's mtime). A pid missing from the
// result had no readable start time (dead, or `ps` unavailable) and the caller
// omits its started_at.
func procStartTimes(pids []int) map[int]time.Time {
	out := map[int]time.Time{}
	list := make([]string, 0, len(pids))
	for _, p := range pids {
		if p > 0 {
			list = append(list, strconv.Itoa(p))
		}
	}
	if len(list) == 0 {
		return out
	}
	o, err := exec.CommandContext(context.Background(),
		"ps", "-o", "pid=,lstart=", "-p", strings.Join(list, ",")).Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(o), "\n") {
		fields := strings.Fields(line)
		// "<pid> Dow Mon DD HH:MM:SS YYYY" → pid + 5 lstart fields. Fields()
		// collapses the padded single-digit day, so a single-space layout
		// parses uniformly.
		if len(fields) < 6 {
			continue
		}
		pid, perr := strconv.Atoi(fields[0])
		if perr != nil {
			continue
		}
		ts := strings.Join(fields[1:6], " ")
		t, terr := time.ParseInLocation("Mon Jan 2 15:04:05 2006", ts, time.Local)
		if terr != nil {
			continue
		}
		out[pid] = t
	}
	return out
}

// descendantPIDs returns every transitive child of root, reading the
// process table from `ps` so it works on macOS and Linux alike without
// depending on /proc.
func descendantPIDs(root int) []int {
	return descendantsIn(root, processTable())
}

// readOSStopTable reads the process table the shutdown sweep needs — parent,
// owner, state and start time of every process — in one `ps` call. An error
// (ps failed, or printed nothing parseable) means the table is unknown, which
// the sweep must never read as empty.
func readOSStopTable() (*stopTable, error) {
	out, err := exec.CommandContext(context.Background(), "ps", "-axo", "pid=,ppid=,uid=,stat=,lstart=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	procs := parseStopTable(string(out), os.Getuid())
	if len(procs) == 0 {
		return nil, errors.New("ps printed no processes")
	}
	return &stopTable{procs: procs, readEnv: readProcEnviron, readArgv: readProcArgv, isAlive: processAlive}, nil
}

// parseStopTable parses `ps -axo pid=,ppid=,uid=,stat=,lstart=`. A process
// owned by another uid is foreign (root, uid 0, may own anything it started,
// so nothing is foreign to it). A state starting with Z is a zombie. An
// unparseable start time is left zero: unknown, never "before the run".
func parseStopTable(out string, uid int) map[int]stopProc {
	procs := map[int]stopProc{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		owner, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		row := stopProc{ppid: ppid, foreign: uid != 0 && owner != uid, exited: strings.HasPrefix(f[3], "Z")}
		if len(f) >= 9 {
			if t, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(f[4:9], " "), time.Local); err == nil {
				row.started = t
			}
		}
		procs[pid] = row
	}
	return procs
}
