package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/reliant-labs/forge/internal/hostlaunch"
	"github.com/reliant-labs/forge/internal/procguard"
)

// Stopping a supervised stack: SIGTERM, a grace period, then SIGKILL.
const (
	// defaultStopGrace is how long a stopping stack's processes get between
	// SIGTERM and SIGKILL when nothing in the stack declares a longer drain.
	defaultStopGrace = 10 * time.Second
	// maxStopGrace caps how far a runner's declared drain extends the grace,
	// so one misconfigured kill_delay cannot hold Ctrl-C for minutes.
	maxStopGrace = 30 * time.Second
	// stopGraceMargin is the time beyond a runner's own kill delay that it
	// needs to escalate to SIGKILL, reap its child and exit itself.
	stopGraceMargin = 3 * time.Second
)

// Shutdown timings a test shrinks. stopReapWait bounds the wait for the
// children's exit to be observed after SIGKILL, so a process holding one of
// their output pipes can never hang the command. stopSweepInterval is how often
// the grace period looks for processes of this run that left the tree.
var (
	stopReapWait      = 5 * time.Second
	stopSweepInterval = time.Second
)

// requireStopGrace extends the shutdown grace so a runner that declares its
// own drain (Air's kill_delay) can finish it before forge escalates. A drain
// that cannot fit under maxStopGrace is reported once, at start, rather than
// discovered as a SIGKILL at the next Ctrl-C.
func (p *procRegistry) requireStopGrace(who string, drain time.Duration) {
	if drain <= 0 {
		return
	}
	need := drain + stopGraceMargin
	if need > maxStopGrace {
		fmt.Printf("[up] %s: its runner drains for %s on stop, longer than forge waits (%s): stopping the stack will SIGKILL it before that drain ends — lower its kill_delay\n",
			who, drain, maxStopGrace-stopGraceMargin)
		need = maxStopGrace
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if need > p.stopGrace && need > defaultStopGrace {
		p.stopGrace = need
		p.graceFor = fmt.Sprintf("%s drains for %s", who, drain)
	}
}

// shutdown stops every process this run started, and everything they started:
//
//  1. SIGTERM each runner's whole tree (killProcessTree), while the runners
//     are still alive to anchor it — and every process carrying this run's
//     FORGE_UP_RUN marker that already left the tree (a runner that died, or
//     detached its child into a session of its own).
//  2. Wait up to the grace for all of it to exit, sweeping for escapees once a
//     stopSweepInterval and signalling any new one.
//  3. At the deadline, SIGKILL the trees and every marked process left.
//  4. Wait, bounded, for the exits to be observed; report anything still
//     running and keep the stack's records so `forge env down` can reach it.
//
// The marker sweep is what makes this hold for any runner: the tree walk sees
// only what is still attached to a live runner, but every descendant inherits
// the marker however it was started. The sweep never selects a process in
// this command's own lineage (procguard), and the marker is unique to this
// run, so a stack another run started is never touched.
func (p *procRegistry) shutdown() {
	p.mu.Lock()
	p.isStopped = true
	procs := make([]*managedProcess, len(p.processes))
	copy(procs, p.processes)
	grace, graceFor := p.stopGrace, p.graceFor
	p.mu.Unlock()
	if grace <= 0 {
		grace = defaultStopGrace
	}
	if graceFor != "" {
		fmt.Printf("[up] allowing %s for a graceful stop (%s)\n", grace.Round(time.Second), graceFor)
	}

	managed := map[int]bool{}
	for _, mp := range procs {
		if pid := procPID(mp); pid > 0 {
			managed[pid] = true
		}
	}
	// termed is every process the tree pass below SIGTERMs. One that later
	// leaves its tree (its runner gave up waiting for it) is still draining,
	// and a second signal could cut that drain short.
	termed := map[int]bool{}
	// Reverse order so later-started frontends die before the host services
	// they may have spoken to — keeps the user's last log lines clean.
	for i := len(procs) - 1; i >= 0; i-- {
		mp := procs[i]
		pid := procPID(mp)
		if pid <= 0 || managedProcessExited(mp) {
			continue
		}
		fmt.Printf("[up] stopping %s (pid=%d)...\n", mp.name, pid)
		for _, d := range descendantPIDs(pid) {
			termed[d] = true
		}
		_ = killProcessTree(pid, syscall.SIGTERM)
	}
	escaped := map[int]bool{}
	p.signalEscaped(managed, escaped, termed, syscall.SIGTERM)

	exited := allExited(procs)
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	tick := time.NewTicker(stopSweepInterval)
	defer tick.Stop()
	timedOut := false
	for !timedOut {
		select {
		case <-exited:
			exited = nil
			p.signalEscaped(managed, escaped, termed, syscall.SIGTERM)
		case <-tick.C:
			p.signalEscaped(managed, escaped, termed, syscall.SIGTERM)
		case <-deadline.C:
			timedOut = true
			continue
		}
		if exited == nil && !anyAlive(escaped) {
			break
		}
	}

	if timedOut {
		for _, mp := range procs {
			pid := procPID(mp)
			if pid <= 0 || managedProcessExited(mp) {
				continue
			}
			fmt.Printf("[up] %s: did not exit, killing.\n", mp.name)
			_ = killProcessTree(pid, syscall.SIGKILL)
		}
		for _, pid := range sortedPIDs(escaped) {
			if processAlive(pid) {
				fmt.Printf("[up] pid %d did not exit, killing.\n", pid)
				_ = killProcessTree(pid, syscall.SIGKILL)
			}
		}
		for _, pid := range p.runRoots(managed) {
			if escaped[pid] {
				continue
			}
			fmt.Printf("[up] pid %d (%s) did not exit, killing.\n", pid, serviceOfPID(pid, newOSProcFacts()))
			_ = killProcessTree(pid, syscall.SIGKILL)
		}
	}

	left := p.awaitStopped(procs, managed)
	if len(left) > 0 {
		fmt.Printf("[up] warning: still running after SIGKILL: %s — the stack's records are kept; `forge env down %s` retries\n",
			strings.Join(left, ", "), p.env)
		return
	}
	removeStackRecords(p.projectID, p.env)
}

// awaitStopped waits (bounded by stopReapWait) for every managed process's
// exit to be observed, then lists what is still alive: a managed process whose
// exit never came, or a process carrying this run's marker. Empty means the
// stack is down.
func (p *procRegistry) awaitStopped(procs []*managedProcess, managed map[int]bool) []string {
	select {
	case <-allExited(procs):
	case <-time.After(stopReapWait):
	}
	var left []string
	for _, mp := range procs {
		if pid := procPID(mp); pid > 0 && !managedProcessExited(mp) && processAlive(pid) {
			left = append(left, fmt.Sprintf("%s (pid %d)", mp.name, pid))
		}
	}
	// A process SIGKILLed a moment ago can still be on its way out.
	stragglers := p.runRoots(managed)
	for until := time.Now().Add(time.Second); len(stragglers) > 0 && time.Now().Before(until); {
		time.Sleep(100 * time.Millisecond)
		stragglers = p.runRoots(managed)
	}
	for _, pid := range stragglers {
		left = append(left, fmt.Sprintf("pid %d", pid))
	}
	return left
}

// signalEscaped records in escaped every process of this run that is outside
// the runners' trees, so the grace waits for it and its deadline kills it, and
// sends sig to the tree of each one newly found — unless the tree pass already
// signalled it (termed) before it left.
func (p *procRegistry) signalEscaped(managed, escaped, termed map[int]bool, sig syscall.Signal) {
	var facts procFacts
	for _, pid := range p.runRoots(managed) {
		if escaped[pid] {
			continue
		}
		escaped[pid] = true
		if termed[pid] {
			continue
		}
		if facts == nil {
			facts = newOSProcFacts()
		}
		fmt.Printf("[up] stopping %s's detached process (pid=%d)...\n", serviceOfPID(pid, facts), pid)
		_ = killProcessTree(pid, sig)
	}
}

// runRoots is every live process tree carrying this run's markers that is NOT
// one of the runners this registry launched — the processes a runner left
// behind. Empty when the process table cannot be read.
func (p *procRegistry) runRoots(managed map[int]bool) []int {
	facts := newOSProcFacts()
	if len(facts.ppids) == 0 {
		return nil
	}
	roots := runMarkedRoots(p.projectID, p.env, p.runID, facts.pidList(), facts, procguard.Self())
	out := roots[:0]
	for _, pid := range roots {
		if !managed[pid] && processAlive(pid) {
			out = append(out, pid)
		}
	}
	return out
}

// runMarkedRoots is the pure selection core of the shutdown sweep: of pids,
// the subtree roots of every process stamped with this run's FORGE_UP_RUN AND
// this stack's (project, env) markers. A process in this command's lineage is
// never selected, and an empty runID or projectID selects nothing.
func runMarkedRoots(projectID, env, runID string, pids []int, f procFacts, lineage procguard.Lineage) []int {
	if runID == "" || projectID == "" {
		return nil
	}
	marked := map[int]bool{}
	for _, pid := range pids {
		if pid <= 1 || lineage.Contains(pid) {
			continue
		}
		environ, ok := f.environ(pid)
		if !ok || markerValue(environ, forgeUpRunVar) != runID {
			continue
		}
		if _, matched := markerMatches(environ, projectID, env); matched {
			marked[pid] = true
		}
	}
	return subtreeRoots(marked, f)
}

// markerValue is the last value of key in a KEY=VALUE environment (exec's
// last-wins semantics), or "".
func markerValue(environ []string, key string) string {
	prefix := key + "="
	v := ""
	for _, kv := range environ {
		if strings.HasPrefix(kv, prefix) {
			v = kv[len(prefix):]
		}
	}
	return v
}

// allExited returns a channel closed once every managed process's exit has
// been observed (its Wait returned).
func allExited(procs []*managedProcess) chan struct{} {
	ch := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, mp := range procs {
			mp := mp
			wg.Add(1)
			go func() {
				defer wg.Done()
				if mp.done != nil {
					<-mp.done
				} else if mp.cmd != nil {
					_ = mp.cmd.Wait()
				}
			}()
		}
		wg.Wait()
		close(ch)
	}()
	return ch
}

