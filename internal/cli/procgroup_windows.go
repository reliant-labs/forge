//go:build windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

// Windows has no POSIX process groups in the kill(2) sense. forge's dev
// loop (`forge up`) is Unix-first — k3d, air, docker — so these are
// best-effort no-op / single-process fallbacks that keep the build green
// on Windows rather than a full job-object implementation.

func startInOwnProcessGroup(_ *exec.Cmd) {}

func signalProcessGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}
