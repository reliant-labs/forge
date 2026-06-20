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

// TestBuildCmd_WorkingDir pins the cross-repo cmd.Dir contract. The
// motivating case: a project declares `WorkingDir: "../sibling-repo"`
// on a HostDeploy whose Air config lives in a sibling repo and resolves
// build paths relative to that repo's root. With ProjectDir set to the
// caller's forge project root, the launched subprocess must chdir to
// the sibling so Air's `build_cmd` paths resolve correctly.
//
// Four cases — empty (inherit parent cwd), absolute, relative+project,
// relative-without-project (verbatim fallback). The runner is "air"
// throughout but the cwd resolution is runner-agnostic so the dispatch
// matrix (go-run/binary/delve) inherits the same behavior automatically.
func TestBuildCmd_WorkingDir(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		workingDir string
		projectDir string
		wantDir    string
	}{
		{
			name:       "empty WorkingDir leaves cmd.Dir empty (inherit parent cwd)",
			workingDir: "",
			projectDir: "/forge/project",
			wantDir:    "",
		},
		{
			name:       "absolute WorkingDir is used verbatim",
			workingDir: "/abs/sibling",
			projectDir: "/forge/project",
			wantDir:    "/abs/sibling",
		},
		{
			name:       "relative WorkingDir resolves against ProjectDir",
			workingDir: "../sibling",
			projectDir: "/forge/project",
			wantDir:    "/forge/sibling",
		},
		{
			name:       "relative WorkingDir with empty ProjectDir falls through verbatim",
			workingDir: "../sibling",
			projectDir: "",
			wantDir:    "../sibling",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := BuildCmd(ctx, "api", RunnerSpec{
				Runner:     "air",
				WorkingDir: c.workingDir,
				ProjectDir: c.projectDir,
			})
			if cmd.Dir != c.wantDir {
				t.Errorf("cmd.Dir: got %q, want %q", cmd.Dir, c.wantDir)
			}
		})
	}
}

// TestBuildCmd_WorkingDir_AppliesAcrossRunners confirms the cwd is set
// for every runner dispatch path (air, binary, delve, go-run), not just
// the runner that surfaced the cross-repo use case.
func TestBuildCmd_WorkingDir_AppliesAcrossRunners(t *testing.T) {
	ctx := context.Background()
	const projectDir = "/forge/project"
	const workingDir = "../sibling"
	const wantDir = "/forge/sibling"
	for _, runner := range []string{"air", "binary", "delve", "go-run", ""} {
		t.Run("runner="+runner, func(t *testing.T) {
			cmd := BuildCmd(ctx, "api", RunnerSpec{
				Runner:     runner,
				WorkingDir: workingDir,
				ProjectDir: projectDir,
			})
			if cmd.Dir != wantDir {
				t.Errorf("cmd.Dir: got %q, want %q", cmd.Dir, wantDir)
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

// TestLoadSecretsFile_CrossRepoPath pins the cross-repo dev-loop
// contract: `HostDeploy.secrets_file = "../sibling/.env"` resolves
// against the caller's working directory and loads the file even
// though the path escapes the project root. Multi-checkout setups
// (forge project here, second binary in a sibling repo) rely on
// this — a future path-traversal sanity check that rejects `..`
// segments would silently break them.
//
// We construct a parent + sibling layout under t.TempDir(), chdir
// into the "project" dir, point secrets_file at "../sibling/.env",
// and assert the loader reads it. Symmetric with the WorkingDir
// cross-repo case already pinned in TestResolveWorkingDir.
func TestLoadSecretsFile_CrossRepoPath(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	sibling := filepath.Join(parent, "sibling")
	for _, d := range []string{project, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	const secretsBody = "DATABASE_URL=postgres://sibling/db\nNATS_PASSWORD=hunter2\n"
	if err := os.WriteFile(filepath.Join(sibling, ".env"), []byte(secretsBody), 0o644); err != nil {
		t.Fatalf("seed sibling .env: %v", err)
	}

	// LoadSecretsFile resolves relative paths against the caller's
	// working directory, mirroring how `forge run` invokes it after
	// chdir'ing into the project dir. Restore cwd on cleanup so the
	// rest of the suite isn't affected.
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatalf("chdir project: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	got, err := LoadSecretsFile("../sibling/.env")
	if err != nil {
		t.Fatalf("LoadSecretsFile(../sibling/.env): %v", err)
	}
	if got["DATABASE_URL"] != "postgres://sibling/db" {
		t.Errorf("DATABASE_URL = %q, want postgres://sibling/db", got["DATABASE_URL"])
	}
	if got["NATS_PASSWORD"] != "hunter2" {
		t.Errorf("NATS_PASSWORD = %q, want hunter2", got["NATS_PASSWORD"])
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

func TestBuildCmd_ExplicitCommandOverridesRunner(t *testing.T) {
	cmd := BuildCmd(context.Background(), "reliant-api-server", RunnerSpec{
		Runner:     "go-run", // should be ignored when Command is set
		Command:    []string{"go", "run", "./cmd/reliant", "server", "api"},
		WorkingDir: "../reliant",
		ProjectDir: "/projects/cp-forge",
	})
	want := []string{"go", "run", "./cmd/reliant", "server", "api"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args = %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, cmd.Args[i], want[i])
		}
	}
	if cmd.Dir != "/projects/reliant" {
		t.Errorf("cmd.Dir = %q, want sibling-resolved path", cmd.Dir)
	}
}

func TestBuildCmd_AirIgnoresCommand(t *testing.T) {
	// A command set alongside runner=air must NOT hijack air (admin-server
	// declares a documentation-only command next to its air runner).
	cmd := BuildCmd(context.Background(), "admin-server", RunnerSpec{
		Runner:    "air",
		AirConfig: ".air.admin-server.toml",
		Command:   []string{"./cp-forge", "server"},
	})
	if cmd.Args[0] != "air" {
		t.Fatalf("air runner hijacked by command: args=%v", cmd.Args)
	}
}
