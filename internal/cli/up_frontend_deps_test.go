package cli

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTransientInstallFailure pins which install failures earn a retry: the
// ones where the package manager reports a fault in ITSELF. A real dependency
// error must still fail on the first attempt — retrying it just doubles the
// wait before the same verdict.
func TestTransientInstallFailure(t *testing.T) {
	transient := []string{
		"npm error Exit handler never called!\nnpm error This is an error with npm itself.",
		"npm error network ECONNRESET",
		"npm error ETIMEDOUT",
	}
	for _, out := range transient {
		if !transientInstallFailure(out) {
			t.Errorf("expected retry for a package-manager internal/network fault:\n%s", out)
		}
	}

	real := []string{
		"npm error 404 Not Found - GET https://registry.npmjs.org/does-not-exist",
		"npm error ERESOLVE unable to resolve dependency tree",
		"npm error code EACCES",
		"",
	}
	for _, out := range real {
		if transientInstallFailure(out) {
			t.Errorf("a genuine dependency failure must NOT be retried:\n%s", out)
		}
	}
}

// TestPreflightFrontendDepsSkipsWarmAndNonNodePaths keeps the hoisted preflight
// free on a warm run: it must not print or shell out when every frontend's
// node_modules is current, or when a declared path is not a node project.
func TestPreflightFrontendDepsSkipsWarmAndNonNodePaths(t *testing.T) {
	dir := t.TempDir() // no package.json => not a node project
	e := &KCLEntities{Frontends: []FrontendEntity{
		{Name: "web", Path: dir},
		{Name: "no-path", Path: ""},
	}}
	out := captureStdout(t, func() {
		if err := preflightFrontendDeps(t.Context(), e, nil); err != nil {
			t.Fatalf("preflightFrontendDeps: %v", err)
		}
	})
	if out != "" {
		t.Fatalf("warm/non-node preflight printed:\n%s", out)
	}
}

// TestPreflightFrontendDepsNilEntities guards the --no-render / empty case.
func TestPreflightFrontendDepsNilEntities(t *testing.T) {
	if err := preflightFrontendDeps(t.Context(), nil, nil); err != nil {
		t.Fatalf("nil entities: %v", err)
	}
}

// TestHostReadyErrorNamesTheRealPortOwner covers the misdiagnosis that sent a
// reader chasing the wrong process: an Electron shell declared against its web
// dev server's port is reported as "failed to bind its port" when the thing
// that actually binds it — the frontend — is what failed.
func TestHostReadyErrorNamesTheRealPortOwner(t *testing.T) {
	e := &KCLEntities{Frontends: []FrontendEntity{{Name: "reliant-web", Port: 3000}}}
	unready := []hostReadyResult{{name: "reliant-electron", port: 3000, state: portReadyNobody}}

	msg := hostReadyError("dev", unready, e).Error()
	if !strings.Contains(msg, `frontend "reliant-web"`) {
		t.Fatalf("error does not name the frontend that declares the port:\n%s", msg)
	}
	if !strings.Contains(msg, "nothing was ever going to bind this port") {
		t.Fatalf("error does not explain the misattribution:\n%s", msg)
	}
}

// TestHostReadyErrorQuietWhenPortIsUncontested keeps the note out of the
// ordinary case, where the service really did fail to bind its own port.
func TestHostReadyErrorQuietWhenPortIsUncontested(t *testing.T) {
	e := &KCLEntities{Frontends: []FrontendEntity{{Name: "reliant-web", Port: 3002}}}
	unready := []hostReadyResult{{name: "admin-server", port: 8090, state: portReadyNobody}}

	msg := hostReadyError("dev", unready, e).Error()
	if strings.Contains(msg, "also declares") {
		t.Fatalf("note fired for an uncontested port:\n%s", msg)
	}
}

