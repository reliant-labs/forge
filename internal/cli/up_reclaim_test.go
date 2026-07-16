package cli

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeFacts is the map-backed procFacts used by the ownership-resolution
// unit tests. A pid absent from env is "unreadable" (environ ok=false),
// modelling a dead pid / SIP-redacted system binary. A pid absent from ppid
// has an unknown parent (parent ok=false), ending the ancestry walk.
type fakeFacts struct {
	env  map[int][]string
	ppid map[int]int
}

func (f fakeFacts) environ(pid int) ([]string, bool) {
	e, ok := f.env[pid]
	return e, ok
}

func (f fakeFacts) parent(pid int) (int, bool) {
	p, ok := f.ppid[pid]
	return p, ok
}

func marker(env, svc string) []string {
	return []string{"PATH=/usr/bin", forgeUpEnvVar + "=" + env, forgeUpServiceVar + "=" + svc}
}

func TestForgeOwnerOfPID(t *testing.T) {
	f := fakeFacts{
		env: map[int][]string{
			100: marker("dev", "api"),     // direct holder carries the marker
			201: marker("dev", "web"),     // ancestor of unreadable 200
			300: {"PATH=/usr/bin"},        // no marker anywhere
			400: marker("staging", "api"), // wrong env
			// 200 and 500 deliberately absent → unreadable
		},
		ppid: map[int]int{
			100: 1,
			200: 201, 201: 1, // 200's env unreadable; marked parent 201
			300: 1,
			400: 1,
			500: 1, // unreadable holder whose only ancestor is init
			// cycle: 600 <-> 601, no markers
			600: 601, 601: 600,
		},
	}

	t.Run("direct marker match", func(t *testing.T) {
		svc, owned := forgeOwnerOfPID(100, "dev", f)
		if !owned || svc != "api" {
			t.Fatalf("got (%q, %v); want (api, true)", svc, owned)
		}
	})
	t.Run("ancestor marker match (grandchild reparent case)", func(t *testing.T) {
		svc, owned := forgeOwnerOfPID(200, "dev", f)
		if !owned || svc != "web" {
			t.Fatalf("got (%q, %v); want (web, true)", svc, owned)
		}
	})
	t.Run("no marker is foreign", func(t *testing.T) {
		if _, owned := forgeOwnerOfPID(300, "dev", f); owned {
			t.Fatal("unmarked process must be foreign")
		}
	})
	t.Run("wrong-env marker is foreign", func(t *testing.T) {
		if _, owned := forgeOwnerOfPID(400, "dev", f); owned {
			t.Fatal("staging-marked process must be foreign for env=dev")
		}
	})
	t.Run("unreadable holder to init is foreign", func(t *testing.T) {
		if _, owned := forgeOwnerOfPID(500, "dev", f); owned {
			t.Fatal("unreadable holder whose only ancestor is init must be foreign")
		}
	})
	t.Run("ppid cycle terminates and is foreign", func(t *testing.T) {
		if _, owned := forgeOwnerOfPID(600, "dev", f); owned {
			t.Fatal("cycle without markers must be foreign (and must terminate)")
		}
	})
	t.Run("init pid is never inspected", func(t *testing.T) {
		// Even if pid 1 somehow carried the marker, the walk stops before it.
		f2 := fakeFacts{env: map[int][]string{1: marker("dev", "root")}, ppid: map[int]int{}}
		if _, owned := forgeOwnerOfPID(1, "dev", f2); owned {
			t.Fatal("pid 1 must never be classified as forge-owned")
		}
	})
}

func TestTopmostForgeOwnedAncestor(t *testing.T) {
	// leaf(700,dev) -> mid(701,dev) -> root(702,no marker) -> init
	f := fakeFacts{
		env: map[int][]string{
			700: marker("dev", "next"),
			701: marker("dev", "npm"),
			702: {"PATH=/usr/bin"},
			800: {"PATH=/usr/bin"},
		},
		ppid: map[int]int{700: 701, 701: 702, 702: 1, 800: 1},
	}
	if got := topmostForgeOwnedAncestor(700, "dev", f); got != 701 {
		t.Errorf("topmost from leaf: got %d, want 701 (the npm forge launched)", got)
	}
	if got := topmostForgeOwnedAncestor(800, "dev", f); got != -1 {
		t.Errorf("not-owned: got %d, want -1", got)
	}
}

