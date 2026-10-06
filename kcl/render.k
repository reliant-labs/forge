"""
Forge KCL — the lowering: `output = forge.render(bundle)`.

`render` is the ONE entrypoint (ADR 0002). A project's env main.k ends with
`output = forge.render(bundle)`; there is no other public top-level var, and
in particular no top-level `manifests` a bare `kcl run -S manifests |
kubectl apply` could consume: the cluster-bound workloads in the applyable
stream are forge.dev Workload RECORDS that forge expands in Go, so an apply
that bypasses forge could never be correct.

THE OUTPUT (internal/cli/testdata/render_contract/ pins it per runtime):

    project, env, image_tag
    lifecycle                 WHO APPLIES this env: "local" | "ephemeral",
                              or null for a real env reconciled from a
                              bundle. Never a refusal here — see Bundle
    clusters, cluster_target, kubeconfig_secrets
    network_policy            the env NetworkPolicy bundle's inputs, or null
    workloads[]               EVERY workload, of every runtime:
        {name, kind, image, build, runtime, spec}
          image   — the REPOSITORY forge builds it into, registry host
                    included ("" when it builds none): the build-state /
                    release-ledger key, and where a push goes
          build   — the typed build, or null (forge builds nothing)
          runtime — {type: host|compose|cluster|hosted|build-only, ...}
          spec    — v1alpha1.WorkloadSpec JSON (the generated
                    tiers.workload_json), identical to the record's spec
    infra[], frontends[], databases[]
    config_maps, gateways, http_routes, grpc_routes, runtime_classes,
    helm_charts, required_secrets, required_dns, secret_provider,
    rendered_secrets, control_plane
    manifests[]               the applyable stream: RuntimeClasses,
                              Namespaces, ConfigMaps, one `forge.dev/v1alpha1
                              Workload` record per CLUSTER-bound workload
                              (a tool is never scheduled, so has none),
                              ManagedDatabase records, Gateway API objects,
                              then Bundle.manifests (forge.Manifests)

Missing optional fields in the runtime/build/infra/frontend blocks project as
`null`, not omitted, so the consumer sees stable keys.

WHAT THE LOWERING DECIDES (per workload, `_lower`):

  * the RUNTIME: the workload's own. There is no env-level default: an env
    is a list of per-workload bindings (ADR 0002 §2);
  * the IMAGE: a cluster workload's image is resolved here, against the env's
    tag and `-D image_digests`, so the record names exactly what the cluster
    pulls. The REGISTRY comes from the image itself — the author declares the
    full reference and forge appends only the tag/digest; an image naming no
    registry host is refused here, naming the workload. A hosted workload carries the
    artifact name (forge's hosted publish pins it to repo@digest) or the
    author's third-party image; host / compose / build-only run no image;
  * ENV: the map becomes the wire's list, with each reference in its
    channel; a forge.WorkloadURL is RESOLVED to a value for a host or
    cluster referrer and KEPT as a reference for a hosted one;
  * PROBES: `_default_probes`, ADR 0002 §5;
  * the RUNTIME MASK: `_mask_violations` refuses, naming the workload, the
    field and the reason, what the bound runtime cannot honour.

The Kubernetes objects a cluster workload becomes (Deployment, Service,
Jobs, RBAC, PDB, NetworkPolicy, ...) are NOT rendered here: that is
pkg/deploy.RenderWorkloads, the one renderer, which the CLI and the control
plane both run.

This file is a SIBLING of schema.k and workload.k so it references Bundle,
Workload etc. as bare names; `import .schema` would fork type identity.
"""

import json
import .tiers
import .lib.gateway as gw_lib
import .lib.jwks as jwks_lib
import .lib.labels as labels_lib
import .lib.workload_url as wurl_lib
import .lib.images as img_lib
import .lib.capabilities as caps
import .lib.crd as crd_lib
import .lib.build as build_lib
import .lib.flux as flux_lib

# Bare aliases for schema instantiation inside lambda bodies (KCL v0.11).
_TWorkload = tiers.Workload
_TEnvVar = tiers.EnvVar
_TProbes = tiers.Probes
_TSecretKeyRef = tiers.SecretKeyRef
_TConfigMapKeyRef = tiers.ConfigMapKeyRef
_TFieldRef = tiers.FieldRef
_TManagedSecretRef = tiers.ManagedSecretRef
_TDatabaseRef = tiers.DatabaseRef
_TWorkloadURLRef = tiers.WorkloadURLRef

# ---------------------------------------------------------------------------
# Runtime resolution.
# ---------------------------------------------------------------------------

# A workload's runtime: its own. Undefined when unbound (Bundle's check
# names the workload).
_bound_runtime = lambda w: any -> any {
    w.runtime if w.runtime else Undefined
}

# A database runs on a cluster or on the control plane: its own runtime,
# else (Undefined) the env's primary cluster target.
_db_runtime = lambda db: any -> any {
    db.runtime if db.runtime else Undefined
}

# The names that occur more than once, sorted.
_duplicates = lambda names: [str] -> [str] {
    sorted([n for n in {x: 0 for x in names} if len([y for y in names if y == n]) > 1])
}

# Every name the env declares: the WorkloadURL namespace and `--target` set.
_env_names = lambda workloads: [any], infra: [any], frontends: [any] -> [str] {
    [w.name for w in workloads] + [i.name for i in infra] + [f.name for f in frontends]
}

# Everything that is published to the control plane.
_hosted_names = lambda workloads: [any], databases: [any], frontends: [any] -> [str] {
    [w.name for w in workloads if w.runtime and w.runtime.type == "hosted"] \
        + [db.name for db in databases if db.runtime and db.runtime.type == "hosted"] \
        + [f.name for f in frontends if f.runtime and f.runtime.type == "hosted"]
}

# A cloud-bound workload whose build produces an image, on a target that
# states no platform (see the Bundle check).
#
# The local-vs-cloud signal is the IMAGE's registry host, because that is
# where a registry is declared. A `localhost:`/`*.localhost` host can only be
# served to the dev host's own k3d cluster, so its nodes share the dev arch and
# an omitted platform correctly tracks it; any real remote host means nodes of
# a fixed, possibly different arch.
# A hostless image is NOT treated as cloud here: it is already a hard render
# error naming the workload (_check_image_names_registry), and that is the more
# precise complaint. Reporting a missing platform as well would ask the author
# to fix the wrong field.
_cloud_build_without_platform = lambda w: any -> bool {
    _rt = w.runtime
    _host = img_lib.registry_host(w.image if w.image else "")
    bool(_rt and _rt.type == "cluster" and not _rt.target.platform \
        and _host and not is_local_registry(_host) and builds_an_image(w))
}

# ---------------------------------------------------------------------------
# WHO APPLIES: the Bundle.lifecycle declaration's support.
#
# A declared lifecycle ("local" / "ephemeral") claims forge may apply to this
# env DIRECTLY. That claim is only honest for a cluster forge itself creates
# and can throw away, so the Bundle check below tests it against every
# kubectl context the env's objects actually land on.
# ---------------------------------------------------------------------------

# Every kubectl context this env's objects target, deduplicated and sorted.
#
# EVERY placement, not just the workloads': a declaration is only as strong
# as its weakest target, and an env that runs one `forge.Manifests` group or
# one Gateway on a cloud cluster is applying to that cloud cluster. Reading
# only `cluster_target` would let such an env claim local while deploying to
# GKE, which is exactly the hole this check exists to close.
# Every field is passed explicitly rather than taking the Bundle, because a
# schema `check:` block has no `self` — it reads its own fields by bare name,
# so a helper it calls must be given them.
_targeted_contexts = lambda workloads: [any], databases: [any], cluster_target: any, \
                            gateways: [any], http_routes: [any], grpc_routes: [any], \
                            helm_charts: [any], manifests: [any], generated: [any], rendered_secrets: [any] -> [str] {
    _from_workloads = [w.runtime.target.cluster for w in workloads \
        if w.runtime and w.runtime.type == "cluster" and w.runtime.target]
    _from_databases = [db.runtime.target.cluster for db in databases \
        if db.runtime and db.runtime.type == "cluster" and db.runtime.target]
    _from_primary = [cluster_target.cluster] if cluster_target else []
    # A Cluster REFERENCE denormalizes to its derived `.context`; a raw
    # forge.Manifests states the context as a string.
    _from_gateways = [g.cluster.context for g in gateways if g.cluster]
    _from_http = [r.cluster.context for r in http_routes if r.cluster]
    _from_grpc = [r.cluster.context for r in grpc_routes if r.cluster]
    _from_charts = [h.cluster.context for h in helm_charts if h.cluster]
    _from_manifests = [m.cluster for m in manifests if m.cluster]
    _from_generated = [g.cluster for g in generated if g.cluster]
    _from_secrets = [s.cluster for s in rendered_secrets if s.cluster]
    _all = _from_workloads + _from_databases + _from_primary + _from_gateways \
        + _from_http + _from_grpc + _from_charts + _from_manifests + _from_generated + _from_secrets
    sorted([c for c in {x: 0 for x in _all if x}])
}

# A context forge CREATES and can delete: a k3d cluster.
#
# Two ways to qualify, because forge has two ways to own a local cluster and
# both are legitimate:
#
#   1. it is declared in `Bundle.clusters` with `provider = "k3d"` — the
#      declarative path, where `forge env up` reconciles the cluster at the
#      head of the run;
#   2. its context follows k3d's own `k3d-<name>` convention — the
#      `deploy/k3d.yaml` path the scaffolded dev env uses, where the cluster
#      is created by `forge cluster up` and so is never a Bundle.clusters
#      entry. kcl/lib/gateway.k's _is_local_cluster already treats this
#      prefix as the local-cluster signal; using a second, stricter rule
#      here would refuse the env forge itself scaffolds.
#
# A GKE / VKE / EKS context satisfies neither, which is the load-bearing
# half: a cloud env can never declare itself local to keep direct apply.
_is_forge_managed_context = lambda ctx: str, clusters: [any] -> bool {
    _declared = any c in clusters {
        c.provider == "k3d" and c.context == ctx
    }
    bool(_declared or ctx.startswith("k3d-"))
}

