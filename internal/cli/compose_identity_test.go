package cli

import (
	"path/filepath"
	"testing"
)

func TestComposeIdentityIncludesExactCheckoutAndEnv(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "one", "project"), filepath.Join(root, "two", "project")
	cm := &ComposeRuntime{Service: "clickstack", Env: map[string]string{"CLICKSTACK_OTLP_GRPC_PORT": "14317"}}
	first := composeSpecFor(a, "dev", cm)
	for _, other := range []*struct{ dir, env string }{{b, "dev"}, {a, "other"}} {
		got := composeSpecFor(other.dir, other.env, cm)
		if first.Env["COMPOSE_PROJECT_NAME"] == got.Env["COMPOSE_PROJECT_NAME"] {
			t.Fatalf("compose selects same project for %s/%s and %s/dev", other.dir, other.env, a)
		}
	}
	if first.ProjectDirectory != a || first.Env["CLICKSTACK_OTLP_GRPC_PORT"] != "14317" {
		t.Fatalf("lost resolved compose input: %+v", first)
	}
	if _, ok := cm.Env["COMPOSE_PROJECT_NAME"]; ok {
		t.Fatal("mutated declaration")
	}
}
