package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/projectstore"
)

// ingressStore builds a ProjectStore whose ingress feature is on (or off) and
// whose env declares the given issuer paths — the input ensureClusterDepsForDeploy
// reads via store.Features().
func ingressStore(ingress bool, env string, issuers []string) projectstore.ProjectStore {
	exp := config.ExperimentalConfig{Ingress: ingress}
	if len(issuers) > 0 {
		exp.IngressIssuers = map[string][]string{env: issuers}
	}
	return projectstore.New(&config.ProjectConfig{
		Features: config.FeaturesConfig{Experimental: exp},
	})
}

// TestShouldEnsureClusterDeps covers the gating predicate — chiefly that
// --no-cluster-ensure SKIPS the ensure phase entirely, alongside the
// dry-run / rollback / external-only skips and the run-it default.
func TestShouldEnsureClusterDeps(t *testing.T) {
	cases := []struct {
		name                               string
		hasK8s, dryRun, rollback, noEnsure bool
		want                               bool
	}{
		{name: "default k8s deploy runs it", hasK8s: true, want: true},
		{name: "--no-cluster-ensure skips entirely", hasK8s: true, noEnsure: true, want: false},
		{name: "dry-run skips", hasK8s: true, dryRun: true, want: false},
		{name: "rollback skips", hasK8s: true, rollback: true, want: false},
		{name: "external-only env skips", hasK8s: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldEnsureClusterDeps(tc.hasK8s, tc.dryRun, tc.rollback, tc.noEnsure); got != tc.want {
				t.Errorf("shouldEnsureClusterDeps(hasK8s=%v,dryRun=%v,rollback=%v,noEnsure=%v) = %v; want %v",
					tc.hasK8s, tc.dryRun, tc.rollback, tc.noEnsure, got, tc.want)
			}
		})
	}
}

// TestEnsureClusterDeps_InstallsMissing asserts the deploy-time ensure phase
// installs forge's ingress/cert stack when it is ABSENT (the self-healing
// fresh-cluster path that makes `forge deploy <env>` "just work").
func TestEnsureClusterDeps_InstallsMissing(t *testing.T) {
	rec := &setupRecorder{gwPresent: false, cmPresent: false}
	installSeams(t, rec)

	store := ingressStore(true, "preprod", nil)
	if err := ensureClusterDepsForDeploy(t.Context(), store, "preprod", "gke-preprod"); err != nil {
		t.Fatalf("ensureClusterDepsForDeploy: %v", err)
	}
	if len(rec.envoyCalls) != 1 || rec.envoyCalls[0] != "gke-preprod" {
		t.Errorf("envoy install fired %v; want exactly [gke-preprod]", rec.envoyCalls)
	}
	if len(rec.certCalls) != 1 || rec.certCalls[0] != "gke-preprod" {
		t.Errorf("cert-manager install fired %v; want exactly [gke-preprod]", rec.certCalls)
	}
}

// TestEnsureClusterDeps_SkipsPresentNeverUpgrades asserts the KEY guardrail:
// when cert-manager + the eg GatewayClass are already PRESENT, the ensure
// phase installs NOTHING — a routine deploy must never helm-upgrade
// cluster-wide infra (version bumps stay an explicit cluster-setup --force).
func TestEnsureClusterDeps_SkipsPresentNeverUpgrades(t *testing.T) {
	rec := &setupRecorder{gwPresent: true, cmPresent: true}
	installSeams(t, rec)

	store := ingressStore(true, "prod", nil)
	if err := ensureClusterDepsForDeploy(t.Context(), store, "prod", "gke-prod"); err != nil {
		t.Fatalf("ensureClusterDepsForDeploy: %v", err)
	}
	if len(rec.envoyCalls) != 0 {
		t.Errorf("envoy install fired %v; want SKIPPED (GatewayClass present — never upgrade)", rec.envoyCalls)
	}
	if len(rec.certCalls) != 0 {
		t.Errorf("cert-manager install fired %v; want SKIPPED (Deployment present — never upgrade)", rec.certCalls)
	}
}

// TestEnsureClusterDeps_NoopWhenIngressDisabled asserts the
// declaration-scoped guardrail: an env that doesn't enable the ingress
// feature gets NOTHING installed (we never install a stack the env doesn't
// declare).
func TestEnsureClusterDeps_NoopWhenIngressDisabled(t *testing.T) {
	rec := &setupRecorder{gwPresent: false, cmPresent: false}
	installSeams(t, rec)

	store := ingressStore(false, "prod", nil)
	if err := ensureClusterDepsForDeploy(t.Context(), store, "prod", "gke-prod"); err != nil {
		t.Fatalf("ensureClusterDepsForDeploy: %v", err)
	}
	if len(rec.envoyCalls) != 0 || len(rec.certCalls) != 0 {
		t.Errorf("ingress disabled — nothing should install; envoy=%v cert=%v", rec.envoyCalls, rec.certCalls)
	}
}

