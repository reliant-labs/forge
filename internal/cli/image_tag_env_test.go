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

// manifestImageTagFixture mirrors the control-plane cloud render shape:
// an `output` entity echo (services carry NO image_tag — the env-wide
// tag rides RenderEnv, not the entity) alongside a `manifests` stream
// whose Deployment container images bake the resolved env tag (here
// "staging"). The build side must recover the tag from the manifests,
// not the (empty) entity field.
const manifestImageTagFixture = `{
  "output": {
    "services": [
      {"name": "reliant-api-server", "image": "reliant", "deploy": {"type": "cluster", "replicas": 1}},
      {"name": "admin-server", "image": "control-plane", "deploy": {"type": "cluster", "replicas": 1}}
    ]
  },
  "manifests": [
    {"kind": "Deployment", "metadata": {"namespace": "control-plane-staging"},
     "spec": {"template": {"spec": {"containers": [
       {"image": "ghcr.io/reliant-labs/control-plane:staging"}
     ]}}}},
    {"kind": "Deployment", "metadata": {"namespace": "control-plane-staging"},
     "spec": {"template": {"spec": {"containers": [
       {"image": "ghcr.io/reliant-labs/reliant:staging"}
     ]}}}}
  ]
}`

// TestParseKCLEntities_ManifestImageTags confirms the env's resolved
// image tag is recovered from the rendered manifests (the deploy ref)
// even when the entity echo carries no per-service image_tag — the
// cloud-env shape.
func TestParseKCLEntities_ManifestImageTags(t *testing.T) {
	ents, err := parseKCLEntities([]byte(manifestImageTagFixture))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	if got := ents.ManifestImageTags["control-plane"]; got != "staging" {
		t.Errorf("control-plane tag: got %q, want staging", got)
	}
	if got := ents.ManifestImageTags["reliant"]; got != "staging" {
		t.Errorf("reliant tag: got %q, want staging", got)
	}
	// envImageTagFor is the build-side accessor.
	if got := envImageTagFor(ents, "control-plane"); got != "staging" {
		t.Errorf("envImageTagFor(control-plane): got %q, want staging", got)
	}
	// An image the env doesn't deploy yields "" → caller falls back to
	// git-describe.
	if got := envImageTagFor(ents, "not-deployed"); got != "" {
		t.Errorf("envImageTagFor(not-deployed): got %q, want empty", got)
	}
	// nil entities (no --env) yields "" too.
	if got := envImageTagFor(nil, "control-plane"); got != "" {
		t.Errorf("envImageTagFor(nil): got %q, want empty", got)
	}
}

// TestBuildExternalServices_TagDefaultsToEnvImageTag is the gotcha-A
// regression: when the rendered env references image `reliant:staging`
// (the deploy ref), an external build of the `reliant` service with NO
// per-service pin must use `staging` as ${TAG} — NOT the env-wide
// build-loop tag the caller threaded (here a stand-in git-describe
// value). This is what makes `forge build --env staging --push` push
// the SAME tag `forge deploy staging` references, instead of pushing
// git-describe and deploying "staging" → ImagePullBackOff.
func TestBuildExternalServices_TagDefaultsToEnvImageTag(t *testing.T) {
	projDir := t.TempDir()
	ents, err := parseKCLEntities([]byte(manifestImageTagFixture))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	services := []ServiceEntity{
		{
			Name:     "reliant-api-server",
			Image:    "reliant",
			BuildCmd: "true", // no build_cwd → runs, writes state with the resolved tag
		},
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

// TestBuildExternalServices_PerServicePinWins confirms an explicit KCL
// per-service image_tag (e2e's reliant_image_tag="e2e" /
// workspace-base "dev-per-daemon") OVERRIDES both the env-wide tag and
// the manifest-derived tag — so e2e's pinned tags keep building exactly
// what the daemon pods pull. This is the property that keeps
// `forge up --env=e2e` building the tags it deploys.
func TestBuildExternalServices_PerServicePinWins(t *testing.T) {
	projDir := t.TempDir()
	// Env render says workspace-base would be :staging, but the service
	// declares an explicit image_tag pin of "dev-per-daemon".
	ents := &KCLEntities{
		ManifestImageTags: map[string]string{"workspace-base": "staging"},
	}
	services := []ServiceEntity{
		{
			Name:     "workspace-base",
			Image:    "workspace-base",
			ImageTag: "dev-per-daemon", // the KCL per-service pin
			BuildCmd: "true",
		},
	}
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
