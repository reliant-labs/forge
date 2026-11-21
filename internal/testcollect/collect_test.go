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
		t.Fatalf("after reset: build %+v, want only the live counters (the flushed 5000 predates the reset at 6000)", b)
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
	ids    map[string][]string
	tars   map[string][]byte
	copied []string
	err    error
}

func (d *fakeDocker) Containers(_ context.Context, c ComposeTarget) ([]string, error) {
	return d.ids[c.Service], d.err
}

func (d *fakeDocker) CopyOut(_ context.Context, id, dir string) ([]byte, error) {
	d.copied = append(d.copied, id+":"+dir)
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
		if wl := doc.Workloads[1]; wl.State != StateUnreachable || !strings.Contains(wl.Reason, "connection refused") {
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
