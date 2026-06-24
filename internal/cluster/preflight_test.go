package cluster

import (
	"context"
	"errors"
	"fmt"
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

// fakeConfigMapGetter resolves ConfigMap keys from an in-memory table,
// mirroring fakeSecretGetter.
type fakeConfigMapGetter struct {
	configMaps map[string]map[string]struct{}
	err        error
}

func (f fakeConfigMapGetter) GetConfigMapKeys(_ context.Context, _, _, name string) (map[string]struct{}, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	keys, ok := f.configMaps[name]
	if !ok {
		return nil, false, nil
	}
	return keys, true, nil
}

// fakeImageChecker resolves image existence from an in-memory set.
// confirmedMiss images return (false, nil) — a real registry not-found that
// must BLOCK. inconclusive images return (false, ErrImageCheckInconclusive)
// — a transport/auth failure that must WARN+proceed, not block. err (when
// set) is a hard checker failure returned for every lookup.
type fakeImageChecker struct {
	present      map[string]struct{}
	inconclusive map[string]struct{}
	err          error
}

func (f fakeImageChecker) ImageExists(_ context.Context, ref string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if _, ok := f.inconclusive[ref]; ok {
		return false, fmt.Errorf("%w: no basic auth credentials", ErrImageCheckInconclusive)
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

// secretVolumeManifest mounts an out-of-band kubeconfig Secret as a volume —
// exactly the shape `ClusterClient external=True` produces (forge does NOT
// mint that Secret). Also carries a projected secret source and an
// imagePullSecret, plus a configMap volume, to exercise every new shape.
const secretVolumeManifest = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: proxy
  namespace: app-prod
spec:
  template:
    spec:
      imagePullSecrets:
        - name: registry-creds
      containers:
        - name: proxy
          image: ghcr.io/acme/proxy:abc123
          volumeMounts:
            - name: kubeconfig
              mountPath: /etc/kube
      volumes:
        - name: kubeconfig
          secret:
            secretName: external-kubeconfig
        - name: settings
          configMap:
            name: proxy-settings
        - name: combined
          projected:
            sources:
              - secret:
                  name: projected-secret
              - configMap:
                  name: projected-config
`

// configMapKeyRefManifest projects a ConfigMap key into env — a missing key
// fails identically to a missing Secret key (CreateContainerConfigError).
const configMapKeyRefManifest = `
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
            - name: LOG_LEVEL
              valueFrom:
                configMapKeyRef:
                  name: app-config
                  key: log_level
          envFrom:
            - configMapRef:
                name: bulk-config
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

// --- CollectManifestRefs: new shapes -------------------------------------

func TestCollectManifestRefs_SecretVolumesAndPullSecrets(t *testing.T) {
	refs := CollectManifestRefs(secretVolumeManifest)

	// volumes[].secret.secretName — the ClusterClient external=True shape.
	if _, ok := refs.Secrets["external-kubeconfig"][""]; !ok {
		t.Errorf("secret volume should record external-kubeconfig (existence-only); got %v", refs.Secrets)
	}
	// volumes[].projected.sources[].secret.name
	if _, ok := refs.Secrets["projected-secret"][""]; !ok {
		t.Errorf("projected secret source should be recorded; got %v", refs.Secrets)
	}
	// imagePullSecrets[].name
	if _, ok := refs.Secrets["registry-creds"][""]; !ok {
		t.Errorf("imagePullSecret should be recorded; got %v", refs.Secrets)
	}
	// volumes[].configMap.name
	if _, ok := refs.ConfigMaps["proxy-settings"][""]; !ok {
		t.Errorf("configMap volume should be recorded; got %v", refs.ConfigMaps)
	}
	// volumes[].projected.sources[].configMap.name
	if _, ok := refs.ConfigMaps["projected-config"][""]; !ok {
		t.Errorf("projected configMap source should be recorded; got %v", refs.ConfigMaps)
	}
}

func TestCollectManifestRefs_ConfigMapKeyRefs(t *testing.T) {
	refs := CollectManifestRefs(configMapKeyRefManifest)

	// configMapKeyRef → keyed reference.
	if _, ok := refs.ConfigMaps["app-config"]["log_level"]; !ok {
		t.Errorf("configMapKeyRef should record app-config/log_level; got %v", refs.ConfigMaps)
	}
	// envFrom configMapRef → whole-ConfigMap marker.
	if _, ok := refs.ConfigMaps["bulk-config"][""]; !ok {
		t.Errorf("envFrom configMapRef should record the whole-ConfigMap marker; got %v", refs.ConfigMaps)
	}
}

// collectRefs must NOT invent references the manifest doesn't make: a bare
// ConfigMap definition (not a reference) and a Service have nothing to
// collect.
func TestCollectManifestRefs_IgnoresNonReferences(t *testing.T) {
	const noRefs = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
data:
  log_level: info
---
apiVersion: v1
kind: Service
metadata:
  name: api
spec:
  ports:
    - port: 80
`
	refs := CollectManifestRefs(noRefs)
	if len(refs.Secrets) != 0 {
		t.Errorf("no Secret references expected; got %v", refs.Secrets)
	}
	if len(refs.ConfigMaps) != 0 {
		t.Errorf("no ConfigMap references expected; got %v", refs.ConfigMaps)
	}
	if len(refs.Images) != 0 {
		t.Errorf("no images expected; got %v", refs.Images)
	}
}

// --- Preflight: missing secret-volume Secret → blocked -------------------

func TestPreflight_MissingSecretVolumeBlocks(t *testing.T) {
	opts := PreflightOpts{
		Manifests: secretVolumeManifest,
		Context:   "gke_prod",
		Namespace: "app-prod",
		// None of the referenced Secrets exist → every one is a whole-Secret
		// miss; the external-kubeconfig is the load-bearing one.
		Secrets:    fakeSecretGetter{secrets: map[string]map[string]struct{}{}},
		ConfigMaps: fakeConfigMapGetter{configMaps: map[string]map[string]struct{}{}},
		Images:     fakeImageChecker{present: keySet("ghcr.io/acme/proxy:abc123")},
	}
	err := Preflight(context.Background(), opts)
	if err == nil {
		t.Fatal("expected preflight to block on a missing secret-volume Secret")
	}
	msg := err.Error()
	if !strings.Contains(msg, "app-prod/external-kubeconfig") {
		t.Errorf("error should name the missing secret-volume Secret; got:\n%s", msg)
	}
	if !strings.Contains(msg, wholeSecretMarker) {
		t.Errorf("a missing whole-Secret should carry the whole-Secret marker; got:\n%s", msg)
	}
}

// --- Preflight: missing configMapKeyRef → blocked ------------------------

func TestPreflight_MissingConfigMapKeyBlocks(t *testing.T) {
	opts := PreflightOpts{
		Manifests: configMapKeyRefManifest,
		Context:   "gke_prod",
		Namespace: "app-prod",
		Secrets:   fakeSecretGetter{secrets: map[string]map[string]struct{}{}},
		ConfigMaps: fakeConfigMapGetter{configMaps: map[string]map[string]struct{}{
			// app-config exists but is missing log_level; bulk-config absent.
			"app-config": keySet("other_key"),
		}},
		Images: fakeImageChecker{present: keySet("ghcr.io/acme/api:abc123")},
	}
	err := Preflight(context.Background(), opts)
	if err == nil {
		t.Fatal("expected preflight to block on a missing ConfigMap key")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ConfigMap app-prod/app-config missing keys") || !strings.Contains(msg, "log_level") {
		t.Errorf("error should name the missing ConfigMap key; got:\n%s", msg)
	}
	if !strings.Contains(msg, "app-prod/bulk-config") {
		t.Errorf("error should flag the entirely-absent bulk-config; got:\n%s", msg)
	}
	if !strings.Contains(msg, "missing ConfigMap key(s)") {
		t.Errorf("remediation footer should mention ConfigMap keys; got:\n%s", msg)
	}
}

// ConfigMap check is skipped when no getter/context is configured (local
// dev), mirroring the Secret check.
func TestPreflight_NoContextSkipsConfigMapCheck(t *testing.T) {
	opts := PreflightOpts{
		Manifests:  configMapKeyRefManifest,
		ConfigMaps: fakeConfigMapGetter{configMaps: map[string]map[string]struct{}{}},
		Images:     fakeImageChecker{present: keySet("ghcr.io/acme/api:abc123")},
	}
	if err := Preflight(context.Background(), opts); err != nil {
		t.Fatalf("missing context should skip the ConfigMap check, got: %v", err)
	}
}

// --- Preflight: image-check inconclusive (auth) → WARN + proceed ----------

func TestPreflight_InconclusiveImageWarnsAndProceeds(t *testing.T) {
	opts := PreflightOpts{
		Manifests: deploymentManifest,
		Context:   "gke_prod",
		Namespace: "app-prod",
		Secrets: fakeSecretGetter{secrets: map[string]map[string]struct{}{
			"app-secrets": keySet("service_secret", "db_url"),
		}},
		// The image check can't reach a verdict (auth) — must NOT block.
		Images: fakeImageChecker{inconclusive: keySet("ghcr.io/acme/api:abc123")},
	}
	if err := Preflight(context.Background(), opts); err != nil {
		t.Fatalf("an inconclusive image check must warn+proceed, not block; got: %v", err)
	}
}

// The inconclusive verdict is surfaced as a structured ImageWarning, distinct
// from a confirmed MissingImage.
func TestRunPreflightChecks_InconclusiveIsWarningNotMiss(t *testing.T) {
	refs := CollectManifestRefs(deploymentManifest)
	opts := PreflightOpts{
		Manifests: deploymentManifest,
		Images:    fakeImageChecker{inconclusive: keySet("ghcr.io/acme/api:abc123")},
	}
	res, err := runPreflightChecks(context.Background(), opts, refs)
	if err != nil {
		t.Fatalf("inconclusive image must not abort: %v", err)
	}
	if len(res.MissingImages) != 0 {
		t.Errorf("inconclusive image must not be a confirmed miss; got MissingImages=%v", res.MissingImages)
	}
	if len(res.ImageWarnings) != 1 || !strings.Contains(res.ImageWarnings[0], "couldn't verify") {
		t.Errorf("expected one couldn't-verify warning; got %v", res.ImageWarnings)
	}
	if !res.OK() {
		t.Errorf("a run with only inconclusive images should be OK() (proceed)")
	}
}

// --- Preflight: image confirmed-404 → blocked ----------------------------

func TestPreflight_Confirmed404ImageBlocks(t *testing.T) {
	opts := PreflightOpts{
		Manifests: deploymentManifest,
		Context:   "gke_prod",
		Namespace: "app-prod",
		Secrets: fakeSecretGetter{secrets: map[string]map[string]struct{}{
			"app-secrets": keySet("service_secret", "db_url"),
		}},
		// Not present, not inconclusive → confirmed miss → block.
		Images: fakeImageChecker{present: map[string]struct{}{}},
	}
	err := Preflight(context.Background(), opts)
	if err == nil {
		t.Fatal("a confirmed-absent image must block the deploy")
	}
	if !strings.Contains(err.Error(), "Images not found") {
		t.Errorf("error should carry the Images not found block; got:\n%s", err.Error())
	}
}

// --- DockerImageChecker: miss-vs-inconclusive classifier -----------------

func TestImageOutputIsConfirmedMiss(t *testing.T) {
	confirmed := []string{
		"manifest unknown",
		"ghcr.io/acme/api:abc123: not found",
		"no such manifest: ghcr.io/x",
		"errors:\n manifest unknown: manifest unknown",
		"NAME_UNKNOWN: repository name not known to registry",
	}
	for _, out := range confirmed {
		if !imageOutputIsConfirmedMiss(out) {
			t.Errorf("expected confirmed miss for %q", out)
		}
	}
	inconclusive := []string{
		"no basic auth credentials",
		"unauthorized: authentication required",
		"dial tcp: lookup registry.example.com: no such host",
		"Cannot connect to the Docker daemon",
		"net/http: TLS handshake timeout",
	}
	for _, out := range inconclusive {
		if imageOutputIsConfirmedMiss(out) {
			t.Errorf("expected inconclusive (not a confirmed miss) for %q", out)
		}
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
