package devidp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testIdentity() Identity {
	return Identity{
		ClientID: "generated-client-id",
		Audience: "generated-project-id",
		Issuer:   "http://localhost:8080",
		JWKSURL:  "http://localhost:8080/oauth/v2/keys",
	}
}

// writeFakeServiceAccountFiles stands in for the namespace/token files
// Kubernetes projects into a real pod, so ConfigMapPublisher's tests can
// run without a cluster.
func writeFakeServiceAccountFiles(t *testing.T, dir, namespace string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "namespace"), []byte(namespace), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("fake-sa-token"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKCLFilePublisher_WritesAllFourValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idp_identity_gen.k")
	pub := &KCLFilePublisher{Path: path}

	if err := pub.Publish(context.Background(), testIdentity()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	for _, want := range []string{
		`"client_id" = "generated-client-id"`,
		`"audience" = "generated-project-id"`,
		`"issuer" = "http://localhost:8080"`,
		`"jwks_url" = "http://localhost:8080/oauth/v2/keys"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated KCL missing %q:\n%s", want, src)
		}
	}
}

// Re-running with the SAME identity must not rewrite the file — a
// converged dev stack should produce no spurious diff on repeated runs.
func TestKCLFilePublisher_IdempotentOnUnchangedIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idp_identity_gen.k")
	pub := &KCLFilePublisher{Path: path}

	if err := pub.Publish(context.Background(), testIdentity()); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := pub.Publish(context.Background(), testIdentity()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.ModTime() != after.ModTime() {
		t.Errorf("Publish rewrote an unchanged identity: mtime %v -> %v", before.ModTime(), after.ModTime())
	}
}

// A CHANGED identity (the issuer rotated to a different client id, say)
// must overwrite the committed file — convergence outranks a stale value.
func TestKCLFilePublisher_OverwritesOnChangedIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idp_identity_gen.k")
	pub := &KCLFilePublisher{Path: path}

	if err := pub.Publish(context.Background(), testIdentity()); err != nil {
		t.Fatal(err)
	}
	updated := testIdentity()
	updated.ClientID = "rotated-client-id"
	if err := pub.Publish(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"client_id" = "rotated-client-id"`) {
		t.Errorf("Publish did not converge to the new client id:\n%s", body)
	}
	if strings.Contains(string(body), "generated-client-id") {
		t.Errorf("stale client id survived a re-publish:\n%s", body)
	}
}

func TestKCLFilePublisher_CreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy", "kcl", "dev", "idp_identity_gen.k")
	pub := &KCLFilePublisher{Path: path}

	if err := pub.Publish(context.Background(), testIdentity()); err != nil {
		t.Fatalf("Publish did not create missing parent directories: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// ConfigMapPublisher.Publish is exercised against a fake API server rather
// than a real cluster: RunningInCluster gates which publisher a caller
// selects, and Publish itself only needs the in-cluster token/namespace
// files (faked here) plus a reachable API server.
func TestConfigMapPublisher_CreatesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	writeFakeServiceAccountFiles(t, dir, "demo-dev")

	var gotPatch, gotPost bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			gotPatch = true
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPost:
			gotPost = true
			if !strings.HasSuffix(r.URL.Path, "/api/v1/namespaces/demo-dev/configmaps") {
				t.Errorf("unexpected create path: %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()

	pub := &ConfigMapPublisher{
		Name:          "idp-identity",
		apiServerBase: srv.URL,
		namespaceFile: filepath.Join(dir, "namespace"),
		tokenFile:     filepath.Join(dir, "token"),
	}

	if err := pub.Publish(context.Background(), testIdentity()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !gotPatch {
		t.Error("Publish never attempted a PATCH first")
	}
	if !gotPost {
		t.Error("Publish did not fall back to POST when the PATCH 404'd")
	}
}

func TestConfigMapPublisher_PatchesWhenExists(t *testing.T) {
	dir := t.TempDir()
	writeFakeServiceAccountFiles(t, dir, "demo-dev")

	var patches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected only PATCH on the happy path, got %s", r.Method)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		patches++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pub := &ConfigMapPublisher{
		Name:          "idp-identity",
		apiServerBase: srv.URL,
		namespaceFile: filepath.Join(dir, "namespace"),
		tokenFile:     filepath.Join(dir, "token"),
	}

	if err := pub.Publish(context.Background(), testIdentity()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if patches != 1 {
		t.Errorf("expected exactly one PATCH, got %d", patches)
	}
}