# The targeted contexts a declared lifecycle cannot account for. Empty => the
# declaration is true of every cluster this env applies to.
_unmanaged_contexts = lambda clusters: [any], workloads: [any], databases: [any], \
                             cluster_target: any, gateways: [any], http_routes: [any], \
                             grpc_routes: [any], helm_charts: [any], manifests: [any], \
                             generated: [any], rendered_secrets: [any] -> [str] {
    [c for c in _targeted_contexts(workloads, databases, cluster_target, gateways, \
            http_routes, grpc_routes, helm_charts, manifests, generated, rendered_secrets) \
        if not _is_forge_managed_context(c, clusters)]
}

# The env's primary cluster target: declared, else the target of the first
# cluster-bound workload (so an env whose workloads all bind one cluster
# need not name it twice), else none.
_primary_target = lambda b: Bundle -> any {
    _bound = [w.runtime.target for w in b.workloads if w.runtime and w.runtime.type == "cluster"]
    b.cluster_target if b.cluster_target else (_bound[0] if _bound else Undefined)
}

# ---------------------------------------------------------------------------
# Probe policy (ADR 0002 §5), in ONE place.
#
# A SERVICE that forge BUILDS serves /readyz + /healthz through serverkit, so
# the lowering writes that probe into the spec EXPLICITLY — paths and port —
# unless the author declared one. A forge-built OPERATOR that declares ports
# gets the same: its subcommand runs the manager under serverkit.Run, which
# serves /healthz + /readyz on the process PORT (its http port) whatever it
# mounts. With no ports there is no listener to name, so none is written. A CR on the wire is then self-describing:
# the hosted platform, the CLI renderer and a human reading the record all
# see the same probe, and nothing is inferred downstream. A third-party
# image (no build) gets none here: forge cannot know it serves /readyz, and
# Go's WorkloadSpec.EffectiveProbes gives a service with ports TCP on its
# first one. A worker or job gets none unless it declares one.
# ---------------------------------------------------------------------------
_default_probes = lambda w: any -> any {
    w.probes if w.probes else (_TProbes {
        readinessPath = "/readyz"
        livenessPath = "/healthz"
        port = _http_port(w)
    } if w.build and (w.kind == "service" or (w.kind == "operator" and w.ports)) else Undefined)
}

# The port a default probe targets: the one named `http`, else the first,
# else pkg/deploy's DefaultServicePort (a service with no ports is given
# `http` :8080 by the renderer).
_http_port = lambda w: any -> int {
    _named = [p.port for p in (w.ports or []) if p.name == "http"]
    _named[0] if _named else ((w.ports or [])[0].port if w.ports else 8080)
}

# ---------------------------------------------------------------------------
# Env: the authoring map -> the wire's list, one channel per entry.
# ---------------------------------------------------------------------------

# The channel of one env value, by its declared type.
_channel = lambda v: any -> str {
    "value" if typeof(v) == "str" else {
        "SecretRef" = "secretRef"
        "ConfigMapRef" = "configMapRef"
        "FieldRef" = "fieldRef"
        "ManagedSecret" = "managedSecret"
        "DatabaseRef" = "databaseRef"
        "WorkloadURL" = "workloadURL"
    }[typeof(v)]
}

# One map entry -> one tiers.EnvVar, lowered for the referrer's runtime
# `rt`: a hosted referrer lowers a config-projected secret to its store key
# as a managedSecret and KEEPS a WorkloadURL; every other referrer resolves
# the WorkloadURL, through its cluster's host gateway when it is a pod.
_env_var = lambda name: str, v: any, where: str, rt: any, bundle: Bundle -> any {
    hosted = rt.type == "hosted"
    _c = _channel(v)
    _TEnvVar {
        name = name
        if _c == "value":
            value = v
        if _c == "secretRef" and hosted and v.store_key:
            managedSecret = _TManagedSecretRef {name = v.store_key, optional = v.optional if v.optional else Undefined}
        if _c == "secretRef" and not (hosted and v.store_key):
            secretRef = _TSecretKeyRef {name = v.name, key = v.key, optional = v.optional if v.optional else Undefined}
        if _c == "configMapRef":
            configMapRef = _TConfigMapKeyRef {name = v.name, key = v.key}
        if _c == "fieldRef":
            fieldRef = _TFieldRef {fieldPath = v.path}
        if _c == "managedSecret":
            managedSecret = _TManagedSecretRef {name = v.name, optional = v.optional if v.optional else Undefined}
        if _c == "databaseRef":
            databaseRef = _TDatabaseRef {name = v.name, key = v.key if v.key else Undefined}
        if _c == "workloadURL" and hosted:
            workloadURL = _TWorkloadURLRef {name = _require_hosted_url_target(v.name, where, bundle)}
        if _c == "workloadURL" and not hosted:
            value = _require_workload_url(v.name, where, bundle, rt.target.host_gateway if rt.type == "cluster" else "localhost")
    }
}

# ---------------------------------------------------------------------------
# forge.WorkloadURL resolution (lib/workload_url.k has the order).
# ---------------------------------------------------------------------------

# The URL a reference resolves to at render, or a refusal naming it.
_require_workload_url = lambda name: str, where: str, bundle: Bundle, from_host: str = "localhost" -> str {
    _names = _env_names(bundle.workloads, bundle.infra, bundle.frontends)
    assert name in _names, \
        "${where}: forge.WorkloadURL names workload '${name}', which this environment does not declare. " \
        + "Declared workloads: " + ", ".join(_names) + "."
    _url = wurl_lib.resolve(name, bundle, from_host)
    assert _url, \
        "${where}: forge.WorkloadURL '${name}' has no URL forge can know at render time. " \
        + "Give it one: an HTTPRoute to it, a custom domain on its exposed port, `listen_ports` on its host runtime " \
        + "(forge env up assigns an undeclared host port only after rendering), or a frontend `port`."
    _url
}

# A HOSTED referrer keeps the reference, and the control plane resolves it
# against what IT runs. So the target must be hosted too: a host process or
# a cluster you operate is unreachable from the platform.
_require_hosted_url_target = lambda name: str, where: str, bundle: Bundle -> str {
    _names = _env_names(bundle.workloads, bundle.infra, bundle.frontends)
    assert name in _names, \
        "${where}: forge.WorkloadURL names workload '${name}', which this environment does not declare. " \
        + "Declared workloads: " + ", ".join(_names) + "."
    _hosted = _hosted_names(bundle.workloads, [], bundle.frontends)
    assert name in _hosted, \
        "${where}: forge.WorkloadURL names '${name}', which does not run on the hosted runtime. A hosted workload's references are resolved by the control plane against workloads IT runs, and it cannot reach a host process or a cluster you operate. Bind '${name}' to forge.OnHosted {} too, or pass its URL as a literal value."
    name
}

# ---------------------------------------------------------------------------
# The workload lowering.
# ---------------------------------------------------------------------------

# The registry-less artifact name a forge-built workload is built into —
# the build-state / release-ledger key, and the repository its build pushes to.
# "" when forge builds nothing for it.
#
# A declared image names it, with any `:tag` stripped: `reliant:e2e` is built
# as IMAGE=reliant, TAG=e2e (the Go build side reads the tag back off the
# resolved spec.image). It used to fall back to the output_name / workload
# name for ANY tagged image, so the build pushed `<registry>/<name>:<env tag>`
# while the deploy pulled `<registry>/reliant:e2e` — ErrImagePull. Only an
# UNSET image derives the name from the build.
_artifact = lambda w: any -> str {
    (img_lib.repository(w.image) if w.image \
        else (w.build.output_name if w.build.output_name else w.name)) if w.build else ""
}

# The build identity: the registry-less artifact plus the tag the workload's
# own image pins, if it pins one ("workspace-base:dev-per-daemon", "api"). It
# is a fact about the BUILD, so it is the same on every runtime — a BuildOnly
# or host workload's spec.image resolves no pull ref, and a pin read back off
# spec.image was silently lost there. "" when forge builds nothing for it.
_build_image = lambda w: any -> str {
    _name = _artifact(w)
    _tail = w.image[w.image.rfind("/") + 1:] if w.image else ""
    (_name + _tail[_tail.rfind(":"):] if _name and w.image and "@" not in w.image and ":" in _tail else _name)
}

# A build cannot produce a digest chosen in advance: a digest names bytes
# that already exist. Building to some tag the deploy then ignores, or
# skipping the declared build, would each leave the declaration lying, so a
# forge-built workload pinned to a digest is refused.
_check_built_digest = lambda w: any -> bool {
    assert not (w.build and w.image and "@" in w.image), \
        "workload '${w.name}': image '${w.image}' pins a digest, but the workload also declares a build. " \
        + "A build cannot produce a pre-chosen digest. Drop `build` to run that exact image, or drop the " \
        + "digest (pin a tag, or leave it unpinned) so the image is the one forge builds; " \
        + "`forge env deploy` pins what the build pushed by digest itself."
    True
}

