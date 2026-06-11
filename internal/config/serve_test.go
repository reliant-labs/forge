package config

import (
	"strings"
	"testing"
)

// serveBaseYAML mirrors validBaseYAML but with a second, types-only
// service so the serve/served_by rules can be exercised by editing one
// entry without disturbing the other phases.
const serveBaseYAML = `name: demo
module_path: github.com/example/demo
version: 0.1.0
hot_reload: false
services:
  - name: api
    type: go_service
    path: handlers/api
  - name: project
    type: go_service
    path: handlers/project
    serve: false
    served_by: control-plane
database:
  driver: postgres
  migrations_dir: db/migrations
ci:
  provider: github
docker:
  registry: ghcr.io
k8s:
  kcl_dir: deploy/kcl
lint:
  contract: true
contracts:
  strict: true
auth:
  provider: none
docs: {}
`

func TestLoadStrict_ServeFalse_WithServedBy_Accepted(t *testing.T) {
	cfg, err := LoadStrict([]byte(serveBaseYAML), "forge.yaml")
	if err != nil {
		t.Fatalf("expected clean load, got: %v", err)
	}
	if cfg.Services[0].IsServed() != true {
		t.Errorf("services[0] (serve absent) must default to served")
	}
	if cfg.Services[1].IsServed() != false {
		t.Errorf("services[1] (serve: false) must report unserved")
	}
	if cfg.Services[1].ServedBy != "control-plane" {
		t.Errorf("served_by not parsed: %+v", cfg.Services[1])
	}
}

func TestLoadStrict_ServeTrue_Explicit_Accepted(t *testing.T) {
	in := strings.Replace(validBaseYAML, "type: go_service", "type: go_service\n    serve: true", 1)
	cfg, err := LoadStrict([]byte(in), "forge.yaml")
	if err != nil {
		t.Fatalf("explicit serve: true must load cleanly, got: %v", err)
	}
	if !cfg.Services[0].IsServed() {
		t.Errorf("explicit serve: true must report served")
	}
}

func TestLoadStrict_ServeFalse_WithWebhooks_Rejected(t *testing.T) {
	in := strings.Replace(serveBaseYAML, "served_by: control-plane",
		"served_by: control-plane\n    webhooks:\n      - name: stripe", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "webhooks", "serve: false", "serving binary") {
		t.Errorf("expected webhooks-vs-serve error, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_ServedBy_WithoutServeFalse_Rejected(t *testing.T) {
	in := strings.Replace(validBaseYAML, "path: handlers/api",
		"path: handlers/api\n    served_by: control-plane", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "served_by", "serve: false") {
		t.Errorf("expected served_by-without-serve-false error, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_ServeFalse_OnWorker_Rejected(t *testing.T) {
	// Splice a serve:false worker into the services list (insert before
	// the database: block so it stays inside services:).
	in := strings.Replace(validBaseYAML, "database:",
		"  - name: refresher\n    type: worker\n    path: workers/refresher\n    serve: false\ndatabase:", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "serve: false", "type=worker", "Connect services") {
		t.Errorf("expected worker serve:false error, got:\n%s", ve.Error())
	}
}

func TestServiceServed_Chokepoint_MatchesAllSpellings(t *testing.T) {
	cfg, err := LoadStrict([]byte(serveBaseYAML), "forge.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cases := []struct {
		name string
		want bool
	}{
		// forge.yaml spelling.
		{"project", false},
		{"api", true},
		// proto service spelling.
		{"ProjectService", false},
		{"ApiService", true},
		// unknown names fail open (served) — the predicate only removes
		// serving on an explicit declaration.
		{"daemon", true},
		{"DaemonService", true},
	}
	for _, tc := range cases {
		if got := cfg.ServiceServed(tc.name); got != tc.want {
			t.Errorf("ServiceServed(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	if names := cfg.UnservedServices(); len(names) != 1 || names[0].Name != "project" {
		t.Errorf("UnservedServices() = %+v, want [project]", names)
	}
}

func TestServiceServed_NilConfig_FailsOpen(t *testing.T) {
	var cfg *ProjectConfig
	if !cfg.ServiceServed("anything") {
		t.Errorf("nil config must serve everything (directory-scan fallback)")
	}
	if cfg.UnservedServices() != nil {
		t.Errorf("nil config must have no unserved services")
	}
}
