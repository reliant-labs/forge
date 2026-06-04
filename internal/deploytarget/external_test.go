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
