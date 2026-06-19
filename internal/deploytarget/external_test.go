package deploytarget

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExternal_Deploy_SecretsMergedEnvFileWins confirms resolved
// secrets reach the `sh -c` process env as the BASE layer, with
// env_file entries overriding on key conflict.
func TestExternal_Deploy_SecretsMergedEnvFileWins(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env.prod")
	if err := os.WriteFile(envFile, []byte("SHARED=from_file\nFILE_ONLY=f\n"), 0o644); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	r := &fakeRunner{}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	spec := &ExternalSpec{DeployCmd: "echo deploy ${SERVICE}", EnvFile: envFile}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "external",
		ImageTag:   "v1.0.0",
		Services: []ResolvedService{
			{
				Name:     "edge",
				External: spec,
				Secrets: map[string]string{
					"SHARED":      "from_secret",
					"SECRET_ONLY": "s",
				},
			},
		},
	}
	if err := p.Deploy(context.Background(), group); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(r.envCalls) == 0 || r.envCalls[0] == nil {
		t.Fatalf("expected a non-nil env overlay on the first RunWithEnv call, got %v", r.envCalls)
	}
	got := r.envCalls[0]
	if got["SHARED"] != "from_file" {
		t.Errorf("env_file should win on conflict: SHARED = %q, want from_file", got["SHARED"])
	}
	if got["SECRET_ONLY"] != "s" {
		t.Errorf("secret-only key missing: SECRET_ONLY = %q, want s", got["SECRET_ONLY"])
	}
	if got["FILE_ONLY"] != "f" {
		t.Errorf("file-only key missing: FILE_ONLY = %q, want f", got["FILE_ONLY"])
	}
}

// TestExternal_Deploy_HappyPath confirms the deploy_cmd is exec'd via
// `sh -c` with the documented ${X} tokens substituted, and that the
// state file lands on success.
func TestExternal_Deploy_HappyPath(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "external",
		ImageTag:   "v1.2.3",
		Services: []ResolvedService{
			{
				Name: "edge",
				External: &ExternalSpec{
					Image:     "ghcr.io/x/edge",
					DeployCmd: "flyctl deploy --image ${IMAGE}:${TAG} --app ${SERVICE} --env ${ENV}",
				},
			},
		},
	}
	if err := p.Deploy(context.Background(), group); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("call count: want 1 (deploy_cmd), got %d (%v)", len(r.calls), r.calls)
	}
	want := "sh -c flyctl deploy --image ghcr.io/x/edge:v1.2.3 --app edge --env prod"
	if r.calls[0] != want {
		t.Errorf("deploy call: want %q, got %q", want, r.calls[0])
	}
	statePath := filepath.Join(dir, ".forge/state/external-prod-edge.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var st DeployState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("parse state file: %v", err)
	}
	if st.Tag != "v1.2.3" || st.Image != "ghcr.io/x/edge" {
		t.Errorf("state file: want image=ghcr.io/x/edge tag=v1.2.3, got %+v", st)
	}
}

// TestExternal_Deploy_HealthCmdRuns confirms that a non-empty
// health_cmd is exec'd after the deploy succeeds, with substitution
// applied.
func TestExternal_Deploy_HealthCmdRuns(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "external",
		ImageTag:   "v9",
		Services: []ResolvedService{
			{
				Name: "edge",
				External: &ExternalSpec{
					Image:     "x/edge",
					DeployCmd: "flyctl deploy",
					HealthCmd: "flyctl status --app ${SERVICE}",
				},
			},
		},
	}
	if err := p.Deploy(context.Background(), group); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("call count: want 2 (deploy + health), got %d (%v)", len(r.calls), r.calls)
	}
	wantHealth := "sh -c flyctl status --app edge"
	if r.calls[1] != wantHealth {
		t.Errorf("health call: want %q, got %q", wantHealth, r.calls[1])
	}
}