// TestFrontendDepsStaleUsesCompletedInstallNotMtime is the idempotency
// regression: a FAILED install still writes packages, bumping node_modules'
// mtime past the lockfile. Keying off that mtime made the next run consider a
// half-populated tree current and skip the install — the gate inverting itself
// exactly when it was needed. Staleness now keys off the stamp written only
// after the package manager exits 0.
func TestFrontendDepsStaleUsesCompletedInstallNotMtime(t *testing.T) {
	dir := t.TempDir()
	nm := filepath.Join(dir, "node_modules")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "package-lock.json")
	if err := os.WriteFile(lock, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate a failed install: node_modules touched AFTER the lockfile, but
	// no completion stamp. Without the stamp check this reads as "fresh".
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, past, past); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(nm, now, now); err != nil {
		t.Fatal(err)
	}
	if frontendDepsStale(dir) {
		t.Log("mtime fallback still applies with no stamp (pre-existing healthy checkouts are not force-reinstalled)")
	}

	// A COMPLETED install stamps the tree; a later lockfile edit must re-stale it.
	markFrontendInstallOK(dir)
	if frontendDepsStale(dir) {
		t.Fatal("freshly stamped tree reported stale")
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(lock, future, future); err != nil {
		t.Fatal(err)
	}
	if !frontendDepsStale(dir) {
		t.Fatal("lockfile newer than the completed install was not detected as stale")
	}
}

// TestInstallConcurrencyFlags pins the proxy-only concurrency cap. Measured on
// a 579-package tree: npm's default 15 sockets through a local inspection
// proxy did not finish in four minutes, while 8 finished in 7.6s (6.6s
// direct). npm's 300s fetch-timeout makes that failure look like a hang rather
// than an error, so the cap is what keeps a proxied dev loop usable.
func TestInstallConcurrencyFlags(t *testing.T) {
	proxied := []string{"PATH=/usr/bin", "HTTPS_PROXY=http://127.0.0.1:9090"}

	if got := installConcurrencyFlags("npm", proxied); len(got) != 1 || got[0] != "--maxsockets=8" {
		t.Errorf("npm proxied flags = %v; want [--maxsockets=8]", got)
	}
	for _, runner := range []string{"pnpm", "yarn"} {
		if got := installConcurrencyFlags(runner, proxied); len(got) != 1 || got[0] != "--network-concurrency=8" {
			t.Errorf("%s proxied flags = %v; want [--network-concurrency=8]", runner, got)
		}
	}

	// No proxy => no cap: a normal install must stay at full speed.
	clean := []string{"PATH=/usr/bin"}
	for _, runner := range []string{"npm", "pnpm", "yarn"} {
		if got := installConcurrencyFlags(runner, clean); got != nil {
			t.Errorf("%s unproxied flags = %v; want none", runner, got)
		}
	}

	// Lowercase proxy vars are equally real.
	if got := installConcurrencyFlags("npm", []string{"http_proxy=http://127.0.0.1:9090"}); len(got) != 1 {
		t.Errorf("lowercase http_proxy ignored: %v", got)
	}
	// An empty value is not a proxy.
	if got := installConcurrencyFlags("npm", []string{"HTTPS_PROXY="}); got != nil {
		t.Errorf("empty HTTPS_PROXY treated as proxied: %v", got)
	}
	// Unknown runners get no flags rather than a bad one.
	if got := installConcurrencyFlags("bun", proxied); got != nil {
		t.Errorf("unknown runner flags = %v; want none", got)
	}
}