# A workload's image must name its own registry, because the registry is
# declared THERE and nowhere else — no env-level field supplies one. An image
# with no host is an incomplete declaration, so it is refused at render,
# naming the workload and the runtime that needs it, rather than rendering a
# reference the kubelet resolves against Docker Hub and cannot pull.
#
# The two runtimes that PULL an image get different runbooks, because the
# registry an author should name is a different question on each.
# `image` may be "" for a runtime that resolves none (a host or compose
# workload, or a cluster workload forge builds nothing for); there is no
# reference to check, so both asserts stay inert.
_check_image_names_registry = lambda w: any, rt: any, image: str -> bool {
    assert not (image and rt.type == "cluster" and not img_lib.has_registry_host(image)), \
        "workload '${w.name}': image '${image}' names no registry host, and it is bound to a cluster runtime " \
        + "that must pull it. The registry is part of the image: declare the full reference on the workload, e.g. " \
        + "`image = \"ghcr.io/<owner>/${image}\"` (or `localhost:5051/${image}` for a k3d-local registry). " \
        + "There is no env-level registry — a ClusterTarget declares the cluster, namespace and platform; " \
        + "the workload declares what to pull."
    # forge.OnHosted DELIBERATELY DOES NOT REQUIRE A HOST (ADR-0003 F1).
    #
    # The registry lives on the image for every runtime whose registry the
    # AUTHOR chooses. A hosted workload is the one case where they do not: the
    # control plane admits images from exactly one subtree,
    # `<registry_base>/<org>`, and refuses every other — so a hosted author
    # writing a host was transcribing a value the platform already knew, and
    # getting it wrong produced a publish-time refusal.
    #
    # So a bare hosted image renders as-is and forge resolves it against the
    # base the control plane reports (internal/cli: resolveHostedImageBase).
    # It is NOT resolved here, because KCL has no network and a render must
    # stay an offline, reproducible projection — the full reference appears in
    # the build's push plan and in the deploy's pinned spec, both of which
    # read the base from the ensure they already make.
    True
}

# The image a runtime runs, resolved per runtime (module docstring). The
# author's registry is kept verbatim; forge contributes the tag and digest.
_spec_image = lambda w: any, rt: any, bundle: Bundle -> str {
    _name = w.image if w.image else _artifact(w)
    assert _check_image_names_registry(w, rt, _name)
    img_lib.image_ref(_name, bundle.image_tag, image_digests()) if rt.type == "cluster" and _name \
        else (_name if rt.type == "hosted" else "")
}

# The spec, as the generated tiers.Workload. `rt` is the resolved runtime.
_spec = lambda w: any, rt: any, bundle: Bundle -> any {
    _TWorkload {
        kind = w.kind
        image = _spec_image(w, rt, bundle) or "-"
        command = w.command
        args = w.args
        replicas = w.replicas
        if w.resources:
            resources = w.resources
        env = [_env_var(k, v, "workload '${w.name}' env ${k}", rt, bundle) for k, v in w.env] if w.env else Undefined
        ports = w.ports
        probes = _default_probes(w)
        storageGiB = w.storageGiB
        schedule = w.schedule
        before = w.before
        deployPhase = w.deployPhase
        namespacedRBAC = w.namespacedRBAC
        clusterRBAC = w.clusterRBAC
        crds = w.crds
        group = w.group
        version = w.version
        leaderElection = w.leaderElection
        serviceAccount = w.serviceAccount
        serviceAccountAnnotations = w.serviceAccountAnnotations
        sidecars = w.sidecars
        volumes = w.volumes
        nodeSelector = w.nodeSelector
        tolerations = w.tolerations
        podAnnotations = w.podAnnotations
        priorityClassName = w.priorityClassName
        strategy = w.strategy
        securityContext = w.securityContext
        terminationGracePeriodSeconds = w.terminationGracePeriodSeconds
        activeDeadlineSeconds = w.activeDeadlineSeconds
    }
}

# The spec JSON: the generated projection, with the placeholder image of a
# runtime that runs none emptied. (tiers.Workload requires an image, which is
# right for the wire; host / compose / build-only have none to state.)
_spec_json = lambda w: any, rt: any, bundle: Bundle -> {str:} {
    _j = tiers.workload_json(_spec(w, rt, bundle))
    {k: ("" if k == "image" and v == "-" else v) for k, v in _j}
}

_render_runtime = lambda w: any, rt: any -> any {
    {
        "type": "host"
        "runner": rt.runner
        "air_config": rt.air_config if rt.air_config else None
        "working_dir": rt.working_dir if rt.working_dir else None
        # `is not Undefined`, NOT truthiness: an explicitly EMPTY list means
        # "binds nothing" and must reach Go distinct from "not declared".
        "listen_ports": rt.listen_ports if rt.listen_ports is not Undefined else None
        "delve_port": rt.delve_port
    } if rt.type == "host" else ({
        "type": "compose"
        "service": rt.service if rt.service else w.name
        "file": rt.file
        "env_file": rt.env_file if rt.env_file else None
        "wait": rt.wait
        "wait_timeout": rt.wait_timeout
        "shared": rt.shared
        # The compose PROCESS env: the workload's literal env under the
        # runtime's own (the runtime block is the more specific statement).
        "env": {k: v for k, v in w.env if typeof(v) == "str"} | rt.env
    } if rt.type == "compose" else ({
        "type": "cluster"
        "cluster": rt.target.cluster
        "namespace": rt.target.namespace
        "connected_cluster": rt.target.connected_cluster if rt.target.connected_cluster else None
        "domain": rt.target.domain if rt.target.domain else None
        "platform": rt.target.platform if rt.target.platform else None
        "image_pull_secrets": rt.target.image_pull_secrets if rt.target.image_pull_secrets else []
    } if rt.type == "cluster" else ({
        "type": "hosted"
        "platform": rt.platform
    } if rt.type == "hosted" else {
        "type": "build-only"
        "build_variants": [_render_build_variant(bv) for bv in rt.build_variants]
    })))
}

# One output.workloads[] entry. Runs the runtime mask first.
_lower = lambda w: any, bundle: Bundle -> any {
    _rt = _bound_runtime(w)
    _bad = _mask_violations(w, _rt)
    assert len(_bad) == 0, "workload '${w.name}' cannot run on the ${_rt.type} runtime:\n  - " + "\n  - ".join(_bad)
    assert _check_built_digest(w)
    {
        "name": w.name
        "kind": w.kind
        "image": _artifact(w)
        "build_image": _build_image(w) or None
        "build": _render_build(w.build)
        "runtime": _render_runtime(w, _rt)
        "spec": _spec_json(w, _rt, bundle)
    }
}

# `before` names a workload the env declares, on the same runtime set that
# orders it (a Go renderer / host runner orders within the env).
_check_before = lambda workloads: [any] -> bool {
    _names = [w.name for w in workloads]
    _dangling = ["${w.name} -> ${b}" for w in workloads for b in (w.before or []) if b != BEFORE_ALL and b not in _names]
    assert len(_dangling) == 0, \
        "Workload.before names a workload this environment does not declare: " + ", ".join(_dangling) \
        + ". Declared workloads: " + ", ".join(_names) + "."
    True
}

# ---------------------------------------------------------------------------
# The RUNTIME MASK (ADR 0002 §3; P2 map §6.2).
#
# What each runtime can honour. `hosted` is GENERATED from Go's
# v1alpha1.FieldProfiles / KindProfiles (tiers/profiles_gen.k), so the
# default-deny table that gates the control plane also gates the author, and
# a field added to the spec is refused on hosted until Go classifies it.
# The Go gate (Workload.Validate(Restricted), server-side) stays the real
# one; this is the same answer at the line that caused it.
#
# host / compose / build-only are forge's own: each field they refuse would
# otherwise be silently ignored by the runtime.
# ---------------------------------------------------------------------------

# Spec fields a runtime that runs a local process or a compose service
# cannot honour, and why.
_LOCAL_REFUSED: {str:str} = {
    "storageGiB": "a local process has no volume to provision; use a host path in its config"
    "namespacedRBAC": "a local process has no Kubernetes identity to grant it to"
    "clusterRBAC": "a local process has no Kubernetes identity to grant it to"
    "serviceAccount": "a local process has no Kubernetes identity"
    "serviceAccountAnnotations": "a local process has no Kubernetes identity to annotate"
    "sidecars": "a local process has no pod to add a container to"
    "volumes": "a local process has no pod to mount into; it reads the host filesystem"
    "nodeSelector": "a local process is not scheduled onto nodes"
    "tolerations": "a local process is not scheduled onto nodes"
    "podAnnotations": "a local process has no pod to annotate"
    "priorityClassName": "a local process is not scheduled, so there is no pod to rank"
    "strategy": "a local process has no Deployment to roll out; forge env up starts it once"
    "securityContext": "a local process runs as the user who started it"
    "terminationGracePeriodSeconds": "a local process is stopped by forge env down, not by a kubelet"
}

_COMPOSE_REFUSED: {str:str} = _LOCAL_REFUSED | {
    "build": "compose builds its own image from the compose file; drop `build`, or bind a runtime forge builds for"
    "ports": "the compose file owns the published ports; declare them there"
    "probes": "the compose file owns the healthcheck; declare it there"
    "replicas": "the compose file owns the scale"
    "command": "the compose file owns the container's command"
    "args": "the compose file owns the container's command"
    "resources": "the compose file owns the container's resources"
    "schedule": "compose has no cron"
    "crds": "compose has no Kubernetes API"
}

_BUILD_ONLY_REFUSED: {str:str} = {
    "ports": "a build-only workload is never run, so nothing listens"
    "probes": "a build-only workload is never run, so there is nothing to probe"
    "replicas": "a build-only workload is never run"
    "storageGiB": "a build-only workload is never run"
}

_set = lambda w: any, f: str -> bool {
    _v = w[f]
    bool(_v) if f != "replicas" else _v != 1
}

# A hosted workload's ports may not declare custom `domains`.
#
# The name is not spec. Bringing your own hostname takes an action at YOUR
# registrar, ownership verification and a certificate — asynchronous and
# human-gated, none of which a deploy converges; it binds to ONE environment
# while a single workloads.k renders to many; and the platform may allocate a
# hostname itself, which a spec field would then contradict. So the domain
# lives in the control plane as its own resource, created once and bound per
# env.
#
# Refused HERE, in the render, rather than at deploy: `forge env render` and
# `forge env deploy --plan` have to fail too, or the declaration survives
# review and only fails against a live control plane.
#
# OnCluster is untouched — there you own the ingress, and forge's own
# Gateway/HTTPRoute serves `domains[0]` (kcl/lib/workload_url.k).
_hosted_domain_violations = lambda w: any -> [str] {
    _d = [d for p in (w.ports or []) for d in (p.domains or [])]
    ["domains ${_d}: a hosted custom hostname is a control-plane resource, not spec — it needs DNS at your registrar, ownership verification and a certificate, and it binds to ONE environment while this file renders to many. Create it once and bind it: `forge domain add ${_d[0]}`, then `forge domain bind ${_d[0]} --env <env> --target ${w.name}`. (forge.OnCluster keeps `Port.domains`: there you own the ingress.)"] if _d else []
}

