// Package cli — `forge cluster-setup <env>` cloud ingress/cert bootstrap.
//
// WHY THIS COMMAND (design rationale)
// -----------------------------------
// forge already bootstraps the ingress stack for the k3d clusters it
// CREATES (installClusterIngress / installIngressBundle in
// cluster_phase.go + dev_cluster_ingress.go). The CLOUD clusters
// (staging Vultr / preprod + prod GKE) are PRE-EXISTING — forge deploys
// to them but never bootstraps their cluster-level dependencies. Today
// that bootstrap is ad-hoc `helm install` run by hand (Taskfile's
// helm-cert-manager / staging-issuer, deploy/certs). This command makes
// it DECLARATIVE: forge drives the same Envoy Gateway install it runs on
// k3d, PLUS cert-manager, PLUS the env's declared ClusterIssuer(s), into
// the env's declared cluster — idempotently.
//
// It is a STANDALONE command rather than a phase of `forge deploy <env>`
// on purpose:
//
//   - Installing cluster-wide infra (cert-manager, Envoy Gateway, CRDs,
//     ClusterIssuers) is a privileged, infrequent BOOTSTRAP. It needs
//     cluster-admin and is orthogonal to the routine app rollout. Wiring
//     it into every `forge deploy prod` would force cluster-admin onto
//     the hot path and slow every deploy with a no-op skip-scan.
//   - Keeping it separate matches how operators think: "set the cluster
//     up once" vs "ship the app many times". `forge deploy` stays the
//     app-rollout verb; `forge cluster-setup` is the one-time (per
//     cluster) dependency bootstrap.
//
// It targets the env's DECLARED kubectl context (forge.K8sCluster.cluster,
// the same source `forge deploy --explain` reads) so it can't land on the
// wrong cluster — there is no current-context fallback.
//
// MECHANISM vs DECLARATION
// ------------------------
// forge is the MECHANISM: install Envoy Gateway + the Gateway API CRDs +
// the `eg` GatewayClass (reusing installIngressBundle), install
// cert-manager (helm jetstack/cert-manager, crds.enabled=true), and apply
// whatever ClusterIssuer files the project declares. The PROJECT declares
// the WHAT: `experimental.ingress_issuers[<env>]` in forge.yaml names the
// issuer manifest files (project-owned, e.g. deploy/certs/issuers/*.yaml).
// forge ships no issuers and hardcodes none.
//
// IDEMPOTENCY
// -----------
// Both helm installs are `upgrade --install` (create cold / upgrade warm).
// On top of that, the Envoy + cert-manager steps SKIP when the cluster
// already carries them (GatewayClass `eg` present; cert-manager Deployment
// present) — so a re-run, and staging (which already has both), is a fast
// no-op for the install steps. The ClusterIssuer apply is always re-run
// (kubectl apply is idempotent) so a freshly-edited issuer heals.
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
)

// clusterSetup*Fn are the indirection seams (mirroring cluster_phase.go's
// *Fn pattern) so the orchestration in runClusterSetup is unit-testable
// without shelling out to helm / kubectl. Production wires them to the real
// implementations; tests stub them to assert each step is invoked and the
// idempotency skips fire.
var (
	gatewayClassExistsFn  = gatewayClassExists
	certManagerExistsFn   = certManagerInstalled
	installEnvoyStackFn   = installEnvoyStack
	installCertManagerFn  = installCertManagerStack
	applyClusterIssuersFn = applyClusterIssuers
	// kubectlApplyBytesFn is the seam the issuer apply pipes through so the
	// per-file apply is unit-testable without shelling out to kubectl.
	kubectlApplyBytesFn = kubectlApplyBytes
)

