package forgedelegate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/atomicfile"
)

const pin = "v0.1.46-0.20261018032013-ee3bded463bc"

func fakeInstall(t *testing.T, calls *[]string) func(context.Context, string, string) error {
	t.Helper()
	return func(_ context.Context, pkg, gobin string) error {
		*calls = append(*calls, pkg)
		return atomicfile.WriteFile(filepath.Join(gobin, exeName()), []byte("#!/bin/sh\n"), 0o755)
	}
}

func newResolver(t *testing.T) (*Resolver, *[]string, *bytes.Buffer) {
	t.Helper()
	var calls []string
	var log bytes.Buffer
	t.Setenv(CacheDirEnv, t.TempDir())
	return &Resolver{Log: &log, Install: fakeInstall(t, &calls)}, &calls, &log
}

// A delegated build is a function of the source, not of the caller's GOFLAGS:
// the cache key does not carry them, so two callers that differ only there
// must get the same bytes. On CI, e2e tests that run forge under
// GOFLAGS=-trimpath and ones that do not built two different forges of one
// checkout, and the generate cache reported "the forge binary changed".
func TestGoBuild_BytesDoNotDependOnCallerGOFLAGS(t *testing.T) {
	if testing.Short() {
		t.Skip("execs `go build` twice; runs in task test")
	}
	src := t.TempDir()
	mustWrite := func(rel, body string) {
		t.Helper()
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("go.mod", "module example.com/fakeforge\n\ngo 1.21\n")
	mustWrite("cmd/forge/main.go", "package main\n\nfunc main() { panic(\"where am I\") }\n")

	build := func(goflags string) []byte {
		t.Helper()
		// Explicit both ways, so a `go env -w GOFLAGS` on this machine
		// cannot make the two callers agree by accident.
		t.Setenv("GOFLAGS", goflags)
		out := filepath.Join(t.TempDir(), exeName())
		if err := goBuild(context.Background(), src, out); err != nil {
			t.Fatalf("goBuild with GOFLAGS=%s: %v", goflags, err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	plain, trimmed := build("-trimpath=false"), build("-trimpath")
	if !bytes.Equal(plain, trimmed) {
		t.Fatalf("one source built two different forges (%d vs %d bytes) depending on the caller's GOFLAGS; a cache keyed by the source cannot hold both", len(plain), len(trimmed))
	}
}

// The first request for a pin builds it; every later one reuses the build.
func TestPinned_BuildsOnceThenHitsCache(t *testing.T) {
	r, calls, log := newResolver(t)
	project := t.TempDir()

	first, err := r.Pinned(context.Background(), project, pin)
	if err != nil {
		t.Fatal(err)
	}
	if want := "github.com/reliant-labs/forge/cmd/forge@" + pin; len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("installed %v, want [%s]", *calls, want)
	}
	if !strings.Contains(log.String(), "building forge "+pin) {
		t.Errorf("no build notice: %q", log.String())
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatalf("result does not exist: %v", err)
	}

	second, err := r.Pinned(context.Background(), project, pin)
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Errorf("second request built again: %v", *calls)
	}
	if second.Path != first.Path {
		t.Errorf("cache path moved: %s then %s", first.Path, second.Path)
	}
}

// A failed build leaves nothing the next request would trust as a hit.
func TestPinned_FailedBuildLeavesNoBinary(t *testing.T) {
	r, _, _ := newResolver(t)
	r.Install = func(context.Context, string, string) error {
		return errors.New("dial tcp: lookup proxy.golang.org: no such host")
	}
	_, err := r.Pinned(context.Background(), t.TempDir(), pin)
	if err == nil || !strings.Contains(err.Error(), "no such host") || !strings.Contains(err.Error(), pin) {
		t.Fatalf("error does not name the version and the cause: %v", err)
	}
	var calls []string
	r.Install = fakeInstall(t, &calls)
	if _, err := r.Pinned(context.Background(), t.TempDir(), pin); err != nil {
		t.Fatalf("a retry after a failed build: %v", err)
	}
	if len(calls) != 1 {
		t.Errorf("the failed build was served as a hit: %v", calls)
	}
}

// A project that already installed its pin (task tools) pays no build.
func TestPinned_ProjectToolsBinaryWhenVersionMatches(t *testing.T) {
	r, calls, _ := newResolver(t)
	project := t.TempDir()
	local := filepath.Join(project, ".tools", "bin", exeName())
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := atomicfile.WriteFile(local, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	reported := pin
	r.Version = func(context.Context, string) (string, error) { return reported, nil }
	r.Same = func(want, got string) bool { return want == got }

	res, err := r.Pinned(context.Background(), project, pin)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != local || len(*calls) != 0 {
		t.Fatalf("got %s after %v, want the project's own binary and no build", res.Path, *calls)
	}

	reported = "v0.1.40"
	res, err = r.Pinned(context.Background(), project, pin)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path == local || len(*calls) != 1 {
		t.Errorf("a .tools/bin/forge of another version was used: %s, builds %v", res.Path, *calls)
	}
}

func TestPinned_NoPin(t *testing.T) {
	r, _, _ := newResolver(t)
	if _, err := r.Pinned(context.Background(), t.TempDir(), " "); err == nil {
		t.Fatal("an empty pin resolved")
	}
}

// The cache keeps the newest few builds and never the one in use.
func TestPinned_PrunesLeastRecentlyUsed(t *testing.T) {
	r, _, _ := newResolver(t)
	cache := os.Getenv(CacheDirEnv)
	old := time.Now().Add(-48 * time.Hour)
	for i := 0; i < keepVersions+3; i++ {
		d := filepath.Join(cache, "v0.0."+string(rune('a'+i)))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		at := old.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(d, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Pinned(context.Background(), t.TempDir(), pin); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(cache)
	if want := keepVersions + 1; len(entries) != want {
		t.Fatalf("%d entries kept, want %d", len(entries), want)
	}
	if _, err := os.Stat(filepath.Join(cache, safeName(pin))); err != nil {
		t.Errorf("the build in use was pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "v0.0.a")); err == nil {
		t.Error("the least recently used entry survived")
	}
}

// A linked checkout is built where it stands, and rebuilt only when it changes.
func TestBridge_BuildsCheckoutPerStamp(t *testing.T) {
	r, _, _ := newResolver(t)
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "cmd", "forge"), 0o755); err != nil {
		t.Fatal(err)
	}
	var builds []string
	r.Build = func(_ context.Context, dir, out string) error {
		builds = append(builds, dir)
		return atomicfile.WriteFile(out, []byte("#!/bin/sh\n"), 0o755)
	}
	a, err := r.Bridge(context.Background(), checkout, "abc+false@0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Bridge(context.Background(), checkout, "abc+false@0"); err != nil {
		t.Fatal(err)
	}
	if len(builds) != 1 || builds[0] != checkout {
		t.Fatalf("builds %v, want one in %s", builds, checkout)
	}
	b, err := r.Bridge(context.Background(), checkout, "abc+true@17")
	if err != nil {
		t.Fatal(err)
	}
	if len(builds) != 2 || a.Path == b.Path {
		t.Errorf("an edited checkout was not rebuilt: builds %v, %s vs %s", builds, a.Path, b.Path)
	}
	if _, err := r.Bridge(context.Background(), t.TempDir(), "x"); err == nil {
		t.Error("a directory with no cmd/forge was built")
	}
}

func TestEnv_CarriesMarker(t *testing.T) {
	t.Setenv(MarkerEnv, "")
	if Delegated() {
		t.Fatal("delegated with the marker unset")
	}
	found := false
	for _, kv := range Env() {
		if kv == MarkerEnv+"=1" {
			found = true
		}
	}
	if !found {
		t.Error("Env() lacks the marker")
	}
	t.Setenv(MarkerEnv, "1")
	if !Delegated() {
		t.Error("not delegated with the marker set")
	}
}

func TestRunVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in")
	}
	for _, line := range []string{"forge version " + pin + " (commit ee3bded)", "reliant forge version " + pin} {
		bin := filepath.Join(t.TempDir(), "forge")
		if err := atomicfile.WriteFile(bin, []byte("#!/bin/sh\necho '"+line+"'\necho '  forge build: x'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := RunVersion(context.Background(), bin)
		if err != nil || got != pin {
			t.Errorf("%q: got %q, %v", line, got, err)
		}
	}
}

func TestSafeName(t *testing.T) {
	if got := safeName("v1/../x y+dirty"); strings.ContainsAny(got, "/ +") {
		t.Errorf("unsafe: %q", got)
	}
}

// Two processes that build one cache entry at once must agree on the binary,
// and the second must never replace the first's: the first may already be
// RUNNING it. os.Rename over an existing name did exactly that on Unix, and
// the running forge then saw "the forge binary changed while generate ran".
func TestBridge_ConcurrentBuildsNeverReplaceAPublishedBinary(t *testing.T) {
	r, _, _ := newResolver(t)
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "cmd", "forge"), 0o755); err != nil {
		t.Fatal(err)
	}
	slowStarted, fastDone := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r.Build = func(_ context.Context, _, out string) error {
		if calls.Add(1) == 1 { // the slow build: finishes after the fast one published
			close(slowStarted)
			<-fastDone
			return atomicfile.WriteFile(out, []byte("slow build\n"), 0o755)
		}
		return atomicfile.WriteFile(out, []byte("fast build\n"), 0o755)
	}
	type result struct {
		res Result
		err error
	}
	slow := make(chan result, 1)
	go func() {
		res, err := r.Bridge(context.Background(), checkout, "abc")
		slow <- result{res, err}
	}()
	<-slowStarted
	fast, err := r.Bridge(context.Background(), checkout, "abc")
	if err != nil {
		t.Fatal(err)
	}
	running, err := os.ReadFile(fast.Path)
	if err != nil {
		t.Fatal(err)
	}
	close(fastDone)
	s := <-slow
	if s.err != nil {
		t.Fatal(s.err)
	}
	after, err := os.ReadFile(fast.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(running) {
		t.Fatalf("a later build replaced the published binary a caller was handed: %q -> %q", running, after)
	}
	if s.res.Path != fast.Path {
		t.Errorf("the two builds resolved different paths: %s vs %s", s.res.Path, fast.Path)
	}
}
