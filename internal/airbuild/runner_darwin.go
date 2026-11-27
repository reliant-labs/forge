//go:build darwin

package airbuild

import (
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

func getpgrp() int { return syscall.Getpgrp() }

// lookupProc reads pid's parent and start time from its kinfo_proc
// (sysctl kern.proc.pid).
func lookupProc(pid int) (procInfo, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return procInfo{}, false
	}
	tv := kp.Proc.P_starttime
	if tv.Sec <= 0 {
		return procInfo{}, false
	}
	start := strconv.FormatInt(int64(tv.Sec), 10) + "." + strconv.FormatInt(int64(tv.Usec), 10)
	return procInfo{ppid: int(kp.Eproc.Ppid), start: start}, true
}
