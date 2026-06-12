package cli

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runWaitLoop is the post-startup supervisor for `forge run`: it blocks
// until a signal, a dev-proxy error, or a child death ends the session.
// The honesty contract (journey fr-00ff2c98d2: air exited Code 1 after a
// generate-induced rebuild race and the orchestrator kept serving the
// proxy + frontend over a dead backend with no message):
//
//   - a SERVER child death gets exactly ONE automatic restart (the
//     rebuild race is transient — the stale binary's listener needs a
//     moment to die); a second death aborts the session.
//   - any other child death aborts immediately.
//   - every death is announced via the announce callback before any
//     decision, so the loud banner prints whether or not we restart.
//   - the loop never restarts more than once (capped — no flap loops).
func TestRunWaitLoop(t *testing.T) {
	t.Parallel()

	newChans := func() (chan os.Signal, chan error, chan *managedProcess) {
		return make(chan os.Signal, 1), make(chan error, 2), make(chan *managedProcess, 8)
	}
	runAsync := func(t *testing.T, sig chan os.Signal, devErr chan error, exits chan *managedProcess, serverName string, restart func() error, announce func(*managedProcess)) chan error {
		t.Helper()
		out := make(chan error, 1)
		go func() { out <- runWaitLoop(sig, devErr, exits, serverName, restart, announce) }()
		return out
	}
	wait := func(t *testing.T, out chan error) error {
		t.Helper()
		select {
		case err := <-out:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("runWaitLoop did not return")
			return nil
		}
	}

	t.Run("signal shuts down cleanly", func(t *testing.T) {
		t.Parallel()
		sig, devErr, exits := newChans()
		out := runAsync(t, sig, devErr, exits, "app", nil, func(*managedProcess) {})
		sig <- syscall.SIGINT
		if err := wait(t, out); err != nil {
			t.Fatalf("signal shutdown = %v, want nil", err)
		}
	})

	t.Run("dev proxy error aborts with non-nil error", func(t *testing.T) {
		t.Parallel()
		sig, devErr, exits := newChans()
		out := runAsync(t, sig, devErr, exits, "app", nil, func(*managedProcess) {})
		devErr <- errors.New("listener revoked")
		err := wait(t, out)
		if err == nil || !strings.Contains(err.Error(), "listener revoked") {
			t.Fatalf("dev proxy death = %v, want error wrapping the cause", err)
		}
	})

	t.Run("frontend death aborts immediately, no restart", func(t *testing.T) {
		t.Parallel()
		sig, devErr, exits := newChans()
		restarts := 0
		var announced []string
		out := runAsync(t, sig, devErr, exits, "app",
			func() error { restarts++; return nil },
			func(p *managedProcess) { announced = append(announced, p.name) })
		exits <- &managedProcess{name: "web", done: make(chan struct{})}
		err := wait(t, out)
		if err == nil || !strings.Contains(err.Error(), "web") {
			t.Fatalf("frontend death = %v, want error naming the process", err)
		}
		if restarts != 0 {
			t.Fatalf("restarts = %d, want 0 — only the server child gets the retry", restarts)
		}
		if len(announced) != 1 || announced[0] != "web" {
			t.Fatalf("announced = %v, want the dead child announced before aborting", announced)
		}
	})

	t.Run("server death restarts once, second death aborts", func(t *testing.T) {
		t.Parallel()
		sig, devErr, exits := newChans()
		restarts := 0
		var announced []string
		out := runAsync(t, sig, devErr, exits, "app",
			func() error { restarts++; return nil },
			func(p *managedProcess) { announced = append(announced, p.name) })

		exits <- &managedProcess{name: "app", done: make(chan struct{})}
		exits <- &managedProcess{name: "app", done: make(chan struct{})}
		err := wait(t, out)
		if err == nil || !strings.Contains(err.Error(), "app") {
			t.Fatalf("second server death = %v, want error naming the server", err)
		}
		if restarts != 1 {
			t.Fatalf("restarts = %d, want exactly 1 (capped)", restarts)
		}
		if len(announced) != 2 {
			t.Fatalf("announced = %v, want both deaths announced", announced)
		}
	})

	t.Run("server death then signal: restart happened, clean shutdown", func(t *testing.T) {
		t.Parallel()
		sig, devErr, exits := newChans()
		restarted := make(chan struct{})
		out := runAsync(t, sig, devErr, exits, "app",
			func() error { close(restarted); return nil },
			func(*managedProcess) {})
		exits <- &managedProcess{name: "app", done: make(chan struct{})}
		select {
		case <-restarted:
		case <-time.After(5 * time.Second):
			t.Fatal("server death never triggered the restart")
		}
		sig <- syscall.SIGINT
		if err := wait(t, out); err != nil {
			t.Fatalf("post-restart signal shutdown = %v, want nil", err)
		}
	})

	t.Run("restart failure aborts", func(t *testing.T) {
		t.Parallel()
		sig, devErr, exits := newChans()
		out := runAsync(t, sig, devErr, exits, "app",
			func() error { return errors.New("air not found") },
			func(*managedProcess) {})
		exits <- &managedProcess{name: "app", done: make(chan struct{})}
		err := wait(t, out)
		if err == nil || !strings.Contains(err.Error(), "air not found") {
			t.Fatalf("failed restart = %v, want error wrapping the restart failure", err)
		}
	})

	t.Run("nil restart func: server death aborts immediately", func(t *testing.T) {
		t.Parallel()
		sig, devErr, exits := newChans()
		out := runAsync(t, sig, devErr, exits, "app", nil, func(*managedProcess) {})
		exits <- &managedProcess{name: "app", done: make(chan struct{})}
		err := wait(t, out)
		if err == nil || !strings.Contains(err.Error(), "app") {
			t.Fatalf("server death with nil restart = %v, want abort error", err)
		}
	})

	t.Run("child death racing a pending signal is a clean shutdown", func(t *testing.T) {
		t.Parallel()
		sig, devErr, exits := newChans()
		// Pre-load both: Ctrl-C delivers SIGINT to the whole process
		// group, so children die from the same keypress that lands on
		// sigCh — the signal is the truth.
		sig <- syscall.SIGINT
		exits <- &managedProcess{name: "web", done: make(chan struct{})}
		out := runAsync(t, sig, devErr, exits, "app", nil, func(*managedProcess) {})
		// Either order of select arms is acceptable as long as the
		// result is a clean shutdown.
		if err := wait(t, out); err != nil {
			t.Fatalf("signal+death race = %v, want nil (signal wins)", err)
		}
	})
}
