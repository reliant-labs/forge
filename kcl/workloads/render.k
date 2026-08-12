"""
Forge KCL — workload render entry points.

Sibling of schema.k so it sees Workload, Port, Resources, EnvVar as
bare names (no import that would fork type identity).

One responsibility: `render_workloads(workloads, env)` — normalize a
list of typed workloads into the Kubernetes manifest list for an
environment. Each workload's kind selects its expansion
(service→Deployment+Service, cron→CronJob, …). Namespace-scoped
boilerplate (the Namespace object, network policies) is added once.

The workload list is a PLAIN KCL VALUE, named EXPLICITLY by the env
that deploys it. A project declares complete workloads in the
user-owned `deploy/kcl/workloads.k`; each env's main.k imports that
file, REFINES the ones it runs, and may add env-only entries:

    import ..workloads as wl

    _api = wl.api | {replicas = 3}
    _workloads = [_api, _postgres]   # explicit: this env IS these

Aggregation is explicit rather than collected: `Schema.instances()`
does not cross package boundaries in KCL 0.11, and naming the list is
what lets dev declare a containerized postgres that prod omits.

There is no generated data file and no `file.read` in this path, so an
env renders from a clean checkout with nothing generated first.
"""

import .expand as comp_lib
import ..lib.netpol as netpol_lib

schema WorkloadEnv:
    """Per-environment knobs the workload manifest renderer needs.
    Mirrors RenderEnv (schema.k) but keyed to the workload model.

    namespace / registry are the env-wide deploy target; image_tag is
    the per-build override (forge passes `-D image_tag=<sha>`).

    `project` is the PROJECT name every rendered object is labelled
    with (`app.kubernetes.io/part-of`). It is the handle a CD pipeline
    uses to find one release's workloads — `kubectl get deploy -n <ns>
    -l app.kubernetes.io/part-of=<project>` — so it must be the
    project, not a workload name. Defaults to "" (each object falls
    back to labelling itself), which is why the generated per-env
    main.k always sets it.
    """
    namespace: str
    project: str = ""
    registry: str = ""
    image_tag: str = "latest"
    network_policies: bool = True

    # WHERE THE DEPLOY-TIME MIGRATION STEP WENT
    # =========================================
    # It used to be a `migrate: [str]` field here — an argv this schema
    # rendered as an initContainer on every Deployment-shaped workload.
    # It is now an ORDINARY WORKLOAD: a `kind = "job"` with
    # `before = ["*"]`, declared in `deploy/kcl/workloads.k` alongside
    # everything else the project ships.
    #
    #     migrate = fw.Workload {
    #         name = "migrate"
    #         kind = "job"
    #         command = ["/app/myapp", "db", "migrate", "up"]
    #         before = [fw.BEFORE_ALL]
    #     }
    #
    # Nothing about the rendered manifest changed: the same init
    # container, in the same position, with the same failure semantics.
    # What changed is that migrate is no longer a SPECIAL CASE. It was
    # the one deploy-time ordering forge knew how to express, and it
    # could only express that one; a project needing a second one (seed
    # reference data, provision an IdP, warm a cache) had nowhere to put
    # it. As a job it is one instance of a general primitive, and the
    # env-wide "run before everything" that made it special is now a
    # selector any job can use.
    #
    # An env that migrates OUT OF BAND — a DBA-run pipeline, a managed
    # database console, a separate release train — omits the workload
    # from the list it deploys. That is the successor to `migrate = []`,
    # and it is the same gesture as dropping any other workload an env
    # does not run.

    # --- NetworkPolicy inputs (read only when network_policies) ---
    #
    # `default-deny` blackholes every egress the allow-list misses, so
    # these are what keep an env's own database, collector and ingress
    # reachable. They are DECLARED rather than guessed: an empty
    # namespace omits its policy instead of rendering one against a
    # namespace that may not exist — a policy allowing traffic from a
    # namespace nothing runs in is a policy that denies it.
    #
    # egress_ports are TCP ports the workload may dial anywhere: 443 for
    # HTTPS (JWKS, object storage, third-party APIs) and 5432 for a
    # managed Postgres, which normally lives outside the cluster. Add
    # the port of any other external dependency (6379, 9092, …).
    egress_ports: [int] = [443, 5432]
    # Namespace running the OTLP collector the OTEL_EXPORTER_OTLP_ENDPOINT
    # points at. Empty when telemetry is not exported off-cluster.
    telemetry_namespace: str = ""
    # Namespace running the Gateway API / ingress controller that
    # terminates public traffic. Must match where the controller for the
    # env's `gateway_class_name` actually runs, or ingress is denied.
    ingress_namespace: str = ""

    check:
        namespace, "WorkloadEnv.namespace is required"

_render_namespace = lambda env: WorkloadEnv -> any {
    {
        apiVersion = "v1"
        kind = "Namespace"
        metadata = {
            name = env.namespace
            labels = {
                "app.kubernetes.io/managed-by" = "forge"
                "pod-security.kubernetes.io/enforce" = "restricted"
                "pod-security.kubernetes.io/audit" = "restricted"
                "pod-security.kubernetes.io/warn" = "restricted"
            }
        }
    }
}

# Normalize a list of typed workloads into the k8s manifest list for
# an environment. The Namespace comes first; each workload expands per
# its kind; network policies (when on) close it out.
#
# `--target` group model: each workload's own manifests carry their
# workload name (stamped in expand.k). The Namespace + network
# policies are env-shared and belong to no workload, so they carry NO
# `app.kubernetes.io/name` — they apply on a bare deploy and are never
# selected by a `--target`. No synthetic env/namespace group.
render_workloads = lambda workloads: [any], env: WorkloadEnv -> [any] {
    # A `before` naming a workload that does not exist is a SILENT no-op
    # otherwise: the job renders, nothing gates on it, and the ordering the
    # author declared simply does not happen. Fail at load with the name
    # they meant and the names available.
    #
    # BEFORE_ALL is exempt: it is a selector, not a name, and it cannot
    # dangle — there is no list to go stale, which is the whole reason it
    # exists.
    _names = [c.name for c in workloads]
    _dangling = [j.name + " -> " + d for j in workloads if j.kind == "job" \
        for d in j.before if d != comp_lib.BEFORE_ALL and d not in _names]
    assert len(_dangling) == 0, \
        "`before` names a workload that does not exist in this environment: " \
        + ", ".join(_dangling) + ". Declared workloads: " + ", ".join(_names) \
        + ". Fix the name, use ['" + comp_lib.BEFORE_ALL + "'] to gate every workload, " \
        + "or drop `before` to render an ungated one-shot Job."

    [_render_namespace(env)] \
        + [item for c in workloads for item in comp_lib.render_workload_in(c, workloads, env.namespace, env.registry, env.image_tag, env.project)] \
        + (netpol_lib.render_network_policies(
            env.namespace, env.egress_ports, env.telemetry_namespace, env.ingress_namespace
        ) if env.network_policies else [])
}
