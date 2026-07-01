package cluster

import (
	"context"
	"strings"
	"testing"
)

// A Deployment that mounts a Secret as a volume (the cp-daemon-kubeconfig
// shape that motivated this gate) AND reads one via a secretKeyRef env var.
const mountManifest = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: workspace-controller
  namespace: control-plane-dev
spec:
  template:
    spec:
      containers:
        - name: ctl
          image: ghcr.io/x/control-plane:dev
          env:
            - name: DB_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: app-secrets
                  key: db-password
          volumeMounts:
            - name: kubeconfig
              mountPath: /etc/kubeconfig
      volumes:
        - name: kubeconfig
          secret:
            secretName: cp-daemon-kubeconfig
`

// renderedSecretDoc is a kind: Secret the bundle renders in-stream.
const renderedSecretDoc = `
apiVersion: v1
kind: Secret
metadata:
  name: app-secrets
  namespace: control-plane-dev
type: Opaque
data:
  db-password: c2VjcmV0
`

// TestCheckSecretSupply_UndeclaredMountFails is the motivating bug: a workload
// mounts Secret "cp-daemon-kubeconfig" but NOTHING declares it. The gate must
// FAIL and name the workload + Secret.
func TestCheckSecretSupply_UndeclaredMountFails(t *testing.T) {
	// Supply renders app-secrets in-stream (so the secretKeyRef is satisfied)
	// but provides NOTHING for cp-daemon-kubeconfig.
	manifests := renderedSecretDoc + docDelimiter + mountManifest
	misses := CheckSecretSupply(manifests, nil)
	if len(misses) != 1 {
		t.Fatalf("expected exactly 1 undeclared mount, got %d: %v", len(misses), misses)
	}
	m := misses[0]
	if m.Secret != "cp-daemon-kubeconfig" {
		t.Fatalf("expected the undeclared secret to be cp-daemon-kubeconfig, got %q", m.Secret)
	}
	if len(m.Workloads) != 1 || m.Workloads[0] != "workspace-controller" {
		t.Fatalf("expected the mount attributed to workspace-controller, got %v", m.Workloads)
	}
	report := FormatUndeclaredSecretMounts(misses)
	if !strings.Contains(report, "cp-daemon-kubeconfig") ||
		!strings.Contains(report, "workspace-controller") ||
		!strings.Contains(report, "FailedMount") ||
		!strings.Contains(report, "KubeconfigSecret") {
		t.Fatalf("report not actionable:\n%s", report)
	}
}

// TestCheckSecretSupply_KubeconfigSecretSatisfies — declaring a
// KubeconfigSecret of the same name PASSES (the fix for the bug).
func TestCheckSecretSupply_KubeconfigSecretSatisfies(t *testing.T) {
	manifests := renderedSecretDoc + docDelimiter + mountManifest
	supply := []SecretSupply{
		{Name: "cp-daemon-kubeconfig", Kind: SupplyKubeconfigSecret},
	}
	misses := CheckSecretSupply(manifests, supply)
	if len(misses) != 0 {
		t.Fatalf("expected no undeclared mounts when a KubeconfigSecret provides it, got %v", misses)
	}
}

// TestCheckSecretSupply_ExternalSecretSatisfies — an ExternalSecret out-of-band
// promise (even in another namespace) counts as SATISFIED.
func TestCheckSecretSupply_ExternalSecretSatisfies(t *testing.T) {
	manifests := renderedSecretDoc + docDelimiter + mountManifest
	supply := []SecretSupply{
		// Declared in a DIFFERENT namespace — still satisfies (name-based, to
		// avoid false positives).
		{Name: "cp-daemon-kubeconfig", Namespace: "other-ns", Kind: SupplyExternalSecret},
	}
	misses := CheckSecretSupply(manifests, supply)
	if len(misses) != 0 {
		t.Fatalf("expected no undeclared mounts when an ExternalSecret promises it, got %v", misses)
	}
}

// TestCheckSecretSupply_RenderedSecretSatisfies — a Secret rendered in the
// manifest stream satisfies a mount of the same name with no extra supply.
func TestCheckSecretSupply_RenderedSecretSatisfies(t *testing.T) {
	// Render cp-daemon-kubeconfig directly in-stream.
	renderedKubeconfig := `
apiVersion: v1
kind: Secret
metadata:
  name: cp-daemon-kubeconfig
  namespace: control-plane-dev
type: Opaque
data:
  kubeconfig: eHg=
