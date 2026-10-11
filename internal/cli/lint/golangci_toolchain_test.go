package lint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bug these pin: `forge lint` ran whatever golangci-lint was on PATH.
// The workspace image's is a release binary built with go1.26.2, and every
// `go 1.27` module (control-plane, reliant) failed the gating lane with
// golangci-lint's own "the Go language version (go1.26) used to build
// golangci-lint is lower than the targeted Go version (1.27)" — no lint ran,
// on any project, and nothing said which binary or how to fix it.

// fakeToolchain is a golangciToolchain whose Go-version answers come from
// maps and whose install writes a placeholder binary, so the decision logic
// runs without real toolchains, network or a minute-long build.
type fakeToolchain struct {
	t          *testing.T
	onPath     string            // "" = not on PATH
	builtWith  map[string]string // binary path → Go release it was built with
	toolchain  string            // what `go env GOVERSION` reports for the module
	installErr error
	installs   []string // "<gobin> <goVersion> <version>" per install call
	notices    []string
	cache      string
}

func newFakeToolchain(t *testing.T) *fakeToolchain {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "golangci-lint")
	if err := os.WriteFile(bin, []byte("placeholder"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeToolchain{
		t:         t,
		onPath:    bin,
		builtWith: map[string]string{bin: "go1.26.2"},
		toolchain: "go1.27.0",
		cache:     t.TempDir(),
	}
}

func (f *fakeToolchain) toolchainFor() golangciToolchain {
	return golangciToolchain{
		lookPath: func(string) (string, error) {
			if f.onPath == "" {
				return "", errors.New("not found")
			}
			return f.onPath, nil
		},
		binaryGoVersion: func(path string) (string, error) {
			if v, ok := f.builtWith[path]; ok {
				return v, nil
			}
			return "", errors.New("not a Go binary")
		},
		moduleGoVersion: func(context.Context, string) (string, error) { return f.toolchain, nil },
		install: func(_ context.Context, gobin, goVersion, version string) error {
			f.installs = append(f.installs, goVersion+" "+version)
			if f.installErr != nil {
				return f.installErr
			}
			// The binary lands in a staging dir and is renamed into
			// place; record the version for its FINAL path.
			if filepath.Dir(gobin) != f.cache {
				f.t.Errorf("install must build into a staging dir under the cache, got %s", gobin)
			}
			if err := os.WriteFile(filepath.Join(gobin, golangciExeName()), []byte("built"), 0o755); err != nil {
				return err
			}
			f.builtWith[filepath.Join(f.cache, version+"-"+goVersion, golangciExeName())] = goVersion
			return nil
		},
		cacheDir: func() (string, error) { return f.cache, nil },
		pin:      "v2.14.0",
		notify:   func(msg string) { f.notices = append(f.notices, msg) },
	}
}

// moduleDir writes a go.mod with the given go directive.
func moduleDir(t *testing.T, goDirective string) string {
	t.Helper()
	dir := t.TempDir()
	body := "module example.com/m\n"
	if goDirective != "" {
		body += "\ngo " + goDirective + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestResolveGolangciLint_OlderThanModuleBuildsWithModuleToolchain is the
// control-plane case: PATH has a go1.26-built golangci-lint, the module is
// go 1.27. forge must lint with a golangci-lint built by the toolchain that
// compiles the module, not hand the module to a binary that refuses it.
func TestResolveGolangciLint_OlderThanModuleBuildsWithModuleToolchain(t *testing.T) {
	f := newFakeToolchain(t)
	dir := moduleDir(t, "1.27")

	res := resolveGolangciLint(context.Background(), dir, f.toolchainFor())
	if res.err != nil || res.missing {
		t.Fatalf("resolution failed: %+v", res)
	}
	want := filepath.Join(f.cache, "v2.14.0-go1.27.0", golangciExeName())
	if res.path != want {
		t.Fatalf("linted with %q, want the forge build %q (the PATH binary %q cannot analyze go 1.27)", res.path, want, f.onPath)
	}
	if len(f.installs) != 1 || f.installs[0] != "go1.27.0 v2.14.0" {
		t.Fatalf("installs = %q, want one build of the pin with the module's toolchain", f.installs)
	}
	if len(f.notices) != 1 || !strings.Contains(f.notices[0], "go1.26.2") || !strings.Contains(f.notices[0], "go 1.27") {
		t.Fatalf("a one-time build must announce why it is happening; notices = %q", f.notices)
	}

	// A second run reuses the build: no second install.
	res2 := resolveGolangciLint(context.Background(), dir, f.toolchainFor())
	if res2.path != want || len(f.installs) != 1 {
		t.Fatalf("second resolution rebuilt or diverged: %+v installs=%q", res2, f.installs)
	}
	// The staging directory is gone; only the final binary remains.
	entries, _ := os.ReadDir(f.cache)
	if len(entries) != 1 || entries[0].Name() != "v2.14.0-go1.27.0" {
		t.Errorf("cache should hold only the finished build, got %v", entries)
	}
}

// TestResolveGolangciLint_CompatibleBinaryOnPathIsUsed keeps the common case
// free: no build, no network.
func TestResolveGolangciLint_CompatibleBinaryOnPathIsUsed(t *testing.T) {
	for _, tc := range []struct{ built, directive string }{
		{"go1.27.0", "1.27"},
		{"go1.27.0", "1.27.3"}, // only the language version matters
		{"go1.28rc1", "1.27"},
		{"go1.26.2", "1.26"},
	} {
		f := newFakeToolchain(t)
		f.builtWith[f.onPath] = tc.built
		res := resolveGolangciLint(context.Background(), moduleDir(t, tc.directive), f.toolchainFor())
		if res.path != f.onPath || res.err != nil || len(f.installs) != 0 {
			t.Errorf("built %s / go %s: got %+v installs=%q, want the PATH binary untouched", tc.built, tc.directive, res, f.installs)
		}
	}
}

// TestResolveGolangciLint_UnfixableMismatchNamesBothVersionsAndTheFix: when
// forge cannot build a compatible golangci-lint, the lane fails with a
// message a person can act on — never golangci-lint's bare exit 3.
func TestResolveGolangciLint_UnfixableMismatchNamesBothVersionsAndTheFix(t *testing.T) {
	f := newFakeToolchain(t)
	f.installErr = errors.New("dial tcp: lookup proxy.golang.org: no such host")

	res := resolveGolangciLint(context.Background(), moduleDir(t, "1.27"), f.toolchainFor())
	if res.err == nil {
		t.Fatalf("an incompatible golangci-lint that could not be replaced must fail the lane, got %+v", res)
	}
	msg := res.err.Error()
	for _, want := range []string{
		f.onPath,           // which binary
		"go1.26.2",         // what it was built with
		"go 1.27",          // what the module needs
		"proxy.golang.org", // why forge could not fix it
		"GOTOOLCHAIN=go1.27.0 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0", // the fix
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not name %q:\n%s", want, msg)
		}
	}
}

// TestResolveGolangciLint_ToolchainOlderThanModule covers GOTOOLCHAIN=local
// with an old `go`: building with it cannot help, so say so.
func TestResolveGolangciLint_ToolchainOlderThanModule(t *testing.T) {
	f := newFakeToolchain(t)
	f.toolchain = "go1.26.2"
	res := resolveGolangciLint(context.Background(), moduleDir(t, "1.27"), f.toolchainFor())
	if res.err == nil || len(f.installs) != 0 {
		t.Fatalf("want a failure without a pointless build, got %+v installs=%q", res, f.installs)
	}
	if !strings.Contains(res.err.Error(), "GOTOOLCHAIN=go1.27.0") {
		t.Errorf("the fix must name a toolchain that can build the module:\n%v", res.err)
	}
}

// TestResolveGolangciLint_NothingToCompare: no golangci-lint skips the lane
// as before; no go directive, or a binary forge cannot read, defers to PATH.
func TestResolveGolangciLint_NothingToCompare(t *testing.T) {
	f := newFakeToolchain(t)
	f.onPath = ""
	if res := resolveGolangciLint(context.Background(), moduleDir(t, "1.27"), f.toolchainFor()); !res.missing {
		t.Errorf("no golangci-lint on PATH must stay a skip, got %+v", res)
	}

	f = newFakeToolchain(t)
	if res := resolveGolangciLint(context.Background(), moduleDir(t, ""), f.toolchainFor()); res.path != f.onPath {
		t.Errorf("no go directive: want the PATH binary, got %+v", res)
	}

	f = newFakeToolchain(t)
	delete(f.builtWith, f.onPath) // a wrapper script, say
	if res := resolveGolangciLint(context.Background(), moduleDir(t, "1.27"), f.toolchainFor()); res.path != f.onPath || len(f.installs) != 0 {
		t.Errorf("unreadable build info: want the PATH binary, got %+v installs=%q", res, f.installs)
	}
}

// TestGolangciLanesReportUnresolvedToolchain wires the resolution into the
// lanes: the gating lane fails with the actionable message (text and JSON),
// the advisory guardrail reports it could not run.
func TestGolangciLanesReportUnresolvedToolchain(t *testing.T) {
	unresolved := errors.New("golangci-lint at /usr/local/bin/golangci-lint was built with go1.26.2, but this module targets go 1.27")
	rc := &lintRunCtx{ctx: context.Background(), paths: []string{"./..."}, golangciMemo: &golangciMemo{}}
	rc.golangciMemo.once.Do(func() { rc.golangciMemo.res = golangciResolution{err: unresolved} })

	gate := findStep(t, "golangci-lint")
	if err := gate.runText(rc); err == nil || !strings.Contains(err.Error(), "go1.26.2") {
		t.Fatalf("text gate: want the actionable error, got %v", err)
	}
	fs, gated, err := gate.collect(rc)
	if err != nil || !gated || len(fs) != 1 || fs[0].Severity != lintSevError || !strings.Contains(fs[0].Message, "go 1.27") {
		t.Fatalf("json gate: want one gating error finding naming the mismatch, got gated=%v err=%v %+v", gated, err, fs)
	}

	guard := findStep(t, "typed-config guardrail")
	var unavail *laneUnavailableError
	if err := guard.runText(rc); !errors.As(err, &unavail) {
		t.Fatalf("advisory guardrail: want a could-not-run error, got %v", err)
	}
}
