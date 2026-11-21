package testcollect

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/testkit"
)

// A real `go build -cover -covermode=atomic` server, end to end: a live pull
// is converted by `go tool covdata`, --reset zeroes it and fences off the
// past, and once the server drains on SIGTERM its exit flush is what the next
// collect reads.
func TestCollect_RealCoverBuild(t *testing.T) {
	testkit.Slow(t, "compiles testdata/covserver with -cover, runs it and converts its covdata with go tool covdata")
	f := newFixture(t)
	bin := filepath.Join(t.TempDir(), "covserver")
	if out, err := exec.Command("go", "build", "-cover", "-covermode=atomic", "-o", bin, "./testdata/covserver").CombinedOutput(); err != nil {
		t.Fatalf("go build -cover: %v\n%s", err, out)
	}
	cover := filepath.Join(f.root, ".forge", "tests", "covdata", "dev", "api")
	if err := os.MkdirAll(cover, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "GOCOVERDIR="+cover)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := ""
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if a, ok := strings.CutPrefix(sc.Text(), "ADDR "); ok {
			addr = a
			break
		}
	}
	if addr == "" {
		t.Fatal("covserver printed no address")
	}
	hit := func() {
		resp, err := http.Get("http://" + addr + "/hit")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	target := Target{Workload: "api", Runtime: "host", Host: &HostTarget{PullAddr: addr, CoverDir: cover}}
	collect := func(reset bool) *Document {
		t.Helper()
		opts := f.options(target)
		opts.Convert = nil // the real `go tool covdata textfmt`
		opts.Reset = reset
		doc, err := collectErr(context.Background(), opts)
		if err != nil {
			t.Fatalf("collect (reset=%v): %v", reset, err)
		}
		return doc
	}

	hit()
	wl := collect(false).Workloads[0]
	if wl.State != StateCollected || wl.Build.Coverage == nil || wl.Build.Coverage.Covered == 0 || wl.Build.Coverage.Statements == 0 {
		t.Fatalf("live pull: %+v build %+v, want converted coverage with statements hit", wl, wl.Build)
	}
	liveCovered := wl.Build.Coverage.Covered

	if wl := collect(true).Workloads[0]; wl.State != StateReset {
		t.Fatalf("reset: %+v", wl)
	}
	// Nothing ran since the reset, but the handler itself did: a pull is
	// never zero. It covers less than a pull after /hit.
	if wl := collect(false).Workloads[0]; wl.Build.Coverage.Covered >= liveCovered {
		t.Errorf("after reset: %d covered, want fewer than the %d /hit reached", wl.Build.Coverage.Covered, liveCovered)
	}

	hit()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("covserver did not drain cleanly: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("covserver did not exit on SIGTERM")
	}
	wl = collect(false).Workloads[0]
	if wl.State != StateCollected || wl.Build.CounterFiles != 1 || wl.Build.Coverage == nil || wl.Build.Coverage.Covered == 0 {
		t.Fatalf("exit flush: %+v build %+v, want the one counter file Go wrote at exit", wl, wl.Build)
	}
	for _, s := range wl.Sources {
		if s.Kind == SourcePull && s.Status != SourceUnreachable {
			t.Errorf("pull after exit: %+v, want unreachable", s)
		}
		if s.Kind == SourceExitFlush && (s.Status != SourceOK || s.Files != 2) {
			t.Errorf("exit flush: %+v, want the meta and the one counter file written after the reset", s)
		}
	}
}
