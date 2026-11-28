package hostslot

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The CPU share. Slots bound how many heavy jobs run at once; they do not
// bound how wide each one runs. Left alone, a holder's `go test ./...` runs
// with GOMAXPROCS set to every CPU, and the go command's package parallelism
// (-p) defaults to GOMAXPROCS: on a 48-CPU host, sixteen holders each started
// up to 48 compiles and links and as many test binaries, each 48 Ps wide, and
// the host ran at load 147–164.
//
// So a holder runs its children on a share of the machine: Slot.CPUEnv
// exports GOMAXPROCS (and, below libuv's own default of 4, UV_THREADPOOL_SIZE)
// to them. GOMAXPROCS bounds every Go process the command runs — the go
// command, which then runs at most that many compiles, links and test
// binaries at once; the compiler and linker; each test binary and its
// -parallel default. A positive integer the child's environment already sets
// is left alone, so a command that chose its own (GOMAXPROCS=16 go test ...)
// keeps it, and so does a child of a holder, which inherits its parent's.
//
// The share is the default's (DefaultShare) for the holders running when the
// slot is taken, so a holder alone on the machine keeps the whole of it and
// a holder among fifteen others gets its part. A share cannot shrink once the
// command runs: GOMAXPROCS is read at process start.
//
// This is GOMAXPROCS, not a hard cap. A cgroup v2 cpu.max would also bind
// what does not read GOMAXPROCS (cc1 under cgo, node), but cgroup v2's
// no-internal-process rule lets a process cap a child cgroup only once every
// process has left the cgroup they share — and a slot holder shares its
// cgroup with whatever started it (task, a shell, a CI runner), processes
// forge does not own and must not move. A supervisor that owns a cgroup per
// command (the Reliant daemon) caps it there, and exports the same share.

const (
	envGOMAXPROCS = "GOMAXPROCS"
	envUVPool     = "UV_THREADPOOL_SIZE"
	// uvDefaultPool is libuv's own default pool size. A share never raises
	// it: UV_THREADPOOL_SIZE is exported only when the share is smaller.
	uvDefaultPool = 4
	// minShare is the smallest default share: a build with one CPU
	// serializes its own compile and link.
	minShare = 2
	// gibPerShareCPU is the memory one unit of share is budgeted. Measured
	// with `go test -count=1 -run '^$'` over 24 test packages of a large
	// module (compiles cached, so links dominate, as in a full test lane),
	// summing the RSS of every process it ran, one link peaking at 3.9 GiB:
	//
	//	-p   peak RSS   mean RSS
	//	 2    7.8 GiB    3.5 GiB
	//	 4   14.1 GiB    5.9 GiB
	//	 6   16.4 GiB    7.9 GiB
	//	 8   22.3 GiB   10.7 GiB
	//	16   23.8 GiB   14.8 GiB
	//
	// About 1.5 GiB per unit of -p on average and 3.5 GiB at the peaks.
	// At 2.5 the holders' summed mean stays near 60% of memory, which
	// leaves room for their peaks, which do not coincide, and for
	// everything else on the machine.
	gibPerShareCPU = 2.5
)

// ShareEnv is the variable that overrides a class's CPU share:
// FORGE_HEAVY_SHARE for "heavy". A positive integer gives every holder that
// many CPUs; "off" exports none (holders run as wide as the machine, as
// before the share existed); "auto" or unset is DefaultShare.
func ShareEnv(class string) string {
	return "FORGE_" + strings.ToUpper(strings.ReplaceAll(class, "-", "_")) + "_SHARE"
}

// DefaultShare is how many CPUs one heavy job may use when busy jobs share
// machine m:
//
//	min(CPUs, max(2, min(3·CPUs / (2·busy), memory GiB / (2.5·busy))))
//
// The CPU term lets every busy job at once ask for at most 1.5x the
// machine: at twice the even split, eight jobs on 31 CPUs were allowed 56,
// and the machine ran at load 150–320 while its commands timed out; at 1.5x
// the CPUs idle between one job's busy phases are still mostly taken by
// another's. The memory term budgets 2.5 GiB per unit of share (see
// gibPerShareCPU): a go test lane links up to GOMAXPROCS test binaries at
// once, and a large module's take ~4 GiB each. Unknown memory drops the
// memory term. Never more CPUs than the machine has.
func DefaultShare(m Machine, busy int) int {
	if busy < 1 {
		busy = 1
	}
	whole := max(1, int(math.Floor(m.CPUs)))
	share := 3 * whole / (2 * busy)
	if m.MemBytes > 0 {
		share = min(share, int(float64(m.MemBytes)/gib/(gibPerShareCPU*float64(busy))))
	}
	return min(whole, max(minShare, share))
}

