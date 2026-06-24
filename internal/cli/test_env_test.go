package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// fakeForwarder records the forwards it was asked to start and reports each
// stop. It never touches kubectl/a cluster.
type fakeForwarder struct {
	mu       sync.Mutex
	started  []config.TestForward
	stopped  []string // service names, in stop order
	startErr error
}

func (f *fakeForwarder) start(ctx context.Context, fwd config.TestForward, stderr io.Writer) (func(), error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.mu.Lock()
	f.started = append(f.started, fwd)
	f.mu.Unlock()
	svc := fwd.Service
	return func() {
		f.mu.Lock()
		f.stopped = append(f.stopped, svc)
		f.mu.Unlock()
	}, nil
}

// fakeRunner records the argv + env it was handed and returns a canned result.
type fakeRunner struct {
	gotArgv []string
	gotEnv  []string
	ran     bool
	err     error
}

func (r *fakeRunner) run(ctx context.Context, argv []string, env []string, stdout, stderr io.Writer) error {
	r.ran = true
	r.gotArgv = argv
	r.gotEnv = env
	return r.err
}

func newFakeDeps(fwd *fakeForwarder, run *fakeRunner) testEnvDeps {
	return testEnvDeps{
		forwarder:   fwd,
		runner:      run,
		waitForPort: func(ctx context.Context, port int) error { return nil },
		stdout:      io.Discard,
		stderr:      io.Discard,
	}
}

func sampleConfig() config.TestConfig {
	return config.TestConfig{
		"e2e": {
			Command: []string{"go", "test", "-tags=e2e", "./e2e/..."},
			Env:     map[string]string{"DAEMON_KUBE_CONTEXT": "k3d-cp-daemon"},
			Forwards: []config.TestForward{
				{
					Service: "admin-server", Context: "k3d-control-plane",
					Namespace: "control-plane-e2e", RemotePort: 8090, LocalPort: 8090,
					URLEnv: "E2E_ADMIN_SERVER_URL",
				},
				{
					Service: "workspace-proxy", Context: "k3d-cp-daemon",
					Namespace: "control-plane-e2e", RemotePort: 8080, LocalPort: 8088,
					URLEnv: "PROXY_URL",
				},
			},
		},
	}
}

func TestRunTestEnv_HappyPath_ForwardsRunTeardown(t *testing.T) {
	fwd := &fakeForwarder{}
	run := &fakeRunner{}
	err := runTestEnvWithConfig(context.Background(), "e2e", sampleConfig(), newFakeDeps(fwd, run))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both forwards were started.
	if len(fwd.started) != 2 {
		t.Fatalf("expected 2 forwards started, got %d", len(fwd.started))
	}
	// The command ran with the declared argv.
	if !run.ran {
		t.Fatal("test command never ran")
	}
	if strings.Join(run.gotArgv, " ") != "go test -tags=e2e ./e2e/..." {
		t.Fatalf("wrong argv: %v", run.gotArgv)
	}
	// Forward URL env vars + literal env map are exported.
	wantEnv := map[string]string{
		"E2E_ADMIN_SERVER_URL": "http://127.0.0.1:8090",
		"PROXY_URL":            "http://127.0.0.1:8088",
		"DAEMON_KUBE_CONTEXT":  "k3d-cp-daemon",
	}
	for k, v := range wantEnv {
		if !envHas(run.gotEnv, k, v) {
			t.Fatalf("expected env %s=%s in command env", k, v)
		}
	}
	// Teardown ran for every forward, LIFO.
	if strings.Join(fwd.stopped, ",") != "workspace-proxy,admin-server" {
		t.Fatalf("expected LIFO teardown [workspace-proxy,admin-server], got %v", fwd.stopped)
	}
}

func TestRunTestEnv_TeardownOnCommandFailure(t *testing.T) {
	fwd := &fakeForwarder{}
	run := &fakeRunner{err: fmt.Errorf("boom")}
	err := runTestEnvWithConfig(context.Background(), "e2e", sampleConfig(), newFakeDeps(fwd, run))
	if err == nil || !strings.Contains(err.Error(), "test command failed") {
		t.Fatalf("expected wrapped command failure, got: %v", err)
	}
	// Even on failure, every started forward is torn down.
	if len(fwd.stopped) != 2 {
		t.Fatalf("expected both forwards torn down on failure, got %v", fwd.stopped)
	}
}