func newClusterSetupCmd() *cobra.Command {
	var (
		force       bool
		skipIngress bool
		skipCertMgr bool
		skipIssuers bool
	)
	cmd := &cobra.Command{
		Use:   "cluster-setup <environment>",
		Short: "Install the ingress/cert stack (Envoy Gateway + cert-manager + ClusterIssuers) on a cloud cluster",
		Long: `Idempotently bootstrap an environment's cluster-level ingress + TLS dependencies.

This is the CLOUD counterpart to the k3d ingress bring-up forge runs for
the clusters it creates. It installs, into the env's DECLARED kubectl
context (forge.K8sCluster.cluster):

  1. The Gateway API standard-channel CRDs + the Envoy Gateway controller
     (helm oci://docker.io/envoyproxy/gateway-helm) + the eg GatewayClass —
     the SAME stack forge installs on k3d.
  2. cert-manager (helm jetstack/cert-manager, crds.enabled=true).
  3. The env's declared ClusterIssuer manifest(s), from
     experimental.ingress_issuers[<env>] in forge.yaml.

Everything is idempotent: the helm installs are upgrade --install, and the
Envoy / cert-manager steps SKIP when the cluster already carries them
(GatewayClass eg present; cert-manager Deployment present). Re-running — or
running against staging, which already has the stack — is a no-op for the
install steps. ClusterIssuers are re-applied each run (kubectl apply).

The env declares its issuers; forge applies whatever it names — forge ships
no issuers of its own.

Examples:
  forge cluster-setup staging       # no-op on the install steps (already present)
  forge cluster-setup preprod       # install Envoy + cert-manager + issuers
  forge cluster-setup prod
  forge cluster-setup prod --force   # re-run install steps even if present
  forge cluster-setup prod --skip-cert-manager  # only the Envoy/Gateway stack`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterSetup(cmd.Context(), args[0], clusterSetupOptions{
				force:           force,
				skipIngress:     skipIngress,
				skipCertManager: skipCertMgr,
				skipIssuers:     skipIssuers,
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Re-run the Envoy + cert-manager installs even when the cluster already carries them (default: skip when present)")
	cmd.Flags().BoolVar(&skipIngress, "skip-ingress", false, "Skip the Envoy Gateway + Gateway API CRDs + eg GatewayClass install")
	cmd.Flags().BoolVar(&skipCertMgr, "skip-cert-manager", false, "Skip the cert-manager install")
	cmd.Flags().BoolVar(&skipIssuers, "skip-issuers", false, "Skip applying the env's declared ClusterIssuer manifests")
	return cmd
}

// clusterSetupOptions bundles the cluster-setup flags.
type clusterSetupOptions struct {
	force           bool
	skipIngress     bool
	skipCertManager bool
	skipIssuers     bool
}

// runClusterSetup is the cluster-setup orchestration. It resolves the
// env's declared kubectl context, verifies it exists in the kubeconfig
// (fail-fast, same guard as deploy), then runs the three idempotent
// install steps via the *Fn seams.
func runClusterSetup(ctx context.Context, envName string, opts clusterSetupOptions) error {
	store, err := loadProjectStore()
	if err != nil {
		return err
	}
	cfg := store.Config()

	if !store.Features().DeployEnabled() {
		return config.DisabledFeatureError(config.FeatureDeploy)
	}
	if !store.Features().IngressEnabled() {
		return fmt.Errorf("cluster-setup requires experimental.ingress: true in forge.yaml " +
			"(it installs the Gateway API + cert-manager ingress stack)")
	}

	// Resolve the env's DECLARED kubectl context — the same source
	// `forge deploy --explain` reads (forge.K8sCluster.cluster). No
	// current-context fallback: the binding lives in the env's KCL, so we
	// can't land the install on the wrong cluster.
	kctx := expectedClusterForEnv(ctx, cfg, envName)
	if kctx == "" {
		return fmt.Errorf(
			"env %q declares no forge.K8sCluster.cluster in deploy/kcl/%s/main.k — "+
				"cluster-setup needs the declared kubectl context to know which cluster to target",
			envName, envName)
	}
	// Fail fast if the declared context isn't in the kubeconfig (mirrors
	// the deploy guard) — better than a cryptic mid-install helm error.
	available, err := kubectlContextNames(ctx)
	if err != nil {
		return err
	}
	if verr := declaredContextExistsVerdict(envName, kctx, available); verr != nil {
		return verr
	}

	fmt.Printf("Cluster-setup for env %q\n", envName)
	fmt.Printf("  kubectl context: %s (declared by forge.K8sCluster.cluster)\n", kctx)
	fmt.Println()

	issuers := store.Features().IngressIssuersForEnv(envName)
	return runClusterSetupSteps(ctx, clusterSetupPlan{
		env:        envName,
		kctx:       kctx,
		issuers:    issuers,
		projectDir: projectDirForKCL(),
		opts:       opts,
	})
}

// clusterSetupPlan is the resolved input to the install-step orchestration:
// the env's declared context, its declared issuer paths, the project root,
// and the flag options. Splitting it out (from the store/context resolution
// in runClusterSetup) lets the step logic — the idempotency skips, the
// step invocation order — be unit-tested via the *Fn seams without a
// project on disk or kubectl configured.
type clusterSetupPlan struct {
	env        string
	kctx       string
	issuers    []string
	projectDir string
	opts       clusterSetupOptions
}

// runClusterSetupSteps runs the three idempotent install steps in order
// (Envoy stack, cert-manager, ClusterIssuers), honouring the skip flags and
// the present-skip idempotency checks. Every external action goes through a
// *Fn seam so tests can assert exactly which steps fired.
func runClusterSetupSteps(ctx context.Context, p clusterSetupPlan) error {
	start := time.Now()

	// 1. Envoy Gateway + Gateway API CRDs + eg GatewayClass.
	if p.opts.skipIngress {
		fmt.Println("[ingress] skipped (--skip-ingress)")
	} else {
		present, err := gatewayClassExistsFn(ctx, p.kctx)
		if err != nil {
			return fmt.Errorf("check eg GatewayClass on %s: %w", p.kctx, err)
		}
		if present && !p.opts.force {
			fmt.Printf("[ingress] eg GatewayClass already present on %s — skipping Envoy install (--force to re-run)\n", p.kctx)
		} else {
			fmt.Printf("[ingress] installing Gateway API + Envoy Gateway on %s...\n", p.kctx)
			if err := installEnvoyStackFn(ctx, p.kctx); err != nil {
				return fmt.Errorf("install Envoy Gateway on %s: %w", p.kctx, err)
			}
		}
	}

	// 2. cert-manager.
	if p.opts.skipCertManager {
		fmt.Println("[cert-manager] skipped (--skip-cert-manager)")
	} else {
		present, err := certManagerExistsFn(ctx, p.kctx)
		if err != nil {
			return fmt.Errorf("check cert-manager on %s: %w", p.kctx, err)
		}
		if present && !p.opts.force {
			fmt.Printf("[cert-manager] cert-manager Deployment already present on %s — skipping install (--force to re-run)\n", p.kctx)
		} else {
			fmt.Printf("[cert-manager] installing cert-manager %s on %s...\n", certManagerChartVersion, p.kctx)
			if err := installCertManagerFn(ctx, p.kctx); err != nil {
				return fmt.Errorf("install cert-manager on %s: %w", p.kctx, err)
			}
		}
	}

	// 3. Project-declared ClusterIssuer(s). Always re-applied (idempotent
	//    kubectl apply) so an edited issuer heals on re-run.
	if p.opts.skipIssuers {
		fmt.Println("[issuers] skipped (--skip-issuers)")
	} else if len(p.issuers) == 0 {
		fmt.Printf("[issuers] env %q declares no experimental.ingress_issuers — no ClusterIssuer applied\n", p.env)
	} else if err := applyClusterIssuersFn(ctx, p.kctx, p.projectDir, p.issuers); err != nil {
		return err
	}

	fmt.Printf("\nCluster-setup completed in %s.\n", time.Since(start).Truncate(time.Millisecond))
	return nil
}

// gatewayClassExists reports whether the `eg` GatewayClass is present in
// the given context — the signal that the Envoy Gateway stack is already
// installed.
func gatewayClassExists(ctx context.Context, kctx string) (bool, error) {
	return resourceExists(ctx, kctx, "gatewayclass", "eg")
}

// certManagerInstalled reports whether the cert-manager controller
// Deployment is present in the cert-manager namespace — the signal that
// cert-manager is already installed.
func certManagerInstalled(ctx context.Context, kctx string) (bool, error) {
	return resourceExists(ctx, kctx, "deployment", "cert-manager", "-n", certManagerNamespace)
}

// installEnvoyStack installs the Gateway API CRDs + Envoy Gateway
// controller + eg GatewayClass into the given context. Reuses the SAME
// installIngressBundle the k3d path runs — it is cluster-agnostic (the
// projectDir/env params are informational; Envoy derives proxy listeners
// from each Gateway's spec.listeners, so there's no k3d-specific state).
func installEnvoyStack(ctx context.Context, kctx string) error {
	return installIngressBundle(ctx, kctx, projectDirForKCL(), "")
}

// installCertManagerStack registers the jetstack helm repo and runs the
// pinned cert-manager helm upgrade --install into the given context.
func installCertManagerStack(ctx context.Context, kctx string) error {
	if err := helmRepoAddUpdate(ctx, certManagerChartRepoName, certManagerChartRepoURL); err != nil {
		return err
	}
	return helmInstallCertManager(ctx, kctx, certManagerChartVersion)
}

// applyClusterIssuers applies each project-declared ClusterIssuer manifest
// (path or glob, relative to projectDir) into the given context via
// `kubectl apply`. Globs that match nothing, and explicit paths that don't
// exist, are hard errors — a declared-but-missing issuer is almost
// certainly a typo, and silently skipping it would leave TLS un-bootstrapped
// with no signal.
func applyClusterIssuers(ctx context.Context, kctx, projectDir string, issuers []string) error {
	for _, decl := range issuers {
		pattern := decl
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(projectDir, pattern)
		}
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("ingress_issuers pattern %q: %w", decl, err)
		}
		if len(matches) == 0 {
			// Not a glob (or matched nothing): if it's a plain path that
			// exists, use it; otherwise error so the typo surfaces.
			if _, statErr := os.Stat(pattern); statErr == nil {
				matches = []string{pattern}
			} else {
				return fmt.Errorf("ingress_issuers entry %q matched no files (looked under %s)", decl, pattern)
			}
		}
		for _, path := range matches {
			fmt.Printf("[issuers] applying %s...\n", path)
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return fmt.Errorf("read issuer %s: %w", path, rerr)
			}
			if aerr := kubectlApplyBytesFn(ctx, kctx, data); aerr != nil {
				return fmt.Errorf("apply issuer %s: %w", path, aerr)
			}
		}
	}
	return nil
}
