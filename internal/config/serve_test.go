// Tests for the retired services[].serve / services[].served_by yaml
// surface. The fields shipped only on an unreleased branch before being
// replaced by registration-in-code (what a binary serves is the row
// list in pkg/app/services.go, not a yaml knob), so loading a config
// that still carries them must fail with the specific migration hint —
// never a Levenshtein "did you mean" suggestion.
package config

import (
	"strings"
	"testing"
)

const serveRemovedBaseYAML = `name: demo
module_path: github.com/example/demo
`

func TestLoadStrict_RemovedServeKey_MigrationHint(t *testing.T) {
	in := serveRemovedBaseYAML + `components:
  - name: project
    kind: server
    path: handlers/project
    serve: false
`
	_, err := LoadStrict([]byte(in), "forge.yaml")
	if err == nil {
		t.Fatalf("expected removed-key error for components[].serve")
	}
	got := err.Error()
	for _, want := range []string{
		`"components[0].serve" was removed`,
		"registration-in-code",
		"pkg/app/services.go",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("error missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "did you mean") {
		t.Errorf("migration hint must suppress the typo suggestion:\n%s", got)
	}
}

func TestLoadStrict_RemovedServedByKey_MigrationHint(t *testing.T) {
	in := serveRemovedBaseYAML + `components:
  - name: api
    kind: server
    path: handlers/api
  - name: project
    kind: server
    path: handlers/project
    served_by: control-plane
`
	_, err := LoadStrict([]byte(in), "forge.yaml")
	if err == nil {
		t.Fatalf("expected removed-key error for components[].served_by")
	}
	got := err.Error()
	for _, want := range []string{
		`"components[1].served_by" was removed`,
		"pkg/app/services.go",
		"comment",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("error missing %q:\n%s", want, got)
		}
	}
}

// TestLoadStrict_PlainServiceEntry_NoServeSurface pins that a vanilla
// services entry (the only shape that ever shipped in a release) still
// loads cleanly after the serve/served_by removal.
func TestLoadStrict_PlainServiceEntry_NoServeSurface(t *testing.T) {
	in := serveRemovedBaseYAML + `components:
  - name: api
    kind: server
    path: handlers/api
`
	cfg, err := LoadStrict([]byte(in), "forge.yaml")
	if err != nil {
		t.Fatalf("clean load expected: %v", err)
	}
	if len(cfg.Components) != 1 || cfg.Components[0].Name != "api" {
		t.Errorf("components = %+v, want the single api entry", cfg.Components)
	}
}
