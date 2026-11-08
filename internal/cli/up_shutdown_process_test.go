//go:build !windows

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestShutdownFakeRunnerHelper is not a test: it is the fake runner and server
// the shutdown tests launch, selected by FORGE_SHUTDOWN_HELPER.
//
//   - runner: starts a server in a NEW SESSION (as Air starts its server in a
//     new process group), handing it this process's stdout — as Air does, so
//     the server holds the pipe forge reads. With FORGE_SHUTDOWN_DETACH set it
//     starts the server through a spawner that exits at once, so the server is
//     reparented to init before any shutdown begins. It exits on SIGTERM
//     without stopping the server: the runner that gives up.
//   - spawner: starts the server the same way and exits.
//   - server: writes its pid, then ignores SIGTERM and SIGINT. Only SIGKILL
//     stops it.
func TestShutdownFakeRunnerHelper(t *testing.T) {
	switch os.Getenv("FORGE_SHUTDOWN_HELPER") {
	case "runner":
		if os.Getenv("FORGE_SHUTDOWN_DETACH") != "" {
			spawnShutdownHelper("spawner")
		} else {
			spawnShutdownHelper("server")
		}
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM)
		<-sigs
		os.Exit(0)
	case "spawner":
		spawnShutdownHelper("server")
		os.Exit(0)
	case "server":
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
		pidFile := os.Getenv("FORGE_SHUTDOWN_PIDFILE")
		if err := os.WriteFile(pidFile+".tmp", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(3)
		}
		if err := os.Rename(pidFile+".tmp", pidFile); err != nil {
			os.Exit(3)
		}
		fmt.Println("server up")
		select {}
	}
}

func spawnShutdownHelper(mode string) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestShutdownFakeRunnerHelper$")
	cmd.Env = append(os.Environ(), "FORGE_SHUTDOWN_HELPER="+mode)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
}

// TestShutdownStopsWhatARunnerDetached is the regression test for servers
// outliving a Ctrl-C of `forge env up`. Each case launches a runner that
// starts a SIGTERM-ignoring server outside its own process group and session,
// and abandons it when signalled; after shutdown nothing it started may be
// alive, and shutdown must return rather than wait forever on the pipe the
// server still holds.
//
//   - ctx cancelled: the runner's command is built with exec.CommandContext on
//     the run's signal context, as every host runner is, and the context is
//     cancelled before shutdown — what Ctrl-C does. That used to SIGKILL the
//     runner on the spot, orphaning the server before any tree walk.
//   - detached before stop: the server has already left the runner's tree
//     when shutdown begins, so only the run's marker can find it.
func TestShutdownStopsWhatARunnerDetached(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real runner and server processes and waits out a stop grace; runs in task test")
	}
	requireProcInspection(t)
	for _, tc := range []struct {
		name   string
		detach bool
	}{
		{"ctx cancelled", false},
		{"detached before stop", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, projectID, env := testStack(t)
			t.Chdir(dir)
			prevReap, prevSweep := stopReapWait, stopSweepInterval
			stopReapWait, stopSweepInterval = 3*time.Second, 100*time.Millisecond
			t.Cleanup(func() { stopReapWait, stopSweepInterval = prevReap, prevSweep })

			pidFile := filepath.Join(t.TempDir(), "server.pid")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShutdownFakeRunnerHelper$")
			cmd.Env = append(os.Environ(), "FORGE_SHUTDOWN_HELPER=runner", "FORGE_SHUTDOWN_PIDFILE="+pidFile)
			if tc.detach {
				cmd.Env = append(cmd.Env, "FORGE_SHUTDOWN_DETACH=1")
			}

			reg := newProcRegistry(projectID, dir, env)
			reg.stopGrace = time.Second
			if err := reg.start("api", cmd, false); err != nil {
				t.Fatal(err)
			}
			runner := reg.processes[0].pid
			t.Cleanup(func() { _ = syscall.Kill(runner, syscall.SIGKILL) })
			server := waitForPIDFile(t, pidFile)
			t.Cleanup(func() { _ = syscall.Kill(server, syscall.SIGKILL) })

			if tc.detach {
				waitUntil(t, "the server is reparented away from the runner", func() bool {
					ppid, ok := ppidMap()[server]
					return ok && ppid != runner
				})
			}
			cancel()
			// The cancelled context must not have killed the runner: shutdown
			// owns that, and a runner killed early is what orphaned the server.
			// (A killed runner lingers as a zombie until the server releases
			// its pipe, so ask ps for its state rather than probing the pid.)
			time.Sleep(200 * time.Millisecond)
			if state := processState(runner); state == "" || strings.HasPrefix(state, "Z") {
				t.Fatalf("cancelling the run's context killed the runner before shutdown (state %q)", state)
			}
			if !tc.detach {
				if ppid := ppidMap()[server]; ppid != runner {
					t.Fatalf("the server's parent is %d, not the runner %d: it was orphaned before shutdown", ppid, runner)
				}
			}

			start := time.Now()
			out := captureStdout(t, reg.shutdown)
			if took := time.Since(start); took > reg.stopGrace+stopReapWait+3*time.Second {
				t.Errorf("shutdown took %s", took)
			}
			waitUntil(t, "the server is gone", func() bool { return !processAlive(server) })
			if processAlive(runner) {
				t.Errorf("runner pid %d survived shutdown", runner)
			}
			if strings.Contains(out, "still running after SIGKILL") {
				t.Errorf("shutdown reported survivors:\n%s", out)
			}
		})
	}
}

// processState is pid's ps STAT column ("" when ps knows no such pid).
func processState(pid int) string {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitUntil(t, "the server writes its pid", func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 0
	})
	return pid
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