// TestExternal_Deploy_CustomEnvMerges confirms user-declared env keys
// land in the substitution map alongside the built-ins, AND that
// built-ins win on conflict (so a user can't shadow ${IMAGE} etc.).
func TestExternal_Deploy_CustomEnvMerges(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "external",
		ImageTag:   "real-tag",
		Services: []ResolvedService{
			{
				Name: "edge",
				External: &ExternalSpec{
					Image:     "x/edge",
					DeployCmd: "echo region=${REGION} image=${IMAGE} tag=${TAG}",
					Env: map[string]string{
						"REGION": "us-east-1",
						// Try to shadow IMAGE — should be overridden by the
						// built-in (spec.Image).
						"IMAGE": "user-shadow-attempt",
					},
				},
			},
		},
	}
	if err := p.Deploy(context.Background(), group); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	want := "sh -c echo region=us-east-1 image=x/edge tag=real-tag"
	if r.calls[0] != want {
		t.Errorf("substitution: want %q, got %q", want, r.calls[0])
	}
}

// TestExternal_DeployFailure_NoStateWrite confirms a failing deploy
// does NOT write the state file — keeping the previously-recorded
// last-good tag intact.
func TestExternal_DeployFailure_NoStateWrite(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{
		runErrs: map[string]error{
			"sh -c flyctl": errors.New("deploy failed"),
		},
	}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "external",
		ImageTag:   "broken",
		Services: []ResolvedService{
			{
				Name:     "edge",
				External: &ExternalSpec{Image: "x/edge", DeployCmd: "flyctl deploy"},
			},
		},
	}
	if err := p.Deploy(context.Background(), group); err == nil {
		t.Fatal("expected deploy failure, got nil")
	}
	statePath := filepath.Join(dir, ".forge/state/external-prod-edge.json")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file should NOT exist after failure, got err=%v", err)
	}
}

// TestExternal_HealthCheckFails_NoStateWrite confirms that a failing
// health_cmd ALSO short-circuits before the state-file write — a
// deploy that "succeeded" but came up unhealthy shouldn't clobber the
// last good tag.
func TestExternal_HealthCheckFails_NoStateWrite(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{
		runErrs: map[string]error{
			"sh -c flyctl status": errors.New("unhealthy"),
		},
	}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "external",
		ImageTag:   "v1",
		Services: []ResolvedService{
			{
				Name: "edge",
				External: &ExternalSpec{
					Image:     "x/edge",
					DeployCmd: "flyctl deploy",
					HealthCmd: "flyctl status --app ${SERVICE}",
				},
			},
		},
	}
	err := p.Deploy(context.Background(), group)
	if err == nil {
		t.Fatal("expected health check failure, got nil")
	}
	if !strings.Contains(err.Error(), "health check") {
		t.Errorf("want 'health check' in error, got %v", err)
	}
	statePath := filepath.Join(dir, ".forge/state/external-prod-edge.json")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file should NOT exist after health failure, got err=%v", err)
	}
}

// TestExternal_Rollback_WithState confirms Rollback substitutes
// ${LAST_TAG} from the state file into rollback_cmd and exec's it.
func TestExternal_Rollback_WithState(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteDeployState(dir, "external", "prod", "edge", DeployState{
		Image: "x/edge",
		Tag:   "v1.0.0",
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	r := &fakeRunner{}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "external",
		Services: []ResolvedService{
			{
				Name: "edge",
				External: &ExternalSpec{
					Image:       "x/edge",
					DeployCmd:   "flyctl deploy --image ${IMAGE}:${TAG}",
					RollbackCmd: "flyctl deploy --image ${IMAGE}:${LAST_TAG} --app ${SERVICE}",
				},
			},
		},
	}
	if err := p.Rollback(context.Background(), group, "v9-current-broken"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("call count: want 1 (rollback_cmd), got %d (%v)", len(r.calls), r.calls)
	}
	// State-file tag wins over the dispatcher-supplied lastGoodTag.
	want := "sh -c flyctl deploy --image x/edge:v1.0.0 --app edge"
	if r.calls[0] != want {
		t.Errorf("rollback call: want %q, got %q", want, r.calls[0])
	}
}

// TestExternal_Rollback_NoState confirms Rollback errors loudly when
// there's no state file AND no fallback tag — guessing would risk
// shipping a regression.
func TestExternal_Rollback_NoState(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "external",
		Services: []ResolvedService{
			{
				Name: "edge",
				External: &ExternalSpec{
					Image:       "x/edge",
					DeployCmd:   "flyctl deploy",
					RollbackCmd: "flyctl deploy --image ${IMAGE}:${LAST_TAG}",
				},
			},
		},
	}
	err := p.Rollback(context.Background(), group, "")
	if err == nil {
		t.Fatal("expected error for missing state file + empty lastGoodTag, got nil")
	}
	if !strings.Contains(err.Error(), "no previous tag recorded") {
		t.Errorf("want 'no previous tag recorded', got %v", err)
	}
}

