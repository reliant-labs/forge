// Package cli — the deploy-time "ensure cluster deps are PRESENT" phase.
//
// WHY THIS PHASE (design rationale)
// ---------------------------------
// `forge cluster-setup <env>` is the EXPLICIT install/upgrade verb for an
// env's cluster-level ingress + TLS stack (cert-manager, Envoy Gateway + the
// `eg` GatewayClass + the Gateway API CRDs, and the project-declared
// ClusterIssuers). It is privileged, infrequent, and stays the place an
// operator goes to BUMP a pinned version (`--force`).
//
// But requiring a separate `cluster-setup` run before every routine
// `forge deploy <env>` is a footgun: a fresh cluster (or one where the
// Gateway API channel was never installed) hard-BLOCKS the deploy at the
// preflight CRD gate ("no matches for kind GRPCRoute"), and the only remedy
// is "go run the other command". `forge deploy <env>` should "just work"
// declaratively — its cluster dependencies should be PRESENT before apply.
//
// This phase folds an "ensure-present" pass into deploy, BEFORE the first
// apply, in a strict INSTALL-IF-MISSING / NEVER-UPGRADE mode:
//
//   - cert-manager / the `eg` GatewayClass / Envoy ABSENT → install the
//     pinned versions (the same install cluster-setup runs).
//   - any of them PRESENT → leave UNTOUCHED. A routine app deploy must NEVER
//     `helm upgrade` cluster-wide infra; a version change stays an explicit,
//     deliberate `forge cluster-setup --force`.
//
// This is exactly what `runClusterSetupSteps` already does when `force` is
// false (it SKIPS the helm installs when the GatewayClass / cert-manager
// Deployment is present, and only re-runs them under `--force`). So the
// ensure phase REUSES that orchestration — never with `force` — and is
// guaranteed to share the install-if-missing logic with cluster-setup.
//
// One deliberate difference from cluster-setup: the ensure phase does NOT
// re-apply the declared ClusterIssuers on every deploy. cluster-setup
// re-applies them (so an edited issuer heals on an explicit re-run); but
// re-running `kubectl apply` of a ClusterIssuer on every routine deploy is
// the same class of "routine deploy mutates cluster-wide infra" the guardrail
// forbids. The ensure phase therefore applies issuers ONLY when they are
// ABSENT (install-if-missing, symmetric with the helm steps). Editing an
// issuer remains a `forge cluster-setup <env>` action.
//
// IDEMPOTENT + CHEAP WHEN PRESENT: the steady state (everything installed) is
// a few `kubectl get` existence checks and zero installs. DECLARATION-SCOPED:
// the phase runs ONLY when the env actually declares the ingress feature, and
// applies only the issuers that env declares — it never installs a stack an
// env doesn't use.
package cli

import (
	"context"
	"fmt"

	"github.com/reliant-labs/forge/internal/projectstore"
)

// shouldEnsureClusterDeps is the gating predicate for the deploy-time ensure
// phase, factored out of runDeploy so the skip conditions are unit-testable.
// The ensure phase runs ONLY for a k8s-targeting deploy that is not a dry-run,
// not a rollback, and not opted out via --no-cluster-ensure:
//
//   - hasK8s: external-only / compose-only envs touch no cluster, so there is
//     no forge ingress stack to ensure.
//   - dryRun: renders only — never touch a cluster.
//   - rollback: reuses what's already in the cluster; installs nothing.
//   - noClusterEnsure: the explicit opt-out for deployers with only
//     namespace-scoped RBAC (they pre-run `forge cluster-setup`).
func shouldEnsureClusterDeps(hasK8s, dryRun, rollback, noClusterEnsure bool) bool {
	return hasK8s && !dryRun && !rollback && !noClusterEnsure
}

// ensureClusterDepsForDeploy is the deploy-time "ensure cluster deps are
// PRESENT" phase. It runs BEFORE the first apply and installs forge's own
// ingress/cert stack (cert-manager + Envoy Gateway + the eg GatewayClass + the
// env's declared ClusterIssuers) ONLY when ABSENT — never upgrading a present
// install. It makes the missing-CRD / missing-controller situation
// self-healing on deploy instead of a hard preflight block + a manual
// cluster-setup step.
//
// It is a NO-OP (returns nil without touching the cluster) when:
//   - the env declares no kubectl context (kctx == ""), or
//   - the project does not enable the ingress feature.
//
// In both cases there is no forge-managed ingress stack to ensure, so deploy
// proceeds as before (and the preflight CRD gate still BLOCKS for any CRD
// deploy genuinely can't fix — e.g. a kind whose controller forge doesn't
// manage).
//
// It reuses runClusterSetupSteps with force=false, which is precisely the
// install-if-missing / never-upgrade contract: present helm installs are
// SKIPPED, absent ones are installed at the pinned versions. The one
// adjustment versus the explicit cluster-setup verb is ensureIssuers=true
// (apply declared issuers only when ABSENT, not the unconditional re-apply
// cluster-setup does) — a routine deploy must not re-mutate cluster-wide infra.
func ensureClusterDepsForDeploy(ctx context.Context, store projectstore.ProjectStore, envName, kctx string) error {
	if kctx == "" {
		// No declared context → nothing forge can target; deploy's own
		// context guards handle the user-facing error if a k8s service needs
		// one. Don't install anything.
		return nil
	}
	if !store.Features().IngressEnabled() {
		// The env doesn't use forge's ingress/cert stack — don't install a
		// stack it doesn't declare. (DECLARATION-SCOPED guardrail.)
		return nil
	}

	fmt.Printf("Ensuring cluster dependencies are present on %s (install-if-missing; never upgrades)...\n", kctx)

	issuers := store.Features().IngressIssuersForEnv(envName)
	return runClusterSetupSteps(ctx, clusterSetupPlan{
		env:        envName,
		kctx:       kctx,
		issuers:    issuers,
		projectDir: projectDirForKCL(),
		opts: clusterSetupOptions{
			// force=false is the GUARDRAIL: a present cert-manager / Envoy /
			// GatewayClass is left UNTOUCHED. A version change stays an
			// explicit `forge cluster-setup --force`.
			force: false,
			// ensureIssuers=true: apply the declared ClusterIssuers only when
			// ABSENT (install-if-missing), rather than the unconditional
			// re-apply the explicit cluster-setup verb does. A routine deploy
			// must not re-mutate cluster-wide infra on every run.
			ensureIssuers: true,
		},
	})
}