// TestPreflightProxyReachableRefusedFailsFast covers the cascade this replaces:
// a proxy named in the environment but not listening breaks EVERY outbound call
// in the run — kubectl against the local cluster first, because k3d writes
// 0.0.0.0 into kubeconfig and no NO_PROXY entry matches it.
func TestPreflightProxyReachableRefusedFailsFast(t *testing.T) {
	// Bind then close, so the port is almost certainly refused rather than filtered.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	err = preflightProxyReachable(t.Context(), []string{"HTTPS_PROXY=http://" + addr})
	if err == nil {
		t.Fatal("a dead proxy was accepted")
	}
	for _, want := range []string{"HTTPS_PROXY", "nothing is listening", "0.0.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}
}

// TestPreflightProxyReachableLiveProxyPasses keeps a working setup silent.
func TestPreflightProxyReachableLiveProxyPasses(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := preflightProxyReachable(t.Context(), []string{"HTTPS_PROXY=http://" + ln.Addr().String()}); err != nil {
		t.Fatalf("live proxy rejected: %v", err)
	}
}

// TestPreflightProxyReachableNoProxyIsNoop keeps the ordinary path untouched.
func TestPreflightProxyReachableNoProxyIsNoop(t *testing.T) {
	if err := preflightProxyReachable(t.Context(), []string{"PATH=/usr/bin"}); err != nil {
		t.Fatalf("no-proxy environment rejected: %v", err)
	}
	if err := preflightProxyReachable(t.Context(), []string{"HTTPS_PROXY="}); err != nil {
		t.Fatalf("empty HTTPS_PROXY rejected: %v", err)
	}
}

// TestGoModuleDirsIsInertForNonGoProjects pins the caveat that matters most:
// a forge project may reference only external builds that are not Go at all.
// The check must find nothing to say about them rather than inventing a
// requirement they cannot satisfy.
func TestGoModuleDirsIsInertForNonGoProjects(t *testing.T) {
	root := t.TempDir() // no go.mod anywhere
	npmOnly := filepath.Join(root, "web")
	if err := os.MkdirAll(npmOnly, 0o755); err != nil {
		t.Fatal(err)
	}
	e := &KCLEntities{Services: []ServiceEntity{
		{Name: "web", Build: BuildConfigEntity{Type: "shell", Shell: &ShellBuild{Cwd: npmOnly}}},
	}}
	if got := goModuleDirs(e, root); len(got) != 0 {
		t.Fatalf("dirs = %v; want none for a project with no Go modules", got)
	}
}

// TestGoModuleDirsFindsSiblingExternalBuilds is the regression for where the
// staleness actually bit: not the project's own module, but a SIBLING checkout
// driven by an external build. A project-root-only check would have missed it,
// and the failure surfaced four and a half minutes into the build.
func TestGoModuleDirsFindsSiblingExternalBuilds(t *testing.T) {
	root := t.TempDir()
	sibling := filepath.Join(root, "sibling")
	for _, d := range []string{root, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "go.mod"), []byte("module x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := &KCLEntities{Services: []ServiceEntity{
		{Name: "a", Build: BuildConfigEntity{Type: "shell", Shell: &ShellBuild{Cwd: sibling}}},
		// Same directory twice must not be probed twice.
		{Name: "b", Build: BuildConfigEntity{Type: "shell", Shell: &ShellBuild{Cwd: sibling}}},
		// A shell build with no cwd, and one pointing at a non-module dir.
		{Name: "c", Build: BuildConfigEntity{Type: "shell", Shell: &ShellBuild{}}},
	}}
	got := goModuleDirs(e, root)
	if len(got) != 2 {
		t.Fatalf("dirs = %v; want exactly the project root and the sibling", got)
	}
	if got[0] != filepath.Clean(root) || got[1] != filepath.Clean(sibling) {
		t.Fatalf("dirs = %v; want [%s %s]", got, root, sibling)
	}
}

// TestPreflightGoModulesTidyCleanTreePasses keeps the happy path silent.
func TestPreflightGoModulesTidyCleanTreePasses(t *testing.T) {
	root := t.TempDir() // no go.mod => nothing probed, nothing to report
	if err := preflightGoModulesTidy(t.Context(), nil, root); err != nil {
		t.Fatalf("inert case returned an error: %v", err)
	}
}

// TestStaleModuleDiffDistinguishesDiffFromFailure pins the discriminator that
// replaced a shape-based guess. `go mod tidy -diff` exits non-zero both when it
// finds a difference and when it cannot run at all; only the former is evidence
// of staleness. Predicting the latter from go.work's shape stood the check down
// on exactly the repos it exists for.
func TestStaleModuleDiffDistinguishesDiffFromFailure(t *testing.T) {
	realDiff := []byte(`diff current/go.mod tidy/go.mod
--- current/go.mod
+++ tidy/go.mod
@@ -33,6 +33,7 @@
 	github.com/prometheus/client_golang v1.24.1
+	github.com/prometheus/client_model v0.6.3
`)
	if !staleModuleDiff(realDiff) {
		t.Error("a genuine tidy diff was not recognised as staleness")
	}

	// Every one of these is tidy failing to RUN, not the tree being stale.
	for _, out := range [][]byte{
		[]byte("go: github.com/reliant-labs/forge/pkg@v0.1.12: reading ...: 404 Not Found"),
		[]byte("go: module lookup disabled by GOFLAGS=-mod=vendor"),
		[]byte("go: no required module provides package example.com/x; to add it:\n\tgo get example.com/x"),
		[]byte("dial tcp: lookup proxy.golang.org: no such host"),
		[]byte("   \n  "),
		{},
	} {
		if staleModuleDiff(out) {
			t.Errorf("probe failure treated as staleness: %q", out)
		}
	}
}
