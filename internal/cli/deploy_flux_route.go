package cli

// WHICH ENVS RECONCILE, and how their bundle becomes fetchable.
//
// THE RULE, in one predicate. An env goes through the reconciler when all
// three hold:
//
//  1. it DECLARES NO LIFECYCLE. `lifecycle = "local" | "ephemeral"` is an
//     env saying it is a cluster forge creates and can delete, so applying to
//     it directly is the intended path (env_lifecycle.go). An env that
//     declares nothing is treated as a REAL environment — that default is
//     deliberate, and it is why forgetting to declare does not silently opt an
//     env out of review;
//  2. its ledger is THIS MACHINE'S. An env with a control plane has a version
//     store that already drives a reconciler; this path is for the env whose
//     version store is the machine ledger;
//  3. it TARGETS A CLUSTER. A host-only or compose-only env has nothing for a
//     reconciler to converge, and writing a pointer for it would create a
//     control loop with no subject.
//
// (1) and (3) together are exactly [DirectApplyAllowed]'s negation, which is
// why this reads it rather than re-deriving them: two spellings of "is this a
// real environment" would eventually disagree, and the one that said yes would
// decide whether an env got reviewed.
//
// THE BUNDLE HAS TO BE SOMEWHERE THE CLUSTER CAN FETCH IT, and for a
// machine-ledger env it starts out somewhere it cannot. `forge env build`
// writes such an env's bundle into the machine ledger's own OCI LAYOUT — a
// directory under `~/.forge` — because a local env's bundle is not shipped
// (env_build_bundle.go states that rule and it is the right one). Flux cannot
// read a directory on the developer's laptop. So this path PUBLISHES the
// bundle to the env's own registry before writing a pointer at it.
//
// That is not a second placement rule competing with the build's. The build
// decides where an env's bundle is RECORDED, which is still the machine
// ledger; this is the reconciler's fetch requirement, satisfied at deploy
// time from bytes the ledger already holds. The bundle is content-addressed,
// so pushing it is idempotent and the digest the pointer pins is the digest
// the ledger recorded — republishing cannot change what is deployed.
//
// WHICH REGISTRY, AND BY WHICH NAME. The env's own declared registry, read
// off its render, addressed AS THE CLUSTER REACHES IT. For a k3d cluster those
// are two different names for one registry: the host pushes to
// `localhost:<port>` and nothing inside the cluster can resolve that, so the
// pointer names `host.k3d.internal:<port>` — the Docker host gateway alias
// forge already maintains in every managed cluster's CoreDNS NodeHosts
// (cluster_phase.go's ensureClusterHostGatewayDNS). Writing the host's own
// spelling into the pointer produces an OCIRepository that fails to fetch
// with a connection-refused naming localhost, which reads as a registry
// outage rather than as an addressing mistake.

import (
	"context"
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/internal/flux"
)

// reconcilesThroughFlux reports whether this env's deploy writes a
// desired-state pointer instead of applying.
//
// It takes the resolved ledger rather than re-resolving one: the ledger
// decision is made once, in resolveReleaseLedger, and a second resolution
// here could select a different backend for the same env than the promotion
// was recorded in.
func reconcilesThroughFlux() bool {
	// Forge never installs Flux into, or writes a pointer at, a deploy
	// target: Flux on a cluster comes only from a control plane's own
	// declaration, and real clusters are reconciled by its hub. Every env
	// without a ControlPlane applies directly. The in-cluster pointer path
	// below is retained only until the follow-up that moves dev-k8s/e2e onto
	// their own control plane deletes it.
	return false
}

// realClusterNoticeLine is printed when a direct apply targets a cluster of
// an env with no control plane.
const realClusterNoticeLine = "Note: real clusters are reconciled by a control plane's hub Flux, not by forge; " +
	"applying directly (transitional). Declare forge.ControlPlane + connected_cluster (forge cluster connect)"

