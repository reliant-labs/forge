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
