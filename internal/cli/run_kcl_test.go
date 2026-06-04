package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/hostlaunch"
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
// admin-server as host with runner=air, env_vars carrying LOG_LEVEL +
// DATABASE_URL, and secrets_file=.env.dev.secrets.
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
	if host.SecretsFile != ".env.dev.secrets" {
		t.Errorf("admin-server secrets_file: got %q, want .env.dev.secrets", host.SecretsFile)
	}
	if got := len(host.EnvVars); got != 2 {
		t.Errorf("admin-server env_vars count: got %d, want 2", got)
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

// TestHostEnvVarsToMap covers the projection from HostDeploy.EnvVars
// to the flat NAME→VALUE map. Inline-value entries pass through;
// secret_ref / config_map_ref entries (which have no host equivalent)
// drop; nil host returns empty (not nil) so callers don't need a
// guard.
func TestHostEnvVarsToMap(t *testing.T) {
	t.Run("nil host returns empty map", func(t *testing.T) {
		got := hostEnvVarsToMap(nil)
		if got == nil {
			t.Fatal("got nil, want empty map")
		}
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})

	t.Run("inline values only", func(t *testing.T) {
		host := &HostDeploy{
			EnvVars: []KCLEnvVar{
				{Name: "LOG_LEVEL", Value: "debug"},
				{Name: "DATABASE_URL", Value: "postgres://x"},
			},
		}
		got := hostEnvVarsToMap(host)
		if got["LOG_LEVEL"] != "debug" || got["DATABASE_URL"] != "postgres://x" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("empty-name and empty-value entries dropped", func(t *testing.T) {
		host := &HostDeploy{
			EnvVars: []KCLEnvVar{
				{Name: "OK", Value: "yes"},
				{Name: "", Value: "no-name"},
				{Name: "EMPTY_VALUE", Value: ""},
			},
		}
		got := hostEnvVarsToMap(host)
		if len(got) != 1 {
			t.Errorf("want 1 entry, got %v", got)
		}
		if got["OK"] != "yes" {
			t.Errorf("OK: got %q", got["OK"])
		}
	})
}

// TestHostEnvComposition_EnvVarsOnly: secrets_file empty, env_vars
// populated → final env carries env_vars only (plus os.Environ baseline).
func TestHostEnvComposition_EnvVarsOnly(t *testing.T) {
	host := &HostDeploy{
		EnvVars: []KCLEnvVar{
			{Name: "FROM_KCL", Value: "kcl-val"},
		},
	}
	secrets, err := hostlaunch.LoadSecretsFile("")
	if err != nil {
		t.Fatalf("LoadSecretsFile(\"\"): %v", err)
	}
	if secrets != nil {
		t.Errorf("empty path: want nil map, got %v", secrets)
	}
	envVars := hostEnvVarsToMap(host)
	final := hostlaunch.LayerHostEnv([]string{"PATH=/usr/bin"}, nil, secrets, envVars)
	if !envSliceContains(final, "FROM_KCL=kcl-val") {
		t.Errorf("FROM_KCL missing from final env: %v", final)
	}
}

// TestHostEnvComposition_SecretsFileOnly: secrets file present, no
// env_vars → final env carries secrets-file values.
func TestHostEnvComposition_SecretsFileOnly(t *testing.T) {
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, ".env.dev.secrets")
	const content = `STRIPE_SECRET_KEY=sk_test_xxx
SUPABASE_ANON_KEY=anon_xxx
`
	if err := os.WriteFile(secretsPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write secrets: %v", err)
	}
	secrets, err := hostlaunch.LoadSecretsFile(secretsPath)
	if err != nil {
		t.Fatalf("LoadSecretsFile: %v", err)
	}
	if secrets["STRIPE_SECRET_KEY"] != "sk_test_xxx" {
		t.Errorf("STRIPE: got %q", secrets["STRIPE_SECRET_KEY"])
	}
	final := hostlaunch.LayerHostEnv([]string{"PATH=/usr/bin"}, nil, secrets, nil)
	if !envSliceContains(final, "STRIPE_SECRET_KEY=sk_test_xxx") {
		t.Errorf("STRIPE missing from final env: %v", final)
	}
	if !envSliceContains(final, "SUPABASE_ANON_KEY=anon_xxx") {
		t.Errorf("SUPABASE missing from final env: %v", final)
	}
}

// TestHostEnvComposition_BothEnvVarsWinOnConflict: when both layers
// declare the same key, env_vars (KCL) overrides secrets_file. This is
// the load-bearing reproducibility invariant — config can't drift
// between developer machines because the gitignored secrets file
// accidentally shadows a KCL value.
func TestHostEnvComposition_BothEnvVarsWinOnConflict(t *testing.T) {
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, ".env.dev.secrets")
	const content = `LOG_LEVEL=trace
STRIPE_SECRET_KEY=sk_test_xxx
`
	if err := os.WriteFile(secretsPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write secrets: %v", err)
	}
	secrets, _ := hostlaunch.LoadSecretsFile(secretsPath)
	envVars := hostEnvVarsToMap(&HostDeploy{
		EnvVars: []KCLEnvVar{
			{Name: "LOG_LEVEL", Value: "debug"}, // collides with secrets_file
		},
	})
	final := hostlaunch.LayerHostEnv([]string{"PATH=/usr/bin"}, nil, secrets, envVars)

	// KCL env_vars wins on LOG_LEVEL.
	if !envSliceContains(final, "LOG_LEVEL=debug") {
		t.Errorf("LOG_LEVEL: KCL should win; final=%v", final)
	}
	if envSliceContains(final, "LOG_LEVEL=trace") {
		t.Errorf("LOG_LEVEL: secrets value leaked through; final=%v", final)
	}
	// Non-conflicting secret still appears.
	if !envSliceContains(final, "STRIPE_SECRET_KEY=sk_test_xxx") {
		t.Errorf("STRIPE: should pass through; final=%v", final)
	}
}

// TestHostEnvComposition_SecretsMissingIsWarnNotError: a missing
// secrets file is non-fatal — the runner warns and continues.
// `forge run` / `forge up` should still launch the subprocess.
func TestHostEnvComposition_SecretsMissingIsWarnNotError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.secrets")

	_, err := hostlaunch.LoadSecretsFile(missing)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("want os.ErrNotExist, got %v", err)
	}
}

