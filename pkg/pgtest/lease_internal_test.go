//go:build !windows

package pgtest

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A lease is live exactly while some open file holds its lock, whoever holds
// it: here two leases of this process (two open files, as two processes
// would have), one abandoned lease file (its holder died, so nothing holds
// it), and a lease of another scope.
func TestLeases_CountTheHeldAndPruneTheAbandoned(t *testing.T) {
	privatePool(t)
	if err := os.MkdirAll(poolRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	const scope = "aaaaaaaaaaaa"
	mine, err := takeLease(scope)
	if err != nil {
		t.Fatal(err)
	}
	defer mine.release()
	other, err := takeLease(scope)
	if err != nil {
		t.Fatal(err)
	}
	abandoned := filepath.Join(poolRoot(), leasePrefix+scope+"-1-dead")
	if err := os.WriteFile(abandoned, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	elsewhere, err := takeLease("bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	defer elsewhere.release()

	if n := liveLeases(scope, mine.path, false); n != 1 {
		t.Errorf("liveLeases without prune = %d, want 1 (the other user's)", n)
	}
	if _, err := os.Stat(abandoned); err != nil {
		t.Errorf("a count without prune removed the abandoned lease: %v", err)
	}
	if n := liveLeases(scope, mine.path, true); n != 1 {
		t.Errorf("liveLeases = %d, want 1 (the other user's)", n)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("the abandoned lease survived a prune (stat err = %v)", err)
	}
	if n := liveLeases(scope, "", true); n != 2 {
		t.Errorf("liveLeases with no exception = %d, want 2", n)
	}

	other.release()
	if n := liveLeases(scope, mine.path, true); n != 0 {
		t.Errorf("liveLeases after the other user released = %d, want 0", n)
	}
}

// A user's lease is released by the kernel when its process dies, however it
// dies: a SIGKILLed user never pins the server open.
func TestLeases_AKilledHoldersLeaseIsNotLive(t *testing.T) {
	privatePool(t)
	if err := os.MkdirAll(poolRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	flock, err := exec.LookPath("flock")
	if err != nil {
		t.Skip("needs util-linux flock(1) to hold a lease from another process")
	}
	const scope = "cccccccccccc"
	path := filepath.Join(poolRoot(), leasePrefix+scope+"-child")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	holder := exec.Command(flock, "--shared", "--close", path, "sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for liveLeases(scope, "", false) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the child never took its lease")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = holder.Process.Kill()
	_ = holder.Wait()
	if n := liveLeases(scope, "", true); n != 0 {
		t.Errorf("liveLeases after its holder was killed = %d, want 0", n)
	}
}

// The last-user decision is the leases', not the pids'. A user in another pid
// namespace holds a lease and records no pid this process could probe; this
// process detaching must leave the server to it. Once that user has gone too,
// the last detach tears the server down as before.
func TestDetach_AnotherUsersLeaseKeepsTheServer(t *testing.T) {
	privatePool(t)
	if err := os.MkdirAll(poolRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	port := silentPort(t)
	fakeServerDir(t, port)
	writePoolState(poolState{URL: "postgres://shared", Port: port})

	other, err := takeLease(poolScope())
	if err != nil {
		t.Fatal(err)
	}
	if poolLease, err = takeLease(poolScope()); err != nil {
		t.Fatal(err)
	}
	detachEmbedded(port)
	if _, err := os.Stat(filepath.Join(runtimeDir(port), "data")); err != nil {
		t.Fatalf("detach tore down a server another user still leases: %v", err)
	}
	if st := readPoolState(); st.Port != port {
		t.Errorf("pool state %+v after detach, want the server on %d kept", st, port)
	}

	other.release()
	if poolLease, err = takeLease(poolScope()); err != nil {
		t.Fatal(err)
	}
	detachEmbedded(port)
	if _, err := os.Stat(runtimeDir(port)); !os.IsNotExist(err) {
		t.Errorf("the last user's detach left the server's dir behind (stat err = %v)", err)
	}
	if st := readPoolState(); st.Port != 0 {
		t.Errorf("pool state %+v after the last detach, want none", st)
	}
}

// silentPort is a loopback port nothing listens on: one the kernel just
// handed out and this test closed again.
func silentPort(t *testing.T) uint32 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	port := uint32(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	return port
}

// answeringServerDir lays out a runtime dir whose postmaster this process
// cannot see — its pid file names a pid that is dead here, as a pid of
// another pid namespace is — while the server still answers on its port.
func answeringServerDir(t *testing.T) uint32 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := uint32(ln.Addr().(*net.TCPAddr).Port)
	fakeServerDir(t, port)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(runtimeDir(port), "data", "postmaster.pid")
	content := strconv.Itoa(dead.Process.Pid) + "\n" + filepath.Join(runtimeDir(port), "data") + "\n0\n" + strconv.Itoa(int(port)) + "\n"
	if err := os.WriteFile(pidFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return port
}

// stopSharedByPort never removes the data directory of a server that still
// answers. Removing it under a live postmaster leaves a server that answers
// pings and fails every CREATE DATABASE — what a sandboxed gate did to the
// host's shared server when it could not see the postmaster's pid.
func TestStopSharedByPort_NeverRemovesTheDirOfAServerThatAnswers(t *testing.T) {
	privatePool(t)
	port := answeringServerDir(t)

	stopSharedByPort(port)

	if err := dataIntact(port); err != nil {
		t.Fatalf("stopSharedByPort removed the data directory of a server that still answers on its port: %v", err)
	}
}

// Nor does the age-based reaper, whatever the dir's age.
func TestReap_NeverRemovesTheDirOfAServerThatAnswers(t *testing.T) {
	privatePool(t)
	port := answeringServerDir(t)
	prev := staleInstanceAge
	staleInstanceAge = time.Nanosecond
	t.Cleanup(func() { staleInstanceAge = prev })
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(runtimeDir(port), old, old)
	_ = os.Chtimes(filepath.Join(runtimeDir(port), "data", "postmaster.pid"), old, old)

	reapStaleInstances()

	if err := dataIntact(port); err != nil {
		t.Fatalf("reapStaleInstances removed the data directory of a server that still answers on its port: %v", err)
	}
}

// A runtime dir whose server is gone is still reclaimed.
func TestStopSharedByPort_RemovesTheDirOfAServerThatIsGone(t *testing.T) {
	privatePool(t)
	port := silentPort(t)
	fakeServerDir(t, port)
	if !stopSharedByPort(port) {
		t.Error("stopSharedByPort reported a server with no postmaster and no listener as still running")
	}
	if _, err := os.Stat(runtimeDir(port)); !os.IsNotExist(err) {
		t.Errorf("the dir of a gone server was left behind (stat err = %v)", err)
	}
}