func TestClassifyPortConflicts(t *testing.T) {
	f := fakeFacts{
		env: map[int][]string{
			100: marker("dev", "admin-server"), // holds :8090, ours
			200: {"PATH=/usr/bin"},             // holds :3000, foreign
		},
		ppid: map[int]int{100: 1, 200: 1},
	}
	resolve := func(port int) int {
		switch port {
		case 8090:
			return 100
		case 3000:
			return 200
		default:
			return 0 // :9999 unresolvable (e.g. lsof missing)
		}
	}
	conflicts := []portConflict{
		{name: "admin-server", port: 8090},
		{name: "reliant-web", port: 3000},
		{name: "ghost", port: 9999},
	}
	owned, foreign := classifyPortConflicts("dev", conflicts, resolve, f)
	if len(owned) != 1 || owned[0].name != "admin-server" {
		t.Fatalf("owned = %v; want [admin-server]", names(owned))
	}
	// Foreign includes both the unmarked holder AND the unresolvable port —
	// an unidentifiable holder is conservatively foreign, never reclaimed.
	if got := names(foreign); len(got) != 2 || got[0] != "reliant-web" || got[1] != "ghost" {
		t.Fatalf("foreign = %v; want [reliant-web ghost]", got)
	}
}

// TestClassify_DeadLedgerPidLiveMarkedOrphan pins the headline case: the
// ledger's tracked pid is dead (air re-exec'd under a new pid), but the LIVE
// process squatting the port carries our marker — it must classify as ours,
// not foreign, so the user gets the --restart hint instead of a lsof+kill
// misdirection.
func TestClassify_DeadLedgerPidLiveMarkedOrphan(t *testing.T) {
	const liveOrphan = 4242 // not the (dead) ledger pid
	f := fakeFacts{
		env:  map[int][]string{liveOrphan: marker("dev", "admin-server")},
		ppid: map[int]int{liveOrphan: 1},
	}
	resolve := func(int) int { return liveOrphan }
	owned, foreign := classifyPortConflicts("dev",
		[]portConflict{{name: "admin-server", port: 8090}}, resolve, f)
	if len(owned) != 1 || len(foreign) != 0 {
		t.Fatalf("owned=%v foreign=%v; want the live marked orphan classified as ours",
			names(owned), names(foreign))
	}
}

func TestMarkedOrphanRootsForPorts_DedupsSharedRoot(t *testing.T) {
	// Two conflicting ports resolve to two pids in the SAME forge subtree
	// (child 900 under parent 901, both marked). The reclaim target must
	// collapse to the single topmost root 901.
	f := fakeFacts{
		env:  map[int][]string{900: marker("dev", "next"), 901: marker("dev", "npm")},
		ppid: map[int]int{900: 901, 901: 1},
	}
	resolve := func(port int) int {
		if port == 3000 {
			return 900
		}
		return 901
	}
	roots := markedOrphanRootsForPorts("dev",
		[]portConflict{{name: "web", port: 3000}, {name: "web2", port: 3001}}, resolve, f)
	if len(roots) != 1 || roots[0] != 901 {
		t.Fatalf("roots = %v; want [901] (deduped topmost)", roots)
	}
}

func TestMarkedOrphanRoots_SweepKeepsSubtreeRoots(t *testing.T) {
	// Table sweep: 900(dev) is a child of 901(dev); 902 is a different env.
	// Only 901 (the subtree root for env=dev) is returned; 900 is dropped
	// (its marked parent is also killed) and 902 is excluded (wrong env).
	f := fakeFacts{
		env: map[int][]string{
			900: marker("dev", "next"),
			901: marker("dev", "npm"),
			902: marker("staging", "api"),
		},
		ppid: map[int]int{900: 901, 901: 1, 902: 1},
	}
	roots := markedOrphanRoots("dev", []int{900, 901, 902}, f)
	sort.Ints(roots)
	if len(roots) != 1 || roots[0] != 901 {
		t.Fatalf("roots = %v; want [901]", roots)
	}
}

