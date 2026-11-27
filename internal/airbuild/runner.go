package airbuild

import "strconv"

// procInfo is what the runner lookup needs of one process: its parent, and a
// start time that, with the pid, names it for its lifetime.
type procInfo struct {
	ppid  int
	start string
}

// CurrentRunner names the air process this command runs under.
//
// air starts each build command in a process group of its own (Setpgid), led
// by the `sh -c` running the config's cmd; everything that command runs (the
// forge shim, `reliant forge`, this process) stays in that group. So the
// group's leader is air's child, and the leader's parent is air: that holds
// however many processes sit between the leader and this one, and when a
// shell execs its last command the leader is this process itself. The ID is
// air's pid and start time, so a later air that reuses the pid is a different
// runner; alive reports whether the leader still has that parent (it is
// reparented when air dies).
//
// ok is false when the platform's process table cannot be read (lookupProc).
func CurrentRunner() (string, func() bool, bool) {
	leader := getpgrp()
	if leader <= 1 {
		return "", nil, false
	}
	l, ok := lookupProc(leader)
	if !ok || l.ppid <= 1 {
		return "", nil, false
	}
	air := l.ppid
	a, ok := lookupProc(air)
	if !ok || a.start == "" {
		return "", nil, false
	}
	alive := func() bool {
		l, ok := lookupProc(leader)
		if !ok || l.ppid != air {
			return false
		}
		now, ok := lookupProc(air)
		return ok && now.start == a.start
	}
	return strconv.Itoa(air) + "@" + a.start, alive, true
}
