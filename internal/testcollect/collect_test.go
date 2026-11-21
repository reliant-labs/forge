package testcollect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/exitcode"
	"github.com/reliant-labs/forge/pkg/testreport"
)

// codedErr is the document's head as an error, the way the CLI returns it.
type codedErr struct {
	code int
	msg  string
}

func (e codedErr) Error() string { return e.msg }
func (e codedErr) ExitCode() int { return e.code }

// collectErr is Collect, with a non-zero head as an error.
func collectErr(ctx context.Context, opts Options) (*Document, error) {
	d := Collect(ctx, opts)
	if d.ExitCode != 0 {
		return d, codedErr{d.ExitCode, d.Error}
	}
	return d, nil
}

// fixture is a project root and a run directory in it.
type fixture struct {
	root, run string
	runs      int // how many times Options.Run was called
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	run := filepath.Join(root, ".forge", "tests", "runs", "01RUN")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	m := testreport.RunManifest{Schema: testreport.RunManifestSchema, ID: "01RUN", Lanes: []string{}}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(run, testreport.RunManifestFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return &fixture{root: root, run: run}
}

func (f *fixture) options(targets ...Target) Options {
	return Options{
		ProjectRoot: f.root,
		Env:         "dev",
		Targets:     targets,
		Run: func(context.Context) (string, []string, error) {
			f.runs++
			return f.run, nil, nil
		},
		Command: []string{"forge", "test", "collect", "dev"},
		Convert: fakeConvert,
	}
}

// fakeConvert stands in for `go tool covdata textfmt`: one block per
// counter file in dir, the first one hit, so a test can see what was
// converted without a -cover build.
func fakeConvert(_ context.Context, _, dir string) ([]testreport.CovdataBuild, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var hash string
	var prof strings.Builder
	prof.WriteString("mode: atomic\n")
	n := 0
	for _, e := range entries {
		f, ok := parseCovName(e.Name())
		if !ok || e.IsDir() {
			continue
		}
		if f.meta {
			hash = f.hash
			continue
		}
		hit := 0
		if n == 0 {
			hit = 3
		}
		fmt.Fprintf(&prof, "example.com/shop/internal/a/a.go:%d.1,%d.2 1 %d\n", 10*(n+1), 10*(n+1), hit)
		n++
	}
	return []testreport.CovdataBuild{{MetaHash: hash, Profile: []byte(prof.String())}}, nil
}

func writeFiles(t *testing.T, dir string, files ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("data of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// coverageServer serves a fixed tar at /debug/coverage and records the
// queries it was asked.
type coverageServer struct {
	mu      sync.Mutex
	queries []string
	tar     []byte
	status  int
}

func (s *coverageServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		q := r.Method + " " + r.URL.RequestURI()
		if h := r.Header.Get(ResetHeader); h != "" {
			q += " " + ResetHeader + "=" + h
		}
		s.queries = append(s.queries, q)
		s.mu.Unlock()
		if r.URL.Path != CoveragePath {
			http.NotFound(w, r)
			return
		}
		if s.status != 0 && s.status != http.StatusOK {
			http.Error(w, "this binary was not built with -cover", s.status)
			return
		}
		_, _ = w.Write(s.tar)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func (s *coverageServer) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
}

// A host workload's coverage is its live pull plus what earlier processes
// flushed to GOCOVERDIR. The live build is the workload's coverage; an older
// air build in GOCOVERDIR is kept apart, labelled stale, and never listed as
// an input. The lane is recorded through capture.json with go-covdata input.
func TestCollectHostPullAndExitFlush(t *testing.T) {
	f := newFixture(t)
	srv := &coverageServer{tar: tarOf(t, "covmeta.aa", "META-A", "covcounters.aa.300.9000", "LIVE")}
	addr := srv.start(t)
	cover := filepath.Join(f.root, ".forge", "tests", "covdata", "dev", "api")
	writeFiles(t, cover,
		"covmeta.aa", "covcounters.aa.200.5000", // an earlier process of the same build
		"covmeta.bb", "covcounters.bb.100.8000", // the build before air's last rebuild
	)

	doc, err := collectErr(context.Background(), f.options(Target{
		Workload: "api", Runtime: "host",
		Host: &HostTarget{PullAddr: addr, CoverDir: cover},
	}))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !doc.OK || doc.ExitCode != 0 || doc.Schema != Schema || doc.Collected != 1 || doc.Run != f.run {
		t.Fatalf("doc head %+v collected=%d run=%q", doc.Head, doc.Collected, doc.Run)
	}
	if q := srv.got(); len(q) != 1 || q[0] != "GET "+CoveragePath {
		t.Errorf("pulled %v, want one GET %s with no reset", q, CoveragePath)
	}
	wl := doc.Workloads[0]
	if wl.State != StateCollected || wl.Build == nil || wl.Build.MetaHash != "aa" || wl.Build.CounterFiles != 2 {
		t.Fatalf("workload %+v build %+v, want build aa with the live and the flushed counters", wl, wl.Build)
	}
	if wl.Build.Dir != "e2e/covdata/api" {
		t.Errorf("build dir %q", wl.Build.Dir)
	}
	if c := wl.Build.Coverage; c == nil || c.Statements != 2 || c.Covered != 1 {
		t.Errorf("converted coverage %+v, want 1 of 2 statements", c)
	}
	if len(wl.Stale) != 1 || wl.Stale[0].MetaHash != "bb" || wl.Stale[0].Label != StaleBuildLabel || wl.Stale[0].Dir != "e2e/covdata/api/stale/bb" {
		t.Errorf("stale builds %+v, want bb labelled %q", wl.Stale, StaleBuildLabel)
	}

	wlDir := filepath.Join(f.run, "e2e", "covdata", "api")
	if got, want := dirNames(t, wlDir), []string{"covcounters.aa.200.5000", "covcounters.aa.300.9000", "covmeta.aa", "stale"}; !slices.Equal(got, want) {
		t.Errorf("workload dir %v, want %v", got, want)
	}
	if got := dirNames(t, filepath.Join(wlDir, "stale", "bb")); !slices.Equal(got, []string{"covcounters.bb.100.8000", "covmeta.bb"}) {
		t.Errorf("stale dir %v", got)
	}

	m, err := testreport.ReadLaneManifest(f.run, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Coverage) != 1 || m.Coverage[0] != (testreport.LaneCoverageInput{Path: "covdata/api/", Format: testreport.CoverageGoCovdata}) {
		t.Errorf("lane coverage %+v, want only the newest build's dir", m.Coverage)
	}
	if m.Results != testreport.LaneResultsMissing || !slices.Equal(m.Command, []string{"forge", "test", "collect", "dev"}) {
		t.Errorf("new lane manifest %+v", m)
	}
	rm, _ := testreport.ReadRunManifest(f.run)
	if !slices.Equal(rm.Lanes, []string{"e2e"}) {
		t.Errorf("run lanes %v", rm.Lanes)
	}
}

// A workload name is joined into directories a collect removes and rewrites,
// so a rendered env naming a workload "../../keep" must not reach outside the
// run: the workload fails, is never pulled, and nothing outside is touched.
func TestCollectRefusesAWorkloadNameThatIsNotOnePathElement(t *testing.T) {
	f := newFixture(t)
	srv := &coverageServer{tar: tarOf(t, "covmeta.aa", "M", "covcounters.aa.300.9000", "LIVE")}
	addr := srv.start(t)
	keep := filepath.Join(f.run, "keep") // where e2e/covdata/../../keep lands
	writeFiles(t, keep, "precious")

	for _, name := range []string{"../../keep", "a/b", `a\b`, "..", ".", ""} {
		doc := Collect(context.Background(), f.options(Target{
			Workload: name, Runtime: "host",
			Host: &HostTarget{PullAddr: addr, CoverDir: filepath.Join(f.root, "cover")},
		}))
		if wl := doc.Workloads[0]; wl.State != StateFailed || !strings.Contains(wl.Reason, "single path element") {
			t.Errorf("workload %q: state %s reason %q, want failed as not a single path element", name, wl.State, wl.Reason)
		}
		if doc.Run != "" {
			t.Errorf("workload %q: recorded run %q", name, doc.Run)
		}
	}
	if q := srv.got(); len(q) != 0 {
		t.Errorf("pulled %v for workloads with unsafe names", q)
	}
	if got := dirNames(t, keep); !slices.Equal(got, []string{"precious"}) {
		t.Errorf("dir outside the run's coverage now holds %v", got)
	}
	if err := validateWorkloadName("api-server_2"); err != nil {
		t.Errorf("a plain workload name was refused: %v", err)
	}
}

// A reset the process refused zeroed nothing, so it fences nothing off: no
// marker is written, and the next collect still keeps the counters flushed
// before it.
func TestFailedResetKeepsEarlierCounters(t *testing.T) {
	f := newFixture(t)
	srv := &coverageServer{tar: tarOf(t, "covmeta.aa", "M", "covcounters.aa.300.9000", "LIVE"), status: http.StatusConflict}
	addr := srv.start(t)
	cover := filepath.Join(f.root, ".forge", "tests", "covdata", "dev", "api")
	writeFiles(t, cover, "covmeta.aa", "covcounters.aa.200.5000")
	target := Target{Workload: "api", Runtime: "host", Host: &HostTarget{PullAddr: addr, CoverDir: cover}}

	opts := f.options(target)
	opts.Reset = true
	opts.Now = func() time.Time { return time.Unix(0, 6000) }
	doc := Collect(context.Background(), opts)
	if wl := doc.Workloads[0]; wl.State != StateFailed {
		t.Fatalf("a refused reset: state %s, want failed", wl.State)
	}
	if _, err := os.Stat(ResetMarker(f.root, "dev", "api")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused reset wrote the fence: %v", err)
	}

	// The process accepts pulls again (a fresh server: the refusing one's
	// status is read unlocked by its handler).
	target.Host.PullAddr = (&coverageServer{tar: srv.tar}).start(t)
	doc, err := collectErr(context.Background(), f.options(target))
	if err != nil {
		t.Fatal(err)
	}
	if b := doc.Workloads[0].Build; b == nil || b.CounterFiles != 2 {
		t.Fatalf("after a refused reset: build %+v, want the live and the earlier flushed counters", b)
	}
}

// roundTripFunc is an http.RoundTripper from a func.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A reset that reached no process (down, or timed out) zeroed nothing: no
// fence is written and the flushed counters stay in the next collect. The
// transport fails in-process, not by dialing a closed port, so it is
// instant and the same on every OS.
func TestUnreachableResetKeepsEarlierCounters(t *testing.T) {
	f := newFixture(t)
	cover := filepath.Join(f.root, ".forge", "tests", "covdata", "dev", "api")
	writeFiles(t, cover, "covmeta.aa", "covcounters.aa.200.5000")
	target := Target{Workload: "api", Runtime: "host", Host: &HostTarget{PullAddr: "127.0.0.1:1", CoverDir: cover}}

	opts := f.options(target)
	opts.Reset = true
	opts.Now = func() time.Time { return time.Unix(0, 6000) }
	opts.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	})}
	doc := Collect(context.Background(), opts)
	if wl := doc.Workloads[0]; wl.State != StateUnreachable {
		t.Fatalf("an unreachable reset: state %s (%s), want unreachable", wl.State, wl.Reason)
	}
	if _, err := os.Stat(ResetMarker(f.root, "dev", "api")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unreachable reset wrote the fence: %v", err)
	}

	target.Host.PullAddr = (&coverageServer{tar: tarOf(t, "covmeta.aa", "M", "covcounters.aa.300.9000", "LIVE")}).start(t)
	doc, err := collectErr(context.Background(), f.options(target))
	if err != nil {
		t.Fatal(err)
	}
	if b := doc.Workloads[0].Build; b == nil || b.CounterFiles != 2 {
		t.Fatalf("after an unreachable reset: build %+v, want the live and the earlier flushed counters", b)
	}
}

