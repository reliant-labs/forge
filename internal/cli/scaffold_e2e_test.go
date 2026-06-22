//go:build e2e

package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/pgtest"
)

// TestE2EScaffoldBasicProject creates a project with a single service,
// runs generate, and verifies the full toolchain: build, vet, test, lint.
func TestE2EScaffoldBasicProject(t *testing.T) {
	requirePublishedForgePkg(t)
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()
	linkForgeSibling(t, dir)

	// Create project
	runCmd(t, dir, forgeBin, "new", "basicapp", "--mod", "example.com/basicapp", "--service", "api")

	projectDir := filepath.Join(dir, "basicapp")
	assertPathExistsE2E(t, filepath.Join(projectDir, "forge.yaml"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "go.mod"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "internal", "handlers", "api"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "proto", "services", "api", "v1", "api.proto"))

	// Generate code
	runCmd(t, projectDir, forgeBin, "generate")

	// Verify generated code exists
	assertPathExistsE2E(t, filepath.Join(projectDir, "gen", "services", "api", "v1"))
	// §2 hybrid DI: the live composition layer is internal/app; the old
	// name-matched pkg/app DI unit (bootstrap.go) is retired.
	assertPathExistsE2E(t, filepath.Join(projectDir, "internal", "app", "inject_gen.go"))
	assertPathNotExistsE2E(t, filepath.Join(projectDir, "pkg", "app", "bootstrap.go"))

	// go mod tidy (may be needed after generate)
	runCmd(t, projectDir, "go", "mod", "tidy")
	runCmd(t, filepath.Join(projectDir, "gen"), "go", "mod", "tidy")

	// Build
	runCmd(t, projectDir, "go", "build", "./...")

	// Vet
	runCmd(t, projectDir, "go", "vet", "./...")

	// Test
	runCmd(t, projectDir, "go", "test", "./...")

	// golangci-lint (if available)
	if toolAvailable("golangci-lint") {
		runCmd(t, projectDir, "golangci-lint", "run", "./...")
	} else {
		t.Log("golangci-lint not available, skipping lint check")
	}

	// buf lint (if available)
	if toolAvailable("buf") {
		runCmd(t, projectDir, "buf", "lint")
	} else {
		t.Log("buf not available, skipping proto lint check")
	}
}

// TestE2EScaffoldMultiServiceProject creates a project with multiple services
// and a frontend, then verifies everything builds.
func TestE2EScaffoldMultiServiceProject(t *testing.T) {
	requirePublishedForgePkg(t)
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()
	linkForgeSibling(t, dir)

	// Create project with multiple services and a frontend
	runCmd(t, dir, forgeBin, "new", "multiapp",
		"--mod", "example.com/multiapp",
		"--service", "api,users,orders",
		"--frontend", "web",
	)

	projectDir := filepath.Join(dir, "multiapp")

	// Verify all services exist
	for _, svc := range []string{"api", "users", "orders"} {
		assertPathExistsE2E(t, filepath.Join(projectDir, "internal", "handlers", svc, "service.go"))
		assertPathExistsE2E(t, filepath.Join(projectDir, "proto", "services", svc, "v1", svc+".proto"))
	}

	// Verify frontend exists
	assertPathExistsE2E(t, filepath.Join(projectDir, "frontends", "web", "package.json"))

	// Verify the thin auth-policy middleware file is generated (CORS and
	// the other mechanisms come from forge/pkg/middleware, wired in
	// cmd/server.go).
	assertPathExistsE2E(t, filepath.Join(projectDir, "pkg", "middleware", "middleware.go"))

	// Generate code
	runCmd(t, projectDir, forgeBin, "generate")

	// go mod tidy
	runCmd(t, projectDir, "go", "mod", "tidy")
	runCmd(t, filepath.Join(projectDir, "gen"), "go", "mod", "tidy")

	// Build
	runCmd(t, projectDir, "go", "build", "./...")

	// Vet
	runCmd(t, projectDir, "go", "vet", "./...")

	// Test
	runCmd(t, projectDir, "go", "test", "./...")

	// golangci-lint
	if toolAvailable("golangci-lint") {
		runCmd(t, projectDir, "golangci-lint", "run", "./...")
	}
}

// TestE2EScaffoldWithEntityProto and TestE2EScaffoldLifecycle (scaffold_lifecycle_e2e_test.go)
// were deleted with the entity-proto subsystem: entity annotations are ignored now and the
// schema-truth lifecycle gate in fixture_corpus_e2e_test.go supersedes them.

