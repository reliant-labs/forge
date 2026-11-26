package hostslot

import (
	"slices"
	"strings"
	"testing"
)

func gibs(n float64) uint64 { return uint64(n * gib) }

func TestDefaultShare(t *testing.T) {
	big := Machine{CPUs: 48, MemBytes: gibs(125.7)}
	vm := Machine{CPUs: 16, MemBytes: gibs(32)}
	for _, tc := range []struct {
		name string
		m    Machine
		busy int
		want int
	}{
		// 48 CPUs / 125.7 GiB, the host the constants were measured on.
		{"alone keeps the machine", big, 1, 48},
		{"four busy", big, 4, 12}, // memory: 125.7/(2.5*4) = 12.6
		{"every slot busy", big, 16, 3},
		// Without memory the CPU term alone: the Reliant daemon's share.
		{"unknown memory", Machine{CPUs: 48}, 16, 4},
		// 16 vCPU / 32 GiB: memory decides, as it decides its 4 slots.
		{"memory-bound VM alone", vm, 1, 12},
		{"memory-bound VM full", vm, 4, 3},
		{"never under two", Machine{CPUs: 8, MemBytes: gibs(4)}, 8, 2},
		{"never over the machine", Machine{CPUs: 1, MemBytes: gibs(64)}, 1, 1},
		{"a quota rounds down", Machine{CPUs: 2.5}, 1, 2},
		{"busy under one is one", Machine{CPUs: 8}, 0, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DefaultShare(tc.m, tc.busy); got != tc.want {
				t.Fatalf("DefaultShare(%v, %d) = %d, want %d", tc.m, tc.busy, got, tc.want)
			}
		})
	}
}

func TestShareEnv(t *testing.T) {
	if got := ShareEnv(Heavy); got != "FORGE_HEAVY_SHARE" {
		t.Errorf("ShareEnv(heavy) = %q", got)
	}
	if got := ShareEnv("gpu-build"); got != "FORGE_GPU_BUILD_SHARE" {
		t.Errorf("ShareEnv(gpu-build) = %q", got)
	}
}

func TestCapacityShare(t *testing.T) {
	m := Machine{CPUs: 48, MemBytes: gibs(125.7)}
	t.Run("default follows the busy holders", func(t *testing.T) {
		for _, v := range []string{"unset", "", "auto", " AUTO "} {
			c, err := resolveShareOf(Capacity{Class: Heavy, Slots: 16, Machine: m}, v)
			if err != nil {
				t.Fatal(err)
			}
			if c.Share != 3 || c.ShareFor(1) != 48 || c.ShareFor(4) != 12 || c.ShareFor(16) != 3 {
				t.Errorf("%q: Share %d, ShareFor(1/4/16) = %d/%d/%d; want 3, 48/12/3", v, c.Share, c.ShareFor(1), c.ShareFor(4), c.ShareFor(16))
			}
			if !strings.Contains(c.ShareSource, "all 16 busy") {
				t.Errorf("%q: source %q should say it is the share with every slot busy", v, c.ShareSource)
			}
		}
	})
	t.Run("a number fixes it", func(t *testing.T) {
		c, err := resolveShareOf(Capacity{Class: Heavy, Slots: 16, Machine: m}, "6")
		if err != nil {
			t.Fatal(err)
		}
		if c.Share != 6 || c.ShareFor(1) != 6 || c.ShareFor(16) != 6 || c.shareSourceFor(1) != "FORGE_HEAVY_SHARE=6" {
			t.Errorf("Share %d, ShareFor(1/16) %d/%d, source %q", c.Share, c.ShareFor(1), c.ShareFor(16), c.shareSourceFor(1))
		}
	})
	t.Run("off exports none", func(t *testing.T) {
		for _, v := range []string{"off", "OFF", "none"} {
			c, err := resolveShareOf(Capacity{Class: Heavy, Slots: 16, Machine: m}, v)
			if err != nil {
				t.Fatal(err)
			}
			if c.Share != 0 || c.ShareFor(1) != 0 {
				t.Errorf("%q: Share %d, ShareFor(1) %d; want none", v, c.Share, c.ShareFor(1))
			}
		}
	})
	t.Run("the limit off exports none", func(t *testing.T) {
		c, err := resolveShareOf(Capacity{Class: Heavy, Unlimited: true, Machine: m}, "unset")
		if err != nil {
			t.Fatal(err)
		}
		if c.Share != 0 || c.ShareFor(1) != 0 {
			t.Errorf("Share %d, ShareFor(1) %d; want none", c.Share, c.ShareFor(1))
		}
	})
	t.Run("a bad value is an error naming the variable", func(t *testing.T) {
		for _, v := range []string{"0", "-2", "half", "1.5"} {
			if _, err := resolveShareOf(Capacity{Class: Heavy, Slots: 16, Machine: m}, v); err == nil || !strings.Contains(err.Error(), "FORGE_HEAVY_SHARE") {
				t.Errorf("%q: err = %v, want one naming FORGE_HEAVY_SHARE", v, err)
			}
		}
	})
}

// resolveShareOf resolves c's share from ShareEnv set to val ("unset": not
// in the environment at all).
func resolveShareOf(c Capacity, val string) (Capacity, error) {
	var env []string
	if val != "unset" {
		env = []string{ShareEnv(c.Class) + "=" + val}
	}
	err := c.resolveShare(env)
	return c, err
}

