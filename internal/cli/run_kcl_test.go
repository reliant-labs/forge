package cli

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestBuildRunHostCmd covers the runner-dispatch matrix that
// `forge run <svc>` uses when the env's KCL declares a runner for the
// service. The nil-host / empty-runner / unknown-runner cases all fall
// through to the legacy `go run ./cmd server <svc>` shape so projects
// without KCL render available stay working.
func TestBuildRunHostCmd(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		svc  string
		host *HostDeploy
		want []string
	}{
		{
			name: "nil host falls through to go run",
			svc:  "api",
			host: nil,
			want: []string{"go", "run", "./cmd", "server", "api"},
		},
		{
			name: "empty runner falls through to go run",
			svc:  "api",
			host: &HostDeploy{},
			want: []string{"go", "run", "./cmd", "server", "api"},
		},
		{
			name: "go-run",
			svc:  "api",
			host: &HostDeploy{Runner: "go-run"},
			want: []string{"go", "run", "./cmd", "server", "api"},
		},
		{
			name: "unknown runner falls through to go run",
			svc:  "api",
			host: &HostDeploy{Runner: "tilt"},
			want: []string{"go", "run", "./cmd", "server", "api"},
		},
		{
			name: "air uses default .air.toml when AirConfig is empty",
			svc:  "api",
			host: &HostDeploy{Runner: "air"},
			want: []string{"air", "-c", ".air.toml"},
		},
		{
			name: "air respects custom AirConfig",
			svc:  "api",
			host: &HostDeploy{Runner: "air", AirConfig: "configs/api.air.toml"},
			want: []string{"air", "-c", "configs/api.air.toml"},
		},
		{
			name: "binary runs ./bin/<svc>",
			svc:  "admin-server",
			host: &HostDeploy{Runner: "binary"},
			want: []string{"./bin/admin-server"},
		},
		{
			name: "delve uses default port 2345 when DelvePort unset",
			svc:  "api",
			host: &HostDeploy{Runner: "delve"},
			want: []string{
				"dlv", "exec", "--headless", "--listen=:2345",
				"--api-version=2", "--accept-multiclient", "--continue",
				"./bin/api",
			},
		},
		{
			name: "delve respects custom DelvePort",
			svc:  "api",
			host: &HostDeploy{Runner: "delve", DelvePort: 4567},
			want: []string{
				"dlv", "exec", "--headless", "--listen=:4567",
				"--api-version=2", "--accept-multiclient", "--continue",
				"./bin/api",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := buildRunHostCmd(ctx, c.svc, c.host)
			got := cmd.Args
			if len(got) != len(c.want) {
				t.Fatalf("args len: got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("args[%d]: got %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestLookupKCLHostDeploy_NoEnvReturnsNil confirms the helper short-
// circuits when env is empty — the legacy `forge run <svc>` codepath
// (no --env, no project dir override) shouldn't shell out to KCL.
func TestLookupKCLHostDeploy_NoEnvReturnsNil(t *testing.T) {
	if host := lookupKCLHostDeploy(context.Background(), "", "api"); host != nil {
		t.Errorf("want nil, got %+v", host)
	}
}

// TestLookupKCLHostDeploy_FixtureDispatch uses the
// FORGE_KCL_RENDER_FIXTURE env-var hook to exercise the lookup against
// a static JSON file so the unit doesn't need a real kcl binary. The
// fixture (sampleKCLJSON from kcl_render_test.go) declares
// admin-server as host with runner=air and workspace-proxy as cluster.
func TestLookupKCLHostDeploy_FixtureDispatch(t *testing.T) {
	dir := t.TempDir()
	fixture := dir + "/render.json"
	if err := writeFile(fixture, sampleKCLJSON); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", fixture)

	// Host-mode service: returns the populated HostDeploy.
	host := lookupKCLHostDeploy(context.Background(), "dev", "admin-server")
	if host == nil {
		t.Fatal("admin-server: want non-nil HostDeploy, got nil")
	}
	if host.Runner != "air" {
		t.Errorf("admin-server runner: got %q, want air", host.Runner)
	}
	if host.EnvFile != ".env.dev" {
		t.Errorf("admin-server env file: got %q, want .env.dev", host.EnvFile)
	}

	// Cluster-mode service: returns nil so the caller falls through.
	if got := lookupKCLHostDeploy(context.Background(), "dev", "workspace-proxy"); got != nil {
		t.Errorf("workspace-proxy: cluster service should return nil, got %+v", got)
	}

	// Unknown service: returns nil.
	if got := lookupKCLHostDeploy(context.Background(), "dev", "nope"); got != nil {
		t.Errorf("unknown service should return nil, got %+v", got)
	}

	// Sanity: the fixture is reachable.
	if !strings.Contains(sampleKCLJSON, "admin-server") {
		t.Fatal("fixture sanity")
	}
}

// writeFile is a tiny helper for the tests above — wraps os.WriteFile
// with a 0644 mode so the call site stays terse.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
