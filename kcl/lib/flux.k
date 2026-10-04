"""
Forge KCL — Flux as a declared platform dependency.

WHY FLUX IS A LIBRARY COMPONENT AND NOT A HIDDEN INSTALL. An env with no
control plane still needs something to converge it: forge records a bundle and
writes a desired-state pointer, and a reconciler in the cluster applies it. The
reconciler is Flux, and Flux is itself a platform dependency — a HelmChart like
cert-manager or Envoy Gateway — so it is DECLARED rather than installed by a
side effect.

That declaration has to be shared rather than copied, for a reason that bit
control-plane first. cp's `e2e` env ALREADY installs Flux into its
control-plane cluster, for hosted tenants, from its own chart declaration. Two
Flux installs in one cluster collide on the same CRDs and the same
`flux-system` namespace, and the collision is not a clean failure — whichever
applied last owns the CRDs, and the other's Kustomizations reconcile against a
controller that was replaced underneath them.

So forge exports the declaration (`forge.flux_chart()`) instead of owning a
second copy of it. A consumer that needs Flux for its own reasons declares
THIS chart, with its own `values` overlay for whatever identity its registry
needs, and `forge cluster up` then sees a declared chart that already provides
Flux and installs nothing. The detection is by CHART REFERENCE (`FLUX_CHART_OCI`
below), never by the chart's `name`: a name is a `--target` selector the
consumer picks freely, so cp calling its chart "flux" rather than
"flux-system" would otherwise decide whether forge installed a second one.

CHART VERSION vs FLUX VERSION — THEY ARE DIFFERENT NUMBERS, and this is the
mistake to not make on a bump. There is no fluxcd-published helm chart; the
canonical install path is the community chart at
oci://ghcr.io/fluxcd-community/charts/flux2, whose version has its own lineage
that does NOT track the flux2 release. Flux v2.9.5 is the APP version, carried
by chart 2.19.1:

    $ helm show chart oci://ghcr.io/fluxcd-community/charts/flux2 \
          --version 2.19.1 | grep appVersion
    appVersion: 2.9.5

Chart 2.19.0, immediately before it, is appVersion 2.9.1 — so the chart minor
is not a rename of the flux minor and must be RE-DERIVED, never incremented.
Confirm with the command above before editing `FLUX_CHART_VERSION`. The
rendered images are the cross-check: source-controller and kustomize-controller
both v1.9.5.

TWO CONTROLLERS, NOT SIX. Only source-controller (fetches the OCI artifact)
and kustomize-controller (applies it) are installed. helm-controller,
notification-controller, image-reflector-controller and
image-automation-controller are disabled, and that is a security decision
rather than a resource one: every controller installed is attack surface that
has to be patched, and none of these four is on this path. What forge ships in
a bundle is plain Kubernetes objects in an already-rendered artifact — there is
no Helm release to reconcile, no alerting fan-out, and no image-tag automation.
helm-controller specifically carries its own cross-tenant DoS history
(CVE-2022-36049), so installing it "in case" would be paying that exposure for
a feature with no use here.

THERE IS NO VERSION KNOB, deliberately. The pointer forge writes names an API
version and a `layerSelector` whose behaviour is a property of a specific
source-controller, and the revision-equality check the wait depends on is
likewise. A consumer that could pin a different Flux could pin one where
`lastAppliedRevision` is not the manifest digest, and the wait would then hang
on a comparison that can never be true. `values` remains open — that is the
seam a consumer needs for its own registry identity — but the chart and its
version are forge's.
"""

# The chart's OCI reference. EXPORTED because it is the SHARED-FLUX
# DETECTOR: `forge cluster up` compares every declared HelmChart's `oci`
# against this string to decide whether a cluster already has a Flux, and
# the Go side (internal/flux.ChartOCI) holds the same spelling. A consumer
# that wants to be recognised as providing Flux declares this ref.
FLUX_CHART_OCI = "oci://ghcr.io/fluxcd-community/charts/flux2"