func TestRunTestEnv_TeardownOnForwardStartFailure(t *testing.T) {
	fwd := &fakeForwarder{startErr: fmt.Errorf("kubectl missing")}
	run := &fakeRunner{}
	err := runTestEnvWithConfig(context.Background(), "e2e", sampleConfig(), newFakeDeps(fwd, run))
	if err == nil || !strings.Contains(err.Error(), "start port-forward") {
		t.Fatalf("expected forward-start error, got: %v", err)
	}
	if run.ran {
		t.Fatal("test command must not run when a forward fails to start")
	}
}

func TestRunTestEnv_UnknownEnv(t *testing.T) {
	err := runTestEnvWithConfig(context.Background(), "staging", sampleConfig(), newFakeDeps(&fakeForwarder{}, &fakeRunner{}))
	if err == nil || !strings.Contains(err.Error(), "no test recipe for env") {
		t.Fatalf("expected unknown-env error, got: %v", err)
	}
}

func TestRunTestEnv_EmptyTestBlock(t *testing.T) {
	err := runTestEnvWithConfig(context.Background(), "e2e", config.TestConfig{}, newFakeDeps(&fakeForwarder{}, &fakeRunner{}))
	if err == nil || !strings.Contains(err.Error(), "no `test:` block") {
		t.Fatalf("expected empty-block error, got: %v", err)
	}
}

func TestRunTestEnv_MissingCommand(t *testing.T) {
	tc := config.TestConfig{"e2e": {Forwards: nil, Command: nil}}
	err := runTestEnvWithConfig(context.Background(), "e2e", tc, newFakeDeps(&fakeForwarder{}, &fakeRunner{}))
	if err == nil || !strings.Contains(err.Error(), "no `command`") {
		t.Fatalf("expected missing-command error, got: %v", err)
	}
}

func TestValidateForwards(t *testing.T) {
	cases := []struct {
		name    string
		fwds    []config.TestForward
		wantErr string
	}{
		{
			name:    "missing service",
			fwds:    []config.TestForward{{Namespace: "ns", RemotePort: 1, LocalPort: 1}},
			wantErr: "`service` is required",
		},
		{
			name:    "missing namespace",
			fwds:    []config.TestForward{{Service: "s", RemotePort: 1, LocalPort: 1}},
			wantErr: "`namespace` is required",
		},
		{
			name:    "bad remote port",
			fwds:    []config.TestForward{{Service: "s", Namespace: "ns", RemotePort: 0, LocalPort: 1}},
			wantErr: "`remote_port` must be > 0",
		},
		{
			name:    "bad local port",
			fwds:    []config.TestForward{{Service: "s", Namespace: "ns", RemotePort: 1, LocalPort: 0}},
			wantErr: "`local_port` must be > 0",
		},
		{
			name: "duplicate local port",
			fwds: []config.TestForward{
				{Service: "a", Namespace: "ns", RemotePort: 1, LocalPort: 9000},
				{Service: "b", Namespace: "ns", RemotePort: 2, LocalPort: 9000},
			},
			wantErr: "already used by svc/a",
		},
		{
			name: "valid",
			fwds: []config.TestForward{
				{Service: "a", Namespace: "ns", RemotePort: 1, LocalPort: 9000},
				{Service: "b", Namespace: "ns", RemotePort: 2, LocalPort: 9001},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateForwards("e2e", tc.fwds)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestTestCommandEnv_RecipeEnvWinsOverForwardURL(t *testing.T) {
	recipe := config.TestEnvConfig{
		Forwards: []config.TestForward{
			{Service: "a", Namespace: "ns", RemotePort: 1, LocalPort: 8090, URLEnv: "ADMIN_URL"},
		},
		Env: map[string]string{"ADMIN_URL": "http://override.example"},
	}
	env := testCommandEnv(recipe)
	// The literal env map is appended after the forward URL, so it wins
	// (last value for a key takes effect in os/exec).
	if !envLastWins(env, "ADMIN_URL", "http://override.example") {
		t.Fatalf("expected recipe env to win for ADMIN_URL, env=%v", lastValuesFor(env, "ADMIN_URL"))
	}
}

func envHas(env []string, key, val string) bool {
	want := key + "=" + val
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// envLastWins reports whether the LAST entry for key equals val (os/exec
// semantics: later entries override earlier ones).
func envLastWins(env []string, key, val string) bool {
	prefix := key + "="
	last := ""
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			last = strings.TrimPrefix(e, prefix)
			found = true
		}
	}
	return found && last == val
}

func lastValuesFor(env []string, key string) []string {
	prefix := key + "="
	var vals []string
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			vals = append(vals, e)
		}
	}
	return vals
}
