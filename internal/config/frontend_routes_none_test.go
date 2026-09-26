package config

import (
	"strings"
	"testing"
)

func TestLoadProject_RoutesNoneIsValid(t *testing.T) {
	in := validBaseYAML + "frontends:\n  - name: web\n    type: nextjs\n    path: web\n    routes: [none]\n"
	cfg, err := LoadProject([]byte(in), "forge.yaml")
	if err != nil {
		t.Fatalf("routes: [none] must load, got %v", err)
	}
	if !cfg.Frontends[0].RoutesNone() {
		t.Errorf("RoutesNone() = false for routes: [none]")
	}
}

// `none` alongside real slugs is a contradiction — "no pages" and "these
// pages" — and forge must not guess which one was meant.
func TestLoadProject_RoutesNoneCannotBeMixed(t *testing.T) {
	in := validBaseYAML + "frontends:\n  - name: web\n    type: nextjs\n    path: web\n    routes: [none, users]\n"
	_, err := LoadProject([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !strings.Contains(ve.Error(), "none") || !strings.Contains(ve.Error(), "routes") {
		t.Errorf("want a routes/none validation error, got:\n%s", ve.Error())
	}
}
