package config

import (
	"strings"
	"testing"
)

// TestLoadProject_FrontendDevRunner pins that forge.yaml accepts
// frontends[].dev_runner — the spelling the KCL Frontend schema already
// uses — so a pnpm app can be adopted through forge.yaml alone. It used to
// be rejected as an unknown key.
func TestLoadProject_FrontendDevRunner(t *testing.T) {
	in := validBaseYAML + `frontends:
  - name: web
    type: nextjs
    path: web
    dev_runner: pnpm
`
	cfg, err := LoadProject([]byte(in), "forge.yaml")
	if err != nil {
		t.Fatalf("dev_runner must load cleanly, got: %v", err)
	}
	if got := cfg.Frontends[0].EffectiveDevRunner(); got != "pnpm" {
		t.Errorf("EffectiveDevRunner = %q, want pnpm", got)
	}
}

func TestLoadProject_FrontendDevRunnerDefaultsToNPM(t *testing.T) {
	if got := (FrontendConfig{}).EffectiveDevRunner(); got != DevRunnerNPM {
		t.Errorf("default EffectiveDevRunner = %q, want npm", got)
	}
}

func TestLoadProject_FrontendDevRunnerRejectsUnknownRunner(t *testing.T) {
	in := validBaseYAML + `frontends:
  - name: web
    type: nextjs
    path: web
    dev_runner: bun
`
	_, err := LoadProject([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !strings.Contains(ve.Error(), "dev_runner") || !strings.Contains(ve.Error(), "bun") {
		t.Errorf("want a dev_runner validation error naming the value, got:\n%s", ve.Error())
	}
}