# Chart 2.19.1 == flux2 v2.9.5 (appVersion). See the header — the two
# numbers are unrelated lineages; re-derive, do not increment.
FLUX_CHART_VERSION = "2.19.1"

# Flux's own namespace. Not configurable: the pointer forge writes goes into
# this namespace by name, and the CRDs the chart installs are cluster-scoped
# either way, so a second spelling would only let the writer and the
# controller disagree about where the OCIRepository lives.
FLUX_NAMESPACE = "flux-system"

# The default `--target` selector. A consumer may name its own chart
# anything; this is the name forge's own install uses.
FLUX_CHART_NAME = "flux"

# flux_chart(values) — Flux as a forge.HelmChart, version-pinned.
#
# `values` is MERGED OVER the posture below, last-wins, which is the seam a
# consumer needs: cp's hosted Flux adds a GAR workload-identity annotation to
# the source-controller ServiceAccount, and turns multitenancy on for its
# tenants, neither of which should need a second chart declaration.
#
# Returns a DICT rather than a typed HelmChart because this file is a
# sub-package of the module that declares the schema, and KCL resolves
# package identity such that a sibling lib cannot name a parent-package
# schema. The dict is accepted verbatim into `Bundle.helm_charts:
# [HelmChart]`, which type-checks it at the consumer — so the schema's
# `check:` block still runs, and a malformed chart is refused at the
# declaration rather than at the helm shell-out.
flux_chart = lambda values: {str: any} = {}, cluster: any = Undefined -> any {
    _base = {
        name = FLUX_CHART_NAME
        oci = FLUX_CHART_OCI
        version = FLUX_CHART_VERSION
        namespace = FLUX_NAMESPACE
        values = _FLUX_VALUES | values
    }
    # `cluster` re-targets the install in a multi-cluster env, exactly as it
    # does on any other HelmChart. Added only when given: the schema's field
    # is optional, and a present-but-None `cluster` is not the same as an
    # absent one to a closed schema.
    _base | ({cluster = cluster} if cluster != Undefined else {})
}

# THE POSTURE, and every line below is load-bearing.
#
#   * `--no-remote-bases=true` rides `container.additionalArgs` because the
#     chart does not template it. It makes the apply HERMETIC: a Kustomize
#     overlay may not fetch a remote base, so the OCI artifact is the ONLY
#     thing that can affect cluster state.
#
#   * `--no-cross-namespace-refs=true`: a Kustomization may only reference a
#     source in its OWN namespace.
#
#   * helm / notification / image-* controllers are off (see the header).
#
# NO `multitenancy` BLOCK, deliberately, and this was measured rather than
# assumed. `multitenancy.enabled` makes kustomize-controller IMPERSONATE a
# namespace-local ServiceAccount (`flux-system:default` unless told otherwise)
# for every apply, and `privileged = False` additionally strips the
# controller's cluster-admin binding. Together they are exactly right when the
# Kustomizations are TENANT-authored, and fatal here: the only writer is forge,
# the only artifact is the env's own bundle, and that bundle legitimately
# carries Namespaces and CRDs. The first deploy failed with
# `namespaces "app" is forbidden: User "system:serviceaccount:flux-system:default"
# cannot patch resource "namespaces"` — a fail-closed control over a boundary
# that has one side. A consumer running tenants through the same Flux turns
# multitenancy on (and provisions the impersonation SA) through `values`,
# which is what cp does.
#
# CRDs land even though forge supplies no bundle (`crds` unset). The
# community chart ships them as TEMPLATES (`templates/*.crds.yaml`) rather than
# in the chart's `crds/` directory, so `helm template --skip-crds` does not
# drop them. forge's CRD-first apply then promotes every CustomResourceDefinition
# into the early batch and waits for Established before the controller
# Deployments, which is the ordering the pointer write needs.
_FLUX_VALUES: {str: any} = {
    kustomizeController.container.additionalArgs = ["--no-remote-bases=true", "--no-cross-namespace-refs=true"]
    helmController.create = False
    notificationController.create = False
    imageAutomationController.create = False
    imageReflectionController.create = False
}
