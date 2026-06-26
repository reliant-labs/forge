package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// setupSeams swaps the cluster-setup *Fn seams for the duration of a test
// and records which install steps fired. Returns a *setupRecorder whose
// fields are asserted by the caller, plus a restore func registered with
// t.Cleanup.
type setupRecorder struct {
	gwPresent   bool
	cmPresent   bool
	envoyCalls  []string
	certCalls   []string
	issuerCalls [][]string
}

func installSeams(t *testing.T, rec *setupRecorder) {
	t.Helper()
	og, ocm := gatewayClassExistsFn, certManagerExistsFn
	oe, oc, oi := installEnvoyStackFn, installCertManagerFn, applyClusterIssuersFn
	t.Cleanup(func() {
		gatewayClassExistsFn, certManagerExistsFn = og, ocm
		installEnvoyStackFn, installCertManagerFn, applyClusterIssuersFn = oe, oc, oi
	})
	gatewayClassExistsFn = func(_ context.Context, _ string) (bool, error) { return rec.gwPresent, nil }
	certManagerExistsFn = func(_ context.Context, _ string) (bool, error) { return rec.cmPresent, nil }
	installEnvoyStackFn = func(_ context.Context, kctx string) error {
		rec.envoyCalls = append(rec.envoyCalls, kctx)
		return nil
	}
	installCertManagerFn = func(_ context.Context, kctx string) error {
		rec.certCalls = append(rec.certCalls, kctx)
		return nil
	}
	applyClusterIssuersFn = func(_ context.Context, _, _ string, issuers []string) error {
		rec.issuerCalls = append(rec.issuerCalls, issuers)
		return nil
	}
}

// TestClusterSetupSteps_ColdInstall asserts that on a cluster carrying
// neither the GatewayClass nor cert-manager, all three steps fire against
// the declared context, with the env's declared issuers applied.
func TestClusterSetupSteps_ColdInstall(t *testing.T) {
	rec := &setupRecorder{gwPresent: false, cmPresent: false}
	installSeams(t, rec)

	plan := clusterSetupPlan{
		env:     "preprod",
		kctx:    "gke_proj_region_preprod",
		issuers: []string{"deploy/certs/issuers/prod-issuer.yaml"},
	}
	if err := runClusterSetupSteps(t.Context(), plan); err != nil {
		t.Fatalf("runClusterSetupSteps: %v", err)
	}

	if len(rec.envoyCalls) != 1 || rec.envoyCalls[0] != plan.kctx {
		t.Errorf("envoy install fired %v; want exactly [%s]", rec.envoyCalls, plan.kctx)
	}
	if len(rec.certCalls) != 1 || rec.certCalls[0] != plan.kctx {
		t.Errorf("cert-manager install fired %v; want exactly [%s]", rec.certCalls, plan.kctx)
	}
	if len(rec.issuerCalls) != 1 || rec.issuerCalls[0][0] != plan.issuers[0] {
		t.Errorf("issuer apply fired %v; want exactly [%v]", rec.issuerCalls, plan.issuers)
	}
}

// TestClusterSetupSteps_IdempotentSkip asserts that when the cluster
// already carries the GatewayClass + cert-manager (the staging case), the
// two helm installs are SKIPPED — but the declared issuers are STILL
// re-applied (kubectl apply is idempotent; an edited issuer must heal).
func TestClusterSetupSteps_IdempotentSkip(t *testing.T) {
	rec := &setupRecorder{gwPresent: true, cmPresent: true}
	installSeams(t, rec)

	plan := clusterSetupPlan{
		env:     "staging",
		kctx:    "vke-staging",
		issuers: []string{"deploy/certs/issuers/staging-http01-gw-issuer.yaml"},
	}
	if err := runClusterSetupSteps(t.Context(), plan); err != nil {
		t.Fatalf("runClusterSetupSteps: %v", err)
	}

	if len(rec.envoyCalls) != 0 {
		t.Errorf("envoy install fired %v; want skipped (GatewayClass present)", rec.envoyCalls)
	}
	if len(rec.certCalls) != 0 {
		t.Errorf("cert-manager install fired %v; want skipped (Deployment present)", rec.certCalls)
	}
	if len(rec.issuerCalls) != 1 {
		t.Errorf("issuer apply fired %d times; want 1 (always re-applied)", len(rec.issuerCalls))
	}
}

// TestClusterSetupSteps_ForceReinstalls asserts --force overrides the
// present-skip and re-runs the installs even when both are already present.
func TestClusterSetupSteps_ForceReinstalls(t *testing.T) {
	rec := &setupRecorder{gwPresent: true, cmPresent: true}
	installSeams(t, rec)

	plan := clusterSetupPlan{
		env:  "prod",
		kctx: "gke-prod",
		opts: clusterSetupOptions{force: true},
	}
	if err := runClusterSetupSteps(t.Context(), plan); err != nil {
		t.Fatalf("runClusterSetupSteps: %v", err)
	}
	if len(rec.envoyCalls) != 1 || len(rec.certCalls) != 1 {
		t.Errorf("--force should re-run installs; envoy=%v cert=%v", rec.envoyCalls, rec.certCalls)
	}
}