`
	manifests := renderedSecretDoc + docDelimiter + renderedKubeconfig + docDelimiter + mountManifest
	misses := CheckSecretSupply(manifests, nil)
	if len(misses) != 0 {
		t.Fatalf("expected no undeclared mounts when the Secret is rendered in-stream, got %v", misses)
	}
}

// TestCheckSecretSupply_GeneratedSatisfies — a caller-supplied generated/known
// Secret satisfies the demand.
func TestCheckSecretSupply_GeneratedSatisfies(t *testing.T) {
	manifests := renderedSecretDoc + docDelimiter + mountManifest
	supply := []SecretSupply{{Name: "cp-daemon-kubeconfig", Kind: SupplyGenerated}}
	if misses := CheckSecretSupply(manifests, supply); len(misses) != 0 {
		t.Fatalf("expected no undeclared mounts for a generated Secret, got %v", misses)
	}
}

// TestCheckSecretSupply_EnvSecretKeyRefUndeclared — an env secretKeyRef to an
// undeclared Secret also fails (the demand isn't only volume mounts).
func TestCheckSecretSupply_EnvSecretKeyRefUndeclared(t *testing.T) {
	// Provide cp-daemon-kubeconfig but NOT app-secrets (the env ref target).
	manifests := mountManifest
	supply := []SecretSupply{{Name: "cp-daemon-kubeconfig", Kind: SupplyKubeconfigSecret}}
	misses := CheckSecretSupply(manifests, supply)
	if len(misses) != 1 || misses[0].Secret != "app-secrets" {
		t.Fatalf("expected app-secrets (env secretKeyRef) flagged, got %v", misses)
	}
}

// TestCheckSecretSupply_ImagePullSecretIgnored — an imagePullSecret is NOT an
// application Secret the bundle is expected to render; the render-time gate
// must not false-fail on it (the live preflight is the backstop).
func TestCheckSecretSupply_ImagePullSecretIgnored(t *testing.T) {
	pullManifest := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: ns
spec:
  template:
    spec:
      imagePullSecrets:
        - name: ghcr-pull
      containers:
        - name: api
          image: ghcr.io/x/api:v1
`
	if misses := CheckSecretSupply(pullManifest, nil); len(misses) != 0 {
		t.Fatalf("imagePullSecret must not fail the render-time gate, got %v", misses)
	}
}

// TestCheckSecretSupply_NoSecretsNoOp — a bundle with no secret refs is a no-op.
func TestCheckSecretSupply_NoSecretsNoOp(t *testing.T) {
	plain := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: ns
spec:
  template:
    spec:
      containers:
        - name: api
          image: ghcr.io/x/api:v1
`
	if misses := CheckSecretSupply(plain, nil); len(misses) != 0 {
		t.Fatalf("expected no misses for a secret-free bundle, got %v", misses)
	}
}

// TestPreflight_UndeclaredSecretMountBlocks wires the gate through the full
// Preflight: a workload mounting an undeclared Secret makes the result NOT OK
// even with NO cluster context (the gate is pure / render-time).
func TestPreflight_UndeclaredSecretMountBlocks(t *testing.T) {
	opts := PreflightOpts{
		Manifests: mountManifest, // mounts cp-daemon-kubeconfig + app-secrets, nothing supplies them
		// No Context, no Secrets — the LIVE checks are inert; only the
		// render-time back-prop gate runs.
	}
	res, err := runPreflightChecks(context.Background(), opts, CollectManifestRefs(opts.Manifests))
	if err != nil {
		t.Fatalf("runPreflightChecks: %v", err)
	}
	if res.OK() {
		t.Fatal("expected result NOT OK when a workload mounts an undeclared Secret")
	}
	if len(res.UndeclaredSecretMounts) != 2 {
		t.Fatalf("expected 2 undeclared mounts (cp-daemon-kubeconfig + app-secrets), got %v", res.UndeclaredSecretMounts)
	}
	report := FormatPreflightReport(res)
	if !strings.Contains(report, "cp-daemon-kubeconfig") || !strings.Contains(report, "FailedMount") {
		t.Fatalf("preflight report missing the undeclared-mount block:\n%s", report)
	}
}

// TestPreflight_UndeclaredSecretMountSatisfiedBySupply — supplying both Secrets
// (one rendered, one KubeconfigSecret) passes through the full Preflight.
func TestPreflight_UndeclaredSecretMountSatisfiedBySupply(t *testing.T) {
	opts := PreflightOpts{
		Manifests:    renderedSecretDoc + docDelimiter + mountManifest, // app-secrets rendered in-stream
		SecretSupply: []SecretSupply{{Name: "cp-daemon-kubeconfig", Kind: SupplyKubeconfigSecret}},
	}
	res, err := runPreflightChecks(context.Background(), opts, CollectManifestRefs(opts.Manifests))
	if err != nil {
		t.Fatalf("runPreflightChecks: %v", err)
	}
	if len(res.UndeclaredSecretMounts) != 0 {
		t.Fatalf("expected no undeclared mounts when supply covers both, got %v", res.UndeclaredSecretMounts)
	}
	if !res.OK() {
		t.Fatalf("expected OK result, got %+v", res)
	}
}
