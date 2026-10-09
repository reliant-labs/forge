package cli

// The objects forge converges IN THE TARGET CLUSTER so the hub's apply can
// work.
//
// ── TWO IDENTITIES ARE IN PLAY, AND THEY ARE NOT THE SAME ONE ───────────────
//
// This is the part of the connected-cluster path most likely to be misread, so
// it is stated here in full. Getting it half right produces an apply that is
// authenticated and still Forbidden.
//
//  1. THE CONNECT IDENTITY authenticates the TLS connection to the target's
//     API server. It is the ServiceAccount forge mints below, and the token
//     the control plane holds is that SA's.
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
// SO forge GRANTS EACH IDENTITY EXACTLY ONE THING:
//
//   - the connect identity may IMPERSONATE the deploy ServiceAccount name, and
//     nothing else. It cannot read a Secret or create a Namespace in its own
//     right, so a leaked connect token is worth no more than the identity it
//     can become;
//   - the impersonated identity — this org's, pinned by namespace — gets the
//     WHOLE CLUSTER (cluster-admin). That is what the owner connected the
//     cluster for: a self-managed env's bundle carries CRDs, ClusterRoles,
//     webhooks and Namespaces, and an apply that may only write some of its
//     own objects is a deploy that fails halfway. It gives nothing on any
//     other cluster: the binding lives here, and a subject names one org.
//
// Both are cluster-scoped objects on the target, bound to a username whose
// namespace (`flux-<org>`) does not exist there. That is deliberate: asking the
// owner to pre-create a namespace literally named after the hub's internals
// would leak them into a customer's cluster as a hard requirement.

import (
	"fmt"
	"io"
	"strings"
)

// The impersonation identity, mirrored from the control plane's
// the flux deploy-identity constants / HubNamespacePrefix.
//
// DUPLICATED DELIBERATELY, because forge does not import the control plane —
// the same reason the wire structs are declared locally. These two strings are
// part of the CONTRACT the connect response implies: an owner's cluster is
// granting access to this username, so it is as public as the RPC's field
// names. The agreement is pinned by a test against the recorded wire fixture.
const (
	hubDeployServiceAccount = "reliant-deploy-tenant"
	hubNamespacePrefix      = "flux-"
)

// hubDeployUsername is the RBAC username the target cluster authenticates when
// the hub applies: a Kubernetes ServiceAccount username is exactly
// `system:serviceaccount:<namespace>:<name>`.
func hubDeployUsername(org string) string {
	return "system:serviceaccount:" + hubNamespacePrefix + org + ":" + hubDeployServiceAccount
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
	// Org is the control plane organization, for the impersonated username.
	Org string
	// TokenNamespace / TokenServiceAccount name the SA forge mints: the
	// connect identity.
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

	{
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
		labels + connectBootstrapRules())
	add("apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: " + name + "\n" +
		labels +
		"roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: " + name + "\n" +
		r.subjectYAML())
	// THE WHOLE CLUSTER, TO THIS ORG'S PINNED IDENTITY. The subject is the
	// ServiceAccount the hub's Flux impersonates for this org; RBAC matches
	// a ServiceAccount subject on the username alone, so the namespace not
	// existing here is fine. Another org's deploy identity is a different
	// namespace, so it matches nothing.
	add("apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: " +
		connectDeployBindingName(r.ClusterName) + "\n" + labels +
		"roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: cluster-admin\n" +
		"subjects:\n- kind: ServiceAccount\n  name: " + hubDeployServiceAccount +
		"\n  namespace: " + hubNamespacePrefix + r.Org + "\n")
	return strings.Join(docs, "\n---\n") + "\n"
}

// subjectYAML renders the connect identity — the minted ServiceAccount — as
// an RBAC subject.
func (r connectRBAC) subjectYAML() string {
	return "subjects:\n- kind: ServiceAccount\n  name: " + r.TokenServiceAccount +
		"\n  namespace: " + r.TokenNamespace + "\n"
}

// connectBootstrapRules is the connect identity's whole authority: become the
// deploy identity, and nothing else.
//
// THE serviceaccounts RESOURCE, NOT users. The deploy username is
// ServiceAccount-shaped, and the API server's impersonation filter checks an
// impersonated `system:serviceaccount:<ns>:<name>` as the `serviceaccounts`
// resource named <name> in <ns>. A `users` rule naming that username is never
// consulted — it used to be here and granted nothing.
//
// resourceNames pins the name; a ClusterRole cannot pin the namespace, so the
// connect identity may become ANY namespace's `reliant-deploy-tenant` on this
// cluster. That reaches no further than this cluster's own grants: the only
// deploy identity with authority here is the one a connect bound, and an org
// can bind one only on a cluster it already administers.
func connectBootstrapRules() string {
	return `rules:
# IMPERSONATE, ON EXACTLY ONE NAME. Flux presents the deploy username, not
# this identity, so a connection with no impersonate grant is authenticated
# and unauthorized.
- apiGroups: [""]
  resources: ["serviceaccounts"]
  verbs: ["impersonate"]
  resourceNames: ["` + hubDeployServiceAccount + `"]
`
}

// connectDeployBindingName is the ClusterRoleBinding granting this connection's
// org the whole cluster. Per connected-cluster name for the reason
// connectBootstrapName is: two orgs connecting one physical cluster must not
// rewrite each other's subject.
func connectDeployBindingName(clusterName string) string {
	return connectBootstrapName(clusterName) + "-deploy"
}

// writeGrantSummary says what was granted and that nothing is owed. There is
// no cloud half: forge applied the RBAC and sent the token, so a reader who
// expects a one-time IAM step learns there is none rather than wondering what
// they missed.
func writeGrantSummary(out io.Writer, r connectRBAC) {
	fmt.Fprintf(out, "\nNothing else to run: forge applied ClusterRole %s through context %q,\n"+
		"bound to %s/%s, whose token the control plane now holds. The token was\n"+
		"sent write-only and is never printed or stored locally. Flux applies as\n"+
		"%s.\n",
		connectBootstrapName(r.ClusterName), r.KubeContext, r.TokenNamespace, r.TokenServiceAccount,
		hubDeployUsername(r.Org))
}