// TestE2EScaffoldAddService creates a project, then adds a service using
// `forge add service`, regenerates, and verifies the build.
func TestE2EScaffoldAddService(t *testing.T) {
	requirePublishedForgePkg(t)
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()
	linkForgeSibling(t, dir)

	// Create initial project
	// The intended behavior is project-may-start-with-or-without-services;
	// use --service for the canonical full scaffold.
	runCmd(t, dir, forgeBin, "new", "addtest", "--mod", "example.com/addtest", "--service", "api")

	projectDir := filepath.Join(dir, "addtest")

	// Add a new service
	runCmd(t, projectDir, forgeBin, "add", "service", "billing")

	// Verify both services exist
	assertPathExistsE2E(t, filepath.Join(projectDir, "internal", "handlers", "api", "service.go"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "internal", "handlers", "billing", "service.go"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "proto", "services", "billing", "v1", "billing.proto"))

	// Regenerate
	runCmd(t, projectDir, forgeBin, "generate")

	// go mod tidy
	runCmd(t, projectDir, "go", "mod", "tidy")
	runCmd(t, filepath.Join(projectDir, "gen"), "go", "mod", "tidy")

	// Build
	runCmd(t, projectDir, "go", "build", "./...")

	// Verify both services are constructed by the generated §2 injector
	// (internal/app/inject_gen.go — by-type DI replacing the retired
	// name-matched wire_gen/services_gen path).
	injectContent := readFileE2E(t, filepath.Join(projectDir, "internal", "app", "inject_gen.go"))
	if !strings.Contains(injectContent, "api.New(") {
		t.Fatal("expected inject_gen.go to construct the api service")
	}
	if !strings.Contains(injectContent, "billing.New(") {
		t.Fatal("expected inject_gen.go to construct the billing service")
	}
}

// TestE2EScaffoldVersion verifies the version subcommand works.
func TestE2EScaffoldVersion(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)

	output := runCmdOutput(t, t.TempDir(), forgeBin, "version")
	if !strings.Contains(strings.ToLower(output), "forge version") && !strings.Contains(output, "version") {
		t.Fatalf("expected version output, got: %s", output)
	}
}

// TestE2EScaffoldServerStartup creates a project and verifies the server
// can start and respond to health checks.
func TestE2EScaffoldServerStartup(t *testing.T) {
	requirePublishedForgePkg(t)
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()
	linkForgeSibling(t, dir)

	// Create project
	runCmd(t, dir, forgeBin, "new", "srvtest", "--mod", "example.com/srvtest", "--service", "api")

	projectDir := filepath.Join(dir, "srvtest")

	// Generate code
	runCmd(t, projectDir, forgeBin, "generate")

	// go mod tidy
	runCmd(t, projectDir, "go", "mod", "tidy")
	runCmd(t, filepath.Join(projectDir, "gen"), "go", "mod", "tidy")

	// Build the server binary
	serverBin := filepath.Join(projectDir, "server")
	runCmd(t, projectDir, "go", "build", "-o", serverBin, "./cmd/...")

	// Start the server with a free port (parallel e2e tests must never
	// share a hard-coded port).
	port := freePortE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, serverBin, "server")
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", port),
		"DATABASE_URL=", // No DB needed for health check
		// The scaffold defaults ENVIRONMENT=production, where a server
		// with no auth provider REFUSES to start (the H2 refusal
		// contract, tested in forge/pkg/authn). This test is about the
		// serve lifecycle (healthz/readyz). Auth bypass is now EXPLICIT —
		// dev mode alone keeps auth on — so opt in with AUTH_DEV_MODE=true
		// to boot the providerless scaffold.
		"ENVIRONMENT=development",
		"AUTH_DEV_MODE=true",
	)

	// Capture output for debugging
	var serverOutput strings.Builder
	cmd.Stdout = &serverOutput
	cmd.Stderr = &serverOutput

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Wait for server to be ready
	addr := fmt.Sprintf("http://127.0.0.1:%d", port)
	ready := waitForServer(t, addr+"/healthz", 10*time.Second)
	if !ready {
		t.Fatalf("server did not become ready within timeout\nserver output:\n%s", serverOutput.String())
	}

	// Verify health endpoint
	resp, err := http.Get(addr + "/healthz")
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /healthz, got %d", resp.StatusCode)
	}

	// Verify readiness endpoint
	resp, err = http.Get(addr + "/readyz")
	if err != nil {
		t.Fatalf("readiness check failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /readyz, got %d", resp.StatusCode)
	}
}

