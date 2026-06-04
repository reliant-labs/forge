package hostlaunch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildCmd_RunnerMatrix covers each runner dispatch path. The
// matrix is the union of `forge run <svc>` and `forge up` host-phase
// cases from before the collapse — running it once here verifies the
// shared dispatch hasn't drifted from either historical impl.
func TestBuildCmd_RunnerMatrix(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		svc  string
		spec RunnerSpec
		want []string
	}{
		{
			name: "empty runner falls through to go-run",
			svc:  "api",
			spec: RunnerSpec{},
			want: []string{"go", "run", "./cmd", "server", "api"},
		},
		{
			name: "go-run explicit",
			svc:  "api",
			spec: RunnerSpec{Runner: "go-run"},
			want: []string{"go", "run", "./cmd", "server", "api"},
		},
		{
			name: "unknown runner falls through to go-run",
			svc:  "api",
			spec: RunnerSpec{Runner: "tilt"},
			want: []string{"go", "run", "./cmd", "server", "api"},
		},
		{
			name: "air default config",
			svc:  "api",
			spec: RunnerSpec{Runner: "air"},
			want: []string{"air", "-c", ".air.toml"},
		},
		{
			name: "air custom config",
			svc:  "api",
			spec: RunnerSpec{Runner: "air", AirConfig: "configs/api.air.toml"},
			want: []string{"air", "-c", "configs/api.air.toml"},
		},
		{
			name: "binary runs ./bin/<svc>",
			svc:  "admin-server",
			spec: RunnerSpec{Runner: "binary"},
			want: []string{"./bin/admin-server"},
		},
		{
			name: "delve default port",
			svc:  "api",
			spec: RunnerSpec{Runner: "delve"},
			want: []string{
				"dlv", "exec", "--headless", "--listen=:2345",
				"--api-version=2", "--accept-multiclient", "--continue",
				"./bin/api",
			},
		},
		{
			name: "delve custom port",
			svc:  "api",
			spec: RunnerSpec{Runner: "delve", DelvePort: 4567},
			want: []string{
				"dlv", "exec", "--headless", "--listen=:4567",
				"--api-version=2", "--accept-multiclient", "--continue",
				"./bin/api",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := BuildCmd(ctx, c.svc, c.spec)
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

// TestIsKnownRunner: only the four documented runners are known; empty
// counts as known (the legacy go-run shape).
func TestIsKnownRunner(t *testing.T) {
	for _, ok := range []string{"", "go-run", "air", "binary", "delve"} {
		if !IsKnownRunner(ok) {
			t.Errorf("IsKnownRunner(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"tilt", "Air", "GO-RUN", "x"} {
		if IsKnownRunner(bad) {
			t.Errorf("IsKnownRunner(%q) = true, want false", bad)
		}
	}
}

// TestLoadSecretsFile covers the three branches of the secrets-file
// loader: empty path → no-op; valid file → parsed; missing/unreadable
// → propagated error. The "secrets stay external to KCL" design relies
// on the empty-path no-op so services without secrets don't need to
// declare anything.
func TestLoadSecretsFile_EmptyPath(t *testing.T) {
	got, err := LoadSecretsFile("")
	if err != nil {
		t.Fatalf("empty path: want (nil, nil), got err=%v", err)
	}
	if got != nil {
		t.Errorf("empty path: want nil map, got %v", got)
	}
}

func TestLoadSecretsFile_Parses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.dev.secrets")
	if err := os.WriteFile(path, []byte("STRIPE=sk_test_x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadSecretsFile(path)
	if err != nil {
		t.Fatalf("LoadSecretsFile: %v", err)
	}
	if got["STRIPE"] != "sk_test_x" {
		t.Errorf("got %v", got)
	}
}

func TestLoadSecretsFile_Missing(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadSecretsFile(filepath.Join(dir, "does-not-exist"))
	if !os.IsNotExist(err) {
		t.Errorf("want os.ErrNotExist, got %v", err)
	}
}

// TestLayerHostEnv pins the layering contract: projectConfig first,
// then secrets on top, then env_vars (KCL) on top so KCL wins on
// conflict; base os.Environ() always wins last (developer shell
// override). The conflict ordering is the load-bearing reproducibility
// invariant — config can't drift because the secrets file accidentally
// shadows a KCL value, and forge.yaml config never silently shadows
// `.env.<env>` developer overrides.
func TestLayerHostEnv(t *testing.T) {
	base := []string{"PATH=/usr/bin", "EDITOR=vim"}
	projectConfig := map[string]string{
		"LOG_LEVEL":   "info",      // overridden by secrets then envVars
		"ENVIRONMENT": "developmt", // pass-through (typo intentional to spot leaks)
	}
	secrets := map[string]string{
		"LOG_LEVEL": "trace",     // overridden by envVars; overrides projectConfig
		"STRIPE":    "sk_test_x", // pass-through
	}
	envVars := map[string]string{
		"LOG_LEVEL":    "debug",       // wins over secrets and projectConfig
		"DATABASE_URL": "postgres://", // pass-through
		"PATH":         "/should/lose",
	}
	got := LayerHostEnv(base, projectConfig, secrets, envVars)

	want := []string{
		"PATH=/usr/bin", // base wins
		"LOG_LEVEL=debug",
		"STRIPE=sk_test_x",
		"DATABASE_URL=postgres://",
		"ENVIRONMENT=developmt",
	}
	for _, w := range want {
		found := false
		for _, kv := range got {
			if kv == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing %q in %v", w, got)
		}
	}
	// Verify the override didn't leak.
	for _, kv := range got {
		if kv == "LOG_LEVEL=trace" {
			t.Errorf("secrets value leaked: %v", got)
		}
		if kv == "LOG_LEVEL=info" {
			t.Errorf("projectConfig value leaked: %v", got)
		}
		if kv == "PATH=/should/lose" {
			t.Errorf("base PATH overridden: %v", got)
		}
	}
}

// TestLayerHostEnv_ProjectConfigOverriddenBySecrets pins the
// `.env.<env>` > forge.yaml config precedence: when both layers
// declare the same key, the gitignored dotenv (developer-local
// override) wins so a developer can shadow a committed forge.yaml
// value without editing tracked files.
func TestLayerHostEnv_ProjectConfigOverriddenBySecrets(t *testing.T) {
	base := []string{"PATH=/usr/bin"}
	projectConfig := map[string]string{
		"LOG_LEVEL": "info", // from forge.yaml
	}
	secrets := map[string]string{
		"LOG_LEVEL": "debug", // from .env.<env> — wins
	}
	got := LayerHostEnv(base, projectConfig, secrets, nil)

	for _, kv := range got {
		if kv == "LOG_LEVEL=info" {
			t.Errorf("forge.yaml value leaked through .env.<env>: %v", got)
		}
	}
	found := false
	for _, kv := range got {
		if kv == "LOG_LEVEL=debug" {
			found = true
		}
	}
	if !found {
		t.Errorf("LOG_LEVEL=debug missing from final env: %v", got)
	}
}

// TestLayerHostEnv_NilProjectConfig confirms the new layer is optional
// — passing nil keeps the legacy two-layer (secrets, envVars) shape so
// callers that don't surface forge.yaml config (e.g. `forge up` host
// phase before it adopts the new layer) compile and behave unchanged.
func TestLayerHostEnv_NilProjectConfig(t *testing.T) {
	base := []string{"PATH=/usr/bin"}
	secrets := map[string]string{"STRIPE": "sk_test_x"}
	envVars := map[string]string{"LOG_LEVEL": "debug"}
	got := LayerHostEnv(base, nil, secrets, envVars)

	wants := []string{"PATH=/usr/bin", "STRIPE=sk_test_x", "LOG_LEVEL=debug"}
	for _, w := range wants {
		found := false
		for _, kv := range got {
			if kv == w {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q in %v", w, got)
		}
	}
}

// TestPIDPath confirms the canonical $HOME/.cache/forge/run/<svc>.pid
// location — `forge run <svc> stop` depends on this convention.
func TestPIDPath(t *testing.T) {
	got, err := PIDPath("admin-server")
	if err != nil {
		t.Fatalf("PIDPath: %v", err)
	}
	if !strings.HasSuffix(got, "/.cache/forge/run/admin-server.pid") {
		t.Errorf("want path ending in /.cache/forge/run/admin-server.pid, got %q", got)
	}
}

// TestReadDotEnvFile_BasicShapes covers the small parser the host-mode
// runner uses to layer .env.dev onto the child process: comments,
// blank lines, quoted values, `export` prefixes, and unquoted strings.
func TestReadDotEnvFile_BasicShapes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.dev")
	content := `# leading comment
EMPTY=
SIMPLE=value
QUOTED="with spaces"
SINGLE_QUOTED='another value'
export EXPORTED=ok
WITH_HASH=val#not-a-comment
   PADDED = value-with-spaces

# trailing comment
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadDotEnvFile(path)
	if err != nil {
		t.Fatalf("ReadDotEnvFile: %v", err)
	}
	want := map[string]string{
		"EMPTY":         "",
		"SIMPLE":        "value",
		"QUOTED":        "with spaces",
		"SINGLE_QUOTED": "another value",
		"EXPORTED":      "ok",
		"WITH_HASH":     "val#not-a-comment",
		"PADDED":        "value-with-spaces",
	}
	if len(got) != len(want) {
		t.Errorf("len(got) = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestReadDotEnvFile_Missing(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadDotEnvFile(filepath.Join(dir, "does-not-exist"))
	if !os.IsNotExist(err) {
		t.Errorf("want os.ErrNotExist, got %v", err)
	}
}

// TestMergeEnv: base wins on conflicts (developer-shell-override
// semantics); non-conflicting extras are appended.
func TestMergeEnv(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/dev", "EDITOR=vim"}
	extra := map[string]string{
		"PATH":     "/should/lose",   // collides with base
		"DATABASE": "postgres://...", // new
	}
	got := MergeEnv(extra, base)

	// base keys come first, unchanged
	if got[0] != "PATH=/usr/bin" {
		t.Errorf("PATH override: got %q, want PATH=/usr/bin", got[0])
	}
	// new key gets appended
	found := false
	for _, kv := range got {
		if kv == "DATABASE=postgres://..." {
			found = true
		}
	}
	if !found {
		t.Errorf("DATABASE not appended; got %v", got)
	}
}
