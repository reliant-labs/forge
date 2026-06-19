package deploytarget

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCompose_Deploy_HappyPath confirms the pull + up -d + ps
// sequence fires in order, and the state file lands.
func TestCompose_Deploy_HappyPath(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{
		outputs: map[string]string{
			// compose ps returns one non-header line referencing the
			// target service — the health check accepts this.
			"docker compose -f docker-compose.yml ps": "edge_1   Up   8080/tcp\n",
		},
	}
	p := ComposeProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "compose",
		ImageTag:   "v1.2.3",
		Services: []ResolvedService{
			{
				Name: "edge",
				Compose: &ComposeSpec{
					ComposeFile: "docker-compose.yml",
				},
			},
		},
	}
	if err := p.Deploy(context.Background(), group); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	wantPrefixes := []string{
		"docker compose -f docker-compose.yml pull edge",
		"docker compose -f docker-compose.yml up -d edge",
		"docker compose -f docker-compose.yml ps --status running edge",
	}
	if len(r.calls) != len(wantPrefixes) {
		t.Fatalf("call count: want %d, got %d (%v)", len(wantPrefixes), len(r.calls), r.calls)
	}
	for i, prefix := range wantPrefixes {
		if !strings.HasPrefix(r.calls[i], prefix) {
			t.Errorf("call[%d]: want prefix %q, got %q", i, prefix, r.calls[i])
		}
	}
	statePath := filepath.Join(dir, ".forge/state/compose-prod-edge.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("state file should exist at %s, got %v", statePath, err)
	}
}

// TestCompose_Deploy_WithEnvFile threads --env-file through to the up
// command when the spec declares one.
func TestCompose_Deploy_WithEnvFile(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{
		outputs: map[string]string{
			"docker compose -f docker-compose.yml ps": "edge_1   Up\n",
		},
	}
	p := ComposeProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "compose",
		Services: []ResolvedService{
			{
				Name: "edge",
				Compose: &ComposeSpec{
					ComposeFile: "docker-compose.yml",
					EnvFile:     ".env.prod",
				},
			},
		},
	}
	if err := p.Deploy(context.Background(), group); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	var upCall string
	for _, c := range r.calls {
		if strings.Contains(c, "up -d") {
			upCall = c
			break
		}
	}
	if !strings.Contains(upCall, "--env-file .env.prod") {
		t.Errorf("up call should include --env-file .env.prod, got %q", upCall)
	}
}

// TestCompose_Deploy_SecretsMergedEnvFileWins confirms resolved secrets
// reach the docker process env as the BASE layer, with env_file entries
// overriding on key conflict.
func TestCompose_Deploy_SecretsMergedEnvFileWins(t *testing.T) {
	dir := t.TempDir()
	// env_file declares ONE key that also exists in Secrets (to prove the
	// file wins) plus a file-only key.
	envFile := filepath.Join(dir, ".env.prod")
	if err := os.WriteFile(envFile, []byte("SHARED=from_file\nFILE_ONLY=f\n"), 0o644); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	r := &fakeRunner{
		outputs: map[string]string{
			"docker compose -f docker-compose.yml ps": "edge_1   Up\n",
		},
	}
	p := ComposeProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "compose",
		Services: []ResolvedService{
			{
				Name: "edge",
				Compose: &ComposeSpec{
					ComposeFile: "docker-compose.yml",
					EnvFile:     envFile,
				},
				Secrets: map[string]string{
					"SHARED":      "from_secret", // overridden by env_file
					"SECRET_ONLY": "s",
				},
			},
		},
	}
	if err := p.Deploy(context.Background(), group); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// pull is the first RunWithEnv call; its overlay is the merged map.
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

// TestCompose_Rollback_NoState confirms Rollback errors loudly when
// there's no state file AND no fallback tag.
func TestCompose_Rollback_NoState(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	p := ComposeProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env:        "prod",
		ProviderID: "compose",
		Services: []ResolvedService{
			{Name: "edge", Compose: &ComposeSpec{ComposeFile: "docker-compose.yml"}},
		},
	}
	err := p.Rollback(context.Background(), group, "")
	if err == nil {
		t.Fatal("expected error for missing state file, got nil")
	}
	if !strings.Contains(err.Error(), "no previous tag recorded") {
		t.Errorf("want 'no previous tag recorded', got %v", err)
	}
}