_mask_violations = lambda w: any, rt: any -> [str] {
    _hosted = ["${f}: ${tiers.RESTRICTED_FIELD_REASONS[f]}" for f in tiers.RESTRICTED_FIELD_REASONS if f in w and _set(w, f)] \
        + ["env ${k} (${_channel(v)}): ${tiers.RESTRICTED_FIELD_REASONS['env.' + _channel(v)]}" for k, v in w.env if "env." + _channel(v) in tiers.RESTRICTED_FIELD_REASONS and not (_channel(v) == "secretRef" and v.store_key)] \
        + (["kind '${w.kind}' (${tiers.RESTRICTED_KIND_REASONS[w.kind]})"] if w.kind in tiers.RESTRICTED_KIND_REASONS else []) \
        + (["image: a hosted workload needs a build or an image to run"] if not w.build and not w.image else []) \
        + _hosted_domain_violations(w) \
        if rt.type == "hosted" else []
    _host = ["${f}: ${_LOCAL_REFUSED[f]}" for f in _LOCAL_REFUSED if _set(w, f)] \
        + (["replicas: a host workload is one local process; forge env up launches it once"] if w.replicas != 1 else []) \
        + (["build: the ${rt.runner} runner derives its command from a forge.GoBuild; add `build = forge.GoBuild {cmd = ...}`, or set `command` to run a binary verbatim"] \
            if rt.runner in ["go-run", "binary", "delve"] and not (w.build and w.build.type == "go") and not w.command else []) \
        + (["kind '${w.kind}': a local process cannot ${'run on a schedule' if w.kind == 'cron' else 'be a controller-runtime manager'}; bind it to forge.OnCluster"] if w.kind in ["cron", "operator"] else []) \
        if rt.type == "host" else []
    _compose = ["${f}: ${_COMPOSE_REFUSED[f]}" for f in _COMPOSE_REFUSED if _set(w, f)] \
        if rt.type == "compose" else []
    _build_only = ["${f}: ${_BUILD_ONLY_REFUSED[f]}" for f in _BUILD_ONLY_REFUSED if _set(w, f)] \
        + (["build: a build-only workload with no build has nothing to produce"] if not w.build else []) \
        if rt.type == "build-only" else []
    _hosted + _host + _compose + _build_only
}

# ---------------------------------------------------------------------------
# The applyable stream.
# ---------------------------------------------------------------------------

# One forge.dev/v1alpha1 Workload record: what pkg/deploy.RenderWorkloads
# expands (grouped by forge.dev/cluster + namespace), and the same spec the
# hosted path publishes. The spec is THE SAME VALUE as the entity's.
_workload_record = lambda entity: any, part_of: str -> any {
    {
        apiVersion = "forge.dev/v1alpha1"
        kind = "Workload"
        metadata = {
            name = entity.name
            namespace = entity.runtime.namespace
            labels = labels_lib.managed_labels(entity.name, part_of) | {"forge.dev/cluster" = entity.runtime.cluster}
        }
        spec = entity.spec
    }
}

# One forge.Manifests group, placed and (when named) owned. Every object is
# stamped with the group's RESOLVED cluster (its own, else the primary's),
# including on the primary: the deploy router replicates an object with no
# forge.dev/cluster to EVERY cluster of a multi-cluster env (it never guesses
# a primary for an unattributed object), so leaving the primary's objects
# unstamped put a primary-only ClusterIssuer on every cluster. Only an env
# with no cluster at all leaves the label off.
_raw_manifests = lambda g: any, primary: any, part_of: str -> [any] {
    _cluster = g.cluster if g.cluster else (primary.cluster if primary else "")
    _ns = g.namespace if g.namespace else (primary.namespace if primary else "")
    [_stamp_cluster(_default_namespace(_stamp_owner(m, g.name, part_of) if g.name else m, _ns), _cluster) \
        if _cluster else _default_namespace(_stamp_owner(m, g.name, part_of) if g.name else m, _ns) \
        for m in g.objects]
}

# Cluster-scoped kinds a namespace must never be written onto.
_CLUSTER_SCOPED: [str] = [
    "Namespace", "CustomResourceDefinition", "ClusterRole", "ClusterRoleBinding",
    "ClusterIssuer", "StorageClass", "PriorityClass", "RuntimeClass",
    "MutatingWebhookConfiguration", "ValidatingWebhookConfiguration", "APIService",
    "PersistentVolume", "GatewayClass", "IngressClass", "CSIDriver", "Node",
]

_default_namespace = lambda m: any, ns: str -> any {
    _meta = m.metadata if m.metadata else {}
    m if not ns or _meta.namespace or m.kind in _CLUSTER_SCOPED else m | {metadata = _meta | {namespace = ns}}
}

_stamp_owner = lambda manifest: any, name: str, part_of: str -> any {
    _meta = manifest.metadata if manifest.metadata else {}
    manifest | {metadata = _meta | {labels = labels_lib.owner_labels(_meta.labels if _meta.labels else {}, name, part_of)}}
}

# The Bundle's ingress: Gateways, their Certificates, and the routes on them.
# All of it is rendered into the PRIMARY namespace.
_ingress = lambda b: Bundle, ns: str -> [any] {
    [item for g in b.gateways for item in gw_lib.render_gateway(g, ns)] \
        + [item for g in b.gateways for item in gw_lib.render_certificate(g, ns)] \
        + [item for r in b.http_routes for item in gw_lib.render_http_route(r, b.gateways, ns, b)] \
        + [item for r in b.grpc_routes for item in gw_lib.render_grpc_route(r, b.gateways, ns, b)]
}

# An object with no cluster of its own lands on the PRIMARY target — the same
# rule _raw_manifests applies, for the same reason: the deploy router
# replicates an unattributed object to EVERY cluster of a multi-cluster env.
# Ingress is never env-wide: a Gateway claims one load balancer (often a
# reserved static IP), its Certificate is one ACME order, and a route's
# backend is a Service on one cluster. Unstamped, a Manifests-only secondary
# cluster received a second Gateway on prod's IP and duplicate certificates.
_place_on_primary = lambda manifest: any, primary: any -> any {
    _meta = manifest.metadata if manifest.metadata else {}
    _labels = _meta.labels if _meta.labels else {}
    manifest if not primary or _labels["forge.dev/cluster"] else _stamp_cluster(manifest, primary.cluster)
}

_stamp_cluster = lambda manifest: any, context: str -> any {
    _meta = manifest.metadata if manifest.metadata else {}
    _labels = _meta.labels if _meta.labels else {}
    manifest | {metadata = _meta | {labels = {k: v for k, v in _labels if k != "forge.dev/cluster"} | {"forge.dev/cluster" = context}}}
}

_render_managed_database_record = lambda db: any, part_of: str -> any {
    {
        apiVersion = "forge.dev/v1alpha1"
        kind = "ManagedDatabase"
        metadata = {
            name = db.name
            namespace = db.namespace
            labels = labels_lib.managed_labels(db.name, part_of) | {"forge.dev/cluster" = db.cluster}
        }
        spec = db.spec
    }
}

# A database's placement: its runtime, else the primary cluster target.
_render_database = lambda db: any, bundle: Bundle -> any {
    _rt = _db_runtime(db)
    _target = (_rt.target if _rt.type == "cluster" else Undefined) if _rt else _primary_target(bundle)
    {
        "name": db.name
        "runtime": "hosted" if _rt and _rt.type == "hosted" else "cluster"
        "cluster": _target.cluster if _target else None
        "namespace": (db.namespace if db.namespace else _target.namespace) if _target else None
        "spec": tiers.managed_database_json(db.spec)
    }
}

# The Namespaces the cluster runtime deploys into, one per (cluster,
# namespace), primary target first. RenderWorkloads does not emit them: the
# namespace belongs to the ENV (restricted PSA, forge.dev/env), not to a
# workload.
#
# EVERY Namespace is stamped with its own cluster, including the primary's —
# the rule _raw_manifests already follows, and for the same reason. The deploy
# router replicates an object carrying no forge.dev/cluster to every cluster
# of a multi-cluster env rather than guessing a primary, so leaving the
# primary's Namespace unstamped sent it to every cluster AND made it collide
# with the stamped Namespace each of those clusters already had. Two
# documents then reached one (cluster, Namespace, name) with different bodies,
# which `kubectl apply` resolves last-wins — silently deciding a namespace's
# pod-security level by document order.
_namespaces = lambda b: Bundle, entities: [any] -> [any] {
    _pt = _primary_target(b)
    _pairs = ([[_pt.cluster, _pt.namespace]] if _pt else []) \
        + [[e.runtime.cluster, e.runtime.namespace] for e in entities if e.runtime.type == "cluster"]
    _uniq = {"${p[0]}/${p[1]}": p for p in _pairs}
    [_render_namespace(p[1], p[0], _pt) for _, p in _uniq]
}

_render_namespace = lambda namespace: str, context: str, primary: any -> any {
    {
        apiVersion = "v1"
        kind = "Namespace"
        metadata = {
            name = namespace
            labels = {
                "app.kubernetes.io/managed-by" = "forge"
                "pod-security.kubernetes.io/enforce" = "restricted"
                "pod-security.kubernetes.io/audit" = "restricted"
                "pod-security.kubernetes.io/warn" = "restricted"
            } | ({"forge.dev/cluster" = context} if context else {})
        }
    }
}

_manifests = lambda b: Bundle, entities: [any] -> [any] {
    _pt = _primary_target(b)
    _ns = _pt.namespace if _pt else ""
    _project = b.project
    _dbs = [_render_database(db, b) for db in b.databases]
    labels_lib.stamp_env_all(
        [_render_runtime_class_manifest(rc, _ns, _project) for rc in b.runtime_classes] \
        + _namespaces(b, entities) \
        + [_render_config_map_manifest(cm, _ns, _project) for cm in b.config_maps] \
        + [_workload_record(e, _project) for e in entities if e.runtime.type == "cluster" and e.kind != "tool"] \
        + [_render_managed_database_record(db, _project) for db in _dbs if db.runtime == "cluster"] \
        + [_place_on_primary(item, _pt) for item in _ingress(b, _ns)] \
        + [item for g in b.manifests for item in _raw_manifests(g, _pt, _project)],
        b.env,
    )
}

