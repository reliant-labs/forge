package cmdutil

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
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

// Two slots of its own, held at once by one process (review B1 on forge#3023):
// the second holder finds the GOMAXPROCS the first exported, so its CPUEnv
// is empty, and it must neither skip the count nor tear the share down early.
// The share stays until the last holder releases, in either order, and then
// the runtime and the environment are what they were.
func TestShareProcess_ComposesAcrossHolders(t *testing.T) {
	if hostslot.DetectMachine().CPUs < 2 {
		t.Skip("needs 2 CPUs: a share of 1 on a 1-CPU machine is the whole machine, and exports nothing")
	}
	for _, firstOut := range []int{0, 1} {
		t.Run(map[int]string{0: "first in, first out", 1: "last in, first out"}[firstOut], func(t *testing.T) {
			setupShare(t)
			before := runtime.GOMAXPROCS(0)
			s := [2]*hostslot.Slot{acquireOwn(t), acquireOwn(t)}
			restore := [2]func(){shareProcess(s[0])}
			assertShare(t, "one holder", 1)
			if got := s[1].CPUEnv(os.Environ()); len(got) != 0 {
				t.Fatalf("second holder's CPUEnv = %q; want none, the case B1 describes", got)
			}
			restore[1] = shareProcess(s[1])
			if holders := shareHolders(); holders != 2 {
				t.Fatalf("procShare.holders = %d with two holders, want 2", holders)
			}
			assertShare(t, "two holders", 1)

			restore[firstOut]()
			restore[firstOut]() // a second call is a no-op, not a second release
			if holders := shareHolders(); holders != 1 {
				t.Fatalf("procShare.holders = %d after one release (called twice), want 1", holders)
			}
			assertShare(t, "one of two released", 1)

			restore[1-firstOut]()
			assertUnshared(t, before)
			if holders := shareHolders(); holders != 0 {
				t.Errorf("procShare.holders = %d after both released, want 0", holders)
			}
		})
	}
}

// Through HoldHeavySlot: goroutines that hold at once, and a hold nested in
// another. Whichever goroutine wins the slot, every holder runs on the share
// while any holds, a nested release leaves the outer hold's share in place,
// and once all are released the runtime and environment are back.
func TestHoldHeavySlot_ConcurrentAndNested(t *testing.T) {
	if hostslot.DetectMachine().CPUs < 2 {
		t.Skip("needs 2 CPUs: a share of 1 on a 1-CPU machine is the whole machine, and exports nothing")
	}
	for _, order := range [][2]int{{0, 1}, {1, 0}} {
		t.Run(fmt.Sprintf("concurrent, released %d then %d", order[0], order[1]), func(t *testing.T) {
			setupShare(t)
			before := runtime.GOMAXPROCS(0)
			var (
				releases [2]func()
				errs     [2]error
				done     sync.WaitGroup
				start    = make(chan struct{})
			)
			done.Add(2)
			for i := range 2 {
				go func() {
					defer done.Done()
					<-start
					releases[i], errs[i] = HoldHeavySlot(context.Background(), io.Discard)
				}()
			}
			close(start)
			done.Wait()
			for _, err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			assertShare(t, "both held", 1)
			releases[order[0]]()
			releases[order[1]]()
			assertUnshared(t, before)
		})
	}
	t.Run("nested", func(t *testing.T) {
		setupShare(t)
		before := runtime.GOMAXPROCS(0)
		outer, err := HoldHeavySlot(context.Background(), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		inner, err := HoldHeavySlot(context.Background(), io.Discard)
		if err != nil {
			outer()
			t.Fatal(err)
		}
		assertShare(t, "nested", 1)
		inner()
		assertShare(t, "inner released", 1)
		outer()
		assertUnshared(t, before)
	})
}

// The runtime's GOMAXPROCS is restored to the value it had, whether that was
// the runtime's default or a value somebody pinned.
func TestHoldHeavySlot_RestoresPinnedGOMAXPROCS(t *testing.T) {
	if hostslot.DetectMachine().CPUs < 3 {
		t.Skip("needs 3 CPUs: pins GOMAXPROCS to 2, above the share of 1 and below the default")
	}
	setupShare(t)
	prev := runtime.GOMAXPROCS(2)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
	release, err := HoldHeavySlot(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	assertShare(t, "held", 1)
	release()
	assertUnshared(t, 2)
}

func setupShare(t *testing.T) {
	t.Helper()
	t.Setenv(hostslot.EnvDir, t.TempDir())
	t.Setenv(hostslot.EnvHeld, "")
	t.Setenv(hostslot.SlotsEnv(hostslot.Heavy), "2")
	t.Setenv(hostslot.ShareEnv(hostslot.Heavy), "1")
	t.Setenv("GOMAXPROCS", "")
	t.Setenv("UV_THREADPOOL_SIZE", "")
}

// acquireOwn takes a heavy slot of its own, never a re-entrant one: the
// environment it reads names no held slot.
func acquireOwn(t *testing.T) *hostslot.Slot {
	t.Helper()
	s, err := hostslot.Acquire(context.Background(), hostslot.Heavy, hostslot.Options{Out: io.Discard, Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Release() })
	if s.Index() < 0 {
		t.Fatalf("slot not owned: %+v", s)
	}
	return s
}

func shareHolders() int {
	procShare.Lock()
	defer procShare.Unlock()
	return procShare.holders
}

func assertShare(t *testing.T, when string, n int) {
	t.Helper()
	want := fmt.Sprint(n)
	if got, env, uv := runtime.GOMAXPROCS(0), os.Getenv("GOMAXPROCS"), os.Getenv("UV_THREADPOOL_SIZE"); got != n || env != want || uv != want {
		t.Errorf("%s: runtime GOMAXPROCS %d, env GOMAXPROCS %q, UV_THREADPOOL_SIZE %q; want %d, %q, %q", when, got, env, uv, n, want, want)
	}
}

func assertUnshared(t *testing.T, before int) {
	t.Helper()
	if got := runtime.GOMAXPROCS(0); got != before {
		t.Errorf("runtime GOMAXPROCS %d after release, want %d back", got, before)
	}
	if env, uv := os.Getenv("GOMAXPROCS"), os.Getenv("UV_THREADPOOL_SIZE"); env != "" || uv != "" {
		t.Errorf("after release: env GOMAXPROCS %q, UV_THREADPOOL_SIZE %q; want both unset again", env, uv)
	}
}