// --reset pulls with ?reset=1 and discards what it got, and fences off the
// counters flushed before it: the next collect leaves them out. No run is
// created for a reset.
func TestResetFencesOffEarlierCounters(t *testing.T) {
	f := newFixture(t)
	srv := &coverageServer{tar: tarOf(t, "covmeta.aa", "M", "covcounters.aa.300.9000", "LIVE")}
	addr := srv.start(t)
	cover := filepath.Join(f.root, ".forge", "tests", "covdata", "dev", "api")
	writeFiles(t, cover, "covmeta.aa", "covcounters.aa.200.5000")
	target := Target{Workload: "api", Runtime: "host", Host: &HostTarget{PullAddr: addr, CoverDir: cover}}

	opts := f.options(target)
	opts.Reset = true
	opts.Now = func() time.Time { return time.Unix(0, 6000) }
	doc, err := collectErr(context.Background(), opts)
	if err != nil || doc.Workloads[0].State != StateReset || doc.Collected != 1 {
		t.Fatalf("reset: %v %+v", err, doc.Workloads)
	}
	if q := srv.got(); len(q) != 1 || q[0] != "POST "+CoveragePath+" "+ResetHeader+"=1" {
		t.Errorf("reset sent %v, want one POST with %s: 1 (serverkit refuses any other reset)", q, ResetHeader)
	}
	if f.runs != 0 || doc.Run != "" {
		t.Errorf("a reset asked for a run (%d calls, run %q)", f.runs, doc.Run)
	}
	if _, err := os.Stat(filepath.Join(f.run, "e2e")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a reset wrote a lane: %v", err)
	}

	doc, err = collectErr(context.Background(), f.options(target))
	if err != nil {
		t.Fatal(err)
	}
	if b := doc.Workloads[0].Build; b == nil || b.CounterFiles != 1 {
		t.Fatalf("after reset: build %+v, want only the live counters (the flushed file was there at the reset)", b)
	}
}