# ---------------------------------------------------------------------------
# The entrypoint.
# ---------------------------------------------------------------------------

render = lambda bundle: Bundle -> any {
    _ok = _check_before(bundle.workloads)
    _entities = [_lower(w, bundle) for w in bundle.workloads]
    _pt = _primary_target(bundle)
    {
        "project": bundle.project
        "env": bundle.env
        "image_tag": bundle.image_tag
        # WHO APPLIES: "local" | "ephemeral" | null (a real env, reconciled
        # from a bundle). Projected unconditionally so the Go consumer reads
        # a stable key, and null is the meaningful default rather than a
        # missing one.
        "lifecycle": bundle.lifecycle if bundle.lifecycle else None
        "clusters": [_render_cluster(c) for c in bundle.clusters]
        if _pt:
            "cluster_target": _render_cluster_target(_pt)
        "kubeconfig_secrets": [_render_kubeconfig_secret(k) for k in bundle.kubeconfig_secrets]
        "network_policy": {
            "egress_ports": bundle.network_policy.egress_ports
            "telemetry_namespace": bundle.network_policy.telemetry_namespace
            "ingress_namespace": bundle.network_policy.ingress_namespace
        } if bundle.network_policy else None
        "workloads": _entities
        "infra": [_render_host_infra(i) for i in bundle.infra]
        "frontends": [_render_frontend(f, bundle) for f in bundle.frontends]
        "databases": [_render_database(db, bundle) for db in bundle.databases]
        "config_maps": [_render_config_map(cm) for cm in bundle.config_maps]
        "gateways": [_render_gateway(g) for g in bundle.gateways]
        "http_routes": [_render_http_route(r) for r in bundle.http_routes]
        "grpc_routes": [_render_grpc_route(r) for r in bundle.grpc_routes]
        "runtime_classes": [_render_runtime_class(rc) for rc in bundle.runtime_classes]
        # Platform deps (helm-as-a-RENDERER): the CLI expands each via `helm
        # template --skip-crds` and folds the result into the apply stream,
        # selected by `--target <name>`. Not in `manifests`.
        "helm_charts": [_render_helm_chart(h) for h in bundle.helm_charts if h.delivery != "bundle"]
        # Bring-your-own YAML, rendered Go-side at extract time and shipped in
        # the bundle (internal/cluster/byo_yaml.go). Keys appear only when
        # declared, so an env with neither renders exactly as before.
        if [h for h in bundle.helm_charts if h.delivery == "bundle"]:
            "bundled_charts": [_render_helm_chart(h) for h in bundle.helm_charts if h.delivery == "bundle"]
        if bundle.generated:
            "generated": [{
                "name": g.name
                "command": g.command
                "dir": g.dir if g.dir else None
                "cluster": g.cluster if g.cluster else None
                "namespace": g.namespace if g.namespace else None
            } for g in bundle.generated]
        # Declared external PREREQUISITES forge does NOT create: a render-time
        # checklist, and the preflight BLOCK on a required-but-absent Secret.
        "required_secrets": [_render_external_secret(s) for s in bundle.required_secrets]
        "required_dns": [_render_dns_record(r) for r in bundle.required_dns]
        # WHERE secret values come from. A declaration only: no secret VALUE
        # ever enters this output.
        if bundle.secret_provider:
            "secret_provider": bundle.secret_provider
        # Explicitly-placed Secrets, placement RESOLVED here (own
        # cluster/namespace, else the primary target's) so the CLI applies
        # each exactly where it says. A `from = "file"` key carries only the
        # store KEY; its value is resolved CLI-side.
        "rendered_secrets": [{
            "name": s.name
            "keys": s.keys
            "cluster": s.cluster or _pt.cluster
            "namespace": s.namespace or (_pt.namespace if _pt else None)
        } for s in bundle.rendered_secrets]
        # The control plane this env talks to: endpoint + the env var NAME
        # its credential is read from. Declaration only.
        if bundle.control_plane:
            "control_plane": bundle.control_plane
        # TYPED OVERRIDES of forge's own rendered objects, carried OUT of KCL
        # for the Go half to apply. They are not applied here and cannot be:
        # the objects most worth overriding (Deployment, Service) do not exist
        # in `manifests` yet — forge's Workload records expand into them in Go,
        # after this render. So KCL's job is to type the surface, check the key
        # grammar and hand the patches on; `internal/cluster` resolves each key
        # against the EXPANDED stream and merges.
        "overrides": bundle.overrides
        "manifests": _manifests(bundle, _entities)
    }
}

# ---------------------------------------------------------------------------
# Projections of the declarations the lowering does not transform. Each
# returns a fully-populated dict so the Go consumer sees stable keys.
# ---------------------------------------------------------------------------

_render_host_infra = lambda d: HostInfra -> any {
    {
        "name": d.name
        "engine": d.engine
        "port": d.port
        "database": d.database
        "user": d.user
        "password": d.password
        "data_dir": d.data_dir if d.data_dir else None
        "version": d.version if d.version else None
        # engine = "zitadel" only: where the dev IdP keeps its state, and the
        # declarative bootstrap it converges to. Projected unconditionally:
        # this lambda is a whitelist, so a field omitted here never reaches
        # the consumer however correctly the env declares it.
        "idp_database": d.idp_database
        "idp_database_port": d.idp_database_port if d.idp_database_port else None
        "idp_masterkey": d.idp_masterkey
        "idp_steps_file": d.idp_steps_file
        "idp_pat_path": d.idp_pat_path
    }
}

# Build union render. One branch per discriminator value, each a
# fully-populated dict (WorkloadEntity.Build). null = forge builds nothing.
_render_build = lambda b: any -> any {
    (_render_go_build(b) if b.type == "go" else \
        (_render_docker_build(b) if b.type == "docker" else \
        (_render_shell_build(b) if b.type == "shell" else _render_remote_build(b)))) if b else None
}

_render_cluster_target = lambda ct: ClusterTarget -> any {
    {
        "cluster": ct.cluster
        "namespace": ct.namespace
        "connected_cluster": ct.connected_cluster if ct.connected_cluster else None
        "domain": ct.domain if ct.domain else None
        "platform": ct.platform if ct.platform else None
        "image_pull_secrets": ct.image_pull_secrets if ct.image_pull_secrets else []
    }
}

# ---------------------------------------------------------------------------
# Frontends. The runtime is PER FRONTEND (ADR 0002 §6): an OnHosted frontend
# is published to the control plane, which writes its config.js and resolves
# its references; every other frontend's references are resolved here.
# ---------------------------------------------------------------------------

_frontend_hosted = lambda f: Frontend -> bool {
    bool(f.runtime and f.runtime.type == "hosted")
}

# The frontend's static output dir: declared, else the type's convention
# (a Next.js static export writes out/, Vite and Expo web write dist/).
_public_dir = lambda f: Frontend -> str {
    f.public_dir if f.public_dir else ("out" if f.type == "nextjs" else "dist")
}

# The production build shape of each frontend the PROJECT declares, by name:
# "static" | "standalone" | "server" — forge.yaml `frontends[].output`,
# resolved the way the scaffolder resolves it (an unset field is the default;
# a Vite or Expo build is always "static").
#
# WHY IT IS AN OPTION AND NOT A Frontend FIELD. Whether the build is a static
# export is a fact about the frontend's CODE — its next.config, its routes —
# and it is the same in every env, because one build is promoted through all
# of them. forge.yaml already declares it, and it is what generates the
# next.config and the Dockerfile. A per-env KCL field would be a second
# declaration of that fact, free to disagree with the first in exactly the
# env nobody is looking at. So forge projects the one declaration into every
# render it drives (`-D frontend_outputs='{"web":"standalone"}'`, bound by
# internal/cli renderKCLRaw), and the env states only where the build goes.
#
# Unbound — a plain `kcl run`, or a frontend forge.yaml does not declare (a
# cross-repo `source` frontend) — reads as "": unknown, and nothing is refused
# on a guess.
_frontend_outputs = lambda -> {str:str} {
    _raw = option("frontend_outputs") or ""
    json.decode(_raw) if _raw else {}
}

# The runtimes that SERVE FILES: each publishes the frontend's public_dir and
# runs nothing behind it.
_STATIC_FRONTEND_RUNTIMES = {
    "hosted" = "forge.OnHosted"
    "bucket" = "forge.OnBucket"
    "firebase" = "forge.OnFirebase"
}

# A frontend bound to a static runtime whose build is NOT a static export.
#
# Without this the binding renders, deploys and builds green: `npm run build`
# produces a Node server under .next-prod/, nothing writes public_dir, and the
# failure arrives at publish time as a missing directory — or, for a project
# that once exported, as a stale out/ shipped as the new release. Refused HERE
# because render is where the author is still looking at the binding, and the
# fix is either one forge.yaml line plus a page migration, or a different
# binding — both named.
_not_static_export = lambda f: Frontend -> [str] {
    _t = f.runtime.type
    _outs = _frontend_outputs()
    _out = _outs[f.name] if f.name in _outs else ""
    _env = option("env") or ""
    _where = " in env '${_env}'" if _env else ""
    _dir = f.path if f.path else "frontends/${f.name}"
    [
        "output '${_out}': its production build is not a static export. forge.yaml declares `output: ${_out}` for frontend '${f.name}' — a Node.js server — and ${_STATIC_FRONTEND_RUNTIMES[_t]} serves only the files a static export writes to public_dir '${_public_dir(f)}', so${_where} nothing would be published. Fix: set `output: static` on frontend '${f.name}' in forge.yaml and migrate what a static export cannot serve — `forge lint --static-export` lists each construct with its file:line and fix — or bind '${f.name}' elsewhere${_where}: a Next.js server is a process, so it ships as a workload (fw.Workload with build = forge.DockerBuild {dockerfile = \"${_dir}/Dockerfile\"})"
    ] if _t in _STATIC_FRONTEND_RUNTIMES and _out and _out != "static" else []
}

