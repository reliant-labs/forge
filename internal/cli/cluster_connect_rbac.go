package cli

// The objects forge converges IN THE TARGET CLUSTER so the hub's apply can
// work, and the one-time cloud IAM grant only the owner can run.
//
// ── TWO IDENTITIES ARE IN PLAY, AND THEY ARE NOT THE SAME ONE ───────────────
//
// This is the part of the connected-cluster path most likely to be misread, so
// it is stated here in full. Getting it half right produces an apply that is
// authenticated and still Forbidden.
//
//  1. THE CONNECT IDENTITY authenticates the TLS connection to the target's
//     API server. For the gcp auth that is the hub's own GCP service account
//     (HubIdentity.gcpServiceAccount) — GKE authenticates a GSA by its email
//     as the RBAC username. For the token auth it is the ServiceAccount forge
//     mints below, and the token is that SA's.
//
//  2. THE IMPERSONATED IDENTITY authorizes the apply once connected, and it is
//     NOT skippable. The hub's kustomize-controller runs with
//     `--default-service-account=reliant-deploy-tenant`, so Flux impersonates
//     `system:serviceaccount:<the Kustomization's own namespace>:reliant-deploy-tenant`
//     whether or not the Kustomization asks for it. The Kustomization lives in
//     the hub namespace in the CONTROL PLANE's cluster, so the username the
//     target cluster authenticates is `system:serviceaccount:flux-<org>:reliant-deploy-tenant`
//     — a namespace that does not exist in the target and does not need to.
//
// SO THE CONNECT IDENTITY NEEDS THREE THINGS, and the grant is wrong without
// any one of them:
//
//   - reach: for gcp, `roles/container.clusterViewer` on the project, which is
//     what lets the hub resolve the cluster and exchange a token for it;
//   - the bootstrap writes: the control plane creates the destination
//     Namespace, the tenant ServiceAccount and its Role/RoleBinding in the
//     target itself, through this credential;
//   - impersonate, on the tenant username above. Without it the connection
//     succeeds and every apply is Forbidden on a user nobody bound.
//
// The alternative — asking the owner to pre-create a namespace literally named
// `flux-<org>` with an SA and RBAC inside it — leaks the hub's internal naming
// into a customer's cluster as a hard requirement, and is a multi-object manual
// step. forge applies the cluster-scoped half instead, and the control plane
// creates the rest with the credential this grant authorizes.

import (
	"fmt"
	"io"
	"strings"
)

// The impersonation identity, mirrored from the control plane's
// fluxtenant.TenantServiceAccount / HubNamespacePrefix.
//
// DUPLICATED DELIBERATELY, because forge does not import the control plane —
// the same reason the wire structs are declared locally. These two strings are
// part of the CONTRACT the connect response implies: an owner's cluster is
// granting access to this username, so it is as public as the RPC's field
// names. The agreement is pinned by a test against the recorded wire fixture.
const (
	hubTenantServiceAccount = "reliant-deploy-tenant"
	hubNamespacePrefix      = "flux-"
)

// hubTenantUsername is the RBAC username the target cluster authenticates when
// the hub applies: a Kubernetes ServiceAccount username is exactly
// `system:serviceaccount:<namespace>:<name>`.
func hubTenantUsername(org string) string {
	return "system:serviceaccount:" + hubNamespacePrefix + org + ":" + hubTenantServiceAccount
}

// connectBootstrapName is the name of the ClusterRole / ClusterRoleBinding
// forge converges, namespaced by the connected cluster's name.
//
// NAMED PER CLUSTER rather than once globally, because two forge projects
// connecting the same physical cluster under two names must not fight over one
// object — each `connect` rewriting the other's subject.
func connectBootstrapName(clusterName string) string {
	return "forge-connect-" + clusterName
}

