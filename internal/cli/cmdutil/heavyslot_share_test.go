package cmdutil

import (
	"context"
	"io"
	"os"
	"runtime"
	"testing"

	"github.com/reliant-labs/forge/pkg/hostslot"
)

// While HoldHeavySlot holds a slot of its own the command runs on the
// slot's CPU share — its runtime and its environment, for the children it
// starts — and both are put back on release. GOMAXPROCS set by whoever ran
// the command stands.
func TestHoldHeavySlot_RunsOnTheShare(t *testing.T) {
	if hostslot.DetectMachine().CPUs < 2 {
		t.Skip("needs 2 CPUs: a share of 1 on a 1-CPU machine is the whole machine, and exports nothing")
	}
	setup := func(t *testing.T, gomaxprocs string) {
		t.Setenv(hostslot.EnvDir, t.TempDir())
		t.Setenv(hostslot.EnvHeld, "")
		t.Setenv(hostslot.SlotsEnv(hostslot.Heavy), "2")
		t.Setenv(hostslot.ShareEnv(hostslot.Heavy), "1")
		t.Setenv("GOMAXPROCS", gomaxprocs)
		t.Setenv("UV_THREADPOOL_SIZE", "")
	}
	t.Run("share applied and restored", func(t *testing.T) {
		setup(t, "")
		before := runtime.GOMAXPROCS(0)
		release, err := HoldHeavySlot(context.Background(), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		got, env, uv := runtime.GOMAXPROCS(0), os.Getenv("GOMAXPROCS"), os.Getenv("UV_THREADPOOL_SIZE")
		release()
		if got != 1 || env != "1" || uv != "1" {
			t.Errorf("while held: runtime GOMAXPROCS %d, env GOMAXPROCS %q, UV_THREADPOOL_SIZE %q; want 1, 1, 1", got, env, uv)
		}
		if after := runtime.GOMAXPROCS(0); after != before {
			t.Errorf("runtime GOMAXPROCS %d after release, want %d back", after, before)
		}
		if env := os.Getenv("GOMAXPROCS"); env != "" {
			t.Errorf("env GOMAXPROCS %q after release, want it unset again", env)
		}
	})
	t.Run("caller's GOMAXPROCS stands", func(t *testing.T) {
		setup(t, "3")
		before := runtime.GOMAXPROCS(0)
		release, err := HoldHeavySlot(context.Background(), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		got, env := runtime.GOMAXPROCS(0), os.Getenv("GOMAXPROCS")
		release()
		if got != before || env != "3" {
			t.Errorf("while held: runtime GOMAXPROCS %d, env %q; want %d and the caller's 3 untouched", got, env, before)
		}
	})
	t.Run("share off", func(t *testing.T) {
		setup(t, "")
		t.Setenv(hostslot.ShareEnv(hostslot.Heavy), "off")
		before := runtime.GOMAXPROCS(0)
		release, err := HoldHeavySlot(context.Background(), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		got, env := runtime.GOMAXPROCS(0), os.Getenv("GOMAXPROCS")
		release()
		if got != before || env != "" {
			t.Errorf("FORGE_HEAVY_SHARE=off: runtime GOMAXPROCS %d, env %q; want %d and none", got, env, before)
		}
	})
}
