package airbuild

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeBuildEnv makes this test binary act as a build command instead of
// running the tests: "fail:<code>" exits with code; "write:<path>:<text>"
// writes text to path and exits 0; "runner" prints CurrentRunner's answer.
const fakeBuildEnv = "AIRBUILD_FAKE_BUILD"

func TestMain(m *testing.M) {
	if spec, ok := os.LookupEnv(fakeBuildEnv); ok {
		os.Exit(fakeBuild(spec))
	}
	os.Exit(m.Run())
}

func fakeBuild(spec string) int {
	kind, arg, _ := strings.Cut(spec, ":")
	switch kind {
	case "fail":
		code, _ := strconv.Atoi(arg)
		_, _ = fmt.Fprintln(os.Stderr, "./main.go:3:1: syntax error")
		return code
	case "write":
		i := strings.LastIndexByte(arg, ':')
		if i < 0 {
			return 2
		}
		path, text := arg[:i], arg[i+1:]
		if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "runner":
		id, alive, ok := CurrentRunner()
		fmt.Printf("%s %t %t\n", id, ok, ok && alive())
		return 0
	}
	return 2
}

// fixture is a tmp_dir holding a binary air is running, built under runner.
type fixture struct {
	t      *testing.T
	out    string
	runner string
	alive  func() bool
	sigs   chan os.Signal
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, out: filepath.Join(dir, "app"), runner: "4242@100", alive: func() bool { return true }, sigs: make(chan os.Signal, 1)}
	return f
}

