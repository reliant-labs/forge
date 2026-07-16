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

// processAlive: best-effort liveness via FindProcess (always succeeds on
// Windows, so this is a weak signal — adequate for the Unix-first dev loop).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}

// killProcessTree: no job-object tree walk on Windows; fall back to
// signalling the single process. The Unix path does the real tree teardown.
func killProcessTree(pid int, sig syscall.Signal) {
	_ = signalProcessGroup(pid, sig)
}

// ppidMap / listPIDs / portListenerPID: the marker-based ownership
// reclaim (up_reclaim.go) is Unix-first. On Windows these return empty so
// ownership resolution degrades to "unidentifiable → foreign" — never a
// misfire — keeping the build green without a tasklist/netstat port.
func ppidMap() map[int]int { return nil }

func listPIDs() []int { return nil }

func portListenerPID(_ int) int { return 0 }