// TestHostEnvComposition_SecretsUnreadable: an existing-but-unreadable
// secrets file (chmod 000) propagates the error. The CLI surfaces this
// as a fatal startup error because it signals an environment problem
// the developer needs to fix rather than the "haven't created secrets
// yet" non-fatal case.
func TestHostEnvComposition_SecretsUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits don't gate read on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file mode bits")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, ".env.dev.secrets")
	if err := os.WriteFile(path, []byte("STRIPE=x\n"), 0o000); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) }) // tempdir cleanup

	_, err := hostlaunch.LoadSecretsFile(path)
	if err == nil {
		t.Fatal("want error reading mode-000 file, got nil")
	}
	if os.IsNotExist(err) {
		t.Errorf("got NotExist; want permission error: %v", err)
	}
}

// TestLoadProjectConfigEnv_ProjectsForgeYAMLConfig confirms forge.yaml
// `environments[<env>].config` is read and projected to env-var
// strings — the layer downstream cp-forge flagged as missing from the
// host-mode runner. snake_case keys are uppercased to SCREAMING_SNAKE
// when no proto descriptor is available (the common fresh-project case).
func TestLoadProjectConfigEnv_ProjectsForgeYAMLConfig(t *testing.T) {
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
      log_format: text
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

	got := loadProjectConfigEnv(cfg, "dev-host")
	want := map[string]string{
		"ENVIRONMENT": "development",
		"LOG_FORMAT":  "text",
		"LOG_LEVEL":   "debug",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("got[%q] = %q, want %q (full=%v)", k, got[k], v, got)
		}
	}
}

