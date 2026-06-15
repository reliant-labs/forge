//go:build !windows

package cli

import (
	"os/exec"
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
