//go:build linux || darwin

package airbuild

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// waiterScript is the held build after execWaiter: it waits, a second at a
// time, while the tmp_dir ($1) exists and the build's process-group leader
// ($2) is still air's ($3) child, then exits with the build's status ($4).
// ps failing (the leader gone) ends the wait too.
const waiterScript = `while [ -d "$1" ]; do p=$(ps -o ppid= -p "$2" 2>/dev/null) || break; [ "${p##* }" = "$3" ] || break; sleep 1; done; exit "$4"`

// execWaiter replaces this process with waiterScript under /bin/sh, so a held
// build costs a shell (about a megabyte) rather than a forge (tens of
// megabytes) for the rest of the air session: one is left per failed build.
// It returns only when it could not exec; the caller then holds in-process.
func execWaiter(out string, code int) error {
	sh, err := exec.LookPath("/bin/sh")
	if err != nil {
		return err
	}
	for _, tool := range []string{"ps", "sleep"} {
		if _, err := exec.LookPath(tool); err != nil {
			return err
		}
	}
	leader := getpgrp()
	l, ok := lookupProc(leader)
	if leader <= 1 || !ok || l.ppid <= 1 {
		return errors.New("no air process to wait on")
	}
	dir, err := filepath.Abs(filepath.Dir(out))
	if err != nil {
		return err
	}
	argv := []string{"sh", "-c", waiterScript, "forge-air-build-held", dir, strconv.Itoa(leader), strconv.Itoa(l.ppid), strconv.Itoa(code)}
	return syscall.Exec(sh, argv, os.Environ()) //nolint:gosec // a fixed script; the arguments are numbers and a path
}