// ── Published-module guard ──────────────────────────────────────────────────
//
// The scaffold e2e tests below exercise the PUBLISHED-module path: a
// scaffolded project resolving github.com/reliant-labs/forge/pkg from the
// module proxy with no local replace (exactly what a real user's first
// build does). When the published snapshot predates the packages current
// templates import (appkit/serverkit — true until the pkg/vX.Y.Z release
// tag is pushed; see the release flow in scripts/release-pkg.sh), these
// tests cannot pass for environmental reasons. They SKIP with that reason
// rather than fail, so the plain command stays honest:
//
//	go test -tags e2e ./internal/cli/
//
// runs everything runnable with zero -run incantations, and the skips
// disappear by themselves the day the tag is published. The local-replace
// fixtures (fixture_corpus, lifecycle) are unaffected — they pin current-
// tree behavior and always run.
var (
	publishedPkgOnce sync.Once
	publishedPkgErr  error
)

func requirePublishedForgePkg(t *testing.T) {
	t.Helper()
	publishedPkgOnce.Do(func() {
		dir, err := os.MkdirTemp("", "forge-pkg-probe-")
		if err != nil {
			publishedPkgErr = err
			return
		}
		defer os.RemoveAll(dir)
		init := exec.Command("go", "mod", "init", "probe.local/probe")
		init.Dir = dir
		if out, err := init.CombinedOutput(); err != nil {
			publishedPkgErr = fmt.Errorf("probe init: %v\n%s", err, out)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		get := exec.CommandContext(ctx, "go", "get", "github.com/reliant-labs/forge/pkg/appkit@latest")
		get.Dir = dir
		if out, err := get.CombinedOutput(); err != nil {
			publishedPkgErr = fmt.Errorf("published forge/pkg lacks appkit/serverkit (push the pkg release tag — see scripts/release-pkg.sh): %v\n%s", err, out)
		}
	})
	if publishedPkgErr != nil {
		t.Skipf("published-module path unavailable: %v", publishedPkgErr)
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// forgeBinaryOnce builds the forge binary exactly once per `go test`
// process and shares the path across every e2e test. Previously each of
// the ~8 e2e tests rebuilt it into its own t.TempDir() — ~15s × 8 of
// pure duplicated compilation per run, the single biggest e2e cost.
//
// The binary is built into a process-scoped temp dir (NOT t.TempDir(),
// which is cleaned when the first test that triggered the build ends —
// that would yank the binary out from under later tests). The OS reaps
// /tmp; we don't bother removing it. sync.Once makes concurrent callers
// (t.Parallel e2e tests) safe.
var (
	forgeBinaryOnce sync.Once
	forgeBinaryPath string
	forgeBinaryErr  error
)

// sharedTestPostgres boots ONE embedded postgres for the whole `go test`
// process and publishes its DSN via FORGE_TEST_POSTGRES_URL, so every
// forge subprocess (schema introspection) and every in-process testkit DB
// connect to the SAME server and only create cheap per-call databases.
//
// Without this, each `forge generate` subprocess would boot its OWN
// embedded postgres; the parallel corpus then spins up dozens at once and
// exhausts the kernel's shared-memory limits. One shared server is both
// far faster and the only way the parallel corpus stays within those
// limits.
//
// os.Setenv (process-global, NOT t.Setenv) is used deliberately: the
// value must reach exec'd subprocesses through os.Environ(), and it is a
// shared read-only resource URL, so it is safe to set once across the
// parallel corpus (unlike the t.Setenv/t.Chdir combo the suite forbids).
// pgtest itself honors FORGE_TEST_POSTGRES_URL, so the first call boots
// the server and later calls (including the subprocesses) reuse it.
func sharedTestPostgres(t *testing.T) {
	t.Helper()
	sharedPGOnce.Do(func() {
		if os.Getenv(pgtest.EnvBaseURL) != "" {
			return // an external server was provided; honor it.
		}
		dsn, _, err := pgtest.NewURL()
		if err != nil {
			sharedPGErr = err
			return
		}
		// Point at the maintenance database so subprocesses can CREATE
		// DATABASE off it; pgtest.New/NewURL derive per-call databases.
		base := dsn
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			if q := strings.IndexByte(base[i:], '?'); q >= 0 {
				base = base[:i+1] + "postgres" + base[i+q:]
			} else {
				base = base[:i+1] + "postgres"
			}
		}
		sharedPGErr = os.Setenv(pgtest.EnvBaseURL, base)
	})
	if sharedPGErr != nil {
		t.Fatalf("provision shared test postgres: %v", sharedPGErr)
	}
}

var (
	sharedPGOnce sync.Once
	sharedPGErr  error
)

func buildforgeBinary(t *testing.T) string {
	t.Helper()
	forgeBinaryOnce.Do(func() {
		repoRoot := findRepoRoot(t)
		dir, err := os.MkdirTemp("", "forge-e2e-bin-")
		if err != nil {
			forgeBinaryErr = err
			return
		}
		bin := filepath.Join(dir, "forge")
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/forge")
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, berr := cmd.CombinedOutput(); berr != nil {
			forgeBinaryErr = fmt.Errorf("failed to build forge binary: %w\n%s", berr, output)
			return
		}
		forgeBinaryPath = bin
	})
	if forgeBinaryErr != nil {
		t.Fatalf("%v", forgeBinaryErr)
	}
	// Every fixture goes through buildforgeBinary; provisioning the shared
	// postgres here means every forge subprocess inherits
	// FORGE_TEST_POSTGRES_URL and connects to the one shared server
	// instead of booting its own. (No-DB fixtures pay one process-wide pg
	// boot; that is cheaper than the parallel corpus booting dozens.)
	sharedTestPostgres(t)
	return forgeBinaryPath
}

// findRepoRoot walks up from the working directory to find the forge repo root.
func findRepoRoot(t *testing.T) string {
	t.Helper()

	// The test runs from internal/cli/ — walk up to repo root
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	dir := cwd
	for {
		goMod := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(goMod); err == nil {
			if strings.Contains(string(data), "module github.com/reliant-labs/forge") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find forge repo root from %s", cwd)
		}
		dir = parent
	}
}

// freePortE2E asks the kernel for an ephemeral port and returns it.
// e2e tests that boot servers run in parallel, so a hard-coded port is
// a collision waiting to happen. There is an inherent TOCTOU window
// between closing the probe listener and the server binding the port,
// but ephemeral allocation makes two parallel tests racing for the
// SAME port vanishingly unlikely (vs. guaranteed with a constant).
func freePortE2E(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// addforgeReplace adds a replace directive for github.com/reliant-labs/forge
// in the gen/go.mod so that generated code resolves the ORM package from the
// local repo checkout (the ORM now lives in-repo under pkg/).
func addforgeReplace(t *testing.T, genDir string) {
	t.Helper()

	repoRoot := findRepoRoot(t)

	goModPath := filepath.Join(genDir, "go.mod")
	data, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read gen/go.mod: %v", err)
	}

	content := string(data)
	if strings.Contains(content, "github.com/reliant-labs/forge") &&
		!strings.Contains(content, "replace github.com/reliant-labs/forge") {
		// Add replace directive pointing at the repo root
		content += fmt.Sprintf("\nreplace github.com/reliant-labs/forge => %s\n", repoRoot)
		if err := os.WriteFile(goModPath, []byte(content), 0o644); err != nil {
			t.Fatalf("write gen/go.mod: %v", err)
		}
	}
}

// runCmd runs a command and fails the test on error.
func runCmd(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GOFLAGS=",       // Clear any global GOFLAGS
		"GONOSUMCHECK=*", // Don't check sums for test modules
		"GOPROXY=https://proxy.golang.org,direct", // Ensure module proxy is set
		"GONOSUMDB=*", // Don't verify sums for test modules
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command %q in %s failed: %v\n%s", append([]string{name}, args...), dir, err, output)
	}
}

// runCmdOutput runs a command and returns its combined output.
func runCmdOutput(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command %q failed: %v\n%s", append([]string{name}, args...), err, output)
	}
	return string(output)
}

