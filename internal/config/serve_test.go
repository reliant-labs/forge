// Tests for the retired services[].serve / services[].served_by surface.
// The fields shipped only on an unreleased branch before being replaced by
// registration-in-code (what a binary serves is the row list in
// pkg/app/services.go, not a knob), so loading a config that still carries
// them must fail.
//
// Component entities now live in components.json (the ProjectStore
// per-service data move) — forge.yaml is global-only and a `components:`
// block there is rejected outright. So the serve/served_by surface, if it
// reappears at all, can only reappear inside components.json, where
// ParseComponentsJSON's DisallowUnknownFields rejects the retired keys.
//
// NOTE: the standalone `components[].serve` / `components[].served_by`
// migration-hint text (registration-in-code / pkg/app/services.go) lived in
// removedSchemaKeys and was only reachable while forge.yaml carried a
// `components:` block that the unknown-key walker descended into. With
// components moved out of forge.yaml, that nested walk no longer happens —
// the top-level `components` removed-key hint short-circuits it — so the
// per-field hint surface is gone. The preserved, still-testable intent is
// that the retired keys are REJECTED (never silently accepted) wherever a
// component is authored; that now happens in components.json.
package config

import (
	"strings"
	"testing"
)

const serveRemovedBaseYAML = `name: demo
module_path: github.com/example/demo
`

func TestLoadStrict_RemovedServeKey_Rejected(t *testing.T) {
	// A `serve` field on a component is no longer a valid surface anywhere.
	// Authored in components.json, it is rejected as an unknown field.
	_, err := ParseComponentsJSON([]byte(`{"components":[
		{"name":"project","kind":"server","path":"handlers/project","serve":false}
	]}`))
	if err == nil {
		t.Fatalf("expected rejection of components[].serve")
	}
	got := err.Error()
	for _, want := range []string{ComponentsFileName, "unknown field", "serve"} {
		if !strings.Contains(got, want) {
			t.Errorf("error missing %q:\n%s", want, got)
		}
	}
}

func TestLoadStrict_RemovedServedByKey_Rejected(t *testing.T) {
	// Same for `served_by`: retired with registration-in-code, rejected as
	// an unknown field when authored in components.json.
	_, err := ParseComponentsJSON([]byte(`{"components":[
		{"name":"api","kind":"server","path":"handlers/api"},
		{"name":"project","kind":"server","path":"handlers/project","served_by":"control-plane"}
	]}`))
	if err == nil {
		t.Fatalf("expected rejection of components[].served_by")
	}
	got := err.Error()
	for _, want := range []string{ComponentsFileName, "unknown field", "served_by"} {
		if !strings.Contains(got, want) {
			t.Errorf("error missing %q:\n%s", want, got)
		}
	}
}

// TestLoadStrict_PlainServiceEntry_NoServeSurface pins that a vanilla
// component entry (the only shape that ever shipped in a release) still
// loads cleanly after the serve/served_by removal. The component is now
// injected via the LoadStrict variadic, since forge.yaml is global-only.
func TestLoadStrict_PlainServiceEntry_NoServeSurface(t *testing.T) {
	cfg, err := LoadStrict([]byte(serveRemovedBaseYAML), "forge.yaml",
		ComponentConfig{Name: "api", Kind: "server", Path: "handlers/api"},
	)
	if err != nil {
		t.Fatalf("clean load expected: %v", err)
	}
	if len(cfg.Components) != 1 || cfg.Components[0].Name != "api" {
		t.Errorf("components = %+v, want the single api entry", cfg.Components)
	}
}