# What a frontend's runtime cannot honour, and why. OnHost and OnHosted are
# the workload schemas, so they carry workload-only fields a frontend has no
# use for: a declared one is REFUSED, naming it, rather than silently ignored.
# The static build facts are refused where nothing consumes them.
_frontend_violations = lambda f: Frontend -> [str] {
    _rt = f.runtime
    _t = _rt.type
    _host = _t == "host"
    (["runner '${_rt.runner}': a frontend's dev server is `<dev_runner> dev`; set Frontend.dev_runner, not OnHost.runner"] if _host and _rt.runner != "go-run" else []) \
        + (["air_config: a frontend's dev server has no air config"] if _host and _rt.air_config else []) \
        + (["working_dir: a frontend's dev server runs in its own path"] if _host and _rt.working_dir else []) \
        + (["listen_ports: a frontend's dev server binds Frontend.port; set that"] if _host and _rt.listen_ports is not Undefined else []) \
        + (["delve_port: a frontend's dev server is not a Go process"] if _host and _rt.delve_port != 2345 else []) \
        + (["build_variants: a frontend has one build"] if _t == "build-only" and _rt.build_variants else []) \
        + (["bundle: the dev server assembles nothing; bundle dirs are assembled into a PUBLISHED site"] if _host and f.bundle else []) \
        + (["cache_control: forge sets Cache-Control only on the objects it uploads to your bucket (forge.OnBucket); ${'the platform owns the CDN and its headers' if _t == 'hosted' else ('Firebase owns the headers' if _t == 'firebase' else 'nothing here serves the build')}"] \
            if f.cache_control and _t != "bucket" else []) \
        + (["image: nothing here pushes a registry artifact — ${'your bucket serves the build' if _t == 'bucket' else ('your Firebase site serves it' if _t == 'firebase' else ('the dev server serves it' if _host else 'the build is not published'))}"] \
            if f.image and _t != "hosted" else []) \
        + (["image is REQUIRED on forge.OnHosted: the site release is pushed to a registry, and the registry is part of the reference (e.g. image = \"ghcr.io/<owner>/${f.name}\"). forge appends the platform's own static.v1 layout to it. There is no env-level registry"] \
            if _t == "hosted" and not f.image else []) \
        + _not_static_export(f) \
        # A BARE image is accepted on forge.OnHosted, exactly as it is on a
        # hosted workload (ADR-0003 F1, _check_image_names_registry): forge
        # resolves it against the base the env DECLARES
        # (<registry_host>/<org>/<project>) and appends the
        # platform's static.v1 layout to the result. The image is still
        # REQUIRED — the release is pushed to a registry and keyed by its
        # repository — but its host is the platform's to supply.
}

# A frontend's runtime_config as the StaticSite spec's `runtimeConfig` wire
# shape, references KEPT (the hosted lowering). Every name is still checked,
# so a typo fails at load rather than at the operator.
_runtime_config_spec = lambda f: Frontend, bundle: Bundle -> {str:} {
    _names = _env_names(bundle.workloads, bundle.infra, bundle.frontends)
    _bad = [k + " -> " + v.name for k, v in f.runtime_config if typeof(v) != "str" and v.name not in _names]
    assert len(_bad) == 0, \
        "Frontend '${f.name}'.runtime_config: forge.WorkloadURL names a workload this environment does not declare: " \
        + ", ".join(_bad) + ". Declared workloads: " + ", ".join(_names) + "."
    {k: ({"value": v} if typeof(v) == "str" else {"workloadURL": {"name": _require_hosted_url_target(v.name, "Frontend '" + f.name + "'.runtime_config." + k, bundle) if _frontend_hosted(f) else v.name}}) for k, v in f.runtime_config}
}

# A frontend's runtime_config RESOLVED to literals — the document forge
# writes itself. Empty for an OnHosted frontend (the control plane writes it).
_runtime_config_resolved = lambda f: Frontend, bundle: Bundle -> {str:str} {
    {} if _frontend_hosted(f) else \
        {k: (v if typeof(v) == "str" else _require_workload_url(v.name, "Frontend '" + f.name + "'.runtime_config." + k, bundle)) for k, v in f.runtime_config}
}

# output.frontends[].runtime: {type, ...} — the dispatch key. Only the
# fields the runtime's consumer reads; optional ones project as null.
_render_frontend_runtime = lambda rt: any -> any {
    {
        "type": "bucket"
        "bucket": rt.bucket
        "cdn": _render_static_site_cdn(rt.cdn) if rt.cdn else None
        # keep_releases projects UNCONDITIONALLY, including its 0 ("retain
        # everything") value. A `if rt.keep_releases else None` would erase
        # exactly the setting that says "never delete", and the consumer
        # would read the absent key as the default 10 and start pruning a
        # bucket the author asked it to leave alone.
        "keep_releases": rt.keep_releases
    } if rt.type == "bucket" else ({
        "type": "firebase"
        "project": rt.project
        "site": rt.site
        "target": rt.target if rt.target else None
        "rewrites": rt.rewrites if rt.rewrites else []
    } if rt.type == "firebase" else {"type": rt.type})
}

_render_frontend = lambda f: Frontend, bundle: Bundle -> any {
    _bad = _frontend_violations(f)
    assert len(_bad) == 0, "frontend '${f.name}' cannot run on the ${f.runtime.type} runtime:\n  - " + "\n  - ".join(_bad)
    # Computed BEFORE the literal: a KCL dict key is in scope for the
    # entries after it, so the "bundle" key would shadow the Bundle.
    _rc = _runtime_config_resolved(f, bundle)
    _rc_spec = _runtime_config_spec(f, bundle)
    {
        "name": f.name
        "type": f.type
        "path": f.path if f.path else None
        "source": _render_git_source(f.source) if f.source else None
        "dev_runner": f.dev_runner
        "port": f.port
        "env_file": f.env_file if f.env_file else None
        "env_vars": [_render_env_var(e) for e in f.env_vars]
        "config": _render_frontend_config(f.config) if f.config else None
        "runtime": _render_frontend_runtime(f.runtime)
        # Where an OnHosted frontend's release is pushed: the reference the
        # frontend DECLARES. Go appends the platform's static.v1 layout.
        "image": f.image if f.image else None
        # The static build, on the frontend: every publishing runtime
        # builds and assembles the same way.
        "public_dir": _public_dir(f)
        "base_path": f.base_path if f.base_path else None
        "bundle": [_render_bundle_dir(b) for b in f.bundle]
        "cache_control": [_render_cache_rule(r) for r in f.cache_control]
        # The browser's runtime config, as DECLARED (references kept) and as
        # RESOLVED for a frontend forge serves or publishes itself. The Go
        # side layers `runtime_config` over the typed per-env config for
        # config.js, and an OnHosted frontend publishes `runtime_config_spec`
        # as the StaticSite spec's runtimeConfig.
        "runtime_config": _rc
        "runtime_config_spec": _rc_spec
    }
}

# Render an EnvVar to its stable dict form. Unset / empty-string
# defaults project as `null` so the consumer sees stable keys — EXCEPT
# `value`, which preserves an explicitly-set empty string (rendered as
# `""`, not `null`). Empty-string `value` is a legal disable sentinel
# downstream (e.g. RUNTIME_CLASS_NAME="" to skip runtimeClassName).
_render_env_var = lambda ev: EnvVar -> any {
    {
        "name": ev.name
        "value": ev.value if ev.value != Undefined else None
        "secret_ref": ev.secret_ref if ev.secret_ref else None
        "secret_key": ev.secret_key if ev.secret_key else None
        "config_map_ref": ev.config_map_ref if ev.config_map_ref else None
        "config_map_key": ev.config_map_key if ev.config_map_key else None
        # Carried for the SECRET PRE-FLIGHT, which reads these entities and
        # has no proto in hand. Emitted only when true so the JSON contract is
        # unchanged for every var that does not set it. Without this the
        # annotation dies here — the flag survives proto -> codegen -> KCL and
        # then silently fails to reach the one consumer that acts on it.
        "secret_optional": True if ev.secret_optional else None
    }
}


_render_build_variant = lambda bv: BuildVariant -> any {
    {
        "name": bv.name
        "ldflags": bv.ldflags
        "tags": bv.tags
        "goos": bv.goos if bv.goos else None
        "goarch": bv.goarch if bv.goarch else None
    }
}


_render_go_build = lambda b: GoBuild -> any {
    {
        "type": "go"
        "output_name": b.output_name if b.output_name else None
        "cmd": b.cmd
        "goos": b.goos if b.goos else None
        "goarch": b.goarch if b.goarch else None
        "ldflags": b.ldflags
        "tags": b.tags
        "flags": b.flags
        "env": b.env if b.env else {}
    }
}

_render_docker_build = lambda b: DockerBuild -> any {
    {
        "type": "docker"
        "output_name": b.output_name if b.output_name else None
        "dockerfile": b.dockerfile
        # The MAIN build context (a project-root-relative dir). None is the
        # project root, which is how every build behaved before this existed.
        "context": b.context if b.context else None
        "platform": b.platform if b.platform else None
        "target": b.target if b.target else None
        "build_args": b.build_args if b.build_args else {}
        # Named build contexts; empty falls back to forge.yaml's
        # docker.build_contexts. Where the image goes is the workload's
        # `image`, not a field here.
        "build_contexts": b.build_contexts if b.build_contexts else {}
    }
}

_render_shell_build = lambda b: ShellBuild -> any {
    {
        "type": "shell"
        "output_name": b.output_name if b.output_name else None
        "cmd": b.cmd
        "cwd": b.cwd if b.cwd else None
        "env": b.env if b.env else {}
    }
}

