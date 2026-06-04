package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
// `forge run <svc>`: forge.yaml environments[<env>].config must reach
// the host child process via cmd.Env just like the run path does. The
// host phase previously dropped this layer (the call site took a `_
// string` env and passed nil to LayerHostEnv); this test guards against
// regressing back to that shape.
func TestBuildHostServiceCmd_LayersProjectConfig(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `name: testproj
module_path: github.com/example/testproj
version: "0.1.0"
binary: shared
services:
  - name: api
    type: go_service
    path: handlers/api
    port: 8080
environments:
  - name: dev-host
    type: local
    config:
      environment: development
      log_level: debug
`
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
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
	got, err := upLogPath("dev", "pf:admin-server:8080")
	if err != nil {
		t.Fatalf("upLogPath: %v", err)
	}
	// Colons must be replaced so the path is safe.
	if strings.Contains(got, ":") {
		t.Errorf("upLogPath returned unsanitised path %q", got)
	}
	if !strings.HasSuffix(got, "pf_admin-server_8080.log") {
		t.Errorf("upLogPath: got %q, want pf_admin-server_8080.log suffix", got)
	}
}