func TestStampForgeOwnership_ForcesAndDedups(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{"PATH=/usr/bin", forgeUpEnvVar + "=stale"}}
	stampForgeOwnership(cmd, "dev", "admin-server")

	got := map[string]int{} // key -> count
	var envVal, svcVal string
	for _, kv := range cmd.Env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			got[kv[:i]]++
			switch kv[:i] {
			case forgeUpEnvVar:
				envVal = kv[i+1:]
			case forgeUpServiceVar:
				svcVal = kv[i+1:]
			}
		}
	}
	if got[forgeUpEnvVar] != 1 {
		t.Errorf("FORGE_UP_ENV must appear exactly once (dedup), got %d: %v", got[forgeUpEnvVar], cmd.Env)
	}
	if envVal != "dev" {
		t.Errorf("FORGE_UP_ENV = %q; want dev (stale value overwritten)", envVal)
	}
	if svcVal != "admin-server" {
		t.Errorf("FORGE_UP_SERVICE = %q; want admin-server", svcVal)
	}
	if got["PATH"] != 1 {
		t.Errorf("PATH must survive stamping: %v", cmd.Env)
	}
}

func TestMarkerFields(t *testing.T) {
	env, svc := markerFields([]string{"A=1", forgeUpEnvVar + "=dev", "B=2", forgeUpServiceVar + "=web"})
	if env != "dev" || svc != "web" {
		t.Fatalf("got (%q,%q); want (dev,web)", env, svc)
	}
	// Last duplicate wins (exec env semantics).
	env, _ = markerFields([]string{forgeUpEnvVar + "=old", forgeUpEnvVar + "=new"})
	if env != "new" {
		t.Fatalf("duplicate: got %q; want new", env)
	}
	if e, s := markerFields([]string{"PATH=/x"}); e != "" || s != "" {
		t.Fatalf("no marker: got (%q,%q); want empty", e, s)
	}
}

// TestHelperBlockProcess is the re-exec target for the real-process
// integration test below: it blocks (bounded, so a leak self-reaps) only
// when the harness env flag is set, and is an inert no-op under normal
// `go test` runs.
func TestHelperBlockProcess(t *testing.T) {
	if os.Getenv("FORGE_RECLAIM_HELPER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
}

// TestForgeOwnership_RealProcess exercises the REAL osProcFacts against a
// live child on the actual platform — the load-bearing, platform-specific
// half (sysctl KERN_PROCARGS2 on darwin, /proc on linux) that the mock
// tests can't cover. It spawns this test binary re-exec'd with the marker
// in its env and asserts osProcFacts reads it back and forgeOwnerOfPID
// classifies it as ours; a child WITHOUT the marker classifies as foreign.
func TestForgeOwnership_RealProcess(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process-env inspection is implemented for darwin/linux only")
	}
	env := fmt.Sprintf("itest-%d", os.Getpid())

	spawn := func(withMarker bool) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperBlockProcess")
		e := append(os.Environ(), "FORGE_RECLAIM_HELPER=1")
		if withMarker {
			e = append(e, forgeUpEnvVar+"="+env, forgeUpServiceVar+"=svc")
		}
		cmd.Env = e
		startInOwnProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper: %v", err)
		}
		return cmd
	}

	marked := spawn(true)
	unmarked := spawn(false)
	t.Cleanup(func() {
		for _, c := range []*exec.Cmd{marked, unmarked} {
			if c.Process != nil {
				killProcessTree(c.Process.Pid, syscall.SIGKILL)
				_ = c.Wait()
			}
		}
	})

	// Give the children a moment to exec into the test binary so their env
	// is in place for inspection.
	time.Sleep(300 * time.Millisecond)

	facts := newOSProcFacts()

	// The marked child's env must be readable and identify it as ours.
	gotEnv, ok := facts.environ(marked.Process.Pid)
	if !ok {
		t.Fatalf("could not read env of our own child pid %d — the marker mechanism is non-functional on %s", marked.Process.Pid, runtime.GOOS)
	}
	if name := markerEnvName(gotEnv); name != env {
		t.Fatalf("marked child FORGE_UP_ENV = %q; want %q (env read: %d vars)", name, env, len(gotEnv))
	}
	if _, owned := forgeOwnerOfPID(marked.Process.Pid, env, facts); !owned {
		t.Fatalf("marked child pid %d classified as foreign; want ours", marked.Process.Pid)
	}

	// The unmarked child must classify as foreign for our env.
	if _, owned := forgeOwnerOfPID(unmarked.Process.Pid, env, facts); owned {
		t.Fatalf("unmarked child pid %d classified as ours; want foreign", unmarked.Process.Pid)
	}
}

func names(cs []portConflict) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.name
	}
	return out
}
