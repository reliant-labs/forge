//go:build linux

package airbuild

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

func getpgrp() int { return syscall.Getpgrp() }

// lookupProc reads pid's parent and start time (clock ticks since boot) from
// /proc/<pid>/stat.
func lookupProc(pid int) (procInfo, bool) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procInfo{}, false
	}
	return parseProcStat(string(stat))
}

// parseProcStat reads ppid (field 4) and starttime (field 22) of a
// /proc/<pid>/stat line. The command name (field 2) is parenthesised and may
// hold spaces or parentheses itself, so fields are counted from its last ')'.
func parseProcStat(stat string) (procInfo, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return procInfo{}, false
	}
	f := strings.Fields(stat[i+1:])
	// f[0] is field 3 (state), so field n is f[n-3].
	if len(f) < 20 {
		return procInfo{}, false
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return procInfo{}, false
	}
	return procInfo{ppid: ppid, start: f[19]}, true
}