// TestClusterSetupSteps_SkipFlags asserts each --skip-* flag suppresses its
// step and only its step.
func TestClusterSetupSteps_SkipFlags(t *testing.T) {
	rec := &setupRecorder{}
	installSeams(t, rec)

	plan := clusterSetupPlan{
		env:     "prod",
		kctx:    "gke-prod",
		issuers: []string{"deploy/certs/issuers/prod-issuer.yaml"},
		opts: clusterSetupOptions{
			skipIngress:     true,
			skipCertManager: true,
		},
	}
	if err := runClusterSetupSteps(t.Context(), plan); err != nil {
		t.Fatalf("runClusterSetupSteps: %v", err)
	}
	if len(rec.envoyCalls) != 0 || len(rec.certCalls) != 0 {
		t.Errorf("skip flags should suppress installs; envoy=%v cert=%v", rec.envoyCalls, rec.certCalls)
	}
	// Issuers NOT skipped — should still apply.
	if len(rec.issuerCalls) != 1 {
		t.Errorf("issuer apply fired %d times; want 1 (not skipped)", len(rec.issuerCalls))
	}
}

// TestClusterSetupSteps_NoIssuersDeclared asserts an env with no declared
// issuers still installs Envoy + cert-manager and simply applies nothing
// for the issuer step (no error).
func TestClusterSetupSteps_NoIssuersDeclared(t *testing.T) {
	rec := &setupRecorder{}
	installSeams(t, rec)

	plan := clusterSetupPlan{env: "preprod", kctx: "gke-preprod", issuers: nil}
	if err := runClusterSetupSteps(t.Context(), plan); err != nil {
		t.Fatalf("runClusterSetupSteps: %v", err)
	}
	if len(rec.envoyCalls) != 1 || len(rec.certCalls) != 1 {
		t.Errorf("installs should still fire; envoy=%v cert=%v", rec.envoyCalls, rec.certCalls)
	}
	if len(rec.issuerCalls) != 0 {
		t.Errorf("no issuers declared — issuer apply should not fire; got %v", rec.issuerCalls)
	}
}

// TestApplyClusterIssuers_AppliesEachFile checks the issuer apply reads
// each declared file (relative to projectDir) and pipes it to kubectl,
// and that a glob expands. The kubectl apply is stubbed via a seam so the
// test never shells out.
func TestApplyClusterIssuers_AppliesEachFile(t *testing.T) {
	dir := t.TempDir()
	issuerDir := filepath.Join(dir, "deploy", "certs", "issuers")
	if err := os.MkdirAll(issuerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.yaml", "b.yaml"} {
		if err := os.WriteFile(filepath.Join(issuerDir, name), []byte("kind: ClusterIssuer\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var applied []string
	orig := kubectlApplyBytesFn
	t.Cleanup(func() { kubectlApplyBytesFn = orig })
	kubectlApplyBytesFn = func(_ context.Context, _ string, data []byte) error {
		applied = append(applied, string(data))
		return nil
	}

	// One explicit path + one glob covering both files.
	issuers := []string{
		"deploy/certs/issuers/a.yaml",
		"deploy/certs/issuers/*.yaml",
	}
	if err := applyClusterIssuers(t.Context(), "gke-prod", dir, issuers); err != nil {
		t.Fatalf("applyClusterIssuers: %v", err)
	}
	// a.yaml (explicit) + a.yaml + b.yaml (glob) = 3 applies.
	if len(applied) != 3 {
		t.Errorf("kubectl apply fired %d times; want 3 (1 explicit + 2 glob)", len(applied))
	}
}

// TestApplyClusterIssuers_MissingFileErrors asserts a declared-but-absent
// issuer path is a hard error (a typo must surface, not silently skip).
func TestApplyClusterIssuers_MissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	err := applyClusterIssuers(t.Context(), "gke-prod", dir, []string{"deploy/certs/issuers/nope.yaml"})
	if err == nil {
		t.Fatal("expected an error for a missing issuer file, got nil")
	}
}

// TestClusterSetupSteps_StepErrorPropagates confirms a failing install
// step aborts the run (the cluster-setup is a fail-fast bootstrap).
func TestClusterSetupSteps_StepErrorPropagates(t *testing.T) {
	rec := &setupRecorder{}
	installSeams(t, rec)
	wantErr := errors.New("helm boom")
	installEnvoyStackFn = func(_ context.Context, _ string) error { return wantErr }

	err := runClusterSetupSteps(t.Context(), clusterSetupPlan{env: "prod", kctx: "gke-prod"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v; want it to wrap %v", err, wantErr)
	}
	// cert-manager should NOT have run after the envoy step failed.
	if len(rec.certCalls) != 0 {
		t.Errorf("cert-manager ran after envoy failure; want abort")
	}
}
