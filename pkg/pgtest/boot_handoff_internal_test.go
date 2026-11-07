package pgtest

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// Reap in another process, at explicit protocol barriers rather than sleeping.
// The directory has no postmaster and is aged, so only kernel ownership locks
// protect it. This also exercises the pid-namespace-independent fallback path.
func TestEmbeddedBootHandoffAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a separate reaper process; runs in task test")
	}
	for _, mode := range []string{"standalone", "pooled"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(key, root)
			}
			port, err := freePort()
			if err != nil {
				t.Fatal(err)
			}
			dir := runtimeDir(port)
			marker := filepath.Join(dir, "data", "not-orphaned")
			ep, _, err := startEmbeddedOnFreshPort(embeddedpostgres.DefaultConfig(), func() (uint32, error) {
				return port, nil
			}, runtimeDir, func(embeddedpostgres.Config) (*Embedded, error) {
				if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(marker, []byte("our data"), 0o600); err != nil {
					t.Fatal(err)
				}
				old := time.Now().Add(-2 * staleInstanceAge)
				if err := os.Chtimes(dir, old, old); err != nil {
					t.Fatal(err)
				}
				return &Embedded{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer ep.releaseBootClaim()
			assertData := func() {
				t.Helper()
				if b, err := os.ReadFile(marker); err != nil || string(b) != "our data" {
					t.Fatalf("reaper removed data during %s ownership handoff: %q %v", mode, b, err)
				}
			}
			// Start returned, but CWD/reachability/owner registration have not.
			runHandoffReaper(t, "reap", port, nil)
			assertData()
			// Child captures its decision before normal ownership publication.
			// It resumes after boot claim release: must recheck under the lock,
			// not use the stale pre-publication portsInUse snapshot.
			var releaseOwner func()
			runHandoffReaper(t, "snapshot", port, func() {
				if mode == "standalone" {
					owner, err := holdOwnerLock(dir)
					if err != nil {
						t.Fatal(err)
					}
					releaseOwner = func() { _ = owner.Close() }
				} else {
					l, err := takeLease(poolScope())
					if err != nil {
						t.Fatal(err)
					}
					releaseOwner = l.release
					if err := writePoolState(poolState{Port: port, URL: "registered"}); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(releaseOwner)
				ep.releaseBootClaim()
			})
			assertData()
			runHandoffReaper(t, "reap", port, nil)
			assertData()
			// Once the actual owner releases, this is genuinely orphaned.
			// The reaper's own boot lock must not masquerade as a live boot.
			releaseOwner()
			runHandoffReaper(t, "reap", port, nil)
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("orphan not reclaimed after ownership ended: %v", err)
			}
		})
	}
}

func runHandoffReaper(t *testing.T, mode string, port uint32, atSnapshot func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEmbeddedBootReaperProcess$")
	cmd.Env = append(os.Environ(), "PGTEST_HANDOFF_REAPER="+mode, "PGTEST_HANDOFF_PORT="+strconv.FormatUint(uint64(port), 10))
	if atSnapshot == nil {
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("reaper child: %v\n%s", err, output)
		}
		return
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(out)
	if line, err := reader.ReadString('\n'); err != nil || line != "snapshot captured\n" {
		cancel()
		_ = cmd.Wait()
		t.Fatalf("child barrier: %q %v", line, err)
	}
	atSnapshot()
	_, _ = io.WriteString(in, "published\n")
	_ = in.Close()
	remaining, _ := io.ReadAll(reader)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("reaper after snapshot: %v\n%s", err, remaining)
	}
}

func TestEmbeddedBootReaperProcess(t *testing.T) {
	mode := os.Getenv("PGTEST_HANDOFF_REAPER")
	if mode == "" {
		return
	}
	p, err := strconv.ParseUint(os.Getenv("PGTEST_HANDOFF_PORT"), 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	dir := runtimeDir(uint32(p))
	if mode == "snapshot" {
		entries, err := os.ReadDir(instanceRoot())
		if err != nil {
			t.Fatal(err)
		}
		// This snapshot precedes the publication and is deliberately ignored
		// by the guarded per-entry reaper after it obtains the boot lock.
		_ = portsInUse()
		fmt.Println("snapshot captured")
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() == filepath.Base(dir) {
				reapStaleInstance(dir, e)
				return
			}
		}
		t.Fatal("runtime directory absent from initial snapshot")
	}
	reapStaleInstances()
}