// toolAvailable checks if a tool is on PATH.
func toolAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// waitForServer polls a URL until it gets a 200 or the timeout expires.
func waitForServer(t *testing.T, url string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 1 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// assertPathExistsE2E fails the test if the path does not exist.
func assertPathExistsE2E(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected path %s to exist: %v", path, err)
	}
}

// assertPathNotExistsE2E fails the test if the path exists.
func assertPathNotExistsE2E(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected path %s to NOT exist", path)
	}
}

// readFileE2E reads a file and fails the test on error.
func readFileE2E(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return string(data)
}

// linkForgeSibling symlinks the forge repo checkout next to the
// scaffold parent dir so `forge new` detects the sibling forge/pkg and
// writes the dev replace — the documented "forge alongside project"
// dev layout; `forge generate` then vendors it into ./.forge-pkg.
// Without it, scaffolds in t.TempDir() resolve forge/pkg from the
// published proxy snapshot, which lags the in-repo packages the
// current scaffold references (e.g. pkg/authn, pkg/appkit), and
// generate's root `go mod tidy` step fails.
func linkForgeSibling(t *testing.T, parentDir string) {
	t.Helper()
	link := filepath.Join(parentDir, "forge")
	if _, err := os.Lstat(link); err == nil {
		return
	}
	if err := os.Symlink(findRepoRoot(t), link); err != nil {
		t.Fatalf("symlink forge sibling checkout: %v", err)
	}
}
