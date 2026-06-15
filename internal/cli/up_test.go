package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/spf13/pflag"
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
			name: "go-run default",
			svc: ServiceEntity{Name: "api", Deploy: DeployConfigEntity{
				Type: "host", Host: &HostDeploy{Runner: "go-run"},
			}},
			want: []string{"go", "run", "./cmd", "server", "api"},
		},
		{
			name: "empty runner defaults to go-run",
			svc: ServiceEntity{Name: "api", Deploy: DeployConfigEntity{
				Type: "host", Host: &HostDeploy{Runner: ""},
			}},
			want: []string{"go", "run", "./cmd", "server", "api"},
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
			cmd, _, err := buildHostServiceCmd(ctx, nil, c.svc, "dev")
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
	cmd, _, err := buildHostServiceCmd(context.Background(), cfg, svc, "dev-host")
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
// the host is the Gateway API ingress path now (forge dev urls); the
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
	cmd := buildFrontendCmd(context.Background(), fe, "dev", parent)

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
	cmd := buildFrontendCmd(context.Background(), fe, "dev", parent)

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
	cmd := buildFrontendCmd(context.Background(), fe, "dev", parent)
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
	cmd := buildFrontendCmd(context.Background(), fe, "dev", parent)

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
	if got := hostEnvPort(nil); got != "" {
		t.Errorf("nil host: got %q, want empty", got)
	}
	host := &HostDeploy{EnvVars: []KCLEnvVar{
		{Name: "DATABASE_URL", Value: "postgres://x"},
		{Name: "PORT", Value: "8080"},
	}}
	if got := hostEnvPort(host); got != "8080" {
		t.Errorf("hostEnvPort: got %q, want 8080", got)
	}
	// config_map_ref-only PORT (no inline value) yields no URL.
	refHost := &HostDeploy{EnvVars: []KCLEnvVar{
		{Name: "PORT", ConfigMapRef: "cfg", ConfigMapKey: "PORT"},
	}}
	if got := hostEnvPort(refHost); got != "" {
		t.Errorf("hostEnvPort ref-only: got %q, want empty", got)
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
