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
)

// TestE2EScaffoldBasicProject creates a project with a single service,
// runs generate, and verifies the full toolchain: build, vet, test, lint.
func TestE2EScaffoldBasicProject(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	// Create project
	runCmd(t, dir, forgeBin, "new", "basicapp", "--mod", "example.com/basicapp", "--service", "api")

	projectDir := filepath.Join(dir, "basicapp")
	assertPathExistsE2E(t, filepath.Join(projectDir, "forge.yaml"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "go.mod"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "handlers", "api"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "proto", "services", "api", "v1", "api.proto"))

	// Generate code
	runCmd(t, projectDir, forgeBin, "generate")

	// Verify generated code exists
	assertPathExistsE2E(t, filepath.Join(projectDir, "gen", "services", "api", "v1"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "pkg", "app", "bootstrap.go"))

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
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	// Create project with multiple services and a frontend
	runCmd(t, dir, forgeBin, "new", "multiapp",
		"--mod", "example.com/multiapp",
		"--service", "api,users,orders",
		"--frontend", "web",
	)

	projectDir := filepath.Join(dir, "multiapp")

	// Verify all services exist
	for _, svc := range []string{"api", "users", "orders"} {
		assertPathExistsE2E(t, filepath.Join(projectDir, "handlers", svc, "service.go"))
		assertPathExistsE2E(t, filepath.Join(projectDir, "proto", "services", svc, "v1", svc+".proto"))
	}

	// Verify frontend exists
	assertPathExistsE2E(t, filepath.Join(projectDir, "frontends", "web", "package.json"))

	// Verify CORS middleware is generated (since frontend exists)
	assertPathExistsE2E(t, filepath.Join(projectDir, "pkg", "middleware", "cors.go"))

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
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	// Create initial project
	// The intended behavior is project-may-start-with-or-without-services;
	// use --service for the canonical full scaffold.
	runCmd(t, dir, forgeBin, "new", "addtest", "--mod", "example.com/addtest", "--service", "api")

	projectDir := filepath.Join(dir, "addtest")

	// Add a new service
	runCmd(t, projectDir, forgeBin, "add", "service", "billing")

	// Verify both services exist
	assertPathExistsE2E(t, filepath.Join(projectDir, "handlers", "api", "service.go"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "handlers", "billing", "service.go"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "proto", "services", "billing", "v1", "billing.proto"))

	// Regenerate
	runCmd(t, projectDir, forgeBin, "generate")

	// go mod tidy
	runCmd(t, projectDir, "go", "mod", "tidy")
	runCmd(t, filepath.Join(projectDir, "gen"), "go", "mod", "tidy")

	// Build
	runCmd(t, projectDir, "go", "build", "./...")

	// Verify bootstrap includes both services
	bootstrapContent := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "bootstrap.go"))
	if !strings.Contains(bootstrapContent, "api.New(") {
		t.Fatal("expected bootstrap to include api service")
	}
	if !strings.Contains(bootstrapContent, "billing.New(") {
		t.Fatal("expected bootstrap to include billing service")
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
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

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

// readFileE2E reads a file and fails the test on error.
func readFileE2E(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return string(data)
}