_render_remote_build = lambda b: RemoteBuild -> any {
    {
        "type": "remote"
        "output_name": b.output_name if b.output_name else None
        # The source pin is projected as a nested dict rather than
        # flattened, so the Go side decodes the SAME GitSource shape the
        # frontend and image halves already use instead of a second
        # spelling of repo/ref/subdir that could drift from it.
        "source": {
            "repo": b.source.repo
            "ref": b.source.ref
            "subdir": b.source.subdir if b.source.subdir else None
        }
        "dockerfile": b.dockerfile
        "target": b.target if b.target else None
        "platform": b.platform
        "build_args": b.build_args if b.build_args else {}
        "cpu_millicores": b.cpu_millicores
        "memory_bytes": b.memory_bytes
        "cache_gib": b.cache_gib
        "timeout_seconds": b.timeout_seconds
    }
}


_render_bundle_dir = lambda b: BundleDir -> any {
    {
        "src": b.src
        "dest": b.dest
    }
}

_render_cache_rule = lambda r: CacheRule -> any {
    {
        "pattern": r.pattern
        "cache_control": r.cache_control
    }
}

# An absent `cdn` block projects as null, which the Go consumer reads as
# "bucket served directly, invalidate nothing" — distinct from a declared
# block whose `invalidate` happens to be "none" (a CDN exists, the deploy
# deliberately leaves its cache alone).
_render_static_site_cdn = lambda c: StaticSiteCDN -> any {
    {
        "url_map": c.url_map
        "invalidate": c.invalidate
        "extra_invalidate_paths": c.extra_invalidate_paths
    }
}


# Render a FrontendConfig to its stable dict form. Optional string knobs
# project as `null` when unset so the consumer sees stable keys; `mock`
# and `telemetry_disabled` always carry their (defaulted) value. An unset
# `config` block projects as `null` (see _render_frontend).
_render_frontend_config = lambda c: FrontendConfig -> any {
    {
        "api_url": c.api_url if c.api_url else None
        "mock": c.mock
        "otel_endpoint": c.otel_endpoint if c.otel_endpoint else None
        "environment": c.environment if c.environment else None
        "telemetry_disabled": c.telemetry_disabled
    }
}

# Render a GitSource to its stable dict form. An unset `source` projects
# as `null` (see _render_frontend), so a path-declared frontend renders
# byte-identically to what it did before cross-repo sources existed.
_render_git_source = lambda s: GitSource -> any {
    {
        "repo": s.repo
        "ref": s.ref
        "subdir": s.subdir if s.subdir else None
    }
}


_render_gateway_listener = lambda l: GatewayListener -> any {
    {
        "name": l.name
        "port": l.port
        "protocol": l.protocol
        "path_prefix": l.path_prefix if l.path_prefix else None
        # Per-listener hostname / TLS. Null means "inherit the gateway's"
        # — consumers (dev urls, doctor, graph) apply the same fallback
        # the renderer does rather than seeing a pre-resolved value, so
        # the JSON stays a faithful projection of what was declared.
        "hostname": l.hostname if l.hostname else None
        "tls": _render_gateway_tls(l.tls) if l.tls else None
    }
}

_render_gateway_tls = lambda t: GatewayTLS -> any {
    {
        "cert_issuer": t.cert_issuer if t.cert_issuer else None
        "secret_name": t.secret_name if t.secret_name else None
        "certmap": t.certmap if t.certmap else None
        "mode": t.mode
    }
}

_render_gateway_address = lambda a: GatewayAddress -> any {
    {
        "type": a.type
        "value": a.value
    }
}

_render_gateway = lambda g: Gateway -> any {
    {
        "name": g.name
        "gateway_class_name": g.gateway_class_name
        "host": g.host if g.host else None
        "tls": _render_gateway_tls(g.tls) if g.tls else None
        "listeners": [_render_gateway_listener(l) for l in g.listeners]
        "addresses": [_render_gateway_address(a) for a in g.addresses] if g.addresses else None
        # cluster is a forge.Cluster REFERENCE; the machine-facing JSON
        # carries the DENORMALIZED kubectl context (k3d-<name>) the scoper
        # matches against — never the bare name.
        "cluster": g.cluster.context if g.cluster else None
    }
}

# The route JSON projections carry what was DECLARED, not what the
# manifest resolved to: `workload` stays a workload NAME and `port` stays
# null when it was inferred. Consumers that need the resolved backend read
# output.manifests, which is the authority; a pre-resolved copy here would
# be a second one that can disagree.
_render_route_retries = lambda r: RouteRetries -> any {
    {
        "on": r.on
        "attempts": r.attempts
        "per_try_timeout": r.per_try_timeout if r.per_try_timeout != Undefined else None
    }
}

_render_route_health_check = lambda hc: RouteHealthCheck -> any {
    {
        "path": hc.path
        "expected_statuses": hc.expected_statuses if hc.expected_statuses != Undefined else None
        "interval": hc.interval
        "timeout": hc.timeout
        "unhealthy_threshold": hc.unhealthy_threshold
        "healthy_threshold": hc.healthy_threshold
    }
}

_render_route_outlier_detection = lambda od: RouteOutlierDetection -> any {
    {
        "consecutive_5xx": od.consecutive_5xx
        "consecutive_gateway_errors": od.consecutive_gateway_errors
        "interval": od.interval
        "base_ejection_time": od.base_ejection_time
        "max_ejection_percent": od.max_ejection_percent
    }
}

_render_route_traffic = lambda t: RouteTraffic -> any {
    {
        "retries": _render_route_retries(t.retries) if t.retries != Undefined else None
        "timeout": t.timeout if t.timeout != Undefined else None
        "health_check": _render_route_health_check(t.health_check) if t.health_check != Undefined else None
        "outlier_detection": _render_route_outlier_detection(t.outlier_detection) if t.outlier_detection != Undefined else None
    }
}

_render_http_route = lambda r: HTTPRoute -> any {
    {
        "name": r.name
        "gateway": r.gateway
        "listener": r.listener
        "service": r.service if r.service else None
        "workload": r.workload if r.workload else None
        "port": r.port if r.port != Undefined else None
        "host": r.host if r.host else None
        "path": r.path if r.path else None
        "traffic": _render_route_traffic(r.traffic) if r.traffic != Undefined else None
        # forge.Cluster reference -> denormalized kubectl context.
        "cluster": r.cluster.context if r.cluster else None
    }
}

_render_grpc_route = lambda r: GRPCRoute -> any {
    {
        "name": r.name
        "gateway": r.gateway
        "listener": r.listener
        "service": r.service if r.service else None
        "workload": r.workload if r.workload else None
        "port": r.port if r.port != Undefined else None
        "host": r.host if r.host else None
        "path": r.path if r.path else None
        "traffic": _render_route_traffic(r.traffic) if r.traffic != Undefined else None
        # forge.Cluster reference -> denormalized kubectl context.
        "cluster": r.cluster.context if r.cluster else None
    }
}

_render_config_map = lambda cm: ConfigMap -> any {
    {
        "name": cm.name
        "namespace": cm.namespace if cm.namespace else None
        "data": cm.data
        "labels": cm.labels
        "annotations": cm.annotations
    }
}


# RuntimeClass JSON projection — stable shape mirroring the schema.
# `overhead` / `scheduling` pass through as raw dicts (or null when
# unset) so the Go consumer can re-emit them into the k8s manifest
# without per-field translation.
_render_runtime_class = lambda rc: RuntimeClass -> any {
    {
        "name": rc.name
        "handler": rc.handler
        "overhead": rc.overhead if rc.overhead else None
        "scheduling": rc.scheduling if rc.scheduling else None
    }
}


# Render a Cluster to its stable dict form. Optional fields project as
# null (not omitted) so the Go consumer (ClusterEntity) sees stable
# keys; servers/agents always carry their (defaulted) int.
#
# Ownership is the `owner` reference; the render layer projects the
# DERIVED `network` (k3d-<owner.name>, null for an owner cluster) and
# `registry_inherit` (true for a secondary) — the two behaviors the
# stringly `network` + `registry_mirror = "inherit"` pair used to carry.
# `context` (k3d-<name>) is projected too so a Go-side consumer can read
# the kubectl context without re-deriving the prefix.
_render_cluster = lambda c: Cluster -> any {
    {
        "name": c.name
        "context": c.context
        "config": c.config if c.config else None
        "image": c.image
        "network": c.network if c.network else None
        "registry_inherit": c.registry_inherit
        "servers": c.servers
        "agents": c.agents
        "api_port": c.api_port if c.api_port else None
        "ingress": c.ingress if c.ingress else None
        "host_ports": c.host_ports if c.host_ports else None
        # The pod / Service CIDRs, emitted ONLY when declared. A CONDITIONAL
        # KEY, not a `None` value like the fields above: the Go side turns a
        # present key into a `--cluster-cidr` k3s arg, so a projected null
        # would be a new key in every existing cluster's JSON for a capability
        # it never asked for. Unset must be indistinguishable from before.
        if c.cluster_cidr:
            "cluster_cidr": c.cluster_cidr
        if c.service_cidr:
            "service_cidr": c.service_cidr
        # NOTE: `kubeconfig` is deliberately NOT projected. It is a HOST fact
        # resolved per-render for KCL consumers to reference
        # (ClusterClient.host_kubeconfig_path); the Go reconcile that reads
        # this entity already knows the machine's kubeconfig without being
        # told, and projecting it would put a machine-specific absolute path
        # into the rendered contract that `forge env render` prints.
    }
}

