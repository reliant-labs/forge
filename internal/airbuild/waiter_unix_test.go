//go:build linux || darwin

package airbuild

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The shell a held build becomes waits while the tmp_dir exists and the
// group leader is still air's child, then exits with the build's status. Here
// a `sleep` plays the leader and this test process plays air.
func TestWaiterScriptWaitsWhileAirRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("polls once a second for real; runs in task test")
	}
	for _, sh := range []string{"/bin/sh", "ps", "sleep"} {
		if _, err := exec.LookPath(sh); err != nil {
			t.Skipf("no %s", sh)
		}
	}
	for _, end := range []string{"tmp_dir removed", "leader gone"} {
		t.Run(end, func(t *testing.T) {
			leader := exec.Command("sleep", "60")
			if err := leader.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = leader.Process.Kill(); _ = leader.Wait() })
			dir := filepath.Join(t.TempDir(), "tmp")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			w := exec.Command("/bin/sh", "-c", waiterScript, "held", dir, strconv.Itoa(leader.Process.Pid), strconv.Itoa(os.Getpid()), "7")
			if err := w.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- w.Wait() }()
			select {
			case err := <-done:
				t.Fatalf("the waiter exited (%v) while air runs", err)
			case <-time.After(1500 * time.Millisecond):
			}
			if end == "tmp_dir removed" {
				_ = os.RemoveAll(dir)
			} else {
				_ = leader.Process.Kill()
				_ = leader.Wait()
			}
			select {
			case err := <-done:
				var ee *exec.ExitError
				if !errors.As(err, &ee) || ee.ExitCode() != 7 {
					t.Fatalf("waiter exit = %v, want the build's status 7", err)
				}
			case <-time.After(10 * time.Second):
				_ = w.Process.Kill()
				t.Fatal("the waiter outlived air")
			}
		})
	}
}
