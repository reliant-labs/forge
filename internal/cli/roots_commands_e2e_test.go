//go:build e2e

package cli

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/pgtest"
)

// TestE2EOneCommandPerRoot pins composition graph PR 4 on a real scaffold: a
// project that declares two roots gets two commands (cmd/<bin>/cmd/roots_gen.go),
// and each runs only the subgraph its root reaches.
//
//   - `server` holds the API listener: it serves the api service and never
//     starts the worker;
//   - `worker` holds the worker alone: it starts the worker, mounts no route,
//     answers /healthz and /readyz on the server address all the same (a
//     probe-only listener), and — reaching no database — never opens one or
//     migrates.
func TestE2EOneCommandPerRoot(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin, "project", "new", "roots", "--mod", "github.com/test/roots", "--service", "api")
	projectDir := filepath.Join(dir, "roots")
	addCorpusForgePkgReplace(t, projectDir)

	// One rpc, so "served" (401 from the auth interceptor) and "not served"
	// (404 from the mux) are distinguishable over HTTP.
	apiProtoPath := filepath.Join(projectDir, "proto", "services", "api", "v1", "api.proto")
	apiProto := readFileE2E(t, apiProtoPath)
	const rpcTODO = "  // TODO: Add your RPC methods here.\n"
	if !strings.Contains(apiProto, rpcTODO) {
		t.Fatalf("scaffolded api.proto no longer carries the RPC TODO anchor:\n%s", apiProto)
	}
	apiProto = strings.Replace(apiProto, rpcTODO, "  rpc Ping(PingRequest) returns (PingResponse) {}\n", 1)
	apiProto += "\nmessage PingRequest {}\n\nmessage PingResponse {\n  string ok = 1;\n}\n"
	if err := os.WriteFile(apiProtoPath, []byte(apiProto), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, projectDir, forgeBin, "scaffold", "worker", "tick")

	rootsGo := `package app

import "github.com/test/roots/internal/workers/tick"

// Server is what ` + "`roots server`" + ` runs: the API.
//
//forge:root server
type Server struct {
	API *APIListener
}

// Worker is what ` + "`roots worker`" + ` runs: the tick worker alone.
//
//forge:root worker
type Worker struct {
	Tick *tick.Worker
}
`
	if err := os.WriteFile(filepath.Join(projectDir, "internal", "app", "roots.go"), []byte(rootsGo), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, projectDir, forgeBin, "generate")

	cmdDir := filepath.Join(projectDir, "cmd", "roots", "cmd")
	assertPathNotExistsE2E(t, filepath.Join(cmdDir, "server.go"))
	gen := readFileE2E(t, filepath.Join(cmdDir, "roots_gen.go"))
	for _, want := range []string{
		`rootSpec{Name: "server", Migrate: true, Run: (*app.Graph).RunServer}`,
		`rootSpec{Name: "worker", Migrate: false, Run: (*app.Graph).RunWorker}`,
	} {
		if !strings.Contains(gen, want) {
			t.Errorf("roots_gen.go lacks %q:\n%s", want, gen)
		}
	}

	bin := filepath.Join(projectDir, "roots-bin")
	runCmd(t, projectDir, "go", "build", "-o", bin, "./cmd/roots")
	help := runCmdStdout(t, projectDir, bin, "--help")
	for _, want := range []string{"server", "worker"} {
		if !strings.Contains(help, "  "+want+" ") {
			t.Errorf("`roots --help` lists no %q command:\n%s", want, help)
		}
	}

	dsn, cleanup, err := pgtest.NewURL()
	if err != nil {
		t.Fatalf("provision boot postgres: %v", err)
	}
	defer cleanup()

	const ping = "/services.api.v1.APIService/Ping"
	serverLog := bootRoot(t, bin, projectDir, dsn, "server", map[string]int{ping: http.StatusUnauthorized})
	if strings.Contains(serverLog, "worker started") {
		t.Errorf("`server` started the worker its root does not reach:\n%s", serverLog)
	}
	workerLog := bootRoot(t, bin, projectDir, dsn, "worker", map[string]int{ping: http.StatusNotFound})
	if !strings.Contains(workerLog, `"worker":"tick"`) || !strings.Contains(workerLog, "worker started") {
		t.Errorf("`worker` never started the tick worker:\n%s", workerLog)
	}
	if strings.Contains(workerLog, "database connected") || strings.Contains(workerLog, "migration") {
		t.Errorf("`worker` reaches no database, yet opened one or migrated:\n%s", workerLog)
	}
}

// bootRoot runs `<bin> <command>`, waits for /readyz, asserts each path's
// status, stops it with SIGTERM and returns its output.
func bootRoot(t *testing.T, bin, projectDir, dsn, command string, want map[string]int) string {
	t.Helper()
	port := freePortE2E(t)
	cmd := exec.Command(bin, command)
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", port),
		"DATABASE_URL="+dsn,
		"ENVIRONMENT=development",
		"PPROF_ADDR=",
		"PRE_STOP_DELAY=-1s",
	)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", command, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if !waitForServing(t, base, 30*time.Second) {
		t.Fatalf("`%s` never became ready\noutput:\n%s", command, out.String())
	}
	for path, code := range want {
		if got := postStatus(t, base+path); got != code {
			t.Errorf("`%s`: %s = %d, want %d\noutput:\n%s", command, path, got, code, out.String())
		}
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatalf("`%s` did not shut down cleanly: %v\noutput:\n%s", command, err, out.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("`%s` did not exit within 30s of SIGTERM\noutput:\n%s", command, out.String())
	}
	return out.String()
}