// fenceNames reads the counter-file names a workload's reset fence holds.
func fenceNames(t *testing.T, root, workload string) []string {
	t.Helper()
	_, f, err := readFence(ResetMarker(root, "dev", workload))
	if err != nil {
		t.Fatal(err)
	}
	out := slices.Clone(f.Excluded)
	for _, e := range f.Entries {
		out = append(out, e.Name)
	}
	slices.Sort(out)
	return out
}

// writeFile writes one file with the given bytes.
func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// [B1] Go names a counter file covcounters.<build>.<pid>.<nanos>, so a file
// flushed after a reset can share a fenced file's name. The fence keys on
// (source, name, content): a same-named file with other bytes, in the same
// GOCOVERDIR, is a different file and is collected.
func TestResetFenceKeepsASameNamedFileWithOtherBytes(t *testing.T) {
	f := newFixture(t)
	cover := filepath.Join(f.root, ".forge", "tests", "covdata", "dev", "api")
	writeFile(t, cover, "covmeta.aa", "M")
	writeFile(t, cover, "covcounters.aa.1.100", "PRE-RESET")
	target := Target{Workload: "api", Runtime: "host", Host: &HostTarget{Withheld: "PPROF_ADDR binds 0.0.0.0:6060", CoverDir: cover}}
	opts := f.options(target)
	opts.Reset = true
	if doc := Collect(context.Background(), opts); doc.Workloads[0].State != StateReset {
		t.Fatalf("reset: %+v", doc.Workloads[0])
	}
	if _, err := collectErr(context.Background(), f.options(target)); err == nil {
		t.Fatal("the fenced file was collected")
	}

	writeFile(t, cover, "covcounters.aa.1.100", "POST-RESET") // same pid and nanos, new bytes
	doc, err := collectErr(context.Background(), f.options(target))
	if err != nil {
		t.Fatalf("the post-reset file was dropped: %v", err)
	}
	b := doc.Workloads[0].Build
	if b == nil || b.CounterFiles != 1 {
		t.Fatalf("build %+v, want the post-reset file", b)
	}
	got, err := os.ReadFile(filepath.Join(f.run, filepath.FromSlash(b.Dir), "covcounters.aa.1.100"))
	if err != nil || string(got) != "POST-RESET" {
		t.Fatalf("collected %q (%v), want the post-reset bytes", got, err)
	}
}

