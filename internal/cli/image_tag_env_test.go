package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSplitImageNameTag covers the registry/name/tag parser that backs
// the env-image-tag recovery. The tricky cases are the registry
// "host:port/img" colon (must NOT be read as a tag) and digest refs
// (no tag to align to).
func TestSplitImageNameTag(t *testing.T) {
	cases := []struct {
		image    string
		wantName string
		wantTag  string
		wantOK   bool
	}{
		{"ghcr.io/reliant-labs/reliant:staging", "reliant", "staging", true},
		{"ghcr.io/reliant-labs/control-plane:stable", "control-plane", "stable", true},
		{"registry.localhost:5051/workspace-base:dev-per-daemon", "workspace-base", "dev-per-daemon", true},
		{"reliant:e2e", "reliant", "e2e", true},
		// Registry port colon, no tag → not a tag.
		{"registry.localhost:5051/img", "", "", false},
		// Digest pin → no tag to align to.
		{"ghcr.io/x/y@sha256:abc", "", "", false},
		// Tagless → no tag.
		{"reliant", "", "", false},
		// Empty tag after colon.
		{"reliant:", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		name, tag, ok := splitImageNameTag(c.image)
		if ok != c.wantOK || name != c.wantName || tag != c.wantTag {
			t.Errorf("splitImageNameTag(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.image, name, tag, ok, c.wantName, c.wantTag, c.wantOK)
		}
	}
}

// envImageTagFixture mirrors the control-plane cloud render shape: the
// workloads carry no per-workload pin, so their resolved spec.image carries
// the env's own image_tag ("staging") — the tag the deploy pulls. The build
// side reads it from output.image_tag and spec.image; nothing is scraped
// from the manifest stream.
const envImageTagFixture = `{"output": {
  "project": "control-plane", "env": "staging", "image_tag": "staging",
  "workloads": [
    {"name": "reliant-api-server", "kind": "service", "image": "reliant",
     "runtime": {"type": "cluster", "cluster": "gke", "namespace": "control-plane-staging"},
     "spec": {"kind": "service", "image": "ghcr.io/reliant-labs/reliant:staging"}},
    {"name": "admin-server", "kind": "service", "image": "control-plane",
     "runtime": {"type": "cluster", "cluster": "gke", "namespace": "control-plane-staging"},
     "spec": {"kind": "service", "image": "ghcr.io/reliant-labs/control-plane:staging"}}
  ]
}}`

// TestEnvImageTagFor_FromImageTagAndSpecImage confirms the env's resolved
// image tag is what `forge build <env>` defaults to: a workload's resolved
// spec.image tag when it carries one, else the env's output.image_tag.
func TestEnvImageTagFor_FromImageTagAndSpecImage(t *testing.T) {
	ents, err := parseKCLEntities([]byte(envImageTagFixture))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	if got := envImageTagFor(ents, "control-plane"); got != "staging" {
		t.Errorf("envImageTagFor(control-plane): got %q, want staging", got)
	}
	// A per-workload pin in spec.image wins over the env tag.
	ents.Workloads[0].Spec.Image = "ghcr.io/reliant-labs/reliant:v1.4.2"
	if got := envImageTagFor(ents, "reliant"); got != "v1.4.2" {
		t.Errorf("envImageTagFor(reliant) with a pinned spec.image: got %q, want v1.4.2", got)
	}
	// A digest-pinned spec.image carries no tag and falls through to the env tag.
	ents.Workloads[0].Spec.Image = "ghcr.io/reliant-labs/reliant@sha256:abc"
	if got := envImageTagFor(ents, "reliant"); got != "staging" {
		t.Errorf("envImageTagFor(reliant) digest-pinned: got %q, want the env tag staging", got)
	}
	// nil entities (no --env) yields "" → caller falls back to git-describe.
	if got := envImageTagFor(nil, "control-plane"); got != "" {
		t.Errorf("envImageTagFor(nil): got %q, want empty", got)
	}
}

// TestBuildExternalServices_TagDefaultsToEnvImageTag is the gotcha-A
// regression: when the rendered env references image `reliant:staging`
// (the deploy ref), an external build of the `reliant` service with NO
// per-service pin must use `staging` as ${TAG} — NOT the env-wide
// build-loop tag the caller threaded (here a stand-in git-describe
// value). This is what makes `forge build staging --push` push
// the SAME tag `forge env deploy staging` references, instead of pushing
// git-describe and deploying "staging" → ImagePullBackOff.
func TestBuildExternalServices_TagDefaultsToEnvImageTag(t *testing.T) {
	projDir := t.TempDir()
	ents, err := parseKCLEntities([]byte(envImageTagFixture))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	services := []WorkloadEntity{
		// no cwd → runs from project root, writes state with the resolved tag
		shellSvc("reliant-api-server", "reliant", "true", "", nil),
	}
	opts := buildOptions{env: "staging", parallel: false}
	// Thread a DIFFERENT env-wide tag (a stand-in for git-describe) to
	// prove the per-service resolution prefers the env image_tag.
	results := buildExternalServices(
		context.Background(), services, opts,
		"ghcr.io/reliant-labs", "e31db62-dirty", projDir, "amd64", ents,
	)
	if len(results) != 1 || results[0].err != nil {
		t.Fatalf("results: %+v", results)
	}
	// The persisted per-service state should record the ENV tag, not the
	// git-describe stand-in.
	st, err := ReadBuildState(projDir, "staging")
	if err != nil {
		t.Fatalf("ReadBuildState: %v", err)
	}
	if st == nil || st.Tag != "staging" {
		t.Errorf("deploy build-state tag: got %+v, want staging", st)
	}
	auditPath := filepath.Join(projDir, ".forge", "state", "build-staging-reliant-api-server.json")
	if _, err := os.Stat(auditPath); err != nil {
		t.Errorf("per-service state at %s: %v", auditPath, err)
	}
}

// TestBuildExternalServices_PerServicePinWins confirms an explicit
// per-workload tag pin in the resolved spec.image (e2e's
// reliant_image_tag="e2e" / workspace-base "dev-per-daemon") OVERRIDES both
// the env-wide build tag and the env's image_tag — so e2e's pinned tags keep building exactly
// what the daemon pods pull. This is the property that keeps
// `forge env up e2e` building the tags it deploys.
func TestBuildExternalServices_PerServicePinWins(t *testing.T) {
	projDir := t.TempDir()
	// The env's tag is :staging, but the workload's resolved image pins
	// "dev-per-daemon".
	wsbase := shellSvc("workspace-base", "workspace-base", "true", "", nil)
	wsbase.Spec.Image = "registry.localhost:5051/workspace-base:dev-per-daemon" // the per-workload pin
	ents := &KCLEntities{ImageTag: "staging", Workloads: []WorkloadEntity{wsbase}}
	services := []WorkloadEntity{wsbase}
	opts := buildOptions{env: "e2e", parallel: false}
	results := buildExternalServices(
		context.Background(), services, opts,
		"registry.localhost:5051", "env-wide-tag", projDir, "amd64", ents,
	)
	if len(results) != 1 || results[0].err != nil {
		t.Fatalf("results: %+v", results)
	}
	st, err := ReadBuildState(projDir, "e2e")
	if err != nil {
		t.Fatalf("ReadBuildState: %v", err)
	}
	if st == nil || st.Tag != "dev-per-daemon" {
		t.Errorf("deploy build-state tag: got %+v, want dev-per-daemon (the per-service pin)", st)
	}
}
