package cli

import (
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// TestResolveUpLifecycle pins the lifecycle decision behind `forge env up`:
// it supervises — holds the foreground and tears the stack down on a signal —
// unless --background asked it to detach. A missing TTY no longer detaches: an
// agent harness runs the command in its own background and stops it with a
// signal, and a stack that outlived the command would be one the harness can
// neither see nor stop.
func TestResolveUpLifecycle(t *testing.T) {
	if got := resolveUpLifecycle(false); got != lifecycleSupervise {
		t.Errorf("resolveUpLifecycle(background=false) = %v, want supervise (TTY or not)", got)
	}
	if got := resolveUpLifecycle(true); got != lifecycleOnce {
		t.Errorf("resolveUpLifecycle(background=true) = %v, want once (detach + return)", got)
	}
}

// TestUpTeardownSignals pins the signals a supervising `forge env up` tears its
// stack down on. SIGTERM is how a harness or a supervisor stops a command, and
// SIGHUP is its terminal closing; catching only Ctrl-C left both to kill forge
// alone, orphaning children that run in process groups of their own.
func TestUpTeardownSignals(t *testing.T) {
	for _, want := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		if !slices.Contains(upTeardownSignals, want) {
			t.Errorf("upTeardownSignals = %v, missing %v", upTeardownSignals, want)
		}
	}
}

// TestProcRegistryStoppedAfterShutdown pins the guard that keeps the deferred
// startup teardown from stopping a stack the hold already stopped.
func TestProcRegistryStoppedAfterShutdown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := newProcRegistry("proj-stopped", t.TempDir(), "dev")
	if p.stopped() {
		t.Fatal("a fresh registry reports stopped")
	}
	p.shutdown()
	if !p.stopped() {
		t.Fatal("registry does not report stopped after shutdown")
	}
}

// TestUpWatchFlagRegistered confirms `forge env up --watch` is still wired —
// scripts pass it — and that its help says the hold is now the default.
func TestUpWatchFlagRegistered(t *testing.T) {
	cmd := newEnvUpCmd()
	f := cmd.Flags().Lookup("watch")
	if f == nil {
		t.Fatal("--watch flag not registered on forge env up")
	}
	if f.DefValue != "false" {
		t.Errorf("--watch default: got %q, want false", f.DefValue)
	}
	if f.Value.Type() != "bool" {
		t.Errorf("--watch should be a bool flag, got %q", f.Value.Type())
	}
	if !strings.Contains(strings.ToLower(f.Usage), "default") {
		t.Errorf("--watch usage should say the hold is the default, got %q", f.Usage)
	}
}

// TestUpWatchBackgroundMutuallyExclusive confirms --watch + --background is
// rejected at flag-parse time: they pull the lifecycle in opposite
// directions (hold vs detach), and a user passing both has a mistaken model.
func TestUpWatchBackgroundMutuallyExclusive(t *testing.T) {
	cmd := newEnvUpCmd()
	cmd.SetArgs([]string{"dev", "--watch", "--background"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected --watch + --background to be mutually exclusive")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutual exclusion, got %v", err)
	}
}