// TestCompose_Rollback_NoImageHint confirms that when the state file
// has a tag but no image name, rollback errors with a clear "manual
// intervention" message rather than silently writing a broken
// override.
func TestCompose_Rollback_NoImageHint(t *testing.T) {
	dir := t.TempDir()
	_, err := WriteDeployState(dir, "compose", "prod", "edge", DeployState{
		Tag: "v1.0.0",
		// Image empty.
	})
	if err != nil {
		t.Fatalf("seed state: %v", err)
	}
	r := &fakeRunner{}
	p := ComposeProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env: "prod", ProviderID: "compose",
		Services: []ResolvedService{
			{Name: "edge", Compose: &ComposeSpec{ComposeFile: "docker-compose.yml"}},
		},
	}
	err = p.Rollback(context.Background(), group, "")
	if err == nil {
		t.Fatal("expected error for missing image hint, got nil")
	}
	if !strings.Contains(err.Error(), "no previous image recorded") {
		t.Errorf("want 'no previous image recorded' message, got %v", err)
	}
}

// TestCompose_Rollback_HappyPath writes an override file and runs up
// -d --force-recreate. The override should be deleted after the call.
func TestCompose_Rollback_HappyPath(t *testing.T) {
	dir := t.TempDir()
	_, err := WriteDeployState(dir, "compose", "prod", "edge", DeployState{
		Image: "ghcr.io/x/edge",
		Tag:   "v1.0.0",
	})
	if err != nil {
		t.Fatalf("seed state: %v", err)
	}
	r := &fakeRunner{}
	p := ComposeProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env: "prod", ProviderID: "compose",
		Services: []ResolvedService{
			{Name: "edge", Compose: &ComposeSpec{ComposeFile: "docker-compose.yml"}},
		},
	}
	if err := p.Rollback(context.Background(), group, ""); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// The up call should reference both compose files and --force-recreate.
	var upCall string
	for _, c := range r.calls {
		if strings.Contains(c, "up -d --force-recreate") {
			upCall = c
		}
	}
	if upCall == "" {
		t.Fatalf("expected up -d --force-recreate call, got %v", r.calls)
	}
	if !strings.Contains(upCall, "-f docker-compose.yml") {
		t.Errorf("up call should reference main compose file, got %q", upCall)
	}
	if !strings.Contains(upCall, "rollback.override.yml") {
		t.Errorf("up call should reference override file, got %q", upCall)
	}
	// Override file should be cleaned up.
	matches, _ := filepath.Glob(filepath.Join(dir, ".forge/state", "compose-prod-edge-rollback.override.yml"))
	if len(matches) > 0 {
		t.Errorf("override file should be deleted after rollback, found %v", matches)
	}
}

// TestCompose_ComposeServiceName_Default confirms the compose service
// defaults to the forge service name when KCL leaves it unset.
func TestCompose_ComposeServiceName_Default(t *testing.T) {
	got := composeServiceName(&ComposeSpec{}, "forge-svc")
	if got != "forge-svc" {
		t.Errorf("default service name: want forge-svc, got %s", got)
	}
}

// TestCompose_ComposeServiceName_Override confirms the explicit
// service field wins when set.
func TestCompose_ComposeServiceName_Override(t *testing.T) {
	got := composeServiceName(&ComposeSpec{Service: "compose-svc"}, "forge-svc")
	if got != "compose-svc" {
		t.Errorf("override: want compose-svc, got %s", got)
	}
}