// resolveShare sets the share fields from ShareEnv(class) in env for the
// current Slots.
func (c *Capacity) resolveShare(env []string) error {
	c.Share, c.ShareSource, c.shareFixed = 0, "", false
	if c.Unlimited {
		c.ShareSource = "none: the limit is off"
		return nil
	}
	name := ShareEnv(c.Class)
	switch v := strings.TrimSpace(lookupEnv(env, name)); strings.ToLower(v) {
	case "", "auto":
		c.Share = DefaultShare(c.Machine, c.Slots)
		c.ShareSource = shareFormula(c.Machine, c.Slots, "all ")
	case "off", "none":
		c.ShareSource = name + "=" + v
	default:
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("%s=%q: want a positive number of CPUs, auto or off", name, v)
		}
		c.Share, c.ShareSource, c.shareFixed = n, name+"="+v, true
	}
	return nil
}

// ShareFor is the share a holder gets when busy holders, itself included,
// run at once: Share when ShareEnv fixes it (or switches it off, 0), else
// DefaultShare.
func (c Capacity) ShareFor(busy int) int {
	if c.Unlimited || c.shareFixed || c.Share == 0 {
		return c.Share
	}
	return DefaultShare(c.Machine, busy)
}

// shareSourceFor says where ShareFor(busy) came from.
func (c Capacity) shareSourceFor(busy int) string {
	if c.Unlimited || c.shareFixed || c.Share == 0 {
		return c.ShareSource
	}
	return shareFormula(c.Machine, busy, "")
}

func shareFormula(m Machine, busy int, all string) string {
	whole := max(1, int(math.Floor(m.CPUs)))
	cpu := fmt.Sprintf("3×%d/(2×%d)", whole, busy)
	if m.MemBytes == 0 {
		return fmt.Sprintf("default min(%d, max(2, %s)), %s%d busy", whole, cpu, all, busy)
	}
	return fmt.Sprintf("default min(%d, max(2, min(%s, %s GiB/(2.5×%d)))), %s%d busy",
		whole, cpu, trimFloat(float64(m.MemBytes)/gib), busy, all, busy)
}

// Share is the CPUs this holder's children run on (Capacity.ShareFor of the
// holders running when it took its slot); 0 when it exports none — the limit
// or the share is off.
func (s *Slot) Share() int {
	if s == nil {
		return 0
	}
	return s.share
}

// ShareSource says where Share came from, for a status line.
func (s *Slot) ShareSource() string {
	if s == nil {
		return ""
	}
	return s.shareSource
}

// CPUEnv returns what to append to env, a child's environment, so the child
// runs on this slot's CPU share: GOMAXPROCS=Share, and UV_THREADPOOL_SIZE
// when Share is under libuv's default of 4, each only where env does not set
// a valid positive integer already (see the package's CPU share notes). Nil
// when there is nothing to export: no share, or one as wide as the machine.
func (s *Slot) CPUEnv(env []string) []string {
	n := s.Share()
	if n < 1 {
		return nil
	}
	var out []string
	goProcs, goErr := strconv.Atoi(lookupEnv(env, envGOMAXPROCS))
	if n < max(1, int(math.Floor(s.capacity.Machine.CPUs))) && (goErr != nil || goProcs < 1) {
		out = append(out, envGOMAXPROCS+"="+strconv.Itoa(n))
	}
	uvPool, uvErr := strconv.Atoi(lookupEnv(env, envUVPool))
	if n < uvDefaultPool && (uvErr != nil || uvPool < 1) {
		out = append(out, envUVPool+"="+strconv.Itoa(n))
	}
	return out
}

// setShare records the share s runs on, for the holders live in dir now.
// A slot this process took counts itself beside the others; one inherited
// from a parent counts only the holders, the parent among them.
func (s *Slot) setShare(dir string) {
	busy := heldOthers(dir, s.capacity.Slots, s.index)
	if s.index >= 0 {
		busy++
	}
	busy = max(1, busy)
	s.share, s.shareSource = s.capacity.ShareFor(busy), s.capacity.shareSourceFor(busy)
}

// heldOthers counts the live holders of the class in dir other than slot
// self. A slot whose record is empty is free (a holder clears it before
// letting go) and is never probed, so the count cannot get in the way of an
// acquirer taking a free slot.
func heldOthers(dir string, slots, self int) int {
	n := 0
	for i := 0; i < slots; i++ {
		if i == self {
			continue
		}
		path := slotPath(dir, i)
		if _, ok := readRecord(path); ok && locked(path) {
			n++
		}
	}
	return n
}