func anyAlive(pids map[int]bool) bool {
	for pid := range pids {
		if processAlive(pid) {
			return true
		}
	}
	return false
}

func sortedPIDs(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// airStopDrain is how long the air runner of spec waits for its server after
// interrupting it (`[build] kill_delay`, read with air's own rule: a value
// under a millisecond is a count of milliseconds). Zero when the config cannot
// be read, or when air does not interrupt at all (send_interrupt = false: it
// SIGKILLs the server at once). dir is the directory air runs in.
func airStopDrain(spec hostlaunch.RunnerSpec, dir string) time.Duration {
	if strings.TrimSpace(spec.Runner) != "air" {
		return 0
	}
	path := spec.AirConfig
	if path == "" {
		path = hostlaunch.DefaultAirConfig
	}
	if !filepath.IsAbs(path) && dir != "" {
		path = filepath.Join(dir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return parseAirStopDrain(data)
}

func parseAirStopDrain(data []byte) time.Duration {
	var cfg struct {
		Build struct {
			SendInterrupt bool          `toml:"send_interrupt"`
			KillDelay     time.Duration `toml:"kill_delay"`
		} `toml:"build"`
	}
	if _, err := toml.Decode(string(data), &cfg); err != nil || !cfg.Build.SendInterrupt {
		return 0
	}
	d := cfg.Build.KillDelay
	if d > 0 && d < time.Millisecond {
		d *= time.Millisecond
	}
	return d
}