# Render a KubeconfigSecret to its stable dict form. The CLI mints the
# kubeconfig + applies the Secret Go-side (the IP is resolved fresh, never
# in KCL); this projection only carries the DECLARATION. Never appears in
# render_manifests — the Secret has a runtime-minted value, not a static one.
# Render a HelmChart (platform dependency) to its stable dict form. The
# CLI reads this list and runs `helm template <chart|oci> --version
# <version> --values <values> -n <namespace> --skip-crds` Go-side, stamps
# `app.kubernetes.io/name = name` on every rendered manifest, and folds
# them into the apply stream — so `--target <name>` selects them via the
# existing FilterManifestsByApp axis. `values` passes through verbatim.
# NEVER appears in render_manifests — the expansion is a Go-side shell-out
# (helm), not a KCL render.
_render_helm_chart = lambda h: HelmChart -> any {
    {
        "name": h.name
        "chart": h.chart if h.chart else None
        "repo": h.repo if h.repo else None
        "oci": h.oci if h.oci else None
        "version": h.version
        "namespace": h.namespace
        "values": h.values
        "crds": h.crds if h.crds else None
        # Raw manifests that ride the chart's --target (GatewayClass,
        # ClusterIssuers): applied with the chart, excluded from a bare
        # app deploy. The CLI stamps the chart's app-label and applies
        # them after the chart's controllers.
        "manifests": h.manifests if h.manifests else None
        # cluster is a forge.Cluster REFERENCE; the machine-facing JSON
        # carries the DENORMALIZED kubectl context (k3d-<name>) the apply
        # runs `kubectl --context` with — never the bare name. Null =>
        # the env's primary cluster (every pre-existing declaration).
        "cluster": h.cluster.context if h.cluster else None
    }
}

# Render an ExternalSecret prerequisite to its stable dict form. forge does
# NOT create this Secret — the projection is consumed by the render-time
# checklist + the deploy preflight (which BLOCKS when the declared-required
# Secret/key is absent on the live target). `value_group` projects null when
# unset (a standalone prereq, no byte-match group).
_render_external_secret = lambda s: ExternalSecret -> any {
    {
        "type": "secret"
        "name": s.name
        "namespace": s.namespace
        "keys": s.keys
        "reason": s.reason if s.reason else None
        "value_group": s.value_group if s.value_group else None
    }
}

# Render a DNSRecord prerequisite. A render-time checklist entry (forge
# can't authoritatively verify external DNS); `target` projects null when
# unset (the expected value is often an ephemeral LB IP unknown at author
# time).
_render_dns_record = lambda r: DNSRecord -> any {
    {
        "host": r.host
        "type": r.type
        "target": r.target if r.target else None
        "reason": r.reason if r.reason else None
    }
}

_render_kubeconfig_secret = lambda k: KubeconfigSecret -> any {
    {
        "name": k.name
        "in_cluster": k.in_cluster
        "target_cluster": k.target_cluster
        "context_name": k.context_name
        "key": k.key
        "namespace": k.namespace if k.namespace else None
        "reachability": k.reachability
        "target_context": k.target_context if k.target_context else None
        "service_account": {
            "name": k.service_account.name
            "namespace": k.service_account.namespace
            "namespaces": k.service_account.namespaces
            "rules": [
                {
                    "api_groups": r.api_groups
                    "resources": r.resources
                    "verbs": r.verbs
                } for r in k.service_account.rules
            ]
        } if k.service_account else None
    }
}

# crd — a minimal apiextensions/v1 CRD object (lib/crd.k), for a
# `forge.Manifests {objects = [...]}` entry.
crd = crd_lib.crd

# flux_chart(values, cluster) — Flux as a version-pinned forge.HelmChart
# (lib/flux.k), for a `Bundle.helm_charts` entry.
#
# Declared by a consumer that needs Flux for its OWN reasons — hosted
# tenants, a reconciler it drives itself — so that `forge cluster up` sees a
# chart already providing Flux and installs no second one. Two Flux installs
# in one cluster collide on the same CRDs and the same `flux-system`.
flux_chart = flux_lib.flux_chart

# FLUX_CHART_OCI / FLUX_CHART_VERSION / FLUX_NAMESPACE — the pinned chart
# reference, its version and Flux's namespace (lib/flux.k). Re-exported
# because FLUX_CHART_OCI is the SHARED-FLUX DETECTOR: a consumer declaring
# this ref is recognised as providing Flux, by reference rather than by the
# chart's freely-chosen name.
FLUX_CHART_OCI = flux_lib.FLUX_CHART_OCI
FLUX_CHART_VERSION = flux_lib.FLUX_CHART_VERSION
FLUX_NAMESPACE = flux_lib.FLUX_NAMESPACE

# target_arch — the GOARCH forge resolved for this build (lib/build.k).
#
# A ShellBuild's `cmd` is plain KCL that forge runs verbatim, so a command
# that cross-compiles reads the arch HERE rather than expecting forge to
# substitute a `${TARGETARCH}` token into its shell:
#
#     cmd = "docker buildx build --platform=linux/${forge.target_arch()} ."
target_arch = build_lib.target_arch

# test_jwks_manifests — top-level re-export of the lib builder so user
# code calls `forge.test_jwks_manifests(jwks, namespace, public_jwk)`
# inside a `forge.Manifests {objects = ...}` of Bundle.manifests. Emits ConfigMap + static-file Deployment + Service serving
# `/.well-known/jwks.json`.
#
# `public_jwk` is the derived PUBLIC JWK. The caller derives it from the
# ES256 private PEM (read from .env.<env>) via the plugin — the same
# pattern as resolve_port, so the plugin is imported in the USER's file
# and the forge module renders fine under a plugin-less `kcl run`:
#
#     import forge
#     import kcl_plugin.forge as plugin
#     _jwks = forge.TestJWKS { name = "auth-jwks", kid = "k1",
#                              private_key = forge.StoreRef { key = "JWKS_PEM" } }
#     _jwk  = plugin.derive_jwk(option("jwks_pem"), _jwks.kid, _jwks.alg)
#     manifests = forge.test_jwks_manifests(_jwks, "auth", _jwk)
test_jwks_manifests = lambda j: TestJWKS, namespace: str, public_jwk: any -> [any] {
    jwks_lib.test_jwks_manifests(j, namespace, public_jwk)
}

# render_gateway / render_certificate / render_http_route /
# render_grpc_route — top-level re-exports of the lib/gateway builders so
# a consumer can emit a Gateway API manifest on a SPECIFIC cluster
# without hand-reimplementing the ~90 lines of builder logic as raw
# dicts. Same pattern as test_jwks_manifests above: a one-line shim onto
# the internal lib function, exposed on the public `forge` namespace.
#
# Pin the manifest to one cluster in a multi-cluster env by setting
# `cluster` on the entity to the forge.Cluster INSTANCE — the builder
# denormalizes its `.context` and stamps `forge.dev/cluster: k3d-<name>`,
# which forge's deploy-time scoper routes to that cluster only:
#
#     import forge
#     _cp = forge.Cluster { name = "control-plane" }
#     _gw = forge.Gateway {
#         name = "cp-public", cluster = _cp
#         listeners = [forge.GatewayListener { name = "http", port = 80, protocol = "HTTP" }]
#     }
#     manifests = [forge.Manifests {objects = forge.render_gateway(_gw, "my-ns")}]
#
# render_http_route / render_grpc_route take the route plus the list of
# gateways (so the builder can resolve the parent listener + host) and
# the namespace, matching the internal signatures.
#
# `bundle` is OPTIONAL and only needed by a route whose backend is a
# `workload`: resolving one is a statement about where that workload runs
# in THIS env, so it reads the env. A route with a `service` backend needs
# no bundle, which is why this stayed a trailing default rather than a
# required fourth argument — and a workload-backed route called without it
# is refused with that explanation rather than rendering a wrong backend.
render_gateway = lambda gw: Gateway, namespace: str -> [any] {
    gw_lib.render_gateway(gw, namespace)
}

render_certificate = lambda gw: Gateway, namespace: str -> [any] {
    gw_lib.render_certificate(gw, namespace)
}

render_http_route = lambda route: HTTPRoute, gateways: [Gateway], namespace: str, bundle: any = Undefined -> [any] {
    gw_lib.render_http_route(route, gateways, namespace, bundle)
}

render_grpc_route = lambda route: GRPCRoute, gateways: [Gateway], namespace: str, bundle: any = Undefined -> [any] {
    gw_lib.render_grpc_route(route, gateways, namespace, bundle)
}



# Render a RuntimeClass as a node.k8s.io/v1 manifest. Cluster-scoped
# (no namespace). Forge labels every RuntimeClass with the standard
# managed-by/part-of so `kubectl get runtimeclasses -l
# app.kubernetes.io/managed-by=forge` is the canonical "what did
# forge create" query and the manifest-driven sweep can prune ones the
# Bundle no longer declares.
_render_runtime_class_manifest = lambda rc: RuntimeClass, namespace: str, part_of: str = "" -> any {
    {
        apiVersion = "node.k8s.io/v1"
        kind = "RuntimeClass"
        metadata = {
            name = rc.name
            # No `app.kubernetes.io/name`: an env-shared resource belongs to no
            # workload, which is what keeps it out of every `--target` group.
            # `part-of` still names the PROJECT (Bundle.project) so the release
            # selector finds it; it falls back to the namespace — the value
            # this label carried before there was a project to name — so a
            # Bundle that declares none renders byte-identically.
            labels = {
                "app.kubernetes.io/managed-by" = "forge"
                "app.kubernetes.io/part-of" = part_of if part_of else namespace
            }
        }
        handler = rc.handler
        if rc.overhead:
            overhead = rc.overhead
        if rc.scheduling:
            scheduling = rc.scheduling
    }
}

# Render a ConfigMap as a k8s manifest. The metadata.namespace
# defaults to the env-wide namespace when the caller hasn't pinned one
# explicitly on the schema.
_render_config_map_manifest = lambda cm: ConfigMap, namespace: str, part_of: str = "" -> any {
    {
        apiVersion = "v1"
        kind = "ConfigMap"
        metadata = {
            name = cm.name
            namespace = cm.namespace if cm.namespace else namespace
            # Same env-shared shape as RuntimeClass above: no workload `name`,
            # and `part-of` names the project with the historical namespace as
            # its fallback. User labels still win on key conflict.
            labels = {
                "app.kubernetes.io/managed-by" = "forge"
                "app.kubernetes.io/part-of" = part_of if part_of else namespace
            } | cm.labels
            if cm.annotations:
                annotations = cm.annotations
        }
        data = cm.data
    }
}