// connectRBAC is everything the bootstrap render needs. Pure inputs, so the
// manifests below are testable with no cluster and `--dry-run` prints exactly
// what a real run applies.
type connectRBAC struct {
	// ClusterName is the connected cluster's forge name (the RPC's `name`).
	ClusterName string
	// KubeContext is the operator's context the bootstrap is applied
	// THROUGH. Distinct from ClusterName: the forge name is what the control
	// plane and the KCL binding address, the context is how the owner's
	// machine reaches the cluster, and they are routinely different.
	KubeContext string
	// Subject is the connect identity as RBAC sees it. For gcp that is the
	// hub's GSA email, bound as a User; for token it is the minted
	// ServiceAccount, bound as a ServiceAccount in TokenNamespace.
	Subject string
	// Auth selects which of the two the Subject is.
	Auth string
	// Org is the control plane organization, for the impersonated username.
	Org string
	// TokenNamespace / TokenServiceAccount name the SA forge mints on the
	// token auth. Empty on gcp.
	TokenNamespace      string
	TokenServiceAccount string
}

// tokenSecretName is the SA token Secret's name, derived so connect and
// disconnect cannot disagree about which object to delete.
func (r connectRBAC) tokenSecretName() string { return r.TokenServiceAccount + "-token" }

// bootstrapManifests renders the objects forge applies THROUGH THE OWNER'S
// OWN CONTEXT — the one write in this command that needs the owner's
// credentials rather than the hub's.
//
// It is a bootstrap write, not an env apply: it grants the hub the ability to
// perform env applies later, and it is converged by `cluster connect` because
// no deploy can create the permission that lets the deploy run.
func (r connectRBAC) bootstrapManifests() string {
	var docs []string
	add := func(s string) { docs = append(docs, strings.TrimRight(s, "\n")) }
	labels := "  labels:\n" +
		"    " + forgeManagedLabelKey + ": " + forgeManagedLabelValue + "\n" +
		"    forge.dev/connected-cluster: " + r.ClusterName + "\n"
	name := connectBootstrapName(r.ClusterName)

	if r.Auth == authToken {
		// The credential itself: a namespace, a ServiceAccount, and a
		// long-lived token Secret.
		//
		// A `kubernetes.io/service-account-token` SECRET, NOT A TokenRequest,
		// and the trade-off is worth stating. A TokenRequest token is bounded
		// and would have to be rotated by re-running `connect` — but nothing
		// rotates it on a schedule, so a cluster nobody re-connects for 90
		// days stops deploying, and the failure arrives as an authentication
		// error during an unrelated release. The Secret token has no such
		// deadline, it is REVOCABLE by deleting it (which `disconnect` does),
		// and its blast radius is bounded by the RBAC below — which is the
		// control that actually matters. When rotation infrastructure exists,
		// this is the one place that changes.
		add("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + r.TokenNamespace + "\n" + labels)
		add("apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: " + r.TokenServiceAccount +
			"\n  namespace: " + r.TokenNamespace + "\n" + labels)
		add("apiVersion: v1\nkind: Secret\nmetadata:\n  name: " + r.tokenSecretName() +
			"\n  namespace: " + r.TokenNamespace + "\n" +
			"  annotations:\n    kubernetes.io/service-account.name: " + r.TokenServiceAccount + "\n" +
			labels + "type: kubernetes.io/service-account-token\n")
	}

	add("apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: " + name + "\n" +
		labels + connectBootstrapRules(r.Org))
	add("apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: " + name + "\n" +
		labels +
		"roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: " + name + "\n" +
		r.subjectYAML())
	return strings.Join(docs, "\n---\n") + "\n"
}

// subjectYAML renders the connect identity as an RBAC subject. A GCP service
// account is a User (GKE authenticates it by email); a minted token is its
// ServiceAccount.
func (r connectRBAC) subjectYAML() string {
	if r.Auth == authToken {
		return "subjects:\n- kind: ServiceAccount\n  name: " + r.TokenServiceAccount +
			"\n  namespace: " + r.TokenNamespace + "\n"
	}
	return "subjects:\n- kind: User\n  name: " + r.Subject +
		"\n  apiGroup: rbac.authorization.k8s.io\n"
}

// connectBootstrapRules is the narrowest rule set that lets the control plane
// deploy into this cluster. Every rule is here because something concrete
// fails without it.
func connectBootstrapRules(org string) string {
	return `rules:
# The destination Namespace per environment. The control plane creates it in
# the target itself, through this credential, exactly as it does for a cluster
# we operate.
- apiGroups: [""]
  resources: ["namespaces"]
  verbs: ["get", "list", "watch", "create", "patch", "update"]
# The tenant ServiceAccount the apply is impersonated as, created in the
# destination namespace alongside its Role and RoleBinding.
- apiGroups: [""]
  resources: ["serviceaccounts"]
  verbs: ["get", "list", "watch", "create", "patch", "update", "delete"]
# The tenant Role and RoleBinding. The bind and escalate verbs are REQUIRED
# and are not a widening: Kubernetes refuses to let a principal create a Role carrying
# rules it does not itself hold, so without them the control plane cannot
# create the tenant Role at all — the apply fails with a privilege-escalation
# denial that reads like a bug somewhere else. These two verbs are the
# documented way to delegate that, and they are scoped to this one API group.
- apiGroups: ["rbac.authorization.k8s.io"]
  resources: ["roles", "rolebindings"]
  verbs: ["get", "list", "watch", "create", "patch", "update", "delete", "bind", "escalate"]
# IMPERSONATE, ON EXACTLY ONE USERNAME. This is the rule whose absence makes a
# perfectly connected cluster refuse every apply: Flux presents the tenant
# username, not this identity, so a connection with no impersonate grant is
# authenticated and unauthorized. resourceNames pins it to the single username
# the hub can ever present, so this grants nothing else.
- apiGroups: [""]
  resources: ["serviceaccounts"]
  verbs: ["impersonate"]
  resourceNames: ["` + hubTenantServiceAccount + `"]
- apiGroups: [""]
  resources: ["users"]
  verbs: ["impersonate"]
  resourceNames: ["` + hubTenantUsername(org) + `"]
`
}

// writeGrantInstructions prints the one-time grant the OWNER runs, and it is
// the whole point of the response carrying HubIdentity.
//
// forge cannot perform this itself: the gcp half is an IAM write in the
// owner's project, against credentials forge does not have and should not ask
// for. So the command's real output is this text — it is the answer, not a
// footnote under a status line.
func writeGrantInstructions(out io.Writer, r connectRBAC, gke gkeContext, hubGSA string) {
	if r.Auth != authGCP {
		// The token path needs no cloud grant: forge already applied the
		// RBAC and sent the token. Say so, rather than printing nothing and
		// leaving the reader wondering what they owe.
		fmt.Fprintf(out, "\nNo cloud IAM grant is needed: this cluster authenticates with the scoped\n"+
			"ServiceAccount token forge just minted, and forge applied its RBAC through\n"+
			"context %q. The token was sent write-only and is never printed or stored locally.\n", r.KubeContext)
		return
	}
	if strings.TrimSpace(hubGSA) == "" {
		// EMPTY IS THE NORMAL STATE IN DEV, where the control plane has no
		// cloud identity. Saying so is correct; printing a gcloud command
		// with a blank principal in it is how someone runs a grant that
		// silently binds nothing.
		fmt.Fprintf(out, "\n⚠ This control plane reports no GCP identity, so there is no principal to grant.\n"+
			"  The cluster is registered, but the hub cannot authenticate to it until the\n"+
			"  deployment has a GCP service account. A dev control plane normally has none —\n"+
			"  use --auth token against a dev cluster.\n")
		return
	}
	fmt.Fprintf(out, `
Run this ONCE, as an owner of project %s. forge cannot: it is an IAM write in
your project, with credentials forge does not hold.

  gcloud projects add-iam-policy-binding %s \
    --member=serviceAccount:%s \
    --role=roles/container.clusterViewer

That is the whole cloud half. forge already applied the in-cluster half through
context %s: ClusterRole %s, bound to %s,
carrying the bootstrap writes the platform needs plus permission to impersonate
%s.
That impersonation is the grant people miss: it is the username Flux presents
when it applies, so without it the cluster connects and every apply is Forbidden.
`,
		gke.Project, gke.Project, hubGSA, r.KubeContext, connectBootstrapName(r.ClusterName), hubGSA,
		hubTenantUsername(r.Org))
}