// CapacityFor resolves the share from the same environment as the slots.
func TestCapacityForResolvesShare(t *testing.T) {
	c, err := CapacityFor(Heavy, []string{SlotsEnv(Heavy) + "=4", ShareEnv(Heavy) + "=5"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Share != 5 {
		t.Errorf("Share = %d, want FORGE_HEAVY_SHARE's 5", c.Share)
	}
	if _, err := CapacityFor(Heavy, []string{ShareEnv(Heavy) + "=lots"}); err == nil {
		t.Error("a bad FORGE_HEAVY_SHARE resolved without an error")
	}
	// Options.Slots re-derives the default share for the count it sets.
	c, err = (Options{Slots: 2, Env: []string{}}).capacity(Heavy)
	if err != nil {
		t.Fatal(err)
	}
	if want := DefaultShare(c.Machine, 2); c.Share != want {
		t.Errorf("Share = %d with 2 requested slots, want %d", c.Share, want)
	}
}

func TestSlotCPUEnv(t *testing.T) {
	slot := func(share int) *Slot {
		return &Slot{share: share, capacity: Capacity{Machine: Machine{CPUs: 48}}}
	}
	for _, tc := range []struct {
		name string
		slot *Slot
		env  []string
		want []string
	}{
		{"share exported", slot(6), nil, []string{"GOMAXPROCS=6"}},
		{"under libuv's 4 bounds node's pool too", slot(2), []string{"PATH=/bin"}, []string{"GOMAXPROCS=2", "UV_THREADPOOL_SIZE=2"}},
		{"the caller's GOMAXPROCS stands", slot(2), []string{"GOMAXPROCS=16"}, []string{"UV_THREADPOOL_SIZE=2"}},
		{"the caller's pool stands", slot(3), []string{"UV_THREADPOOL_SIZE=8"}, []string{"GOMAXPROCS=3"}},
		{"an empty value is unset", slot(6), []string{"GOMAXPROCS="}, []string{"GOMAXPROCS=6"}},
		{"the last assignment wins", slot(6), []string{"GOMAXPROCS=16", "GOMAXPROCS="}, []string{"GOMAXPROCS=6"}},
		{"the whole machine exports nothing", slot(48), nil, nil},
		{"no share", slot(0), nil, nil},
		{"no slot", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.slot.CPUEnv(tc.env); !slices.Equal(got, tc.want) {
				t.Fatalf("CPUEnv(%q) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// Acquire gives each holder the share for the holders running when it took
// its slot, itself included; a slot inherited from a parent counts the
// holders alone.
func TestAcquire_ShareFollowsTheHolders(t *testing.T) {
	opt := testOpts(t, 4)
	a := acquire(t, opt)
	defer func() { _ = a.Release() }()
	capy := a.Capacity()
	if a.Share() != capy.ShareFor(1) {
		t.Errorf("first holder's share %d, want ShareFor(1) = %d", a.Share(), capy.ShareFor(1))
	}
	b := acquire(t, opt)
	if b.Share() != capy.ShareFor(2) {
		t.Errorf("second holder's share %d, want ShareFor(2) = %d", b.Share(), capy.ShareFor(2))
	}
	if !strings.Contains(b.ShareSource(), "2 busy") {
		t.Errorf("second holder's share source %q should count 2 busy", b.ShareSource())
	}

	child := opt
	child.Env = b.Env()
	in := acquire(t, child)
	if in.Reentrant() == "" {
		t.Fatalf("a child of a holder took a slot of its own: %s", in)
	}
	if in.Share() != capy.ShareFor(2) {
		t.Errorf("inherited slot's share %d, want ShareFor(2) = %d: the parent is one of the 2", in.Share(), capy.ShareFor(2))
	}

	_ = b.Release()
	c := acquire(t, opt)
	defer func() { _ = c.Release() }()
	if c.Share() != capy.ShareFor(2) {
		t.Errorf("a holder after a release got %d, want ShareFor(2) = %d: a released slot is not busy", c.Share(), capy.ShareFor(2))
	}

	rep, err := Status(Heavy, opt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Share != capy.ShareFor(3) {
		t.Errorf("status: next holder's share %d, want ShareFor(3) = %d", rep.Share, capy.ShareFor(3))
	}
}

func TestAcquire_FixedAndOffShares(t *testing.T) {
	opt := testOpts(t, 4)
	opt.Env = []string{ShareEnv(Heavy) + "=1"}
	s := acquire(t, opt)
	if s.Share() != 1 || s.ShareSource() != "FORGE_HEAVY_SHARE=1" {
		t.Errorf("fixed: share %d (%s), want 1", s.Share(), s.ShareSource())
	}
	_ = s.Release()

	opt.Env = []string{ShareEnv(Heavy) + "=off"}
	s = acquire(t, opt)
	if s.Share() != 0 || s.CPUEnv(nil) != nil {
		t.Errorf("off: share %d, CPUEnv %q; want none", s.Share(), s.CPUEnv(nil))
	}
	_ = s.Release()

	off := Options{Dir: t.TempDir(), Env: []string{SlotsEnv(Heavy) + "=off"}}
	s = acquire(t, off)
	if s.Share() != 0 || s.CPUEnv(nil) != nil {
		t.Errorf("limit off: share %d, CPUEnv %q; want none", s.Share(), s.CPUEnv(nil))
	}
}