// TestEnsureClusterDeps_NoopWhenNoContext asserts the phase no-ops when the
// env declares no kubectl context (nothing forge can target).
func TestEnsureClusterDeps_NoopWhenNoContext(t *testing.T) {
	rec := &setupRecorder{gwPresent: false, cmPresent: false}
	installSeams(t, rec)

	store := ingressStore(true, "prod", nil)
	if err := ensureClusterDepsForDeploy(t.Context(), store, "prod", ""); err != nil {
		t.Fatalf("ensureClusterDepsForDeploy: %v", err)
	}
	if len(rec.envoyCalls) != 0 || len(rec.certCalls) != 0 {
		t.Errorf("no context — nothing should install; envoy=%v cert=%v", rec.envoyCalls, rec.certCalls)
	}
}

// TestEnsureIssuers_InstallIfMissing asserts the ensure phase's issuer step is
// INSTALL-IF-MISSING: a declared ClusterIssuer that is ABSENT is applied, and
// one that is already PRESENT is left untouched (a routine deploy must not
// re-mutate a present cluster-wide ClusterIssuer). Driven through
// runClusterSetupSteps with ensureIssuers=true so it exercises the same path
// the ensure phase takes.
func TestEnsureIssuers_InstallIfMissing(t *testing.T) {
	dir := t.TempDir()
	issuerDir := filepath.Join(dir, "deploy", "certs", "issuers")
	if err := os.MkdirAll(issuerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIssuer := func(file, name string) {
		body := "apiVersion: cert-manager.io/v1\nkind: ClusterIssuer\nmetadata:\n  name: " + name + "\n"
		if err := os.WriteFile(filepath.Join(issuerDir, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeIssuer("present.yaml", "already-here")
	writeIssuer("absent.yaml", "needs-install")

	// "already-here" reports present; "needs-install" reports absent.
	origExists := clusterIssuerExistsFn
	t.Cleanup(func() { clusterIssuerExistsFn = origExists })
	clusterIssuerExistsFn = func(_ context.Context, _, name string) (bool, error) {
		return name == "already-here", nil
	}

	var applied []string
	origApply := kubectlApplyBytesFn
	t.Cleanup(func() { kubectlApplyBytesFn = origApply })
	kubectlApplyBytesFn = func(_ context.Context, _ string, data []byte) error {
		applied = append(applied, clusterIssuerName(data))
		return nil
	}

	err := applyClusterIssuers(t.Context(), "gke-prod", dir,
		[]string{"deploy/certs/issuers/present.yaml", "deploy/certs/issuers/absent.yaml"},
		true /* installIfMissing */)
	if err != nil {
		t.Fatalf("applyClusterIssuers: %v", err)
	}
	if len(applied) != 1 || applied[0] != "needs-install" {
		t.Errorf("install-if-missing applied %v; want exactly [needs-install] (present one left untouched)", applied)
	}
}

// TestExplicitIssuers_AlwaysReapply asserts the EXPLICIT cluster-setup verb
// (installIfMissing=false) re-applies EVERY declared issuer regardless of
// presence — so an edited issuer heals. The existence seam must not even be
// consulted on this path.
func TestExplicitIssuers_AlwaysReapply(t *testing.T) {
	dir := t.TempDir()
	issuerDir := filepath.Join(dir, "deploy", "certs", "issuers")
	if err := os.MkdirAll(issuerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "apiVersion: cert-manager.io/v1\nkind: ClusterIssuer\nmetadata:\n  name: prod-issuer\n"
	if err := os.WriteFile(filepath.Join(issuerDir, "prod.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	existsCalled := false
	origExists := clusterIssuerExistsFn
	t.Cleanup(func() { clusterIssuerExistsFn = origExists })
	clusterIssuerExistsFn = func(_ context.Context, _, _ string) (bool, error) {
		existsCalled = true
		return true, nil
	}

	var applied int
	origApply := kubectlApplyBytesFn
	t.Cleanup(func() { kubectlApplyBytesFn = origApply })
	kubectlApplyBytesFn = func(_ context.Context, _ string, _ []byte) error {
		applied++
		return nil
	}

	err := applyClusterIssuers(t.Context(), "gke-prod", dir,
		[]string{"deploy/certs/issuers/prod.yaml"}, false /* installIfMissing */)
	if err != nil {
		t.Fatalf("applyClusterIssuers: %v", err)
	}
	if applied != 1 {
		t.Errorf("explicit verb applied %d times; want 1 (unconditional re-apply)", applied)
	}
	if existsCalled {
		t.Errorf("explicit verb consulted the ClusterIssuer-exists seam; want it bypassed (always re-apply)")
	}
}
