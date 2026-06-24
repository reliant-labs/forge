package cluster

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
)

// --- fakes ---------------------------------------------------------------

// fakeSecretGetter resolves Secret keys from an in-memory table.
// secrets[name] is the set of keys present; a name absent from the map
// reports exists=false. err (when set) is returned for every lookup.
type fakeSecretGetter struct {
	secrets map[string]map[string]struct{}
	err     error
}

func (f fakeSecretGetter) GetSecretKeys(_ context.Context, _, _, name string) (map[string]struct{}, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	keys, ok := f.secrets[name]
	if !ok {
		return nil, false, nil
	}
	return keys, true, nil
}

// fakeImageChecker resolves image existence from an in-memory set.
type fakeImageChecker struct {
	present map[string]struct{}
	err     error
}

func (f fakeImageChecker) ImageExists(_ context.Context, ref string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	_, ok := f.present[ref]
	return ok, nil
}

func keySet(keys ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

// --- manifest fixtures ---------------------------------------------------

const deploymentManifest = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: app-prod
spec:
  template:
    spec:
      containers:
        - name: api
          image: ghcr.io/acme/api:abc123
          env:
            - name: SERVICE_SECRET
              valueFrom:
                secretKeyRef:
                  name: app-secrets
                  key: service_secret
            - name: DB_URL
              valueFrom:
                secretKeyRef:
                  name: app-secrets
                  key: db_url
`

const jobWithEnvFrom = `
apiVersion: batch/v1
kind: Job
metadata:
  name: migrate
  namespace: app-prod
spec:
  template:
    spec:
      containers:
        - name: migrate
          image: ghcr.io/acme/migrate:abc123
          envFrom:
            - secretRef:
                name: bulk-secrets
`

// --- CollectManifestRefs -------------------------------------------------

func TestCollectManifestRefs(t *testing.T) {
	refs := CollectManifestRefs(deploymentManifest + "\n---\n" + jobWithEnvFrom)

	// Images.
	wantImages := []string{"ghcr.io/acme/api:abc123", "ghcr.io/acme/migrate:abc123"}
	gotImages := make([]string, 0, len(refs.Images))
	for img := range refs.Images {
		gotImages = append(gotImages, img)
	}
	sort.Strings(gotImages)
	if strings.Join(gotImages, ",") != strings.Join(wantImages, ",") {
		t.Fatalf("images: got %v, want %v", gotImages, wantImages)
	}

	// secretKeyRef keys.
	appKeys := refs.Secrets["app-secrets"]
	if _, ok := appKeys["service_secret"]; !ok {
		t.Errorf("app-secrets should reference service_secret; got %v", appKeys)
	}
	if _, ok := appKeys["db_url"]; !ok {
		t.Errorf("app-secrets should reference db_url; got %v", appKeys)
	}

	// envFrom secretRef → whole-Secret marker (key "").
	bulkKeys := refs.Secrets["bulk-secrets"]
	if _, ok := bulkKeys[""]; !ok {
		t.Errorf("bulk-secrets should carry the whole-Secret marker; got %v", bulkKeys)
	}
}

func TestCollectManifestRefs_Empty(t *testing.T) {
	refs := CollectManifestRefs("")
	if len(refs.Secrets) != 0 || len(refs.Images) != 0 {
		t.Fatalf("empty manifests should yield no refs; got %+v", refs)
	}
}

// --- Preflight: all present → proceeds -----------------------------------

func TestPreflight_AllPresent(t *testing.T) {
	opts := PreflightOpts{
		Manifests: deploymentManifest,
		Context:   "gke_prod",
		Namespace: "app-prod",
		Secrets: fakeSecretGetter{secrets: map[string]map[string]struct{}{
			"app-secrets": keySet("service_secret", "db_url"),
		}},
		Images: fakeImageChecker{present: keySet("ghcr.io/acme/api:abc123")},
	}
	if err := Preflight(context.Background(), opts); err != nil {
		t.Fatalf("expected preflight to pass, got: %v", err)
	}
}

// --- Preflight: missing secret key → error listing it --------------------

func TestPreflight_MissingSecretKey(t *testing.T) {
	opts := PreflightOpts{
		Manifests: deploymentManifest,
		Context:   "gke_prod",
		Namespace: "app-prod",
		Secrets: fakeSecretGetter{secrets: map[string]map[string]struct{}{
			// db_url present, service_secret missing.
			"app-secrets": keySet("db_url"),
		}},
		Images: fakeImageChecker{present: keySet("ghcr.io/acme/api:abc123")},
	}
	err := Preflight(context.Background(), opts)
	if err == nil {
		t.Fatal("expected preflight to fail on missing secret key")
	}
	msg := err.Error()
	if !strings.Contains(msg, "app-prod/app-secrets") {
		t.Errorf("error should name the Secret app-prod/app-secrets; got:\n%s", msg)
	}
	if !strings.Contains(msg, "service_secret") {
		t.Errorf("error should list the missing key service_secret; got:\n%s", msg)
	}
	if strings.Contains(msg, "db_url") {
		t.Errorf("error should NOT list the present key db_url; got:\n%s", msg)
	}
	if !strings.Contains(msg, "--skip-preflight") {
		t.Errorf("error should mention the --skip-preflight escape hatch; got:\n%s", msg)
	}
}

// --- Preflight: secret entirely absent → whole-secret marker -------------

func TestPreflight_SecretEntirelyMissing(t *testing.T) {
	opts := PreflightOpts{
		Manifests: deploymentManifest,
		Context:   "gke_prod",
		Namespace: "app-prod",
		// app-secrets not in the table → exists=false.
		Secrets: fakeSecretGetter{secrets: map[string]map[string]struct{}{}},
		Images:  fakeImageChecker{present: keySet("ghcr.io/acme/api:abc123")},
	}
	err := Preflight(context.Background(), opts)
	if err == nil {
		t.Fatal("expected preflight to fail on absent Secret")
	}
	if !strings.Contains(err.Error(), wholeSecretMarker) {
		t.Errorf("error should carry the whole-Secret marker; got:\n%s", err.Error())
	}
}

// --- Preflight: missing image → error ------------------------------------

func TestPreflight_MissingImage(t *testing.T) {
	opts := PreflightOpts{
		Manifests: deploymentManifest,
		Context:   "gke_prod",
		Namespace: "app-prod",
		Secrets: fakeSecretGetter{secrets: map[string]map[string]struct{}{
			"app-secrets": keySet("service_secret", "db_url"),
		}},
		// image set empty → not present.
		Images: fakeImageChecker{present: map[string]struct{}{}},
	}
	err := Preflight(context.Background(), opts)
	if err == nil {
		t.Fatal("expected preflight to fail on missing image")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Images not found") {
		t.Errorf("error should have an Images not found block; got:\n%s", msg)
	}
	if !strings.Contains(msg, "ghcr.io/acme/api:abc123") {
		t.Errorf("error should list the missing image; got:\n%s", msg)
	}
}

// --- Preflight: groups EVERYTHING missing in one report ------------------

func TestPreflight_GroupsAllMissing(t *testing.T) {
	opts := PreflightOpts{
		Manifests: deploymentManifest,
		Context:   "gke_prod",
		Namespace: "app-prod",
		Secrets:   fakeSecretGetter{secrets: map[string]map[string]struct{}{}},
		Images:    fakeImageChecker{present: map[string]struct{}{}},
	}
	err := Preflight(context.Background(), opts)
	if err == nil {
		t.Fatal("expected preflight failure")
	}
	msg := err.Error()
	// Both the Secret block AND the image block must appear — the whole
	// point is one report listing everything.
	if !strings.Contains(msg, "app-secrets") || !strings.Contains(msg, "Images not found") {
		t.Errorf("report should group BOTH missing secrets and images; got:\n%s", msg)
	}
}

// --- Preflight: SkipImageRef skips local images --------------------------

func TestPreflight_SkipsLocalImages(t *testing.T) {
	const localManifest = `
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
        - name: api
          image: registry.localhost:5000/api:dev
`
	opts := PreflightOpts{
		Manifests: localManifest,
		// No secrets referenced; image is local and should be skipped even
		// though the checker reports it absent.
		Images:       fakeImageChecker{present: map[string]struct{}{}},
		SkipImageRef: LocalImageRef,
	}
	if err := Preflight(context.Background(), opts); err != nil {
		t.Fatalf("local image should be skipped, got: %v", err)
	}
}

// --- Preflight: no secret context → secret check skipped -----------------

func TestPreflight_NoContextSkipsSecretCheck(t *testing.T) {
	opts := PreflightOpts{
		Manifests: deploymentManifest,
		// Context empty → secret check skipped entirely, even with a getter.
		Secrets: fakeSecretGetter{secrets: map[string]map[string]struct{}{}},
		Images:  fakeImageChecker{present: keySet("ghcr.io/acme/api:abc123")},
	}
	if err := Preflight(context.Background(), opts); err != nil {
		t.Fatalf("missing secret context should skip the secret check, got: %v", err)
	}
}

// --- Preflight: nothing to check → no-op ----------------------------------

func TestPreflight_NothingToCheck(t *testing.T) {
	const bare = `
kind: ConfigMap
metadata:
  name: settings
data:
  log_level: info
`
	opts := PreflightOpts{
		Manifests: bare,
		Context:   "gke_prod",
		Namespace: "app-prod",
		Secrets:   fakeSecretGetter{secrets: map[string]map[string]struct{}{}},
		Images:    fakeImageChecker{present: map[string]struct{}{}},
	}
	if err := Preflight(context.Background(), opts); err != nil {
		t.Fatalf("no refs to check should no-op, got: %v", err)
	}
}

// --- Preflight: getter error aborts (not treated as missing) -------------

func TestPreflight_SecretGetterErrorAborts(t *testing.T) {
	opts := PreflightOpts{
		Manifests: deploymentManifest,
		Context:   "gke_prod",
		Namespace: "app-prod",
		Secrets:   fakeSecretGetter{err: errors.New("kubectl: connection refused")},
		Images:    fakeImageChecker{present: keySet("ghcr.io/acme/api:abc123")},
	}
	err := Preflight(context.Background(), opts)
	if err == nil {
		t.Fatal("expected the getter error to abort the preflight")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("getter error should propagate; got: %v", err)
	}
	// A lookup failure must NOT be reported as a missing-key verdict.
	if strings.Contains(err.Error(), "missing keys") {
		t.Errorf("lookup error must not masquerade as a missing-key report; got: %v", err)
	}
}

func TestLocalImageRef(t *testing.T) {
	cases := map[string]bool{
		"registry.localhost:5000/api:dev": true,
		"localhost:5050/api:dev":          true,
		"127.0.0.1:5000/api:dev":          true,
		"ghcr.io/acme/api:abc123":         false,
		"us-docker.pkg.dev/p/r/api:v1":    false,
	}
	for ref, want := range cases {
		if got := LocalImageRef(ref); got != want {
			t.Errorf("LocalImageRef(%q) = %v, want %v", ref, got, want)
		}
	}
}