// running writes the binary a previous successful build under f.runner left.
func (f *fixture) running() *fixture {
	f.t.Helper()
	if err := os.WriteFile(f.out, []byte("last good build"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(Options{Out: f.out}.okPath(), []byte(f.runner), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return f
}

func (f *fixture) opts(spec string) Options {
	return Options{
		Out:    f.out,
		Argv:   []string{os.Args[0]},
		Env:    append(os.Environ(), fakeBuildEnv+"="+spec),
		Stderr: io.Discard,
		GOOS:   "linux",
		Poll:   5 * time.Millisecond,
		Runner: func() (string, func() bool, bool) {
			return f.runner, f.alive, true
		},
		Signals: f.sigs,
	}
}

// startRun runs o in the background; the result arrives on the channel.
func startRun(o Options) <-chan Result {
	ch := make(chan Result, 1)
	go func() { ch <- Run(context.Background(), o) }()
	return ch
}

// waitHeld waits until a build has announced its hold on stderr.
func waitHeld(t *testing.T, stderr *syncBuffer, done <-chan Result) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stderr.String(), "keeps serving the last good build") {
		select {
		case r := <-done:
			t.Fatalf("the failed build returned %+v instead of holding; stderr:\n%s", r, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no hold announced; stderr:\n%s", stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestFailedBuildHoldsWhileAirRuns is item #2 of the Ranger friction log:
// air restarted the old binary after every failed build. A failed build must
// not return to air while air runs (air restarts the server when it returns),
// must leave the running binary in place, and must keep holding after the
// next build succeeds: a superseded build that returned would take the next
// build's stop token out of air's one-slot channel (see the package comment).
func TestFailedBuildHoldsWhileAirRuns(t *testing.T) {
	f := newFixture(t).running()
	var mu sync.Mutex
	airGone := false
	f.alive = func() bool { mu.Lock(); defer mu.Unlock(); return !airGone }

	failErr := &syncBuffer{}
	failed := startRun(f.opts("fail:1").withStderr(failErr))
	waitHeld(t, failErr, failed)
	if got := readFile(t, f.out); got != "last good build" {
		t.Fatalf("the running binary was replaced by a failed build: %q", got)
	}

	// The fix builds and installs while the failed build is still held.
	next := Run(context.Background(), f.opts("write:"+f.out+".tmp:fixed build"))
	if next.ExitCode != 0 || next.Err != nil || next.Held {
		t.Fatalf("the next build = %+v, want a plain success", next)
	}
	if got := readFile(t, f.out); got != "fixed build" {
		t.Fatalf("the next build did not install its output: %q", got)
	}
	select {
	case r := <-failed:
		t.Fatalf("the superseded failed build returned %+v while air still runs", r)
	case <-time.After(50 * time.Millisecond):
	}
	if strings.Count(failErr.String(), "failed to build") != 1 {
		t.Fatalf("want one failure line on stderr, got:\n%s", failErr.String())
	}

	mu.Lock()
	airGone = true
	mu.Unlock()
	select {
	case r := <-failed:
		if r.ExitCode != 1 || !r.Held {
			t.Fatalf("released = %+v, want held, then the build's status 1", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held build outlived air")
	}
}

// Failures in a row each hold; a signal ends a hold with the build's status.
func TestFailuresInARowEachHold(t *testing.T) {
	f := newFixture(t).running()
	firstErr, secondErr := &syncBuffer{}, &syncBuffer{}
	first := startRun(f.opts("fail:1").withStderr(firstErr))
	waitHeld(t, firstErr, first)
	second := startRun(f.opts("fail:2").withStderr(secondErr))
	waitHeld(t, secondErr, second)
	select {
	case r := <-first:
		t.Fatalf("the first failed build returned %+v when the second started", r)
	case <-time.After(50 * time.Millisecond):
	}
	f.sigs <- syscall.SIGTERM
	f.sigs <- syscall.SIGTERM
	got := map[int]bool{}
	for _, ch := range []<-chan Result{first, second} {
		select {
		case r := <-ch:
			got[r.ExitCode] = true
		case <-time.After(10 * time.Second):
			t.Fatal("a held build ignored SIGTERM")
		}
	}
	if !got[1] || !got[2] {
		t.Fatalf("exit codes %v, want each build's own 1 and 2", got)
	}
}

// Holding keeps a server running only when one is: each of these exits with
// the build's status at once, which is air's own behaviour.
func TestFailedBuildReturnsAtOnceWhenNothingIsRunning(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fixture) Options
	}{
		{"first build: no binary yet", func(f *fixture) Options { return f.opts("fail:3") }},
		{"binary left by an air that is gone", func(f *fixture) Options {
			f.running()
			f.runner = "4243@200"
			return f.opts("fail:3")
		}},
		{"windows: air stopped the binary before building", func(f *fixture) Options {
			f.running()
			o := f.opts("fail:3")
			o.GOOS = "windows"
			return o
		}},
		{"no air process to tell apart", func(f *fixture) Options {
			f.running()
			o := f.opts("fail:3")
			o.Runner = func() (string, func() bool, bool) { return "", nil, false }
			return o
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			o := tc.setup(f)
			var stderr bytes.Buffer
			o.Stderr = &stderr
			r := Run(context.Background(), o)
			if r.ExitCode != 3 || r.Held {
				t.Fatalf("Run = %+v, want the build's status 3 with no hold; stderr:\n%s", r, stderr.String())
			}
			if strings.Contains(stderr.String(), "keeps serving") {
				t.Fatalf("announced a hold it did not do:\n%s", stderr.String())
			}
		})
	}
}

// A held build never outlives air: it ends when air is gone, or when air
// removed its tmp_dir on exit (clean_on_exit), with the build's own status.
func TestHeldBuildEndsWhenAirGoes(t *testing.T) {
	t.Run("air process gone", func(t *testing.T) {
		f := newFixture(t).running()
		var mu sync.Mutex
		gone := false
		f.alive = func() bool { mu.Lock(); defer mu.Unlock(); return !gone }
		stderr := &syncBuffer{}
		done := startRun(f.opts("fail:1").withStderr(stderr))
		waitHeld(t, stderr, done)
		mu.Lock()
		gone = true
		mu.Unlock()
		if r := <-done; r.ExitCode != 1 || !r.Held {
			t.Fatalf("Run = %+v, want held and released with status 1", r)
		}
	})
	t.Run("tmp_dir removed", func(t *testing.T) {
		f := newFixture(t).running()
		stderr := &syncBuffer{}
		done := startRun(f.opts("fail:1").withStderr(stderr))
		waitHeld(t, stderr, done)
		if err := os.RemoveAll(filepath.Dir(f.out)); err != nil {
			t.Fatal(err)
		}
		if r := <-done; r.ExitCode != 1 {
			t.Fatalf("Run = %+v, want released with status 1", r)
		}
	})
}

// A successful build installs <out>.tmp onto <out> and records the runner it
// ran under; a .tmp the build did not write is not installed.
func TestSuccessfulBuildInstallsItsOutput(t *testing.T) {
	t.Run("staged output", func(t *testing.T) {
		f := newFixture(t)
		r := Run(context.Background(), f.opts("write:"+f.out+".tmp:v1"))
		if r.ExitCode != 0 || r.Err != nil {
			t.Fatalf("Run = %+v", r)
		}
		if got := readFile(t, f.out); got != "v1" {
			t.Fatalf("out = %q, want v1", got)
		}
		if _, err := os.Stat(f.out + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf("the staged file is still there: %v", err)
		}
		if got := readFile(t, Options{Out: f.out}.okPath()); got != f.runner {
			t.Fatalf("recorded runner %q, want %q", got, f.runner)
		}
	})
	t.Run("build writes out itself; a stale .tmp stays", func(t *testing.T) {
		f := newFixture(t)
		if err := os.WriteFile(f.out+".tmp", []byte("stale"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := Run(context.Background(), f.opts("write:"+f.out+":direct"))
		if r.ExitCode != 0 || r.Err != nil {
			t.Fatalf("Run = %+v", r)
		}
		if got := readFile(t, f.out); got != "direct" {
			t.Fatalf("out = %q, want the build's own write", got)
		}
	})
}

func TestBuildThatCannotStartIs127(t *testing.T) {
	f := newFixture(t)
	o := f.opts("")
	o.Argv = []string{filepath.Join(t.TempDir(), "no-such-build")}
	if r := Run(context.Background(), o); r.ExitCode != 127 || r.Err == nil {
		t.Fatalf("Run = %+v, want 127 with an error", r)
	}
}

func (o Options) withStderr(w *syncBuffer) Options {
	o.Stderr = w
	return o
}

// syncBuffer is a bytes.Buffer safe to read while a build writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestFailedPrecheckDoesNotBuildOrRestart(t *testing.T) {
	f := newFixture(t).running()
	stderr := &syncBuffer{}
	o := f.opts("write:" + f.out + ".tmp:must not run").withStderr(stderr)
	o.PrecheckStatus = 7
	failed := startRun(o)
	waitHeld(t, stderr, failed)
	if _, err := os.Stat(f.out + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("a failed precheck ran the build: stat error %v", err)
	}
	f.sigs <- syscall.SIGTERM
	select {
	case r := <-failed:
		if r.ExitCode != 7 || !r.Held {
			t.Fatalf("released = %+v, want held then precheck's exit 7", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("held precheck outlived shutdown")
	}
}
