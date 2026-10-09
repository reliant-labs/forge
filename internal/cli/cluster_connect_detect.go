package cli

// Reading a cluster's ADDRESS out of the operator's kubeconfig. Every function
// here is pure or reads only the kubeconfig, so the refusals are testable
// without a cluster.

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
)

// authToken is the one way the hub authenticates to a connected cluster: a
// scoped ServiceAccount token forge mints in the cluster, as the
// controlplane.v1.ClusterAuth value name protojson encodes.
//
// ONE AUTH, ON EVERY CLUSTER. GKE workload identity was a second mode until
// the Flux-only deploy epic: it needed gcloud and an IAM grant in the owner's
// project, it worked on one cloud, and it was the path control-plane's own
// prod connected through. A token works on GKE, EKS, AKS, VKE, k3s and bare
// metal alike, so the platform protects every customer — and itself — with
// the same mechanism. Its cost (a bearer token at rest) is bounded by the RBAC
// forge applies and revoked by `disconnect`.
const authToken = "CLUSTER_AUTH_SERVICE_ACCOUNT_TOKEN"

// connectTarget is everything forge learns about a cluster from the
// operator's kubeconfig: the whole ConnectCluster request bar the token,
// decided before any call is made.
type connectTarget struct {
	// Context is the kubectl context forge read this from, and the context
	// the bootstrap RBAC is applied THROUGH. It is also the key a bundle
	// renders under (release.BundleClusterPath), which is what makes a
	// cluster_bindings entry addressable.
	Context string
	// Address is the API server URL the HUB will dial — the context's own
	// server. https only.
	//
	// The operator chooses it by choosing the context: for a GKE cluster the
	// hub reaches over a private endpoint, that is a context written by
	// `gcloud container clusters get-credentials --internal-ip`. forge does
	// not second-guess it by asking a cloud API, which would need that
	// cloud's CLI and credentials on this machine.
	Address string
	// CAPEM is the PEM bundle that signed the API server's certificate.
	CAPEM string
}

// readConnectTarget assembles the ConnectCluster request from the operator's
// kubeconfig entry for kctx, and refuses an address the hub cannot use.
//
// ONLY THE ADDRESS AND THE CA ARE READ. A cloud context's credential is an
// `exec` plugin, inert anywhere but this machine; the hub authenticates with
// the token forge mints.
func readConnectTarget(kctx string) (connectTarget, error) {
	kctx = strings.TrimSpace(kctx)
	if kctx == "" {
		return connectTarget{}, fmt.Errorf("--context is required: it names the kubectl context forge reads the " +
			"cluster's address and CA from, and applies the bootstrap RBAC through")
	}
	address, caPEM, err := kubeconfigClusterOf(kctx)
	if err != nil {
		return connectTarget{}, err
	}
	if err := refuseUnusableAddress(address); err != nil {
		return connectTarget{}, err
	}
	if caPEM == "" {
		// Never degrade to skipping verification. A target we cannot verify
		// is one we decline to deploy to — the alternative is sending a
		// standing credential to whatever answers that address.
		return connectTarget{}, fmt.Errorf("context %q carries no certificate authority for its cluster, and forge "+
			"will not connect a cluster whose API server it cannot verify\n"+
			"fix: the kubeconfig entry needs certificate-authority-data or certificate-authority "+
			"(re-run your provider's get-credentials to refresh it)", kctx)
	}
	return connectTarget{Context: kctx, Address: address, CAPEM: caPEM}, nil
}

// refuseUnusableAddress rejects the two addresses that fail AGAINST A HEALTHY
// CLUSTER, which is why they are worth their own refusal.
//
// The hub's kustomize-controller dials this address from a POD, where
// `127.0.0.1` and `0.0.0.0` mean THAT POD. So a loopback address does not fail
// as "you gave me a loopback address" — it fails as "connection refused"
// against a target cluster that is perfectly healthy, which is the one place
// the fault is not. A non-https scheme would carry the credential in clear
// text.
func refuseUnusableAddress(address string) error {
	u, err := url.Parse(address)
	if err != nil || address == "" {
		return fmt.Errorf("the context's cluster.server is %q, which is not a URL", address)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("the cluster's API server address is %q, whose scheme is %q\n"+
			"forge connects over https only: any other scheme would carry the hub's credential in clear text",
			address, u.Scheme)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return fmt.Errorf("the cluster's API server address is %q, whose host %q is a loopback or bind address.\n"+
			"The hub dials it from a POD, where %q is that pod — so the deploy would fail as "+
			"\"connection refused\" against a cluster that is perfectly healthy.\n"+
			"fix: connect this cluster by an address reachable from outside it (its public or "+
			"peered API endpoint); a local k3d cluster is not a connect target, it is applied "+
			"to directly", address, host, host)
	}
	if host == "localhost" {
		return fmt.Errorf("the cluster's API server address is %q, whose host is localhost.\n"+
			"The hub dials it from a POD, where localhost is that pod.\n"+
			"fix: connect this cluster by an address reachable from outside it", address)
	}
	return nil
}

// kubeconfigClusterOf reads ONE context's cluster.server and CA out of the
// operator's kubeconfig.
//
// ONLY THE ADDRESS AND THE CA ARE TAKEN, never the credential beside them. A
// GKE kubeconfig authenticates through an `exec` plugin — an instruction to
// run a binary on the operator's machine with the operator's own credentials —
// which is inert anywhere else. The hub's credential is the token forge mints;
// this function is reading a public address and a public certificate.
//
// A var so tests supply a kubeconfig without one on disk.
var kubeconfigClusterOf = func(kctx string) (address, caPEM string, err error) {
	raw, err := clientcmd.NewDefaultPathOptions().GetStartingConfig()
	if err != nil {
		return "", "", fmt.Errorf("read kubeconfig: %w", err)
	}
	ctxEntry, ok := raw.Contexts[kctx]
	if !ok {
		known := make([]string, 0, len(raw.Contexts))
		for name := range raw.Contexts {
			known = append(known, name)
		}
		return "", "", fmt.Errorf("no context %q in your kubeconfig (it has: %s)\n"+
			"fix: `kubectl config get-contexts` and name one of them, or run your provider's "+
			"get-credentials to add it", kctx, strings.Join(known, ", "))
	}
	clusterEntry, ok := raw.Clusters[ctxEntry.Cluster]
	if !ok {
		return "", "", fmt.Errorf("context %q names cluster %q, which your kubeconfig does not define",
			kctx, ctxEntry.Cluster)
	}
	ca := string(clusterEntry.CertificateAuthorityData)
	if ca == "" && clusterEntry.CertificateAuthority != "" {
		// An out-of-line CA is equally valid kubeconfig, and the control
		// plane needs the BYTES — so read the file rather than refusing a
		// kubeconfig that is merely spelled the other way.
		b, rerr := os.ReadFile(clusterEntry.CertificateAuthority)
		if rerr != nil {
			return "", "", fmt.Errorf("read certificate-authority %q for context %q: %w",
				clusterEntry.CertificateAuthority, kctx, rerr)
		}
		ca = string(b)
	}
	return strings.TrimSpace(clusterEntry.Server), ca, nil
}
