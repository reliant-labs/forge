//go:build !windows

package pgtest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// loseTemplate1 deletes template1's directory from under the running server
// at port, as a /tmp cleaner aging out its never-rewritten files does. The
// postmaster keeps running and answering; every CREATE DATABASE then fails
// with 58P01 `could not open file "base/1/pg_filenode.map"` (control-plane's
// v1.9.0-rc.18 cut, 2026-11-07).
func loseTemplate1(t *testing.T, port uint32) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(runtimeDir(port), "data", "base", "1")); err != nil {
		t.Fatal(err)
	}
}

func createOn(url, name string) error {
	db, err := openBase(url)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec("CREATE DATABASE " + name)
	return err
}

// A shared server whose data directory was deleted under it is replaced by
// the next attach, even while a live user holds it: it can serve none of
// them, and before this it failed every caller until killed by hand.
func TestAttach_ABrokenSharedServerIsReplaced(t *testing.T) {
	if testing.Short() {
		t.Skip("boots two embedded postgres servers; runs in task test")
	}
	privatePool(t)
	port, url, pid := sharedPostmaster(t)
	loseTemplate1(t, port)

	err := createOn(url, "broken_probe")
	if err == nil || !brokenServerError(err) {
		t.Fatalf("CREATE DATABASE on the broken server: %v, want a 58P01 failure (the premise)", err)
	}

	gotURL, gotPort, mode, err := attachEmbedded()
	if err != nil {
		t.Fatalf("attach to a broken shared server: %v", err)
	}
	t.Cleanup(func() { stopSharedByPort(gotPort) })
	if mode != poolModePooled || gotPort == port {
		t.Fatalf("attach returned port %d mode %d, want a fresh pooled server replacing %d", gotPort, mode, port)
	}
	for i := 0; i < 50 && processAlive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Errorf("the broken shared server's postmaster %d is still running", pid)
	}
	if st := readPoolState(); st.Port != gotPort || st.PID <= 0 {
		t.Errorf("pool state %+v, want the new server on %d with its postmaster recorded", st, gotPort)
	}
	if err := createOn(gotURL, "healed_probe"); err != nil {
		t.Errorf("CREATE DATABASE on the replacement: %v", err)
	}
}

// The public path heals itself: New after the shared server lost its data
// re-attaches and succeeds, instead of returning 58P01 to every caller.
func TestNew_SucceedsAfterTheSharedServerLosesItsData(t *testing.T) {
	if testing.Short() {
		t.Skip("boots two embedded postgres servers; runs in task test")
	}
	privatePool(t)
	Shutdown()
	t.Cleanup(Shutdown)

	db, cleanup, err := New()
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	cleanup()
	_ = db.Close()
	st := readPoolState()
	if st.Port == 0 {
		t.Fatal("no shared server recorded after New")
	}
	loseTemplate1(t, st.Port)

	db, cleanup, err = New()
	if err != nil {
		t.Fatalf("New after the shared server lost template1: %v", err)
	}
	defer cleanup()
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping the healed scratch database: %v", err)
	}
	if now := readPoolState(); now.Port == st.Port {
		t.Errorf("the pool still records the broken server on %d", st.Port)
	}
}

// reapStaleInstances never removes the runtime dir of a postmaster that is
// still running: a healthy young one is left alone entirely, and a broken
// one is STOPPED before its dir goes, so no live postgres is ever left
// serving from a deleted directory.
func TestReap_NeverRemovesALiveServersDir(t *testing.T) {
	if testing.Short() {
		t.Skip("boots two embedded postgres servers; runs in task test")
	}
	privatePool(t)
	boot := func() (uint32, int) {
		_, port, ep, err := bootEmbedded()
		if err != nil {
			t.Fatal(err)
		}
		retainEmbedded(ep)
		t.Cleanup(func() { stopSharedByPort(port) })
		pid, alive := postmaster(filepath.Join(runtimeDir(port), "data", "postmaster.pid"))
		if !alive {
			t.Fatal("no live postmaster after boot")
		}
		return port, pid
	}
	healthyPort, healthyPid := boot()
	brokenPort, brokenPid := boot()
	loseTemplate1(t, brokenPort)

	// Neither is in use (no pool user, no owner file) and both are young.
	reapStaleInstances()

	if !processAlive(healthyPid) {
		t.Error("the reaper killed a young, healthy server")
	}
	if _, err := os.Stat(filepath.Join(runtimeDir(healthyPort), "data")); err != nil {
		t.Errorf("the reaper removed a young, healthy server's data: %v", err)
	}
	for i := 0; i < 50 && processAlive(brokenPid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	_, dirErr := os.Stat(runtimeDir(brokenPort))
	if processAlive(brokenPid) {
		if dirErr != nil {
			t.Fatalf("the reaper removed the dir of postmaster %d while it still runs", brokenPid)
		}
		t.Errorf("the reaper left the broken server %d running", brokenPid)
	} else if dirErr == nil {
		t.Errorf("the reaper stopped the broken server but left its dir %s", runtimeDir(brokenPort))
	}
}