// fluxRegistryBase is the registry subtree this env's bundles are published
// to, addressed as the HOST reaches it.
//
// It is read from the env's WORKLOADS, because that is the only place a
// registry is declared: an environment does not have one, a workload does, as
// part of its image. The bundle is published beside the images it pins — the
// first cluster workload whose image names a registry host, minus the
// repository's own name (`localhost:5050/acme/api:1` -> `localhost:5050/acme`).
//
// Empty means no cluster workload names a registry, which is a real state and
// not an error here: the caller refuses with a message naming what to declare,
// rather than this function inventing a default registry and pointing a
// cluster at it.
func fluxRegistryBase(entities *KCLEntities) string {
	if entities == nil {
		return ""
	}
	for _, w := range entities.WorkloadsOn(RuntimeCluster) {
		repo := imageRepository(strings.TrimSpace(w.Spec.Image))
		if registryHost(repo) == "" {
			continue
		}
		if slash := strings.LastIndex(repo, "/"); slash > 0 {
			return repo[:slash]
		}
	}
	return ""
}

// clusterReachableRegistry rewrites a registry address the HOST pushes to
// into the one a POD pulls from.
//
// `localhost:<port>` and `127.0.0.1:<port>` are the host's own loopback and
// resolve, inside a pod, to the pod itself. `host.k3d.internal` is the Docker
// host-gateway alias k3d seeds and forge keeps in every managed cluster's
// CoreDNS NodeHosts, so it resolves from a pod to the machine running the
// registry — which is where the port is published.
//
// `registry.localhost:<port>` is deliberately NOT used even though forge's
// containerd mirror config names it. That alias exists for the KUBELET, which
// resolves it through the node's `/etc/hosts` and the mirror block; a POD does
// not, and Flux's source-controller is a pod. The two reach the same registry
// by different mechanisms, and using the kubelet's name for the pod's fetch is
// the mistake this function exists to prevent.
//
// Any other host is returned unchanged: a real registry is reachable by the
// same name from everywhere, and rewriting one would be inventing an address.
func clusterReachableRegistry(ref string) string {
	host, rest, hasPath := strings.Cut(strings.TrimSpace(ref), "/")
	hostOnly, port, hasPort := strings.Cut(host, ":")
	switch hostOnly {
	case "localhost", "127.0.0.1", "registry.localhost":
		rewritten := k3dHostGatewayAlias
		if hasPort {
			rewritten += ":" + port
		}
		if hasPath {
			return rewritten + "/" + rest
		}
		return rewritten
	}
	return ref
}

// publishBundleForFlux puts the bundle's bytes in the registry the pointer
// will name, and returns the repository as the CLUSTER addresses it.
//
// IT PUSHES FROM THE HOST AND NAMES THE CLUSTER'S VIEW. Those are the same
// registry under two names, so the push target is the declared (host-side)
// address and the returned repository is the rewritten one. Deriving both from
// one declaration is what keeps them the same registry: a pointer built from a
// separately-configured address could name a registry the push never reached,
// and the OCIRepository would fail with a not-found on a digest that does
// exist — somewhere else.
func publishBundleForFlux(ctx context.Context, projectDir, env string, entities *KCLEntities, digest string) (string, error) {
	base := fluxRegistryBase(entities)
	if base == "" {
		return "", fmt.Errorf(
			"env %s reconciles from its bundle, but no cluster workload names a registry for the cluster to fetch it from.\n"+
				"  Give a workload an image that carries a registry host (`image = \"<host>/<path>/<name>\"`): the\n"+
				"  bundle is published beside the images it pins, and the in-cluster reconciler fetches it from there", env)
	}
	repo, err := republishLedgerBundle(ctx, projectDir, env, base, digest)
	if err != nil {
		return "", err
	}
	return clusterReachableRegistry(repo), nil
}

// fluxPointerFor builds one cluster's pointer from a published bundle.
//
// `insecure` is DERIVED from the address, never passed in: flux.InsecureRegistry
// answers it from the host, so the only addresses that can produce a plaintext
// fetch are ones no certificate could cover. A caller able to supply it could
// downgrade a real registry's transport.
func fluxPointerFor(in fluxPointerInput) (flux.Pointer, error) {
	return flux.BuildPointer(flux.PointerInput{
		Env:          in.env,
		Repository:   in.repository,
		Digest:       in.digest,
		Cluster:      in.cluster,
		ClusterPaths: in.clusterPaths,
		Insecure:     flux.InsecureRegistry(in.repository),
		RequestedAt:  in.requestedAt,
	})
}