// [B1] Across sources: a container started after the reset writes a file of
// the same name, and even the same bytes, as a fenced container's file. The
// fence names the container it saw, so the new one is collected; the fenced
// container's file stays out until its bytes change.
func TestResetFenceIsPerSource(t *testing.T) {
	f := newFixture(t)
	d := &fakeDocker{
		ids:  map[string][]string{"api": {"c1"}},
		tars: map[string][]byte{"c1": tarOf(t, "./covmeta.aa", "M", "./covcounters.aa.1.100", "SAME")},
	}
	target := Target{Workload: "api", Runtime: "compose", Compose: &ComposeTarget{Service: "api", CoverDir: "/tmp"}}
	opts := f.options(target)
	opts.Docker, opts.Reset = d, true
	if doc := Collect(context.Background(), opts); doc.Workloads[0].State != StateReset {
		t.Fatalf("reset: %+v", doc.Workloads[0])
	}
	opts.Reset = false

	d.ids["api"] = []string{"c1", "c2"}
	d.tars["c2"] = tarOf(t, "./covmeta.aa", "M", "./covcounters.aa.1.100", "SAME")
	doc, err := collectErr(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if b := doc.Workloads[0].Build; b == nil || b.CounterFiles != 1 {
		t.Fatalf("build %+v, want c2's file only: c1's is fenced, c2 is another source", b)
	}

	// c1 flushes again under the same name, with new counts: both are kept,
	// the name collision resolved as build.write resolves any.
	d.tars["c1"] = tarOf(t, "./covmeta.aa", "M", "./covcounters.aa.1.100", "NEW")
	if doc, err = collectErr(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if b := doc.Workloads[0].Build; b == nil || b.CounterFiles != 2 {
		t.Fatalf("build %+v, want c1's new file and c2's", b)
	}
}

// A fence written by the first version (bare names) is still read: a name it
// lists is left out from any source, with any bytes, and a later reset keeps
// it while adding entries of the current form.
func TestLegacyNameOnlyFenceStillFences(t *testing.T) {
	f := newFixture(t)
	cover := filepath.Join(f.root, ".forge", "tests", "covdata", "dev", "api")
	writeFile(t, cover, "covmeta.aa", "M")
	writeFile(t, cover, "covcounters.aa.1.100", "ANY")
	writeFile(t, cover, "covcounters.aa.2.200", "KEPT")
	marker := ResetMarker(f.root, "dev", "api")
	writeFile(t, filepath.Dir(marker), filepath.Base(marker), `{"schema":"forge.test.collect.reset/v1","reset_at":"2026-11-21T00:00:00Z","excluded":["covcounters.aa.1.100"]}`)
	target := Target{Workload: "api", Runtime: "host", Host: &HostTarget{Withheld: "PPROF_ADDR binds 0.0.0.0:6060", CoverDir: cover}}
	doc, err := collectErr(context.Background(), f.options(target))
	if err != nil {
		t.Fatal(err)
	}
	if b := doc.Workloads[0].Build; b == nil || b.CounterFiles != 1 {
		t.Fatalf("build %+v, want only the file the legacy fence does not name", b)
	}
	opts := f.options(target)
	opts.Reset = true
	Collect(context.Background(), opts)
	_, fe, err := readFence(marker)
	if err != nil || !slices.Equal(fe.Excluded, []string{"covcounters.aa.1.100"}) || len(fe.Entries) != 2 {
		t.Fatalf("rewritten fence %+v (%v), want the legacy name kept and both files entered", fe, err)
	}
}

// [B1] A reset after an instrumented process has already exited. Its
// counters are collectable until the reset, and the reset (which starts the
// run from zero) then fences off that file by name, and only that file:
// anything flushed after the reset is collected whatever time its name
// encodes, and a second reset keeps the first one's exclusions.
func TestResetAfterAProcessExited(t *testing.T) {
	f := newFixture(t)
	addr := (&coverageServer{tar: tarOf(t, "covmeta.aa", "M", "covcounters.aa.300.9000", "LIVE")}).start(t)
	cover := filepath.Join(f.root, ".forge", "tests", "covdata", "dev", "api")
	writeFiles(t, cover, "covmeta.aa", "covcounters.aa.100.5000") // a process that exited
	target := Target{Workload: "api", Runtime: "host", Host: &HostTarget{PullAddr: addr, CoverDir: cover}}
	collect := func(want int, why string) {
		t.Helper()
		doc, err := collectErr(context.Background(), f.options(target))
		if err != nil {
			t.Fatal(err)
		}
		if b := doc.Workloads[0].Build; b == nil || b.CounterFiles != want {
			t.Fatalf("%s: build %+v, want %d counter file(s)", why, b, want)
		}
	}
	reset := func(at int64) {
		t.Helper()
		opts := f.options(target)
		opts.Reset = true
		opts.Now = func() time.Time { return time.Unix(0, at) }
		if doc := Collect(context.Background(), opts); doc.Workloads[0].State != StateReset {
			t.Fatalf("reset: %+v", doc.Workloads[0])
		}
	}

	collect(2, "before any reset, the exited process's counters are collected")
	reset(6000)
	if got := fenceNames(t, f.root, "api"); !slices.Equal(got, []string{"covcounters.aa.100.5000"}) {
		t.Fatalf("fence %v, want exactly the file flushed before the reset", got)
	}
	collect(1, "after the reset, the exited process's file is left out")

	// Flushed after the reset by a process whose name encodes an earlier time
	// (a timestamp comparison would drop it), and by one stamped later.
	writeFiles(t, cover, "covcounters.aa.200.1000", "covcounters.aa.400.7000")
	collect(3, "files flushed after the reset are collected, whatever their timestamps")

	reset(8000)
	want := []string{"covcounters.aa.100.5000", "covcounters.aa.200.1000", "covcounters.aa.400.7000"}
	if got := fenceNames(t, f.root, "api"); !slices.Equal(got, want) {
		t.Fatalf("fence after a second reset %v, want %v: it only grows", got, want)
	}
	collect(1, "a second reset excludes what it saw, and keeps the first one's exclusion")
}

// The inverse: a reset with nothing flushed fences nothing, and every file
// that appears afterwards is collected.
func TestFileFlushedAfterResetIsCollected(t *testing.T) {
	f := newFixture(t)
	cover := filepath.Join(f.root, ".forge", "tests", "covdata", "dev", "api")
	target := Target{Workload: "api", Runtime: "host", Host: &HostTarget{Withheld: "PPROF_ADDR binds 0.0.0.0:6060", CoverDir: cover}}
	opts := f.options(target)
	opts.Reset = true
	opts.Now = func() time.Time { return time.Unix(0, 1<<62) } // far later than any name below
	if doc := Collect(context.Background(), opts); doc.Workloads[0].State != StateReset {
		t.Fatalf("reset: %+v", doc.Workloads[0])
	}
	if got := fenceNames(t, f.root, "api"); len(got) != 0 {
		t.Fatalf("fence %v, want empty: nothing was flushed", got)
	}
	writeFiles(t, cover, "covmeta.aa", "covcounters.aa.1.10", "covcounters.aa.2.20")
	doc, err := collectErr(context.Background(), f.options(target))
	if err != nil {
		t.Fatal(err)
	}
	if b := doc.Workloads[0].Build; b == nil || b.CounterFiles != 2 {
		t.Fatalf("build %+v, want both files flushed after the reset", b)
	}
}

// Compose: the reset fences the names in every container; a container that
// cannot be read fails the reset rather than fencing too little.
func TestResetComposeFencesContainerFiles(t *testing.T) {
	f := newFixture(t)
	d := &fakeDocker{
		ids:  map[string][]string{"api": {"c1", "c2"}},
		tars: map[string][]byte{"c1": tarOf(t, "./covmeta.aa", "M", "./covcounters.aa.1.100", "OLD"), "c2": tarOf(t, "./tmpfile", "x")},
	}
	target := Target{Workload: "api", Runtime: "compose", Compose: &ComposeTarget{Service: "api", CoverDir: "/tmp"}}
	opts := f.options(target)
	opts.Docker, opts.Reset = d, true
	if doc := Collect(context.Background(), opts); doc.Workloads[0].State != StateReset {
		t.Fatalf("reset: %+v", doc.Workloads[0])
	}
	if got := fenceNames(t, f.root, "api"); !slices.Equal(got, []string{"covcounters.aa.1.100"}) {
		t.Fatalf("fence %v", got)
	}
	d.tars["c2"] = tarOf(t, "./covmeta.aa", "M", "./covcounters.aa.1.50", "NEW") // c2's process exits after the reset
	opts.Reset = false
	doc, err := collectErr(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if b := doc.Workloads[0].Build; b == nil || b.CounterFiles != 1 {
		t.Fatalf("build %+v, want only c2's file flushed after the reset", b)
	}

	g := newFixture(t)
	opts = g.options(target)
	opts.Reset = true
	opts.Docker = &fakeDocker{ids: map[string][]string{"api": {"c1"}}, copyErr: map[string]error{"c1": errors.New("daemon gone")}}
	if doc := Collect(context.Background(), opts); doc.Workloads[0].State != StateFailed {
		t.Fatalf("an unreadable container: %+v, want failed", doc.Workloads[0])
	}
	if _, err := os.Stat(ResetMarker(g.root, "dev", "api")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed reset wrote a fence: %v", err)
	}
}

// Compose withholds the pull endpoint, so a container's GOCOVERDIR is copied
// out (`docker cp`, which nests it under "./"). A container whose process
// has not exited yet has nothing there, which is reported, not failed.
func TestCollectCompose(t *testing.T) {
	f := newFixture(t)
	d := &fakeDocker{
		ids: map[string][]string{"api": {"c1", "c2"}, "worker": {"c3"}},
		tars: map[string][]byte{
			"c1": tarOf(t, "./", "<dir>", "./covmeta.aa", "M", "./covcounters.aa.1.100", "X", "./other.log", "x"),
			"c2": tarOf(t, "./covmeta.aa", "M", "./covcounters.aa.1.200", "Y"),
			"c3": tarOf(t, "./tmpfile", "x"),
		},
	}
	opts := f.options(
		Target{Workload: "api", Runtime: "compose", Compose: &ComposeTarget{Service: "api", WorkingDir: f.root, CoverDir: "/tmp"}},
		Target{Workload: "worker", Runtime: "compose", Compose: &ComposeTarget{Service: "worker", WorkingDir: f.root, CoverDir: "/tmp"}},
	)
	opts.Docker = d
	doc, err := collectErr(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	api, worker := doc.Workloads[0], doc.Workloads[1]
	if api.State != StateCollected || api.Build.CounterFiles != 2 {
		t.Errorf("api %+v, want both containers' counters", api)
	}
	if worker.State != StateEmpty || !strings.Contains(strings.Join(worker.Notes, " "), "when its process exits") {
		t.Errorf("worker %+v, want empty with the exit-flush note", worker)
	}
	if !slices.Equal(d.copied, []string{"c1:/tmp", "c2:/tmp", "c3:/tmp"}) {
		t.Errorf("copied %v", d.copied)
	}
}

type fakeDocker struct {
	ids     map[string][]string
	tars    map[string][]byte
	copyErr map[string]error
	copied  []string
	err     error
}

func (d *fakeDocker) Containers(_ context.Context, c ComposeTarget) ([]string, error) {
	return d.ids[c.Service], d.err
}

func (d *fakeDocker) CopyOut(_ context.Context, id, dir string) ([]byte, error) {
	d.copied = append(d.copied, id+":"+dir)
	if err := d.copyErr[id]; err != nil {
		return nil, err
	}
	return d.tars[id], nil
}

// A cluster workload is pulled pod by pod through a port-forward; pods of
// two rollouts are two builds, and the newest pod's is current.
func TestCollectCluster(t *testing.T) {
	f := newFixture(t)
	t0 := time.Unix(1_700_000_000, 0)
	k := &fakeKube{
		pods: []Pod{{Name: "api-old", Started: t0}, {Name: "api-new", Started: t0.Add(time.Hour)}},
		tars: map[string][]byte{
			"api-old": tarOf(t, "covmeta.aa", "M", "covcounters.aa.1.100", "X"),
			"api-new": tarOf(t, "covmeta.bb", "M", "covcounters.bb.1.50", "Y"),
		},
	}
	opts := f.options(Target{Workload: "api", Runtime: "cluster", Cluster: &ClusterTarget{
		Context: "k3d-dev", Namespace: "shop-dev", Selector: "app.kubernetes.io/name=api", Port: 6060,
	}})
	opts.Kube = k
	doc, err := collectErr(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	wl := doc.Workloads[0]
	if wl.State != StateCollected || wl.Build.MetaHash != "bb" || len(wl.Stale) != 1 || wl.Stale[0].MetaHash != "aa" {
		t.Fatalf("workload %+v, want bb current and aa stale", wl)
	}
	want := []string{
		"GET k3d-dev/shop-dev/api-old:6060 " + CoveragePath + " host=127.0.0.1:6060",
		"GET k3d-dev/shop-dev/api-new:6060 " + CoveragePath + " host=127.0.0.1:6060",
	}
	if !slices.Equal(k.gets, want) {
		t.Errorf("GETs %v, want %v", k.gets, want)
	}

	opts.Reset = true
	k.gets = nil
	if _, err := collectErr(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	resetCall := "POST k3d-dev/shop-dev/api-old:6060 " + CoveragePath + " host=127.0.0.1:6060 " + ResetHeader + "=1"
	if len(k.gets) != 2 || k.gets[0] != resetCall || !strings.HasPrefix(k.gets[1], "POST ") {
		t.Errorf("reset requests %v, want POST with %s on each pod (%q)", k.gets, ResetHeader, resetCall)
	}
}

type fakeKube struct {
	pods []Pod
	tars map[string][]byte
	gets []string
	err  error
}

func (k *fakeKube) Pods(context.Context, string, string, string) ([]Pod, error) { return k.pods, k.err }

func (k *fakeKube) Do(_ context.Context, kctx, ns, pod string, port int, req *http.Request) (*http.Response, error) {
	call := fmt.Sprintf("%s %s/%s/%s:%d %s host=%s", req.Method, kctx, ns, pod, port, req.URL.RequestURI(), req.URL.Host)
	if h := req.Header.Get(ResetHeader); h != "" {
		call += " " + ResetHeader + "=" + h
	}
	k.gets = append(k.gets, call)
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(k.tars[pod]))}, nil
}

// Exit codes: 0 when anything was collected or there was nothing to
// collect, 2 when an instrumented workload could not be reached and nothing
// was collected.
func TestCollectExitCodes(t *testing.T) {
	closed := closedAddr(t)
	unavailable := Target{Workload: "web", Runtime: "cluster", Coverage: CoverageUnavailable}
	plain := Target{Workload: "migrate", Runtime: "host", Coverage: CoverageNotInstrumented}
	down := func(root string) Target {
		return Target{Workload: "api", Runtime: "host", Host: &HostTarget{PullAddr: closed, CoverDir: filepath.Join(root, "none")}}
	}

	t.Run("nothing instrumented", func(t *testing.T) {
		f := newFixture(t)
		doc, err := collectErr(context.Background(), f.options(unavailable, plain))
		if err != nil || doc.ExitCode != 0 {
			t.Fatalf("err %v code %d, want 0", err, doc.ExitCode)
		}
		if doc.Workloads[0].State != StateUnavailable || doc.Workloads[0].Coverage != "unavailable (not a forge Go build)" ||
			doc.Workloads[1].State != StateNotInstrumented || doc.Workloads[1].Coverage != "not instrumented" {
			t.Errorf("workloads %+v", doc.Workloads)
		}
		if f.runs != 0 {
			t.Error("a collect with nothing to write asked for a run")
		}
	})
	t.Run("unreachable and nothing collected", func(t *testing.T) {
		f := newFixture(t)
		doc, err := collectErr(context.Background(), f.options(unavailable, down(f.root)))
		if exitcode.Of(err) != 2 || doc.ExitCode != 2 || doc.OK {
			t.Fatalf("err %v doc %+v, want exit 2", err, doc.Head)
		}
		if wl := doc.Workloads[1]; wl.State != StateUnreachable || !strings.Contains(wl.Reason, "refused") { // POSIX "connection refused", Winsock "actively refused it"
			t.Errorf("api %+v", wl)
		}
		if !strings.Contains(doc.Error, "api could not be reached") {
			t.Errorf("error %q, want the unreachable workload named", doc.Error)
		}
	})
	t.Run("unreachable but another collected", func(t *testing.T) {
		f := newFixture(t)
		cover := filepath.Join(f.root, "flushed")
		writeFiles(t, cover, "covmeta.aa", "covcounters.aa.1.1")
		worker := Target{Workload: "worker", Runtime: "host", Host: &HostTarget{Withheld: "PPROF_ADDR binds 0.0.0.0:6060", CoverDir: cover}}
		doc, err := collectErr(context.Background(), f.options(down(f.root), worker))
		if err != nil || doc.Collected != 1 {
			t.Fatalf("err %v collected %d, want 0 with one collected", err, doc.Collected)
		}
		m, _ := testreport.ReadLaneManifest(f.run, "e2e")
		if len(m.Warnings) != 1 || !strings.Contains(m.Warnings[0], "api unreachable") {
			t.Errorf("lane warnings %v, want the unreachable workload named", m.Warnings)
		}
	})
	t.Run("a binary without -cover", func(t *testing.T) {
		f := newFixture(t)
		srv := &coverageServer{status: http.StatusNotFound}
		doc, err := collectErr(context.Background(), f.options(Target{Workload: "api", Runtime: "host", Host: &HostTarget{PullAddr: srv.start(t), CoverDir: filepath.Join(f.root, "none")}}))
		if exitcode.Of(err) != 2 || doc.Workloads[0].State != StateFailed || !strings.Contains(doc.Workloads[0].Reason, "404") {
			t.Fatalf("err %v workload %+v, want failed (404), exit 2", err, doc.Workloads[0])
		}
	})
	t.Run("a bad lane", func(t *testing.T) {
		f := newFixture(t)
		opts := f.options()
		opts.Lane = "../x"
		if _, err := collectErr(context.Background(), opts); exitcode.Of(err) != 1 {
			t.Fatalf("err %v, want exit 1", err)
		}
	})
}

// closedAddr is a loopback address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// A collect after `forge test capture --lane e2e` joins that lane: its
// results and earlier coverage inputs are kept, the workload's input is
// replaced, not repeated, on a second collect.
func TestCollectJoinsACapturedLane(t *testing.T) {
	f := newFixture(t)
	laneDir := filepath.Join(f.run, "e2e")
	writeFiles(t, laneDir)
	captured := testreport.LaneManifest{
		Schema: testreport.LaneManifestSchema, Lane: "e2e", Command: []string{"go", "test", "./e2e/..."},
		ExitCode: 1, Format: testreport.LaneFormatGoJSON, Results: testreport.LaneResultsParsed, ResultsFile: "results.json",
		Coverage: []testreport.LaneCoverageInput{{Path: "cover.out", Format: testreport.CoverageGoProfile}},
	}
	b, _ := json.Marshal(captured)
	if err := os.WriteFile(filepath.Join(laneDir, testreport.LaneManifestFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	cover := filepath.Join(f.root, "flushed")
	writeFiles(t, cover, "covmeta.aa", "covcounters.aa.1.1")
	target := Target{Workload: "api", Runtime: "host", Host: &HostTarget{Withheld: "PPROF_ADDR binds 0.0.0.0:6060", CoverDir: cover}}
	for range 2 {
		if _, err := collectErr(context.Background(), f.options(target)); err != nil {
			t.Fatal(err)
		}
	}
	m, err := testreport.ReadLaneManifest(f.run, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	if m.ExitCode != 1 || m.Results != testreport.LaneResultsParsed || !slices.Equal(m.Command, captured.Command) {
		t.Errorf("the captured lane's record changed: %+v", m)
	}
	want := []testreport.LaneCoverageInput{
		{Path: "covdata/api/", Format: testreport.CoverageGoCovdata},
		{Path: "cover.out", Format: testreport.CoverageGoProfile},
	}
	if !slices.Equal(m.Coverage, want) {
		t.Errorf("coverage %+v, want %+v", m.Coverage, want)
	}
}

func TestWriteText(t *testing.T) {
	f := newFixture(t)
	cover := filepath.Join(f.root, "flushed")
	writeFiles(t, cover, "covmeta.aabbccddeeff0011", "covcounters.aabbccddeeff0011.1.9", "covmeta.99", "covcounters.99.1.1")
	doc, _ := collectErr(context.Background(), f.options(
		Target{Workload: "api", Runtime: "host", Host: &HostTarget{Withheld: "PPROF_ADDR binds 0.0.0.0:6060", CoverDir: cover}},
		Target{Workload: "web", Runtime: "cluster", Coverage: CoverageUnavailable},
	))
	var buf bytes.Buffer
	WriteText(&buf, doc)
	out := buf.String()
	for _, want := range []string{
		"forge test collect dev → " + f.run + "/e2e",
		"api  host     collected  build aabbccddeeff, 1 counter file(s), 100.0% of 1 statements (exit-flush flushed)",
		"stale build 99: 1 counter file(s) in e2e/covdata/api/stale/99, never merged",
		"web  cluster  coverage: unavailable (not a forge Go build)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
}
