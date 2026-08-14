//go:build !windows

package hostinfra

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the child in its OWN SESSION so it outlives the command
// that started it.
//
// Without this the process dies with the launching shell's process group, and
// a host-infra service started by `forge run` would not survive the command
// returning — which is the whole point of a host-infra service.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// terminateProcess asks the process to shut down cleanly (SIGTERM), giving it
// the chance to flush and release its port before it is killed outright.
func terminateProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// killProcess terminates the process immediately (SIGKILL). Used only after a
// graceful stop has already timed out.
func killProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}
