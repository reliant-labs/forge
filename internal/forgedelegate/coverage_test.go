package forgedelegate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as the `go` command: fakeGo copies it to
// <dir>/go(.exe) and puts <dir> first on PATH. Invoked under that name,
// TestMain records what it was asked and what environment it was given, then
// plays `go install` (writing $GOBIN/forge) or `go build` (writing the -o
// file). That drives goInstall, goBuild and runCaptured through the real
// os/exec path on every OS, with no network and no toolchain build.
//
//	FAKEGO_LOG   file the call is appended to as a JSON line
//	FAKEGO_FAIL  non-empty: fail with this message on stderr and exit 2
//	FAKEGO_NOOUT non-empty: succeed without writing the binary
func TestMain(m *testing.M) {
	if strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe") == "go" {
		os.Exit(runFakeGo(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type goCall struct {
	Args   []string
	Dir    string
	GOBIN  string
	TMPDIR string
	GOWORK string
	Priv   string
	NoSum  string
}

func runFakeGo(args []string) int {
	cwd, _ := os.Getwd()
	call := goCall{Args: args, Dir: cwd, GOBIN: os.Getenv("GOBIN"), TMPDIR: os.Getenv("TMPDIR"),
		GOWORK: os.Getenv("GOWORK"), Priv: os.Getenv("GOPRIVATE"), NoSum: os.Getenv("GONOSUMDB")}
	if p := os.Getenv("FAKEGO_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			line, _ := json.Marshal(call)
			f.Write(append(line, '\n'))
			f.Close()
		}
	}
	if msg := os.Getenv("FAKEGO_FAIL"); msg != "" {
		os.Stderr.WriteString(msg + "\n")
		return 2
	}
	if os.Getenv("FAKEGO_NOOUT") != "" {
		return 0
	}
	out := ""
	switch args[0] {
	case "install":
		out = filepath.Join(call.GOBIN, exeName())
	case "build":
		for i, a := range args {
			if a == "-o" && i+1 < len(args) {
				out = args[i+1]
			}
		}
	}
	if out != "" {
		if err := os.WriteFile(out, []byte("fake forge\n"), 0o755); err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			return 3
		}
	}
	return 0
}

func fakeGo(t *testing.T) (calls func() []goCall) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	dir := t.TempDir()
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	src, err := os.Open(self)
	if err != nil {
		t.Skipf("cannot read the test binary: %v", err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls.jsonl")
	t.Setenv("FAKEGO_LOG", log)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GONOSUMDB", "")
	return func() []goCall {
		data, err := os.ReadFile(log)
		if err != nil {
			return nil
		}
		var out []goCall
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var c goCall
			if json.Unmarshal([]byte(line), &c) == nil {
				out = append(out, c)
			}
		}
		return out
	}
}

func TestGoInstall_RunsGoInstallIntoGOBINWithPrivateModules(t *testing.T) {
	calls := fakeGo(t)
	gobin := t.TempDir()
	if err := goInstall(context.Background(), cmdPackage+"@"+pin, gobin); err != nil {
		t.Fatalf("goInstall: %v", err)
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("calls = %+v", got)
	}
	c := got[0]
	if strings.Join(c.Args, " ") != "install "+cmdPackage+"@"+pin {
		t.Errorf("args = %v", c.Args)
	}
	if c.GOBIN != gobin {
		t.Errorf("GOBIN = %q, want %q", c.GOBIN, gobin)
	}
	if c.GOWORK != "off" {
		t.Errorf("GOWORK = %q, want off: a go.work above the temp directory must not be consulted", c.GOWORK)
	}
	if c.Priv != "github.com/reliant-labs/*" || c.NoSum != "github.com/reliant-labs/*" {
		t.Errorf("GOPRIVATE/GONOSUMDB = %q/%q: a fresh forge commit is on GitHub before the module proxy", c.Priv, c.NoSum)
	}
	scratch := filepath.Join(gobin, ".work")
	if c.TMPDIR != scratch || evalPath(t, c.Dir) != evalPath(t, scratch) {
		t.Errorf("TMPDIR %q, cwd %q: the build's scratch must live inside the staging directory %q", c.TMPDIR, c.Dir, scratch)
	}
	if _, err := os.Stat(filepath.Join(gobin, exeName())); err != nil {
		t.Errorf("the installed binary is missing: %v", err)
	}
}

func evalPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestGoInstall_KeepsAnExplicitGOPRIVATE(t *testing.T) {
	calls := fakeGo(t)
	t.Setenv("GOPRIVATE", "corp.example/*")
	t.Setenv("GONOSUMDB", "corp.example/*")
	if err := goInstall(context.Background(), "x@y", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if c := calls()[0]; c.Priv != "corp.example/*" || c.NoSum != "corp.example/*" {
		t.Errorf("GOPRIVATE/GONOSUMDB = %q/%q, want the caller's values kept", c.Priv, c.NoSum)
	}
}

func TestGoInstall_FailureCarriesTheTailOfGosOutput(t *testing.T) {
	fakeGo(t)
	t.Setenv("FAKEGO_FAIL", "go: github.com/reliant-labs/forge/cmd/forge@v9: unknown revision v9")
	err := goInstall(context.Background(), "x@y", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unknown revision v9") {
		t.Fatalf("err = %v, want go's own message", err)
	}
}

func TestGoInstall_ScratchCannotBeCreated(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := goInstall(context.Background(), "x@y", blocker); err == nil {
		t.Error("a staging directory that is a file must fail before go runs")
	}
	if err := goBuild(context.Background(), t.TempDir(), filepath.Join(blocker, "forge")); err == nil {
		t.Error("goBuild must fail when its scratch cannot be created")
	}
}

func TestGoBuild_BuildsCmdForgeFromTheCheckout(t *testing.T) {
	calls := fakeGo(t)
	checkout := t.TempDir()
	staging := t.TempDir()
	out := filepath.Join(staging, exeName())
	if err := goBuild(context.Background(), checkout, out); err != nil {
		t.Fatalf("goBuild: %v", err)
	}
	c := calls()[0]
	joined := strings.Join(c.Args, " ")
	for _, want := range []string{
		"build", "-buildvcs=true",
		"-X github.com/reliant-labs/forge/internal/buildinfo.DevForgeRoot=" + checkout,
		"-o " + out, "./cmd/forge",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q lack %q", joined, want)
		}
	}
	if evalPath(t, c.Dir) != evalPath(t, checkout) {
		t.Errorf("go build ran in %q, want the checkout %q", c.Dir, checkout)
	}
	if c.TMPDIR != filepath.Join(staging, ".work") {
		t.Errorf("TMPDIR = %q: scratch must be inside the staging directory", c.TMPDIR)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "fake forge\n" {
		t.Errorf("output = %q, %v", b, err)
	}

	t.Setenv("FAKEGO_FAIL", "cmd/forge/main.go:9: undefined: x")
	if err := goBuild(context.Background(), checkout, filepath.Join(t.TempDir(), "forge")); err == nil || !strings.Contains(err.Error(), "undefined: x") {
		t.Errorf("a failing build = %v, want go's output", err)
	}
}

func TestPinned_DefaultInstallBuildsThroughGo(t *testing.T) {
	calls := fakeGo(t)
	t.Setenv(CacheDirEnv, t.TempDir())
	var log strings.Builder
	r := &Resolver{Log: &log, Slot: func(context.Context, string) (func(), error) { return func() {}, nil }}
	res, err := r.Pinned(context.Background(), t.TempDir(), pin)
	if err != nil {
		t.Fatalf("Pinned: %v", err)
	}
	if res.Kind != KindBuilt || !strings.HasSuffix(res.Source, "(just built)") {
		t.Errorf("result = %+v", res)
	}
	if n := len(calls()); n != 1 {
		t.Errorf("go ran %d times, want once", n)
	}
	if b, err := os.ReadFile(res.Path); err != nil || string(b) != "fake forge\n" {
		t.Errorf("published binary = %q, %v", b, err)
	}
	// A second request is a cache hit and never runs go again.
	if res2, err := r.Pinned(context.Background(), t.TempDir(), pin); err != nil || res2.Kind != KindCache || len(calls()) != 1 {
		t.Errorf("second Pinned = (%+v, %v), calls %d", res2, err, len(calls()))
	}
}

func TestBridge_DefaultBuildAndFailureModes(t *testing.T) {
	calls := fakeGo(t)
	t.Setenv(CacheDirEnv, t.TempDir())
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "cmd", "forge"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Slot: func(context.Context, string) (func(), error) { return func() {}, nil }}

	res, err := r.Bridge(context.Background(), checkout, "head+clean")
	if err != nil {
		t.Fatalf("Bridge: %v", err)
	}
	if res.Kind != KindBuilt || len(calls()) != 1 {
		t.Errorf("result %+v after %d go calls", res, len(calls()))
	}
	if again, err := r.Bridge(context.Background(), checkout, "head+clean"); err != nil || again.Kind != KindCache || again.Path != res.Path {
		t.Errorf("an unchanged checkout must be served from the cache: (%+v, %v)", again, err)
	}

	t.Run("a failing build is named and leaves nothing behind", func(t *testing.T) {
		t.Setenv("FAKEGO_FAIL", "link failed")
		_, err := r.Bridge(context.Background(), checkout, "another-stamp")
		if err == nil || !strings.Contains(err.Error(), "could not build forge from "+checkout) || !strings.Contains(err.Error(), "link failed") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a build that produced no binary is an error", func(t *testing.T) {
		t.Setenv("FAKEGO_NOOUT", "1")
		_, err := r.Bridge(context.Background(), checkout, "empty-stamp")
		if err == nil || !strings.Contains(err.Error(), "the build produced no") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a slot that cannot be taken stops the build", func(t *testing.T) {
		noSlot := &Resolver{Slot: func(context.Context, string) (func(), error) { return nil, errors.New("no slots") }}
		if _, err := noSlot.Bridge(context.Background(), checkout, "slot-stamp"); err == nil || !strings.Contains(err.Error(), "no slots") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestCacheDir(t *testing.T) {
	r := &Resolver{}
	t.Setenv(CacheDirEnv, "  /srv/pinned  ")
	if got, err := r.cacheDir(); err != nil || got != "/srv/pinned" {
		t.Errorf("cacheDir = (%q, %v), want the trimmed override", got, err)
	}
	t.Setenv(CacheDirEnv, "")
	base, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no user cache dir: %v", err)
	}
	if got, err := r.cacheDir(); err != nil || got != filepath.Join(base, "forge", "pinned") {
		t.Errorf("cacheDir = (%q, %v), want forge/pinned under the user cache", got, err)
	}
	if runtime.GOOS == "linux" {
		t.Setenv("XDG_CACHE_HOME", "")
		t.Setenv("HOME", "")
		_, err := r.cacheDir()
		if err == nil || !strings.Contains(err.Error(), CacheDirEnv) {
			t.Errorf("with no cache directory at all: %v, want an error naming %s", err, CacheDirEnv)
		}
		if _, perr := (&Resolver{}).Pinned(context.Background(), t.TempDir(), pin); perr == nil {
			t.Error("Pinned must fail when there is nowhere to keep a build")
		}
	}
}

func TestExeName(t *testing.T) {
	want := "forge"
	if runtime.GOOS == "windows" {
		want = "forge.exe"
	}
	if got := exeName(); got != want {
		t.Errorf("exeName = %q, want %q", got, want)
	}
}

func TestLogw(t *testing.T) {
	var buf strings.Builder
	(&Resolver{Log: &buf}).logw().Write([]byte("hi"))
	if buf.String() != "hi" {
		t.Errorf("logw did not write to Log: %q", buf.String())
	}
	if n, err := (&Resolver{}).logw().Write([]byte("dropped")); n != 7 || err != nil {
		t.Errorf("a nil Log must discard silently, got (%d, %v)", n, err)
	}
}

func TestVersionOf(t *testing.T) {
	ctx := context.Background()
	t.Run("CommandVersion wins and receives the arguments", func(t *testing.T) {
		r := &Resolver{
			CommandVersion: func(_ context.Context, path string, args []string) (string, error) {
				return path + "|" + strings.Join(args, " "), nil
			},
			Version: func(context.Context, string) (string, error) { return "plain", nil },
		}
		if got, err := r.VersionOf(ctx, "/bin/reliant", []string{"forge"}); err != nil || got != "/bin/reliant|forge" {
			t.Errorf("VersionOf = (%q, %v)", got, err)
		}
	})
	t.Run("a bare path falls back to Version", func(t *testing.T) {
		r := &Resolver{Version: func(_ context.Context, path string) (string, error) { return "v1@" + path, nil }}
		if got, err := r.VersionOf(ctx, "/bin/forge", nil); err != nil || got != "v1@/bin/forge" {
			t.Errorf("VersionOf = (%q, %v)", got, err)
		}
	})
	t.Run("arguments with no CommandVersion cannot be probed", func(t *testing.T) {
		r := &Resolver{Version: func(context.Context, string) (string, error) { return "x", nil }}
		_, err := r.VersionOf(ctx, "/bin/reliant", []string{"forge"})
		if err == nil || !strings.Contains(err.Error(), "no version probe for /bin/reliant forge") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("no probe at all", func(t *testing.T) {
		if _, err := (&Resolver{}).VersionOf(ctx, "/bin/forge", nil); err == nil {
			t.Error("a resolver with no probe must say so")
		}
	})
}

func TestTail(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc"},
		{"a\nb", 5, "a\nb"},
		{"only", 1, "only"},
		{"", 3, ""},
		{"a\n\n\n", 1, "a"},
	}
	for _, tt := range tests {
		if got := tail(tt.in, tt.n); got != tt.want {
			t.Errorf("tail(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

func TestBuildSettingsKey(t *testing.T) {
	a := buildSettingsKey([]string{"GOFLAGS=-trimpath", "PATH=/bin", "GOOS=linux"})
	b := buildSettingsKey([]string{"GOFLAGS=", "PATH=/other", "GOOS=linux"})
	if a == b {
		t.Error("GOFLAGS changes the bytes a build produces and must change the key")
	}
	if buildSettingsKey([]string{"GOFLAGS=-trimpath", "PATH=/bin"}) != buildSettingsKey([]string{"PATH=/elsewhere", "GOFLAGS=-trimpath"}) {
		t.Error("variables that do not affect the build must not affect the key")
	}
	if got := buildSettingsKey([]string{"GOARCH=amd64", "GOARCH=arm64"}); !strings.Contains(got, "GOARCH=arm64") || strings.Contains(got, "amd64") {
		t.Errorf("the last assignment wins, as it does for exec: %q", got)
	}
	if got := buildSettingsKey([]string{"not-an-assignment"}); strings.Contains(got, "not-an-assignment") {
		t.Errorf("an entry with no '=' must be ignored: %q", got)
	}
}

func TestPublish(t *testing.T) {
	dir := t.TempDir()
	built := filepath.Join(dir, "built")
	if err := os.WriteFile(built, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := publish(filepath.Join(dir, "absent"), filepath.Join(dir, "to")); err == nil || !strings.Contains(err.Error(), "the build produced no absent") {
		t.Errorf("publishing nothing = %v", err)
	}
	to := filepath.Join(dir, "to")
	if err := publish(built, to); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(to); string(b) != "new" {
		t.Errorf("published = %q", b)
	}
	if err := publish(filepath.Join(dir, "missing-dir", "x"), filepath.Join(dir, "to2")); err == nil {
		t.Error("publishing a missing file must fail")
	}
	// Into a directory that does not exist: neither the link nor the rename can work.
	if err := publish(built, filepath.Join(dir, "no", "such", "dir", "forge")); err == nil || !strings.Contains(err.Error(), "publish ") {
		t.Errorf("publishing into a missing directory = %v", err)
	}
}

func TestSweepStagingRemovesOnlyStagingDirectories(t *testing.T) {
	entry := t.TempDir()
	for _, d := range []string{stagingPrefix + "dead1", stagingPrefix + "dead2", "keep-me"} {
		if err := os.MkdirAll(filepath.Join(entry, d, "inner"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(entry, stagingPrefix+"file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sweepStaging(entry)
	left := map[string]bool{}
	ents, _ := os.ReadDir(entry)
	for _, e := range ents {
		left[e.Name()] = true
	}
	if left[stagingPrefix+"dead1"] || left[stagingPrefix+"dead2"] {
		t.Errorf("a dead build's staging directory survived: %v", left)
	}
	if !left["keep-me"] || !left[stagingPrefix+"file"] {
		t.Errorf("sweep removed more than staging directories: %v", left)
	}
	sweepStaging(filepath.Join(entry, "does-not-exist")) // must not panic
}

func TestPrune_KeepsTheNewestAndTheNamedEntry(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	total := keepVersions + 3
	for i := 0; i < total; i++ {
		p := filepath.Join(dir, "v"+strings.Repeat("x", i+1))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		age := time.Duration(total-i) * time.Hour // later index = newer
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	oldest := filepath.Join(dir, "vx") // the oldest of all, and the one in use
	(&Resolver{}).prune(dir, "vx")
	if _, err := os.Stat(oldest); err != nil {
		t.Errorf("the entry in use was pruned: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	if want := keepVersions + 1; len(ents) != want {
		t.Errorf("%d entries left, want the %d newest others plus the one in use", len(ents), want)
	}
	(&Resolver{}).prune(filepath.Join(dir, "absent"), "x") // must not panic
}

func TestHeavySlot_AcquiresAndReleases(t *testing.T) {
	t.Setenv("FORGE_HEAVY_SLOTS", "off")
	var log strings.Builder
	release, err := heavySlot(&log)(context.Background(), "forge: building")
	if err != nil {
		t.Skipf("no slot available on this host: %v", err)
	}
	release()
}

func TestTouch(t *testing.T) {
	root := t.TempDir()
	entry := filepath.Join(root, "ver", "linux-amd64")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{entry, filepath.Dir(entry)} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	touch(entry)
	for _, p := range []string{entry, filepath.Dir(entry)} {
		st, _ := os.Stat(p)
		if time.Since(st.ModTime()) > time.Hour {
			t.Errorf("%s was not marked used now: %v", p, st.ModTime())
		}
	}
	touch(filepath.Join(root, "absent")) // harmless on failure
}
