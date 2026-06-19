package cli

import (
	"context"
	"reflect"
	"testing"
)

func TestBuildStateLookupEnvs(t *testing.T) {
	cases := []struct {
		env  string
		want []string
	}{
		{"prod", []string{"prod", "default"}},
		{"staging", []string{"staging", "default"}},
		{"", []string{"default"}},
		{"default", []string{"default"}},
	}
	for _, c := range cases {
		if got := buildStateLookupEnvs(c.env); !reflect.DeepEqual(got, c.want) {
			t.Errorf("buildStateLookupEnvs(%q) = %v, want %v", c.env, got, c.want)
		}
	}
}

// TestResolveDeployImageTag_DefaultFallback proves the handoff that the
// --push-gate fix enables: `forge build --docker` (no --env) writes the
// "default" record, and `forge deploy prod` reads it back via the fallback
// when there is no prod-specific record.
func TestResolveDeployImageTag_DefaultFallback(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBuildState(dir, "default", BuildState{Tag: "cutover-v61", Image: "app", Pushed: false}); err != nil {
		t.Fatalf("write default state: %v", err)
	}

	tag, src, err := resolveDeployImageTag(context.Background(), dir, "prod", "")
	if err != nil {
		t.Fatalf("resolveDeployImageTag: %v", err)
	}
	if tag != "cutover-v61" {
		t.Fatalf("tag = %q, want cutover-v61 (default fallback not used)", tag)
	}
	if src == "" {
		t.Fatalf("source should name the default build-state file, got empty")
	}
}

// TestResolveDeployImageTag_EnvWinsOverDefault: a prod-specific record
// takes precedence over the default one.
func TestResolveDeployImageTag_EnvWinsOverDefault(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBuildState(dir, "default", BuildState{Tag: "from-default"}); err != nil {
		t.Fatalf("write default: %v", err)
	}
	if err := WriteBuildState(dir, "prod", BuildState{Tag: "from-prod"}); err != nil {
		t.Fatalf("write prod: %v", err)
	}
	tag, _, err := resolveDeployImageTag(context.Background(), dir, "prod", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tag != "from-prod" {
		t.Fatalf("tag = %q, want from-prod (env-specific should win)", tag)
	}
}

// TestResolveDeployImageTag_FlagOverridesAll: --tag bypasses build-state.
func TestResolveDeployImageTag_FlagOverridesAll(t *testing.T) {
	dir := t.TempDir()
	_ = WriteBuildState(dir, "prod", BuildState{Tag: "from-state"})
	tag, src, err := resolveDeployImageTag(context.Background(), dir, "prod", "explicit")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tag != "explicit" {
		t.Fatalf("tag = %q, want explicit", tag)
	}
	if src != "explicit --tag flag" {
		t.Fatalf("src = %q, want explicit flag", src)
	}
}

func TestShortSHA(t *testing.T) {
	if got := shortSHA("5fe39fe57ea80e970eba"); got != "5fe39fe57ea8" {
		t.Errorf("shortSHA long = %q", got)
	}
	if got := shortSHA(""); got != "unknown" {
		t.Errorf("shortSHA empty = %q, want unknown", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("shortSHA short = %q", got)
	}
}
