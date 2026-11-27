//go:build linux || darwin

package airbuild

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// CurrentRunner names the parent of this process group's leader: air, which
// starts each build command in a group of its own led by `sh -c`. Here the
// test process plays air.
func TestCurrentRunnerIsTheParentOfTheGroupLeader(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	for _, script := range []string{
		`"$0" && true`, // the shell stays the leader, as with the scaffold's `a && b`
		`exec "$0"`,    // a shell that execs its last command: this process leads
	} {
		cmd := exec.Command("sh", "-c", script, os.Args[0])
		cmd.Env = append(os.Environ(), fakeBuildEnv+"=runner")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v: %s", script, err, out)
		}
		f := strings.Fields(string(out))
		if len(f) != 3 || f[1] != "true" || f[2] != "true" {
			t.Fatalf("%s: CurrentRunner in an air-shaped child = %q, want an ID, ok and alive", script, out)
		}
		pid, _, _ := strings.Cut(f[0], "@")
		if pid != strconv.Itoa(os.Getpid()) {
			t.Fatalf("%s: runner pid %s, want this process (%d), the leader's parent", script, pid, os.Getpid())
		}
	}
}
