package cli

import (
	"reflect"
	"strings"
	"testing"
)

// TestRefRepo confirms the repo extraction used to match the right
// RepoDigests entry: it must strip a `:tag` or `@digest` while preserving a
// `registry:port/` host (the port colon is NOT a tag separator).
func TestRefRepo(t *testing.T) {
	cases := []struct{ ref, want string }{
		{"ghcr.io/reliant/control-plane:v1.4.0", "ghcr.io/reliant/control-plane"},
		{"ghcr.io/reliant/control-plane@sha256:abc", "ghcr.io/reliant/control-plane"},
		{"localhost:5051/app:latest", "localhost:5051/app"},
		{"localhost:5051/app", "localhost:5051/app"},
		{"app", "app"},
		{"app:tag", "app"},
	}
	for _, c := range cases {
		if got := refRepo(c.ref); got != c.want {
			t.Errorf("refRepo(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
}

// TestBuildState_DigestRoundTrip proves Step 1: a BuildState carrying a
// captured Digest + Platforms persists and reads back intact, so a build push
// that records the digest hands it to a subsequent deploy.
func TestBuildState_DigestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := BuildState{
		Image:     "control-plane",
		Tag:       "v1.4.0",
		Registry:  "ghcr.io/reliant",
		Pushed:    true,
		PushedAt:  nowRFC3339(),
		Digest:    "sha256:" + strings.Repeat("c", 64),
		Platforms: []string{"linux/amd64"},
	}
	if err := WriteBuildState(dir, "prod", want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadBuildState(dir, "prod")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == nil {
		t.Fatal("read returned nil")
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("round-trip mismatch:\n  want %+v\n  got  %+v", want, *got)
	}
}
