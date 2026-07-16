package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/reliant-labs/forge/internal/config"
)

// TestBuildHostServiceCmd covers each runner dispatch — go-run / air /
// binary / delve — plus the unknown-runner error. Each case asserts
// the exec.Cmd's program + args match the expected shape; we don't
// exercise the readDotEnvFile path here (that's a separate unit).
func TestBuildHostServiceCmd(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		svc     ServiceEntity
		want    []string
		wantErr string
	}{
		{
			// No build block → EffectiveBuild synthesizes the
			// ./cmd/<name> default, so the go-run target is ./cmd/api,
			// NOT the legacy ./cmd hardcode.
			name: "go-run default uses ./cmd/<name>",
			svc: ServiceEntity{Name: "api", Deploy: DeployConfigEntity{
				Type: "host", Host: &HostDeploy{Runner: "go-run"},
			}},
			want: []string{"go", "run", "./cmd/api", "server", "api"},
		},
		{
			name: "empty runner defaults to go-run",
			svc: ServiceEntity{Name: "api", Deploy: DeployConfigEntity{
				Type: "host", Host: &HostDeploy{Runner: ""},
			}},
			want: []string{"go", "run", "./cmd/api", "server", "api"},
		},
		{
			// An explicit GoBuild.cmd flows into the go-run target so
			// host-run matches the build target exactly (shared binary at
			// ./cmd/<project>, not ./cmd/<service>).
			name: "go-run uses explicit GoBuild.cmd",
			svc: ServiceEntity{
				Name:   "api",
				Deploy: DeployConfigEntity{Type: "host", Host: &HostDeploy{Runner: "go-run"}},
				Build:  BuildConfigEntity{Type: "go", Go: &GoBuild{Cmd: "./cmd/myproj"}},
			},
			want: []string{"go", "run", "./cmd/myproj", "server", "api"},
		},
		{
			name: "air with custom config",
			svc: ServiceEntity{Name: "api", Deploy: DeployConfigEntity{
				Type: "host", Host: &HostDeploy{Runner: "air", AirConfig: ".air.custom.toml"},
			}},
			want: []string{"air", "-c", ".air.custom.toml"},
		},
		{
			name: "air default config",
			svc: ServiceEntity{Name: "api", Deploy: DeployConfigEntity{
				Type: "host", Host: &HostDeploy{Runner: "air"},
			}},
			want: []string{"air", "-c", ".air.toml"},
		},
		{
			name: "binary runner",
			svc: ServiceEntity{Name: "admin-server", Deploy: DeployConfigEntity{
				Type: "host", Host: &HostDeploy{Runner: "binary"},
			}},
			want: []string{"./bin/admin-server"},
		},
		{
			name: "delve runner default port",
			svc: ServiceEntity{Name: "api", Deploy: DeployConfigEntity{
				Type: "host", Host: &HostDeploy{Runner: "delve"},
			}},
			want: []string{"dlv", "exec", "--headless", "--listen=:2345", "--api-version=2", "--accept-multiclient", "--continue", "./bin/api"},
		},
		{
			name: "delve runner custom port",
			svc: ServiceEntity{Name: "api", Deploy: DeployConfigEntity{
				Type: "host", Host: &HostDeploy{Runner: "delve", DelvePort: 3030},
			}},
			want: []string{"dlv", "exec", "--headless", "--listen=:3030", "--api-version=2", "--accept-multiclient", "--continue", "./bin/api"},
		},
		{
			name: "unknown runner errors",
			svc: ServiceEntity{Name: "api", Deploy: DeployConfigEntity{
				Type: "host", Host: &HostDeploy{Runner: "tilt"},
			}},
			wantErr: `unknown host runner "tilt"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// nil cfg is the test-shaped projectConfig — the dispatch
			// matrix shouldn't depend on forge.yaml layering at all,
			// and a nil cfg makes that explicit.
			cmd, _, err := buildHostServiceCmd(ctx, nil, c.svc, nil, "dev")
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("want err containing %q, got %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			got := cmd.Args
			if len(got) != len(c.want) {
				t.Fatalf("args len mismatch: got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("args[%d]: got %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestEntitiesEmpty confirms the sanity check fires only when every
// declaration kind is absent.
func TestEntitiesEmpty(t *testing.T) {
	if !entitiesEmpty(nil) {
		t.Error("nil: want empty")
	}
	if !entitiesEmpty(&KCLEntities{}) {
		t.Error("zero value: want empty")
	}
	if entitiesEmpty(&KCLEntities{Services: []ServiceEntity{{Name: "a"}}}) {
		t.Error("one service: want non-empty")
	}
	if entitiesEmpty(&KCLEntities{Frontends: []FrontendEntity{{Name: "web"}}}) {
		t.Error("one frontend: want non-empty")
	}
	if entitiesEmpty(&KCLEntities{CronJobs: []CronJobEntity{{Name: "cron"}}}) {
		t.Error("one cronjob: want non-empty")
	}
}

// TestUpStatePath covers the canonical $HOME/.cache/forge/up/<env>.pids
// path so the contract with `forge up stop` is stable.
func TestUpStatePath(t *testing.T) {
	got, err := upStatePath("dev")
	if err != nil {
		t.Fatalf("upStatePath: %v", err)
	}
	if !strings.HasSuffix(got, "/.cache/forge/up/dev.pids") {
		t.Errorf("upStatePath: got %q, want suffix /.cache/forge/up/dev.pids", got)
	}
}

// TestBuildHostServiceCmd_LayersProjectConfig pins the symmetry with
// `forge run <svc>`: the sibling `config.<env>.yaml` must reach the
// host child process via cmd.Env just like the run path does. The host
// phase previously dropped this layer (the call site took a `_ string`
// env and passed nil to LayerHostEnv); this test guards against
// regressing back to that shape.
func TestBuildHostServiceCmd_LayersProjectConfig(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `name: testproj
module_path: github.com/example/testproj
version: "0.1.0"
binary: shared
`
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	writeComponentsJSON(t, dir, config.ComponentConfig{
		Name:  "api",
		Kind:  "server",
		Path:  "handlers/api",
		Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: 8080}},
	})
	siblingContent := `environment: development
log_level: debug
`
	if err := os.WriteFile(filepath.Join(dir, "config.dev-host.yaml"), []byte(siblingContent), 0o644); err != nil {
		t.Fatalf("write sibling config: %v", err)
	}
	t.Chdir(dir)

	cfg, err := loadProjectConfig()
	if err != nil {
		t.Fatalf("loadProjectConfig: %v", err)
	}

	svc := ServiceEntity{
		Name: "api",
		Deploy: DeployConfigEntity{
			Type: "host",
			Host: &HostDeploy{Runner: "go-run"},
		},
	}
	cmd, _, err := buildHostServiceCmd(context.Background(), cfg, svc, nil, "dev-host")
	if err != nil {
		t.Fatalf("buildHostServiceCmd: %v", err)
	}

	wantPairs := []string{"ENVIRONMENT=development", "LOG_LEVEL=debug"}
	for _, p := range wantPairs {
		found := false
		for _, e := range cmd.Env {
			if e == p {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("cmd.Env missing %q. Got env:\n%s", p, strings.Join(cmd.Env, "\n"))
		}
	}
}

func TestUpLogPath_Sanitises(t *testing.T) {
	got, err := upLogPath("dev", "frontend:admin/web")
	if err != nil {
		t.Fatalf("upLogPath: %v", err)
	}
	// Colons and slashes must be replaced so the path is safe.
	if strings.Contains(got, ":") {
		t.Errorf("upLogPath returned unsanitised path %q", got)
	}
	if !strings.HasSuffix(got, "frontend_admin_web.log") {
		t.Errorf("upLogPath: got %q, want frontend_admin_web.log suffix", got)
	}
}

// TestUpCmd_NoPortForwardSurface pins the phase-3 ingress refactor:
// `forge up` no longer mentions port-forward in any user-facing string
// (Short / Long / Example / flag help). Reaching cluster services from
// the host is the Gateway API ingress path now (forge cluster urls); the
// orchestrator must not advertise a port-forward phase that doesn't
// exist.
func TestUpCmd_NoPortForwardSurface(t *testing.T) {
	cmd := newUpCmd()
	surfaces := map[string]string{
		"Short": cmd.Short,
		"Long":  cmd.Long,
	}
	for label, s := range surfaces {
		if strings.Contains(strings.ToLower(s), "port-forward") {
			t.Errorf("%s mentions port-forward: %q", label, s)
		}
	}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if strings.Contains(strings.ToLower(f.Usage), "port-forward") {
			t.Errorf("flag --%s usage mentions port-forward: %q", f.Name, f.Usage)
		}
	})
}

// TestBuildFrontendCmd_PortFromKCLOverridesParent pins Item 1: the
// frontend's KCL-declared port is force-injected into the child env,
// overriding any PORT bleeding in from the orchestrator's own
// os.Environ(). Before this fix a parent shell with `PORT=8080`
// exported for an unrelated service silently shifted the dev server's
// bind port out from under the user.
func TestBuildFrontendCmd_PortFromKCLOverridesParent(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "PORT=8080", "EDITOR=vim"}
	fe := FrontendEntity{Name: "web", Path: "frontend", Port: 3000, EnvFile: "/does/not/exist"}
	cmd := buildFrontendCmd(context.Background(), fe, "dev", parent, nil)

	// PORT=3000 (KCL) must be present, PORT=8080 (parent) must NOT.
	hasKCLPort := false
	for _, kv := range cmd.Env {
		if kv == "PORT=3000" {
			hasKCLPort = true
		}
		if kv == "PORT=8080" {
			t.Errorf("parent PORT=8080 leaked into child env: %v", cmd.Env)
		}
	}
	if !hasKCLPort {
		t.Errorf("expected PORT=3000 from KCL; got env: %v", cmd.Env)
	}
	// Sanity: the rest of the parent env passed through.
	hasPath := false
	for _, kv := range cmd.Env {
		if kv == "PATH=/usr/bin" {
			hasPath = true
		}
	}
	if !hasPath {
		t.Errorf("expected parent PATH to survive; got env: %v", cmd.Env)
	}
}

// TestBuildFrontendCmd_KCLEnvVarsInjected confirms KCL-declared env_vars
// (e.g. a VITE_ADMIN_URL composed from forge.resolve_port) reach the dev
// process env, alongside the forced PORT and the passed-through shell.
func TestBuildFrontendCmd_KCLEnvVarsInjected(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "SHELL_ONLY=keepme"}
	fe := FrontendEntity{
		Name: "reliant-web", Path: "web", Port: 3000, EnvFile: "/does/not/exist",
		EnvVars: []KCLEnvVar{
			{Name: "VITE_ADMIN_URL", Value: "http://localhost:3000/admin"},
			{Name: "VITE_API_URL", Value: "http://localhost:3090"},
		},
	}
	cmd := buildFrontendCmd(context.Background(), fe, "dev", parent, nil)

	got := map[string]string{}
	for _, kv := range cmd.Env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			got[kv[:i]] = kv[i+1:]
		}
	}
	want := map[string]string{
		"VITE_ADMIN_URL": "http://localhost:3000/admin",
		"VITE_API_URL":   "http://localhost:3090",
		"PORT":           "3000",
		"SHELL_ONLY":     "keepme",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %q, want %q (full: %v)", k, got[k], v, cmd.Env)
		}
	}
}

// TestBuildFrontendCmd_ParentShellOverridesEnvVars pins the precedence:
// the developer's shell wins over a KCL env_var of the same name (escape
// hatch), matching host-service layering.
func TestBuildFrontendCmd_ParentShellOverridesEnvVars(t *testing.T) {
	parent := []string{"VITE_ADMIN_URL=http://override"}
	fe := FrontendEntity{Name: "web", Path: "web", EnvFile: "/does/not/exist",
		EnvVars: []KCLEnvVar{{Name: "VITE_ADMIN_URL", Value: "http://kcl"}}}
	cmd := buildFrontendCmd(context.Background(), fe, "dev", parent, nil)
	for _, kv := range cmd.Env {
		if kv == "VITE_ADMIN_URL=http://kcl" {
			t.Errorf("KCL env_var should not override the parent shell; env: %v", cmd.Env)
		}
	}
}

// TestBuildFrontendCmd_PortZeroLeavesParentPortAlone confirms the
// fe.Port == 0 fallback (legacy projects whose KCL doesn't emit the
// port field): we don't force-inject "PORT=0" because that would crash
// the dev server. The parent's PORT (if any) is left untouched.
func TestBuildFrontendCmd_PortZeroLeavesParentPortAlone(t *testing.T) {
	parent := []string{"PORT=8080"}
	fe := FrontendEntity{Name: "web", Path: "frontend", Port: 0, EnvFile: "/does/not/exist"}
	cmd := buildFrontendCmd(context.Background(), fe, "dev", parent, nil)

	for _, kv := range cmd.Env {
		if kv == "PORT=0" {
			t.Errorf("PORT=0 must not be injected for fe.Port==0; got %v", cmd.Env)
		}
	}
	found := false
	for _, kv := range cmd.Env {
		if kv == "PORT=8080" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected parent PORT=8080 to pass through when fe.Port==0; got env: %v", cmd.Env)
	}
}

// TestUpNoDeployFlag pins Item 5: the --no-deploy flag is registered
// on `forge up` with the expected help text. The actual short-circuit
// behaviour is read straight off opts.noDeploy in runUp (`if !opts.noDeploy`
// gate around the deploy phase) — this test guards the flag wiring so
// a future refactor can't quietly delete the surface.
func TestUpNoDeployFlag(t *testing.T) {
	cmd := newUpCmd()
	flag := cmd.Flags().Lookup("no-deploy")
	if flag == nil {
		t.Fatal("--no-deploy flag missing from forge up")
	}
	if flag.DefValue != "false" {
		t.Errorf("--no-deploy default: got %q, want false", flag.DefValue)
	}
	if !strings.Contains(flag.Usage, "cluster apply") {
		t.Errorf("--no-deploy usage: got %q, want a phrase mentioning 'cluster apply'", flag.Usage)
	}
	// Verify the flag actually parses into opts.noDeploy by exercising
	// the cobra parser. We can't easily run the RunE without a project,
	// but flag parse is enough to confirm the BoolVar wiring is intact.
	if err := cmd.ParseFlags([]string{"--env=dev", "--no-deploy"}); err != nil {
		t.Fatalf("parse --no-deploy: %v", err)
	}
	got, err := cmd.Flags().GetBool("no-deploy")
	if err != nil {
		t.Fatalf("GetBool --no-deploy: %v", err)
	}
	if !got {
		t.Errorf("--no-deploy: parsed value got false, want true")
	}
}

func TestSummaryLogPath_MatchesUpLogPath(t *testing.T) {
	// The displayed path must be the tail of the file upLogPath writes,
	// so a printed path is exactly what `grep`/`tail` will find.
	full, err := upLogPath("dev", "frontend:admin/web")
	if err != nil {
		t.Fatalf("upLogPath: %v", err)
	}
	disp := summaryLogPath("dev", "frontend:admin/web")
	if !strings.HasSuffix(full, disp) {
		t.Errorf("summaryLogPath %q is not a suffix of upLogPath %q", disp, full)
	}
	if want := ".forge/logs/dev/frontend_admin_web.log"; disp != want {
		t.Errorf("summaryLogPath: got %q, want %q", disp, want)
	}
}

func TestHostEnvPort(t *testing.T) {
	if got := hostEnvPort("svc", nil); got != "" {
		t.Errorf("nil host: got %q, want empty", got)
	}
	// Only PORT set → use it.
	host := &HostDeploy{EnvVars: []KCLEnvVar{
		{Name: "DATABASE_URL", Value: "postgres://x"},
		{Name: "PORT", Value: "8080"},
	}}
	if got := hostEnvPort("api", host); got != "8080" {
		t.Errorf("hostEnvPort PORT-only: got %q, want 8080", got)
	}
	// Both PORT and <NAME>_PORT → the service-specific one wins (the real
	// bind port; the generic PORT is often a vestigial default).
	both := &HostDeploy{EnvVars: []KCLEnvVar{
		{Name: "PORT", Value: "8080"},
		{Name: "ADMIN_SERVER_PORT", Value: "8090"},
	}}
	if got := hostEnvPort("admin-server", both); got != "8090" {
		t.Errorf("hostEnvPort specific-wins: got %q, want 8090", got)
	}
	// config_map_ref-only PORT (no inline value) yields no URL.
	refHost := &HostDeploy{EnvVars: []KCLEnvVar{
		{Name: "PORT", ConfigMapRef: "cfg", ConfigMapKey: "PORT"},
	}}
	if got := hostEnvPort("api", refHost); got != "" {
		t.Errorf("hostEnvPort ref-only: got %q, want empty", got)
	}
}

// TestHostEnvPorts is the Gap-A fix: a host service binds EVERY declared
// <...>_PORT, not just the first/canonical one. Probing only one let a real
// conflict slip past the pre-flight guard.
func TestHostEnvPorts(t *testing.T) {
	eq := func(t *testing.T, got, want []int) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}

	if got := hostEnvPorts("svc", nil); got != nil {
		t.Errorf("nil host: got %v, want nil", got)
	}

	// The headline case: one service with several distinct bind ports, each a
	// <...>_PORT env var. ALL are returned, in declaration order.
	multi := &HostDeploy{EnvVars: []KCLEnvVar{
		{Name: "DATABASE_URL", Value: "postgres://x"},
		{Name: "API_PORT", Value: "8081"},
		{Name: "METRICS_PORT", Value: "3091"},
		{Name: "PPROF_PORT", Value: "6060"},
	}}
	eq(t, hostEnvPorts("api", multi), []int{8081, 3091, 6060})

	// Only generic PORT declared → it is the bind port.
	eq(t, hostEnvPorts("api", &HostDeploy{EnvVars: []KCLEnvVar{
		{Name: "PORT", Value: "8080"},
	}}), []int{8080})

	// Generic PORT + the service-specific <NAME>_PORT → PORT is a vestigial
	// default the binary ignores, so it is dropped; the specific one wins.
	// (Over-detecting a vestigial PORT would mean a false pre-flight conflict
	// and a readiness gate waiting for a port that never binds.)
	eq(t, hostEnvPorts("admin-server", &HostDeploy{EnvVars: []KCLEnvVar{
		{Name: "PORT", Value: "8080"},
		{Name: "ADMIN_SERVER_PORT", Value: "8090"},
	}}), []int{8090})

	// Generic PORT alongside a NON-name-matching *_PORT (e.g. a metrics port):
	// no service-specific override, so BOTH are real bind ports.
	eq(t, hostEnvPorts("api", &HostDeploy{EnvVars: []KCLEnvVar{
		{Name: "PORT", Value: "8080"},
		{Name: "METRICS_PORT", Value: "9090"},
	}}), []int{9090, 8080})

	// Duplicate values collapse; ref-only ports have no host-side literal and
	// are skipped.
	eq(t, hostEnvPorts("api", &HostDeploy{EnvVars: []KCLEnvVar{
		{Name: "API_PORT", Value: "8081"},
		{Name: "HTTP_PORT", Value: "8081"},                                  // dup value
		{Name: "GRPC_PORT", ConfigMapRef: "cfg", ConfigMapKey: "GRPC_PORT"}, // ref, no literal
		{Name: "BAD_PORT", Value: "not-a-number"},                           // unparseable
	}}), []int{8081})

	// No inline port at all → empty.
	if got := hostEnvPorts("api", &HostDeploy{EnvVars: []KCLEnvVar{
		{Name: "DATABASE_URL", Value: "postgres://x"},
	}}); got != nil {
		t.Errorf("no port: got %v, want nil", got)
	}
}

func TestPersistUsesCapturedPid(t *testing.T) {
	// Regression: `forge up --background` Release()s the child (resetting
	// cmd.Process.Pid to -1), so persist() must use the PID captured at
	// Start, and skip entries with no usable PID rather than writing -1/0.
	env := "test-persist-pidcapture"
	statePath, err := upStatePath(env)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(statePath)

	p := newProcRegistry(env)
	p.processes = []*managedProcess{
		{name: "svc-a", pid: 4242, cmd: &exec.Cmd{}}, // detached: captured pid, no live handle
		{name: "svc-b", pid: 0, cmd: &exec.Cmd{}},    // never captured: must be skipped
	}
	p.persist()

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "svc-a\t4242\n") {
		t.Errorf("captured pid not persisted; got %q", got)
	}
	if strings.Contains(got, "svc-b") {
		t.Errorf("entry without a pid should be skipped; got %q", got)
	}
	if strings.Contains(got, "-1") {
		t.Errorf("a -1 pid leaked into the state file; got %q", got)
	}
}

func TestFrontendDepsStale(t *testing.T) {
	dir := t.TempDir()
	// No node_modules yet → stale.
	writeFileAt(t, dir, "package.json", `{"name":"x"}`)
	writeFileAt(t, dir, "package-lock.json", `{}`)
	if !frontendDepsStale(dir) {
		t.Fatal("missing node_modules should be stale")
	}
	// Install: node_modules newer than manifests → fresh.
	writeFileAt(t, dir, "node_modules/.keep", "")
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "package.json"), past, past)
	_ = os.Chtimes(filepath.Join(dir, "package-lock.json"), past, past)
	_ = os.Chtimes(filepath.Join(dir, "node_modules"), future, future)
	if frontendDepsStale(dir) {
		t.Fatal("node_modules newer than manifests should be fresh")
	}
	// Touch the lockfile newer than node_modules → stale again.
	_ = os.Chtimes(filepath.Join(dir, "package-lock.json"), future.Add(time.Minute), future.Add(time.Minute))
	if !frontendDepsStale(dir) {
		t.Fatal("lockfile newer than node_modules should be stale")
	}
}

func TestProcPID_PrefersCaptured(t *testing.T) {
	// Captured pid wins (survives Release on the detach path).
	if got := procPID(&managedProcess{pid: 4242, cmd: &exec.Cmd{}}); got != 4242 {
		t.Errorf("captured pid: got %d, want 4242", got)
	}
	// No captured pid, no live process → 0 (callers skip, never signal).
	if got := procPID(&managedProcess{cmd: &exec.Cmd{}}); got != 0 {
		t.Errorf("unset: got %d, want 0", got)
	}
}

func TestSignalProcessGroup_NonPositiveIsNoop(t *testing.T) {
	// A 0/-1 pid must never fan a signal out (negative pid = whole group).
	if err := signalProcessGroup(0, syscall.SIGTERM); err != nil {
		t.Errorf("pid 0: %v", err)
	}
	if err := signalProcessGroup(-1, syscall.SIGTERM); err != nil {
		t.Errorf("pid -1: %v", err)
	}
}

func TestInTargetSet(t *testing.T) {
	// Empty filter matches everything (default).
	if !inTargetSet(nil, "admin-server") {
		t.Error("empty filter should match everything")
	}
	// Non-empty: only named entries match.
	targets := []string{"admin-server", "reliant-web"}
	if !inTargetSet(targets, "admin-server") {
		t.Error("named target should match")
	}
	if inTargetSet(targets, "workspace-proxy") {
		t.Error("unnamed target should not match")
	}
}

// TestPortInUse asserts the real-socket probe: a held listener reads as
// in-use, a port nothing binds reads as free. The post-close flip is
// best-effort (ephemeral reuse can be racy) — we assert the true case
// against a held listener and the false case against a never-bound port.
func TestPortInUse(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	held := ln.Addr().(*net.TCPAddr).Port

	if !portInUse(held) {
		t.Errorf("portInUse(%d) = false; want true (listener is held)", held)
	}

	// Find a port that is (almost certainly) free: bind, capture, release.
	free, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free: %v", err)
	}
	freePort := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()
	if portInUse(freePort) {
		t.Errorf("portInUse(%d) = true; want false (port was released)", freePort)
	}
}

// TestConflictingPorts exercises the pure collection logic with an
// injected probe — no real sockets. It covers --target scoping, the
// host-service inline-PORT skip, the frontend feature gate, and the
// fe.Port==0 skip.
func TestConflictingPorts(t *testing.T) {
	entities := &KCLEntities{
		Services: []ServiceEntity{
			{Name: "admin-server", Deploy: DeployConfigEntity{Type: "host", Host: &HostDeploy{
				EnvVars: []KCLEnvVar{{Name: "ADMIN_SERVER_PORT", Value: "8090"}},
			}}},
			// Multi-port host service: three distinct declared bind ports.
			{Name: "api", Deploy: DeployConfigEntity{Type: "host", Host: &HostDeploy{
				EnvVars: []KCLEnvVar{
					{Name: "API_PORT", Value: "8081"},
					{Name: "METRICS_PORT", Value: "3091"},
					{Name: "PPROF_PORT", Value: "6060"},
				},
			}}},
			{Name: "noport", Deploy: DeployConfigEntity{Type: "host", Host: &HostDeploy{}}},
			{Name: "cluster-svc", Deploy: DeployConfigEntity{Type: "cluster", Cluster: &K8sCluster{}}},
		},
		Frontends: []FrontendEntity{
			{Name: "reliant-web", Port: 3000},
			{Name: "noportfe", Port: 0},
		},
	}
	// busy treats these ports as in-use.
	busy := func(ports ...int) func(int) bool {
		set := map[int]bool{}
		for _, p := range ports {
			set[p] = true
		}
		return func(p int) bool { return set[p] }
	}

	names := func(cs []portConflict) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.name
		}
		return out
	}

	t.Run("both host and frontend ports busy", func(t *testing.T) {
		got := conflictingPorts(entities, nil, true, busy(8090, 3000))
		if g := names(got); len(g) != 2 || g[0] != "admin-server" || g[1] != "reliant-web" {
			t.Fatalf("got %v; want [admin-server reliant-web]", g)
		}
	})

	t.Run("nothing busy → no conflicts", func(t *testing.T) {
		if got := conflictingPorts(entities, nil, true, busy()); len(got) != 0 {
			t.Fatalf("got %v; want none", names(got))
		}
	})

	t.Run("target scopes the check", func(t *testing.T) {
		// admin-server is up, but we only target reliant-web → allowed.
		got := conflictingPorts(entities, []string{"reliant-web"}, true, busy(8090))
		if len(got) != 0 {
			t.Fatalf("got %v; want none (admin-server not in target set)", names(got))
		}
		// Now reliant-web's own port is busy → it should flag.
		got = conflictingPorts(entities, []string{"reliant-web"}, true, busy(3000))
		if g := names(got); len(g) != 1 || g[0] != "reliant-web" {
			t.Fatalf("got %v; want [reliant-web]", g)
		}
	})

	t.Run("frontend gate off skips frontends", func(t *testing.T) {
		got := conflictingPorts(entities, nil, false, busy(8090, 3000))
		if g := names(got); len(g) != 1 || g[0] != "admin-server" {
			t.Fatalf("got %v; want [admin-server] (frontends gated off)", g)
		}
	})

	t.Run("host service without inline PORT is skipped", func(t *testing.T) {
		// Even if some port the probe would call true for, noport declares
		// no PORT so it never contributes a conflict.
		got := conflictingPorts(entities, []string{"noport"}, true, func(int) bool { return true })
		if len(got) != 0 {
			t.Fatalf("got %v; want none (noport declares no PORT)", names(got))
		}
	})

	t.Run("multi-port service surfaces EVERY busy port", func(t *testing.T) {
		// Gap A: api binds :8081, :3091, :6060. A conflict on any one of them
		// must be caught — previously only the first was probed, so a stale
		// stack squatting :3091 slipped past and a second stack launched.
		got := conflictingPorts(entities, []string{"api"}, true, busy(3091))
		if g := names(got); len(g) != 1 || g[0] != "api" || got[0].port != 3091 {
			t.Fatalf("got %v (ports %v); want one api conflict on :3091", g, ports(got))
		}
		// Two of its three ports busy → two conflicts, both named api.
		got = conflictingPorts(entities, []string{"api"}, true, busy(8081, 6060))
		if len(got) != 2 || got[0].port != 8081 || got[1].port != 6060 {
			t.Fatalf("got ports %v; want [8081 6060] both on api", ports(got))
		}
	})
}

func ports(cs []portConflict) []int {
	out := make([]int, len(cs))
	for i, c := range cs {
		out[i] = c.port
	}
	return out
}

// TestCollectUpServices verifies the summary/services row collection:
// host services + EVERY declared frontend are surfaced (multiple
// frontends never collapse to one), ports/URLs resolve from the KCL
// conventions, the health probe is applied, and --target / the frontend
// gate scope the set.
func TestCollectUpServices(t *testing.T) {
	entities := &KCLEntities{
		Services: []ServiceEntity{
			{Name: "admin-server", Deploy: DeployConfigEntity{Type: "host", Host: &HostDeploy{
				EnvVars: []KCLEnvVar{{Name: "ADMIN_SERVER_PORT", Value: "8090"}},
			}}},
			{Name: "noport", Deploy: DeployConfigEntity{Type: "host", Host: &HostDeploy{}}},
			{Name: "cluster-svc", Deploy: DeployConfigEntity{Type: "cluster", Cluster: &K8sCluster{}}},
		},
		Frontends: []FrontendEntity{
			{Name: "admin-web", Port: 3000},
			{Name: "reliant-web", Port: 3001},
		},
	}
	// admin-server (:8090) and reliant-web (:3001) are listening; admin-web
	// (:3000) is not (still booting).
	probe := func(p int) bool { return p == 8090 || p == 3001 }

	rows := collectUpServices(entities, "dev", nil, true, probe)

	// Order: host services first, then ALL frontends. cluster-svc is dropped
	// (not host-mode).
	var got []string
	for _, r := range rows {
		got = append(got, r.Kind+":"+r.Name)
	}
	want := []string{"host:admin-server", "host:noport", "frontend:admin-web", "frontend:reliant-web"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rows: got %v, want %v", got, want)
	}

	byName := map[string]upServiceRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if r := byName["admin-server"]; r.Port != 8090 || r.URL != "http://localhost:8090" || !r.Listening {
		t.Errorf("admin-server: got %+v; want port 8090, url set, listening", r)
	}
	if r := byName["noport"]; r.Port != 0 || r.URL != "" || r.Listening {
		t.Errorf("noport: got %+v; want no port/url and not listening", r)
	}
	if r := byName["admin-web"]; r.Port != 3000 || r.Listening {
		t.Errorf("admin-web: got %+v; want port 3000, not listening", r)
	}
	if r := byName["reliant-web"]; r.Port != 3001 || !r.Listening {
		t.Errorf("reliant-web: got %+v; want port 3001, listening", r)
	}
	// Both frontends present — never collapsed to one.
	feCount := 0
	for _, r := range rows {
		if r.Kind == "frontend" {
			feCount++
		}
	}
	if feCount != 2 {
		t.Errorf("frontend rows: got %d, want 2 (all frontends surfaced)", feCount)
	}

	// Frontend gate off drops frontends entirely.
	rows = collectUpServices(entities, "dev", nil, false, probe)
	for _, r := range rows {
		if r.Kind == "frontend" {
			t.Errorf("frontend %q listed with gate off", r.Name)
		}
	}

	// --target scopes to one service.
	rows = collectUpServices(entities, "dev", []string{"reliant-web"}, true, probe)
	if len(rows) != 1 || rows[0].Name != "reliant-web" {
		t.Errorf("target scope: got %v; want [reliant-web]", rows)
	}
}

// TestRenderUpSummary formats the block to a buffer and asserts it lists
// every service with its URL, health word, and log path — the greppable
// contract an agent relies on. It also prints the block so the test log
// shows the real output shape.
func TestRenderUpSummary(t *testing.T) {
	rows := []upServiceRow{
		{Name: "admin-server", Kind: "host", URL: "http://localhost:8090", Port: 8090, Log: ".forge/logs/dev/admin-server.log", Listening: true, PID: 4242, Owned: true},
		{Name: "worker", Kind: "host", Log: ".forge/logs/dev/worker.log"},
		{Name: "admin-web", Kind: "frontend", URL: "http://localhost:3000", Port: 3000, Log: ".forge/logs/dev/frontend_admin-web.log", Listening: true, PID: 5151, Owned: false},
		{Name: "reliant-web", Kind: "frontend", URL: "http://localhost:3001", Port: 3001, Log: ".forge/logs/dev/frontend_reliant-web.log", Listening: false},
	}
	var b bytes.Buffer
	renderUpSummary(&b, "dev", rows, "down", true, []string{"Ctrl-C to stop."})
	out := b.String()
	t.Logf("\n%s", out)

	for _, want := range []string{
		"admin-server", "http://localhost:8090", "up (pid 4242)",
		"admin-web", "up (pid 5151, not forge-owned)",
		"reliant-web", "http://localhost:3001", "down",
		"worker", "(no port declared)",
		".forge/logs/dev/frontend_reliant-web.log",
		"Logs   .forge/logs/dev/",
		"forge cluster urls",
		"forge up services --env=dev",
		"Host services", "Frontends",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---\n%s", want, out)
		}
	}
}

// TestRunUpServices_EndToEnd drives the `forge up services` code path all
// the way through: render KCL (via the fixture override) → collect rows →
// probe a real listener → enrich ownership → format. It binds a live
// socket on the host service's port so the health snapshot reports it
// "up", and leaves the two frontends unbound so they report "down" — and
// asserts BOTH frontends appear (never collapsed). Covers the text table
// and the --json contract.
func TestRunUpServices_EndToEnd(t *testing.T) {
	// A real listener on a free port so the probe reports the host service up.
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	writeForgeYAML(t, dir, `name: demo
module_path: github.com/example/demo
version: "0.1.0"
binary: shared
features:
  codegen: true
  frontend: true
`)
	fixture := fmt.Sprintf(`{
      "services": [
        {"name": "admin-server", "deploy": {"type": "host", "runner": "go-run",
          "env_vars": [{"name": "ADMIN_SERVER_PORT", "value": "%d"}]}},
        {"name": "cluster-only", "deploy": {"type": "cluster", "cluster": "k3d-demo"}}
      ],
      "frontends": [
        {"name": "admin-web", "path": "web/admin", "port": 3100},
        {"name": "reliant-web", "path": "web/reliant", "port": 3101}
      ]
    }`, port)
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t, fixture))
	t.Chdir(dir)

	// Text output.
	out := captureStdout(t, func() {
		if err := runUpServices(context.Background(), "dev", false); err != nil {
			t.Fatalf("runUpServices: %v", err)
		}
	})
	t.Logf("\n%s", out)
	for _, want := range []string{
		"admin-server", fmt.Sprintf("http://localhost:%d", port),
		"admin-web", "http://localhost:3100",
		"reliant-web", "http://localhost:3101", // BOTH frontends surfaced
		"down", // an unbound frontend port
		"forge up services --env=dev",
		".forge/logs/dev/",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "cluster-only") {
		t.Errorf("cluster service leaked into host/frontend table:\n%s", out)
	}

	// JSON output.
	jsonOut := captureStdout(t, func() {
		if err := runUpServices(context.Background(), "dev", true); err != nil {
			t.Fatalf("runUpServices json: %v", err)
		}
	})
	var rep upServicesReport
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil {
		t.Fatalf("parse json %q: %v", jsonOut, err)
	}
	if rep.Env != "dev" {
		t.Errorf("json env: got %q, want dev", rep.Env)
	}
	byName := map[string]upServiceRow{}
	for _, r := range rep.Services {
		byName[r.Name] = r
	}
	if len(byName) != 3 { // 1 host + 2 frontends (cluster excluded)
		t.Fatalf("json services: got %d (%v), want 3", len(byName), byName)
	}
	if r := byName["admin-server"]; r.Port != port || !r.Listening || r.PID <= 0 {
		t.Errorf("admin-server json: got %+v; want port %d, listening, pid>0", r, port)
	}
	if _, present := byName["admin-web"]; !present {
		t.Error("admin-web missing from json")
	}
	if r, present := byName["reliant-web"]; !present || r.Listening {
		t.Errorf("reliant-web json: got %+v (present=%v); want present and not listening", r, present)
	}
}

// listeningSet builds an injected `listening` probe for the readiness tests.
func listeningSet(ports ...int) func(int) bool {
	set := map[int]bool{}
	for _, p := range ports {
		set[p] = true
	}
	return func(p int) bool { return set[p] }
}

// TestClassifyPortReadiness is the Gap-B core: after launch, tell OUR OWN
// marked child apart from a foreign/stale holder and from nothing-listening.
// listening / resolvePID / procFacts are all injected — no real sockets or
// lsof.
func TestClassifyPortReadiness(t *testing.T) {
	f := fakeFacts{
		env: map[int][]string{
			100: marker("dev", "api"), // our child for env=dev
			200: {"PATH=/usr/bin"},    // foreign: no marker
		},
		ppid: map[int]int{100: 1, 200: 1},
	}
	resolve := func(port int) int {
		switch port {
		case 8081:
			return 100 // ours
		case 3091:
			return 200 // foreign
		case 7000:
			return 0 // listening but holder unresolvable
		default:
			return 0
		}
	}

	// our-child-bound.
	if got := classifyPortReadiness(8081, "dev", listeningSet(8081, 3091, 7000), resolve, f); got != portReadyOurs {
		t.Errorf("our child bound: got %v, want portReadyOurs", got)
	}
	// foreign-holds-port (the stale-holder trap).
	if got := classifyPortReadiness(3091, "dev", listeningSet(8081, 3091, 7000), resolve, f); got != portReadyForeign {
		t.Errorf("foreign holder: got %v, want portReadyForeign", got)
	}
	// nothing-listening (silent bind failure).
	if got := classifyPortReadiness(9999, "dev", listeningSet(8081, 3091, 7000), resolve, f); got != portReadyNobody {
		t.Errorf("nothing listening: got %v, want portReadyNobody", got)
	}
	// Listening but holder unresolvable (no lsof / Windows) → degrade to
	// "ours" rather than a FALSE foreign that would fail a healthy run.
	if got := classifyPortReadiness(7000, "dev", listeningSet(7000), resolve, f); got != portReadyOurs {
		t.Errorf("unresolvable holder: got %v, want portReadyOurs (graceful degrade)", got)
	}
	// A marker for a DIFFERENT env is foreign to us (mirrors the reclaim
	// guard's per-env ownership).
	f2 := fakeFacts{env: map[int][]string{300: marker("staging", "api")}, ppid: map[int]int{300: 1}}
	if got := classifyPortReadiness(8081, "dev", listeningSet(8081), func(int) int { return 300 }, f2); got != portReadyForeign {
		t.Errorf("wrong-env marker: got %v, want portReadyForeign", got)
	}
}

// TestEvalHostReadiness checks the whole-service snapshot: every declared
// bind port of a multi-port host service is classified, non-host / no-port
// services contribute nothing, --target scopes the set, and the failure list
// + error name the offending ports with the right foreign-vs-nobody wording.
func TestEvalHostReadiness(t *testing.T) {
	entities := &KCLEntities{
		Services: []ServiceEntity{
			{Name: "api", Deploy: DeployConfigEntity{Type: "host", Host: &HostDeploy{
				EnvVars: []KCLEnvVar{
					{Name: "API_PORT", Value: "8081"},
					{Name: "METRICS_PORT", Value: "3091"},
					{Name: "PPROF_PORT", Value: "6060"},
				},
			}}},
			{Name: "noport", Deploy: DeployConfigEntity{Type: "host", Host: &HostDeploy{}}},
			{Name: "cluster-svc", Deploy: DeployConfigEntity{Type: "cluster", Cluster: &K8sCluster{}}},
		},
	}
	// :8081 bound by our child (100, marked dev); :3091 held by a foreign
	// process (200, unmarked); :6060 nothing listening.
	f := fakeFacts{
		env:  map[int][]string{100: marker("dev", "api"), 200: {"PATH=/x"}},
		ppid: map[int]int{100: 1, 200: 1},
	}
	listening := listeningSet(8081, 3091)
	resolve := func(p int) int {
		switch p {
		case 8081:
			return 100
		case 3091:
			return 200
		default:
			return 0
		}
	}

	rs := evalHostReadiness(entities, "dev", nil, listening, resolve, f)
	if len(rs) != 3 { // one per api port; noport + cluster-svc contribute none
		t.Fatalf("got %d results, want 3 (one per api bind port): %+v", len(rs), rs)
	}
	byPort := map[int]portReadyState{}
	for _, r := range rs {
		if r.name != "api" {
			t.Errorf("unexpected service in readiness set: %q", r.name)
		}
		byPort[r.port] = r.state
	}
	if byPort[8081] != portReadyOurs {
		t.Errorf(":8081 = %v, want ours", byPort[8081])
	}
	if byPort[3091] != portReadyForeign {
		t.Errorf(":3091 = %v, want foreign", byPort[3091])
	}
	if byPort[6060] != portReadyNobody {
		t.Errorf(":6060 = %v, want nobody", byPort[6060])
	}

	// Only the not-ours ports keep the gate waiting / name the failure.
	unready := hostReadyUnready(rs)
	if len(unready) != 2 {
		t.Fatalf("unready = %d, want 2 (the foreign + the nobody)", len(unready))
	}
	msg := hostReadyError("dev", unready).Error()
	for _, want := range []string{"api", "3091", "6060", "forge up --env=dev --restart"} {
		if !strings.Contains(msg, want) {
			t.Errorf("readiness error missing %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "stale/foreign") || !strings.Contains(msg, "failed to bind") {
		t.Errorf("readiness error should distinguish foreign vs nobody:\n%s", msg)
	}
	// :8081 (ours) must NOT appear as a failure.
	if strings.Contains(msg, "8081") {
		t.Errorf("readiness error names an already-bound port:\n%s", msg)
	}

	// All ports ours → nothing unready (the healthy pass).
	allOurs := evalHostReadiness(entities, "dev", nil, func(int) bool { return true },
		func(int) int { return 100 }, f)
	if u := hostReadyUnready(allOurs); len(u) != 0 {
		t.Errorf("all-ours: want no unready, got %+v", u)
	}

	// --target scopes the gate: targeting only the no-port service yields
	// nothing to wait on.
	if got := evalHostReadiness(entities, "dev", []string{"noport"}, listening, resolve, f); len(got) != 0 {
		t.Errorf("target=noport: want 0 results, got %+v", got)
	}
}

// TestWaitHostServicesReady_NoPortsIsInstantPass pins the fast path: when no
// host service declares a bind port there is nothing to gate, so the wrapper
// returns nil immediately without any real socket/lsof work or polling.
func TestWaitHostServicesReady_NoPortsIsInstantPass(t *testing.T) {
	e := &KCLEntities{Services: []ServiceEntity{
		{Name: "noport", Deploy: DeployConfigEntity{Type: "host", Host: &HostDeploy{}}},
		{Name: "cluster-svc", Deploy: DeployConfigEntity{Type: "cluster", Cluster: &K8sCluster{}}},
	}}
	done := make(chan error, 1)
	go func() { done <- waitHostServicesReady(e, "dev", nil, hostReadyTimeout, hostReadyPoll) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("no declared ports: want nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitHostServicesReady did not return promptly when no ports are declared")
	}
}