// TestExternal_Rollback_NoRollbackCmd confirms Rollback errors clearly
// when the user didn't declare a rollback_cmd — forge can't synthesise
// one for an arbitrary CLI.
func TestExternal_Rollback_NoRollbackCmd(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteDeployState(dir, "external", "prod", "edge", DeployState{
		Image: "x/edge",
		Tag:   "v1.0.0",
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	r := &fakeRunner{}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "external",
		Services: []ResolvedService{
			{
				Name: "edge",
				External: &ExternalSpec{
					Image:     "x/edge",
					DeployCmd: "flyctl deploy",
					// RollbackCmd intentionally empty.
				},
			},
		},
	}
	err := p.Rollback(context.Background(), group, "v0")
	if err == nil {
		t.Fatal("expected error for missing rollback_cmd, got nil")
	}
	if !strings.Contains(err.Error(), "no rollback_cmd declared") {
		t.Errorf("want 'no rollback_cmd declared', got %v", err)
	}
}

// TestExternal_Deploy_DryRun confirms --dry-run prints the resolved
// deploy_cmd + health_cmd lines but does NOT exec anything and does
// NOT write the state file. The trap this guards against: a user runs
// `forge deploy prod --dry-run` expecting a preview and instead ships
// their external CLI for real.
func TestExternal_Deploy_DryRun(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "external",
		ImageTag:   "v9",
		DryRun:     true,
		Services: []ResolvedService{
			{
				Name: "edge",
				External: &ExternalSpec{
					Image:     "x/edge",
					DeployCmd: "flyctl deploy --image ${IMAGE}:${TAG} --app ${SERVICE}",
					HealthCmd: "flyctl status --app ${SERVICE}",
				},
			},
		},
	}
	out := captureStdout(t, func() {
		if err := p.Deploy(context.Background(), group); err != nil {
			t.Fatalf("Deploy: %v", err)
		}
	})
	if len(r.calls) != 0 {
		t.Fatalf("dry-run should NOT exec, got %d call(s): %v", len(r.calls), r.calls)
	}
	wantLines := []string{
		"[DRY-RUN] would exec: sh -c flyctl deploy --image x/edge:v9 --app edge",
		"[DRY-RUN] would exec: sh -c flyctl status --app edge",
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("stdout should contain %q, got:\n%s", want, out)
		}
	}
	// And confirm no state file was written.
	statePath := filepath.Join(dir, ".forge/state/external-prod-edge.json")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file should NOT exist after dry-run, got err=%v", err)
	}
}