// TestLoadProjectConfigEnv_UnknownEnvReturnsEmpty: missing env name
// yields an empty map (not an error) so the caller can pass the result
// straight to LayerHostEnv without guarding.
func TestLoadProjectConfigEnv_UnknownEnvReturnsEmpty(t *testing.T) {
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
  - name: dev
    type: local
`
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	t.Chdir(dir)

	cfg, err := loadProjectConfig()
	if err != nil {
		t.Fatalf("loadProjectConfig: %v", err)
	}

	got := loadProjectConfigEnv(cfg, "nonexistent")
	if got == nil {
		t.Error("got nil, want empty map (callers shouldn't need to nil-check)")
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty map", got)
	}
}

// TestHostEnvComposition_ProjectConfigUnderSecretsFile pins the
// precedence chain `forge run` host-mode applies:
//
//	os.Environ() ⊕ forge.yaml config ⊕ .env.<env> ⊕ KCL env_vars
//
// (base wins; among extras, later overrides earlier on key conflict.)
// This is the cp-forge friction-log regression: forge.yaml config must
// be visible to host-mode services, and `.env.<env>` developer-local
// overrides must win over committed forge.yaml values.
func TestHostEnvComposition_ProjectConfigUnderSecretsFile(t *testing.T) {
	// forge.yaml config (lowest extra layer).
	projectConfig := map[string]string{
		"ENVIRONMENT": "development", // pass-through (no conflict)
		"LOG_LEVEL":   "info",        // overridden by .env.<env>
		"LOG_FORMAT":  "text",        // overridden by KCL env_vars
	}
	// .env.<env> secrets (middle extra layer).
	secrets := map[string]string{
		"LOG_LEVEL":  "debug",      // wins over forge.yaml; overridden by KCL
		"STRIPE_KEY": "sk_test_xx", // pass-through (no conflict)
	}
	// KCL env_vars (top extra layer).
	envVars := map[string]string{
		"LOG_FORMAT": "json", // wins over forge.yaml
	}
	base := []string{"PATH=/usr/bin"}

	final := hostlaunch.LayerHostEnv(base, projectConfig, secrets, envVars)

	wants := map[string]string{
		"PATH":        "/usr/bin",   // base wins
		"ENVIRONMENT": "development", // forge.yaml passes through
		"LOG_LEVEL":   "debug",      // .env.<env> wins over forge.yaml
		"LOG_FORMAT":  "json",       // KCL wins over forge.yaml
		"STRIPE_KEY":  "sk_test_xx", // .env.<env> passes through
	}
	for k, v := range wants {
		if !envSliceContains(final, k+"="+v) {
			t.Errorf("want %s=%s; final=%v", k, v, final)
		}
	}
	// Verify no overridden value leaked through.
	leaks := []string{"LOG_LEVEL=info", "LOG_FORMAT=text"}
	for _, leak := range leaks {
		if envSliceContains(final, leak) {
			t.Errorf("value leaked: %q in final=%v", leak, final)
		}
	}
}

// TestHostEnvComposition_DotEnvWinsOverForgeYAML is the smallest possible
// statement of the load-bearing precedence rule: when forge.yaml config
// and `.env.<env>` declare the same key, `.env.<env>` wins. forge.yaml
// is committed and non-secret; `.env.<env>` is gitignored and carries
// developer-local overrides — the dev-local layer has to win or the
// override is invisible.
func TestHostEnvComposition_DotEnvWinsOverForgeYAML(t *testing.T) {
	projectConfig := map[string]string{"LOG_LEVEL": "info"}      // committed
	secrets := map[string]string{"LOG_LEVEL": "trace"}           // dev-local override
	final := hostlaunch.LayerHostEnv([]string{"PATH=/usr/bin"}, projectConfig, secrets, nil)

	if !envSliceContains(final, "LOG_LEVEL=trace") {
		t.Errorf("LOG_LEVEL=trace missing from final env: %v", final)
	}
	if envSliceContains(final, "LOG_LEVEL=info") {
		t.Errorf("forge.yaml value leaked through .env override: %v", final)
	}
}

// envSliceContains is a tiny test helper.
func envSliceContains(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

// writeFile is a tiny helper for the tests above — wraps os.WriteFile
// with a 0644 mode so the call site stays terse.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