// TestCompose_Deploy_DryRun confirms --dry-run prints the pull and
// up commands but does NOT exec anything and does NOT write the state
// file. Same contract External honors — preview without side effects.
func TestCompose_Deploy_DryRun(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	p := ComposeProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env: "prod", ProviderID: "compose", ImageTag: "v1.2.3",
		DryRun: true,
		Services: []ResolvedService{
			{
				Name:    "edge",
				Compose: &ComposeSpec{ComposeFile: "docker-compose.yml"},
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
		"[DRY-RUN] would run: docker compose -f docker-compose.yml pull edge",
		"[DRY-RUN] would run: docker compose -f docker-compose.yml up -d edge",
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("stdout should contain %q, got:\n%s", want, out)
		}
	}
	statePath := filepath.Join(dir, ".forge/state/compose-prod-edge.json")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file should NOT exist after dry-run, got err=%v", err)
	}
}

// TestCompose_Rollback_DryRun confirms the rollback dry-run path
// prints the override + up commands without writing the override file
// or exec'ing docker.
func TestCompose_Rollback_DryRun(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteDeployState(dir, "compose", "prod", "edge", DeployState{
		Image: "ghcr.io/x/edge",
		Tag:   "v1.0.0",
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	r := &fakeRunner{}
	p := ComposeProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env: "prod", ProviderID: "compose", DryRun: true,
		Services: []ResolvedService{
			{Name: "edge", Compose: &ComposeSpec{ComposeFile: "docker-compose.yml"}},
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
	for _, want := range []string{
		"[DRY-RUN] would write override",
		"ghcr.io/x/edge:v1.0.0",
		"[DRY-RUN] would run: docker compose -f docker-compose.yml",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout should contain %q, got:\n%s", want, out)
		}
	}
	// Override file should NOT have been written.
	matches, _ := filepath.Glob(filepath.Join(dir, ".forge/state", "compose-prod-edge-rollback.override.yml"))
	if len(matches) > 0 {
		t.Errorf("override file should NOT exist after dry-run rollback, found %v", matches)
	}
}

// TestCompose_Deploy_EnvFileMerges confirms the dotenv contents are
// threaded onto the docker-compose process env so `${VAR}` references
// in the compose file resolve even when the user forgets to export.
func TestCompose_Deploy_EnvFileMerges(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env.prod")
	if err := os.WriteFile(envFile, []byte("POSTGRES_PASSWORD=hunter2\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	r := &fakeRunner{
		outputs: map[string]string{
			"docker compose -f docker-compose.yml ps": "edge_1   Up\n",
		},
	}
	p := ComposeProvider{ProjectDir: dir, Runner: r}
	group := ServiceGroup{
		Env: "prod", ProviderID: "compose",
		Services: []ResolvedService{
			{
				Name: "edge",
				Compose: &ComposeSpec{
					ComposeFile: "docker-compose.yml",
					EnvFile:     envFile,
				},
			},
		},
	}
	if err := p.Deploy(context.Background(), group); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// The pull and up calls should both carry the env overlay; the
	// ps call (Output, not Run) does not.
	var pullEnv, upEnv map[string]string
	for i, c := range r.calls {
		if strings.Contains(c, "pull edge") {
			pullEnv = r.envCalls[i]
		}
		if strings.Contains(c, "up -d edge") {
			upEnv = r.envCalls[i]
		}
	}
	if pullEnv == nil || pullEnv["POSTGRES_PASSWORD"] != "hunter2" {
		t.Errorf("pull env should include POSTGRES_PASSWORD=hunter2, got %v", pullEnv)
	}
	if upEnv == nil || upEnv["POSTGRES_PASSWORD"] != "hunter2" {
		t.Errorf("up env should include POSTGRES_PASSWORD=hunter2, got %v", upEnv)
	}
}

// TestComposeHasRunningLine_Header confirms the header line is
// skipped when scanning compose ps output.
func TestComposeHasRunningLine_Header(t *testing.T) {
	out := []byte("NAME      STATUS\nedge_1    Up\n")
	if !composeHasRunningLine(out, "edge") {
		t.Error("should find edge in body, ignoring header")
	}
	if composeHasRunningLine(out, "missing") {
		t.Error("should NOT find a service that isn't in body")
	}
}