// TestExternal_Rollback_DryRun confirms the rollback dry-run path
// prints the substituted rollback_cmd and exec's nothing.
func TestExternal_Rollback_DryRun(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteDeployState(dir, "external", "prod", "edge", DeployState{
		Image: "x/edge",
		Tag:   "v1.0.0",
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	r := &fakeRunner{}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env: "prod", ProviderID: "external", DryRun: true,
		Services: []ResolvedService{
			{
				Name: "edge",
				External: &ExternalSpec{
					Image:       "x/edge",
					DeployCmd:   "flyctl deploy",
					RollbackCmd: "flyctl deploy --image ${IMAGE}:${LAST_TAG}",
				},
			},
		},
	}
	out := captureStdout(t, func() {
		if err := p.Rollback(context.Background(), group, ""); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
	})
	if len(r.calls) != 0 {
		t.Fatalf("dry-run rollback should NOT exec, got %d call(s): %v", len(r.calls), r.calls)
	}
	want := "[DRY-RUN] would exec: sh -c flyctl deploy --image x/edge:v1.0.0"
	if !strings.Contains(out, want) {
		t.Errorf("stdout should contain %q, got:\n%s", want, out)
	}
}

// TestExternal_Deploy_EnvFileMerges confirms a declared env_file is
// parsed and threaded onto the exec'd process's env. The user-supplied
// CLI sees API_KEY=secret-value without having to write `--env-file
// ${ENV_FILE}` in deploy_cmd.
func TestExternal_Deploy_EnvFileMerges(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env.prod")
	if err := os.WriteFile(envFile, []byte("API_KEY=secret-value\n# a comment\nREGION=us-east-1\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	r := &fakeRunner{}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env: "prod", ProviderID: "external", ImageTag: "v1",
		Services: []ResolvedService{
			{
				Name: "edge",
				External: &ExternalSpec{
					Image:     "x/edge",
					DeployCmd: "flyctl deploy",
					EnvFile:   envFile,
				},
			},
		},
	}
	if err := p.Deploy(context.Background(), group); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(r.envCalls) != 1 {
		t.Fatalf("want 1 RunWithEnv call, got %d", len(r.envCalls))
	}
	env := r.envCalls[0]
	if env["API_KEY"] != "secret-value" {
		t.Errorf("API_KEY: want secret-value, got %q", env["API_KEY"])
	}
	if env["REGION"] != "us-east-1" {
		t.Errorf("REGION: want us-east-1, got %q", env["REGION"])
	}
}

// TestExternal_Deploy_EnvFileMissing confirms a missing env_file path
// is a warning, not a hard error — same semantic hostlaunch's
// secrets_file uses. Lets users commit an env_file path that's
// optional on some dev machines.
func TestExternal_Deploy_EnvFileMissing(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env: "prod", ProviderID: "external", ImageTag: "v1",
		Services: []ResolvedService{
			{
				Name: "edge",
				External: &ExternalSpec{
					Image:     "x/edge",
					DeployCmd: "flyctl deploy",
					EnvFile:   filepath.Join(dir, "does-not-exist.env"),
				},
			},
		},
	}
	out := captureStdout(t, func() {
		if err := p.Deploy(context.Background(), group); err != nil {
			t.Fatalf("Deploy should not fail on missing env_file, got %v", err)
		}
	})
	if !strings.Contains(out, "env_file") || !strings.Contains(out, "not found") {
		t.Errorf("expected env_file missing warning in stdout, got:\n%s", out)
	}
	// The exec must still happen — the env_file is optional, not a gate.
	if len(r.calls) != 1 {
		t.Fatalf("deploy should still exec when env_file missing, got %d calls", len(r.calls))
	}
}

// TestExternal_EnvFileVar_StillExposed confirms the ${ENV_FILE}
// substitution token is preserved alongside the new auto-merge
// behaviour, so users who DO want to reference the path in their
// command (e.g. `docker run --env-file ${ENV_FILE}`) still can.
func TestExternal_EnvFileVar_StillExposed(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env.prod")
	if err := os.WriteFile(envFile, []byte("X=1\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	r := &fakeRunner{}
	p := ExternalProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env: "prod", ProviderID: "external", ImageTag: "v1",
		Services: []ResolvedService{
			{
				Name: "edge",
				External: &ExternalSpec{
					Image:     "x/edge",
					DeployCmd: "docker run --env-file ${ENV_FILE} ${IMAGE}:${TAG}",
					EnvFile:   envFile,
				},
			},
		},
	}
	if err := p.Deploy(context.Background(), group); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected 1 call, got %d (%v)", len(r.calls), r.calls)
	}
	if !strings.Contains(r.calls[0], "--env-file "+envFile) {
		t.Errorf("call should reference --env-file %s, got %q", envFile, r.calls[0])
	}
}

// TestExpandVars_Basic confirms the substitution helper handles the
// documented ${X} tokens and leaves unknown keys empty.
func TestExpandVars_Basic(t *testing.T) {
	got := expandVars("a=${A} b=${B} unknown=${X}", map[string]string{
		"A": "alpha",
		"B": "beta",
	})
	want := "a=alpha b=beta unknown="
	if got != want {
		t.Errorf("expand: want %q, got %q", want, got)
	}
}
