"""
Forge KCL — workload expansion (deploy-as-data normalization).

Pure render functions that take a typed forge.workloads.Workload and
emit its Kubernetes resources. This is where the workload shape a
project declares in `deploy/kcl/workloads.k` gets normalized into k8s
manifests — forge's Go owns ZERO of this; the schema + these expanders
own ALL of it.

Per kind:
  * service  → Deployment + Service (named ports → containerPort/ServicePort)
  * worker   → Deployment
  * cron     → CronJob (or one-shot Job when schedule == "")
  * job      → one-shot: init container on each `before` dependent
  * operator → manager Deployment + cluster-scoped RBAC
  * tool     → NOTHING (built into the image, never scheduled)

Lambdas take `any` for the workload arg by convention — see the
package-identity note in `lib/services.k` for why KCL forces dynamic
field access here.
"""

import crypto
import ..lib.rbac as rbac_lib
import ..lib.quantity as qty_lib

# The BROADCAST selector for `Workload.before` — "gate every workload in
# this environment", as opposed to an enumerated list of dependent names.
# See `job_gates` below for what it selects, and schema.k's `before` field
# for why it exists.
#
# A workload can never be NAMED this: `Workload.name` is used verbatim as
# a Kubernetes object name and "*" is not a legal DNS-1123 label, so the
# wildcard cannot collide with a real dependent. That is what lets one
# field carry both forms without a second field to disambiguate them.
BEFORE_ALL: str = "*"

# `part_of` is the PROJECT the workload belongs to — the "higher level
# application" the k8s recommended-labels spec means. It is what CD
# pipelines select on to find every workload of one release
# (`-l app.kubernetes.io/part-of=<project>`); labelling it with the
# workload's own name made that selector match nothing. Empty falls
# back to the workload name so a caller that has no project context
# still emits a well-formed (if ungrouped) label set.
_managed_labels = lambda name: str, part_of: str = "" -> {str: str} {
    {
        "app.kubernetes.io/name" = name
        "app.kubernetes.io/managed-by" = "forge"
        "app.kubernetes.io/part-of" = part_of if part_of else name
    }
}

_default_pod_security_context = {
    runAsNonRoot = True
    runAsUser = 65532
    runAsGroup = 65532
    seccompProfile = {type = "RuntimeDefault"}
}

_default_container_security_context = {
    runAsNonRoot = True
    runAsUser = 65532
    runAsGroup = 65532
    readOnlyRootFilesystem = True
    allowPrivilegeEscalation = False
    capabilities = {drop = ["ALL"]}
}

_default_resources = {
    cpu_request_millicores = 100
    cpu_limit_millicores = 500
    memory_request_bytes = 134217728    # 128Mi
    memory_limit_bytes = 536870912      # 512Mi
}

_image_ref = lambda image: str, image_registry: str, image_tag: str -> str {
    "${image_registry}/${image}:${image_tag}" if image_registry \
        else "${image}:${image_tag}"
}

_pull_policy = lambda image_ref: str -> str {
    _last_slash = image_ref.rfind("/")
    _ref_tail = image_ref[_last_slash + 1:] if _last_slash >= 0 else image_ref
    _last_colon = _ref_tail.rfind(":")
    _tag = _ref_tail[_last_colon + 1:] if _last_colon >= 0 else "latest"
    "Always" if _tag.endswith("-dirty") or _tag == "latest" else "IfNotPresent"
}

# k8s ADAPTER: neutral Resources (millicores + bytes) -> k8s quantity
# strings, via the shared quantity.k formatters.
_resource_block = lambda r: any -> any {
    {
        requests = {cpu = qty_lib.fmt_k8s_cpu(r.cpu_request_millicores), memory = qty_lib.fmt_k8s_mem(r.memory_request_bytes)}
        limits = {cpu = qty_lib.fmt_k8s_cpu(r.cpu_limit_millicores), memory = qty_lib.fmt_k8s_mem(r.memory_limit_bytes)}
    }
}

# Project an EnvVar to a k8s env entry.
#
# EXACTLY ONE channel is emitted. k8s rejects an env entry that carries
# both `value` and `valueFrom` — "may not be specified when `value` is
# not empty" — and it is a rejection only the API SERVER makes, so
# `kubectl apply --dry-run=client` waves it through and the deploy dies
# against the real cluster. A referenced source therefore WINS over a
# literal: the literal is the placeholder the projection emits before a
# field is bound to a Secret, and the binding is the intent.
#
# `field_ref` (the Downward API channel, declared on EnvVar and handled
# by the legacy render path) was dropped entirely here: the entry
# rendered as a bare `{name: POD_IP}`, which k8s accepts as an EMPTY
# variable. Silently empty is the worst of the three outcomes.
_env_source = lambda ev: any -> any {
    {
        name = ev.name
        if ev.secret_ref:
            valueFrom = {secretKeyRef = {name = ev.secret_ref, key = ev.secret_key if ev.secret_key else ev.name}}
        elif ev.config_map_ref:
            valueFrom = {configMapKeyRef = {name = ev.config_map_ref, key = ev.config_map_key if ev.config_map_key else ev.name}}
        elif ev.field_ref:
            valueFrom = {fieldRef = {fieldPath = ev.field_ref}}
        elif ev.value != Undefined:
            value = ev.value
    }
}

# Project the base `env` string map to k8s env entries (plain values),
# then append the typed per-env env_vars (secret/config-map backed).
_container_env = lambda c: any -> [any] {
    [{name = k, value = c.env[k]} for k in c.env] + [_env_source(e) for e in c.env_vars]
}

# Resolve the container image: the workload's image (set per
# env, typically the project image so a shared binary's workloads all run
# one image), falling back to the workload name.
_resolved_image = lambda c: any, env_registry: str, image_tag: str -> str {
    _img = c.image if c.image else c.name
    _reg = c.registry if c.registry else env_registry
    _image_ref(_img, _reg, image_tag)
}

# ---------------------------------------------------------------------------
# Health probes, drain window, and disruption budget.
# ---------------------------------------------------------------------------

# The kinds that serve HTTP health endpoints BY CONSTRUCTION. A server
# and a worker both run through serverkit, which binds a listener and
# mounts /healthz + /readyz — a worker declares no Service but still
# answers on its PORT. An `operator` runs a controller-runtime manager
# on its own endpoints and a `cron` runs to completion, so neither is
# probed unless the workload DECLARES a probe via `health_check`. An
# HTTP probe against a process that serves no HTTP is not a safety net,
# it is a guaranteed CrashLoopBackOff — probes must be absent rather
# than present-and-wrong.
_PROBED_KINDS = ["service", "worker"]

# Read one env var's literal value off a workload. The per-env
# `env_vars` overlay wins over the base `env` map, matching the
# precedence _container_env renders.
_env_literal = lambda c: any, key: str, fallback: str -> str {
    _from_vars = [e.value for e in c.env_vars if e.name == key and e.value != Undefined]
    _from_env = c.env[key] if key in c.env else ""
    _resolved = _from_vars[-1] if len(_from_vars) > 0 else _from_env
    _resolved if _resolved else fallback
}

# Seconds from a Go duration literal ("5s" / "2m" / "1h"). Anything
# that is not a plain <int><unit> falls back rather than rendering a
# nonsense number.
_duration_seconds = lambda raw: str, fallback: int -> int {
    _num = raw[:-1] if raw.endswith("s") or raw.endswith("m") or raw.endswith("h") else raw
    _mult = 60 if raw.endswith("m") else (3600 if raw.endswith("h") else 1)
    int(_num) * _mult if _num and _num.isdigit() else fallback
}

_int_or = lambda raw: str, fallback: int -> int {
    int(raw) if raw and raw.isdigit() else fallback
}

# The health_check overlay, or an empty dict. Component.health_check
# mirrors forge.HealthCheck (liveness_path / readiness_path /
# http_port / exec_command / initial_delay / period / timeout /
# failure_threshold); every field is optional and defaulted here.
_health_check = lambda c: any -> any {
    c.health_check if c.health_check != Undefined and c.health_check else {}
}

_hc_get = lambda hc: any, key: str, fallback: any -> any {
    hc[key] if key in hc and hc[key] != Undefined else fallback
}

# The port probes target: an explicitly declared http_port, else the
# workload's `http` port, else its first declared port, else the PORT
# it is configured to listen on (serverkit binds PORT and mounts
# /healthz + /readyz there), else 8080.
_probe_port = lambda c: any, hc: any -> int {
    _declared = _hc_get(hc, "http_port", 0)
    _named = [p.port for p in c.ports if p.name == "http"]
    _any_port = [p.port for p in c.ports]
    _declared if _declared else \
        (_named[0] if len(_named) > 0 else \
        (_any_port[0] if len(_any_port) > 0 else _int_or(_env_literal(c, "PORT", ""), 8080)))
}

_is_probed = lambda c: any, hc: any -> bool {
    True if c.kind in _PROBED_KINDS else bool(hc)
}

# The readiness + liveness probe pair, as a dict to merge onto the
# container ({} when the workload gets none).
#
# The two probes answer different questions and must NOT share timings.
# Readiness gates TRAFFIC: it is fast (5s period) so a wedged replica
# leaves the Service endpoints in ~15s, and it never restarts anything
# — which is what makes a dependency outage degrade instead of
# crash-looping the fleet. Liveness RESTARTS: its initial delay is
# derived from the whole readiness budget so it can never race a cold
# start, however the readiness knobs are tuned.
_probe_block = lambda c: any -> any {
    _hc = _health_check(c)
    _port = _probe_port(c, _hc)
    _exec = _hc_get(_hc, "exec_command", [])
    _period = _hc_get(_hc, "period", 5)
    _timeout = _hc_get(_hc, "timeout", 2)
    _failures = _hc_get(_hc, "failure_threshold", 3)
    _initial = _hc_get(_hc, "initial_delay", 0)
    # Liveness must outlast the full readiness budget plus a margin, so
    # a slow start is never mistaken for a deadlock.
    _live_initial = _initial + _period * _failures + 15
    _ready_handler = {exec = {command = _exec}} if _exec else \
        {httpGet = {path = _hc_get(_hc, "readiness_path", "/readyz"), port = _port}}
    _live_handler = {exec = {command = _exec}} if _exec else \
        {httpGet = {path = _hc_get(_hc, "liveness_path", "/healthz"), port = _port}}
    {
        readinessProbe = _ready_handler | {
            initialDelaySeconds = _initial
            periodSeconds = _period
            timeoutSeconds = _timeout
            failureThreshold = _failures
        }
        livenessProbe = _live_handler | {
            initialDelaySeconds = _live_initial
            periodSeconds = _period * 2
            timeoutSeconds = _timeout
            failureThreshold = _failures
        }
    } if _is_probed(c, _hc) else {}
}

# terminationGracePeriodSeconds must cover the app's OWN drain
# sequence. On SIGTERM serverkit flips /readyz to failing, sleeps
# PRE_STOP_DELAY so load balancers observe it, then shuts down bounded
# by SHUTDOWN_TIMEOUT. The scaffold's 5s + 30s against k8s's implicit
# 30s default means the kubelet SIGKILLs the process mid-drain on every
# single rollout. Derived from the workload's OWN env so retuning
# either knob cannot silently reintroduce the truncation.
#
# No preStop hook is emitted, deliberately: the app owns the drain, and
# a hook would stack a second delay on top of the one it already takes.
_grace_period = lambda c: any -> int {
    _pre_stop = _duration_seconds(_env_literal(c, "PRE_STOP_DELAY", ""), 5)
    _shutdown = _duration_seconds(_env_literal(c, "SHUTDOWN_TIMEOUT", ""), 30)
    _pre_stop + _shutdown + 5
}

# Spread replicas across nodes. Soft (`ScheduleAnyway`) on purpose: a
# hard constraint leaves replicas Pending forever on a single-node dev
# cluster, and an unschedulable pod is a worse failure than a colocated
# one.
_spread_constraints = lambda name: str -> [any] {
    [{
        maxSkew = 1
        topologyKey = "kubernetes.io/hostname"
        whenUnsatisfiable = "ScheduleAnyway"
        labelSelector = {matchLabels = {"app.kubernetes.io/name" = name}}
    }]
}

# A PodDisruptionBudget for multi-replica workloads, so a node drain
# takes one replica at a time instead of all of them at once. Omitted
# for single-replica workloads: a PDB there would either be vacuous or
# block node drains indefinitely.
_pdb = lambda c: any, namespace: str, part_of: str = "" -> [any] {
    [{
        apiVersion = "policy/v1"
        kind = "PodDisruptionBudget"
        metadata = {name = "${c.name}-pdb", namespace = namespace, labels = _managed_labels(c.name, part_of)}
        spec = {
            maxUnavailable = 1
            selector = {matchLabels = {"app.kubernetes.io/name" = c.name}}
        }
    }] if c.replicas > 1 else []
}

# A one-shot Job lowered onto ONE of the workloads it gates, as an
# initContainer.
#
# An init container is the only ordering Kubernetes enforces ITSELF. A
# standalone batch/v1 Job applied next to a Deployment orders nothing —
# the API server creates both at once and the Deployment's pods race the
# Job — so `before` would be a promise the manifests do not keep. Because
# the guarantee is the API's and not the applier's, it holds identically
# under `kubectl apply -f -`, Argo CD, Flux and `forge env deploy`, with
# no Job-name-per-release or Job-immutability bookkeeping.
#
# This is the mechanism the DEPLOY-TIME MIGRATION STEP used to have
# hand-written for it, before migrate became an ordinary broadcast job.
# The properties that made it correct there are properties of the
# lowering, so they carry over unchanged: a failing job wedges the init
# container, which stalls the rollout with the OLD pods still serving —
# the correct outcome, since shipping code that needs a schema the
# database does not have is worse than not shipping. Replicas racing the
# same one-shot are the job's own problem to be idempotent about;
# golang-migrate, the motivating case, takes a postgres advisory lock so
# one applies and the rest block, then find nothing to do.
#
# The job runs with ITS OWN command and env (it is a different program
# than the workload it gates), but with the DEPENDENT's image, because a
# forge project is one image with cobra subcommands.
#
# Resources are the DEFAULT block, not the dependent's per-env block: k8s
# computes a pod's effective request as max(largest init request, sum of
# container requests), so a small init request is free, while inheriting a
# 2-CPU app request would inflate scheduling for a step that runs for a
# second.
#
# The job's OWN env is used verbatim: a job that needs a secret declares
# it on itself, exactly as a server does.
_job_init = lambda j: any, image: str -> any {
    _env = _container_env(j)
    {
        name = j.name
        image = image
        imagePullPolicy = _pull_policy(image)
        command = j.command
        resources = _resource_block(_default_resources)
        if _env:
            env = _env
        securityContext = _default_container_security_context
    }
}

# The kinds a BROADCAST job (`before = ["*"]`) gates. Everything a
# release rolls out, and nothing that is not part of a rollout:
#
#   service / worker / operator — the Deployment-shaped kinds. These are
#       what serve traffic and consume queues, so these are exactly what
#       must not come up against a world the job was to prepare.
#   job — a NON-broadcast one-shot. A seed job that writes rows must not
#       beat the migration that creates the table it writes to. (A
#       broadcast job gating another broadcast job is excluded below,
#       which is what makes the graph acyclic by construction rather
#       than by a cycle check.)
#
# Deliberately EXCLUDED:
#   cron — a CronJob pod fires on a SCHEDULE, not as part of a release.
#       Gating it would re-run the job on every tick, forever, to no
#       purpose. This preserves exactly the behaviour the bespoke
#       migration step had.
#   tool — built into the image and never scheduled. There is no pod to
#       gate.
BROADCAST_GATED_KINDS: [str] = ["service", "worker", "operator", "job"]

# Does job `j` gate workload `target`?
#
# Two forms, one field. An ENUMERATED `before` names its dependents, and
# gates exactly those. The BROADCAST form (`["*"]`) gates every workload
# of a gateable kind — no list, so nothing to leave stale when a
# workload is added later.
#
# A broadcast job never gates ITSELF (a job cannot be its own init
# container) and never gates ANOTHER broadcast job: two of them would
# each claim to precede the other, which is not an ordering. Two
# broadcast jobs are peers — both run before everything else, in
# declaration order — and that is the honest reading of "run before
# everything".
# PUBLIC because the compose lowering delegates to it (compose.k). Two
# lowerings of one declaration must agree on what that declaration MEANS;
# re-implementing the predicate per target is how they drift.
job_gates = lambda j: any, target: any -> bool {
    _broadcast = BEFORE_ALL in j.before
    (target.name != j.name and target.kind in BROADCAST_GATED_KINDS \
        and not (target.kind == "job" and BEFORE_ALL in target.before)) \
        if _broadcast else (target.name in j.before)
}

# The init containers a workload gets from the Jobs that gate it, in
# declaration order. `jobs` is the full workload list; the filter keeps
# this a pure function of the workload set (no side channel).
#
# Declaration order is the ordering contract between multiple gates: k8s
# runs initContainers in list order, so a migration job declared before
# a provisioning job runs first, and the provisioner sees a current
# schema. That is the same guarantee the bespoke migration step gave by
# concatenating itself ahead of the job list, now expressed by where the
# author puts the declaration.
_gating_jobs = lambda target: any, jobs: [any], image: str -> [any] {
    [_job_init(j, image) for j in jobs if j.kind == "job" and job_gates(j, target)]
}

# Shared Deployment builder. `service_account` is set for operators
# (which need a bound SA for cluster RBAC); empty otherwise.
#
# `gating` is the initContainer list contributed by the one-shot Jobs
# that gate this workload, in declaration order. The deploy-time
# migration step is one of these now — an ordinary broadcast job — not a
# separate parameter threaded alongside it.
_deployment = lambda c: any, namespace: str, image: str, ports: [any], extra_env: [any], service_account: str, part_of: str = "", gating: [any] = [] -> any {
    _name = c.name
    _labels = _managed_labels(_name, part_of)
    _resources = c.resources if c.resources else _default_resources
    _replicas = c.replicas
    # A probed workload always declares the port it answers on, even
    # when nothing routes to it (a worker has no Service). containerPort
    # is documentary in k8s, and stating it is what makes the manifest
    # self-describing: a container with a port serves HTTP and must be
    # probed; a container without one has nothing to probe.
    _probes = _probe_block(c)
    _declared_ports = ports if ports else \
        ([{containerPort = _probe_port(c, _health_check(c)), protocol = "TCP", name = "http"}] if _probes else [])
    _all_env = _container_env(c) + extra_env
    _container = {
        name = _name
        image = image
        imagePullPolicy = _pull_policy(image)
        if c.command:
            command = c.command
        if _declared_ports:
            ports = _declared_ports
        resources = _resource_block(_resources)
        if _all_env:
            env = _all_env
        securityContext = _default_container_security_context
    }
    _init_containers = gating
    {
        apiVersion = "apps/v1"
        kind = "Deployment"
        metadata = {name = _name, namespace = namespace, labels = _labels}
        spec = {
            replicas = _replicas
            selector = {matchLabels = {"app.kubernetes.io/name" = _name}}
            template = {
                metadata = {labels = _labels}
                spec = {
                    if service_account:
                        serviceAccountName = service_account
                    securityContext = _default_pod_security_context
                    terminationGracePeriodSeconds = _grace_period(c)
                    if _replicas > 1:
                        topologySpreadConstraints = _spread_constraints(_name)
                    # Absent (not empty) when nothing gates this workload, so a
                    # project with no one-shots renders no initContainers key
                    # at all.
                    if _init_containers:
                        initContainers = _init_containers
                    containers = [_container | _probes]
                }
            }
        }
    }
}

# kind="service" → Deployment + Service. Named ports become both
# containerPorts and ServicePorts.
render_service = lambda c: any, namespace: str, env_registry: str, image_tag: str, part_of: str = "", gating: [any] = [] -> [any] {
    _name = c.name
    _labels = _managed_labels(_name, part_of)
    _image = _resolved_image(c, env_registry, image_tag)
    _ports = c.ports if len(c.ports) > 0 else [{name = "http", port = 8080, protocol = "tcp", expose = True}]
    _container_port_list = [{containerPort = p.port, protocol = "TCP", name = p.name} for p in _ports]
    # Bind the workload's own ServiceAccount. Leaving it unset ran
    # every pod as the namespace `default` SA — see lib/rbac.k.
    _deploy = _deployment(c, namespace, _image, _container_port_list, [], _name, part_of, gating)
    _service = {
        apiVersion = "v1"
        kind = "Service"
        metadata = {name = _name, namespace = namespace, labels = _labels}
        spec = {
            selector = {"app.kubernetes.io/name" = _name}
            ports = [{name = p.name, port = p.port, targetPort = p.port, protocol = "TCP"} for p in _ports]
        }
    }
    [_deploy, _service] + _pdb(c, namespace, part_of) \
        + rbac_lib.render_service_account(_name, namespace, part_of)
}

# kind="worker" → Deployment only (no Service). Nothing dials a worker,
# so it gets no Service; a distinct entrypoint comes from `command`.
render_worker = lambda c: any, namespace: str, env_registry: str, image_tag: str, part_of: str = "", gating: [any] = [] -> [any] {
    _image = _resolved_image(c, env_registry, image_tag)
    [_deployment(c, namespace, _image, [], [], c.name, part_of, gating)] + _pdb(c, namespace, part_of) \
        + rbac_lib.render_service_account(c.name, namespace, part_of)
}

# Cron → CronJob (or one-shot Job when schedule == "").
render_cron = lambda c: any, namespace: str, env_registry: str, image_tag: str, part_of: str = "" -> [any] {
    _name = c.name
    _labels = _managed_labels(_name, part_of)
    _image = _resolved_image(c, env_registry, image_tag)
    _pod_spec = {
        restartPolicy = "OnFailure"
        containers = [
            {
                name = _name
                image = _image
                imagePullPolicy = _pull_policy(_image)
                if c.command:
                    command = c.command
                _env = _container_env(c)
                if _env:
                    env = _env
            }
        ]
    }
    _jobs = [{
        apiVersion = "batch/v1"
        kind = "CronJob"
        metadata = {name = _name, namespace = namespace, labels = _labels}
        spec = {
            schedule = c.schedule
            jobTemplate = {spec = {template = {metadata = {labels = _labels}, spec = _pod_spec}}}
        }
    }] if c.schedule else [{
        apiVersion = "batch/v1"
        kind = "Job"
        metadata = {name = _name, namespace = namespace, labels = _labels}
        spec = {template = {metadata = {labels = _labels}, spec = _pod_spec}}
    }]
    _jobs + rbac_lib.render_service_account(_name, namespace, part_of)
}

# Job → a one-shot batch/v1 Job, and ONLY when it gates nothing.
#
# A Job that declares `before` renders NO standalone Job object: it is
# lowered into an initContainer on each dependent (see _gating_jobs).
# Rendering both would run the command twice — once unordered as a Job,
# once ordered as an init container — and the unordered copy is exactly
# the race the `before` declaration exists to remove.
#
# `restartPolicy = OnFailure` + `backoffLimit` let a transient failure
# (the database still accepting connections, the IdP still starting) retry
# rather than wedge, while a genuinely broken command still gives up
# instead of retrying forever.
#
# `gating` is the initContainer list from the one-shots that gate THIS
# job — in practice a broadcast job such as the migration step. A job is
# gateable for the same reason a Deployment is: a seed one-shot that
# writes rows must not run before the migration that creates the table.
render_job = lambda c: any, namespace: str, env_registry: str, image_tag: str, part_of: str = "", gating: [any] = [] -> [any] {
    _name = c.name
    _labels = _managed_labels(_name, part_of)
    _image = _resolved_image(c, env_registry, image_tag)
    _env = _container_env(c)

    # The pod spec this Job would run. Built once, then hashed for the name
    # below — so the hash is derived from the rendered result rather than from
    # a hand-listed set of fields that would silently stop covering a new one.
    _pod = {
        restartPolicy = "OnFailure"
        serviceAccountName = _name
        securityContext = _default_pod_security_context
        if gating:
            initContainers = gating
        containers = [{
            name = _name
            image = _image
            imagePullPolicy = _pull_policy(_image)
            command = c.command
            resources = _resource_block(c.resources if c.resources else _default_resources)
            if _env:
                env = _env
            securityContext = _default_container_security_context
        }]
    }

    # A Job's spec.template is IMMUTABLE. With a fixed name, re-applying an
    # unchanged Job is not a no-op — the API server rejects it with
    #
    #     Job.batch "<name>" is invalid: spec.template: ... field is immutable
    #
    # and because that surfaces at the END of a whole-manifest apply, every
    # Deployment has already been updated by then. The deploy reports FAILURE
    # on a rollout that in fact succeeded, which is a genuinely dangerous
    # signal: normalising "the deploy is red but it's fine" is how a real
    # failure eventually gets waved through. Working around it out-of-band
    # means delete-then-recreate, and `kubectl delete job --wait` returns
    # before the object is gone, so the re-apply races the deletion.
    #
    # Putting the spec hash in the NAME dissolves the problem instead of
    # handling it. An unchanged spec renders an identical name, so the apply
    # is a true no-op; a changed spec renders a NEW name, so a new Job is
    # created and the old one remains as history. This is the same trick a
    # Deployment plays one layer down, where pod-template-hash names the
    # ReplicaSet.
    #
    # The name is a plain expression, so a project that needs a fixed name
    # (something outside the render referencing it by exact name) overrides it
    # with ordinary KCL config-unification — no schema field, no knob:
    #
    #     _job | {metadata: {name = "control-plane-migrate"}}
    #
    # Use `:` and not `=`. The `:` form deep-merges and keeps the labels
    # below; `=` REPLACES the whole metadata map and silently drops them.
    #
    # Hashing the rendered pod spec means the image is hashed as RESOLVED —
    # a digest when one was captured. So a deploy that only moves the tag
    # (a fresh commit SHA over identical bytes) leaves the hash alone and
    # recreates nothing.
    _spec_hash = crypto.sha256(str(_pod))[:10]

    _job = [{
        apiVersion = "batch/v1"
        kind = "Job"
        metadata = {
            name = _name + "-" + _spec_hash
            namespace = namespace
            # job-name is the STABLE handle: `kubectl get job -l
            # forge.dev/job-name=<name>` finds the Job whatever its spec, which
            # a hashed metadata.name alone would not give you. spec-hash
            # answers "did this actually change?" without diffing YAML.
            labels = _labels | {
                "forge.dev/job-name" = _name
                "forge.dev/spec-hash" = _spec_hash
            }
            # Collect superseded Jobs rather than accumulating one per spec
            # change forever. 24h keeps a failed run inspectable for a day.
            # Overridable the same way the name is.
        }
        spec = {
            backoffLimit = 6
            ttlSecondsAfterFinished = 86400
            template = {
                metadata = {labels = _labels | {"forge.dev/job-name" = _name}}
                spec = _pod
            }
        }
    }] if not c.before else []
    # Most one-shots never touch the Kubernetes API — a migration talks to
    # postgres, not to the cluster — so they get the bare identity
    # render_service_account gives every non-RBAC workload: an SA with no
    # Role and no mounted token, byte-identical to before this field
    # existed. A job that DECLARES namespaced_rbac (a convergence step
    # that writes a ConfigMap, say) gets a real Role + RoleBinding instead
    # — the same namespaced-RBAC shape a Service gets, because the need is
    # identical: in-cluster writes this specific workload does and nothing
    # else in the namespace should inherit.
    _job + (rbac_lib.render_namespaced_rbac(_name, namespace, c.namespaced_rbac, [], part_of) \
        if c.namespaced_rbac else rbac_lib.render_service_account(_name, namespace, part_of))
}

# Operator → manager Deployment + cluster-scoped RBAC. The Deployment
# binds the operator's ServiceAccount; leader-election env is injected.
render_operator = lambda c: any, namespace: str, env_registry: str, image_tag: str, part_of: str = "", gating: [any] = [] -> [any] {
    _name = c.name
    _image = _resolved_image(c, env_registry, image_tag)
    _leader_env = [{name = "LEADER_ELECTION", value = "true"}] if c.leader_election else []
    _deploy = _deployment(c, namespace, _image, [], _leader_env, _name, part_of, gating)
    # Operators get cluster RBAC with the default config-read rules
    # (rbac_lib adds them); operator CRDs don't carry custom rules in
    # the denormalized shape, so pass an empty rule set.
    [_deploy] + rbac_lib.render_cluster_rbac(_name, namespace, {rules = []}, [], part_of)
}

# Dispatch a single workload on its kind. Each branch produces the
# kind's full manifest list. Unknown kinds produce nothing.
#
# `tool` deliberately renders NOTHING: it is built into the image and
# never scheduled, so it has no runtime manifest. It reaches the build
# contract through the env main.k bridge, not through here.
#
# `gating` is the initContainer list contributed by the one-shot jobs
# that gate this workload. Callers that have the whole workload list
# should use `render_workload_in` (below), which computes it; this entry
# point stays for callers that expand one workload in isolation.
#
# `cron` never receives gating: a CronJob pod fires on a SCHEDULE, not as
# part of a release rollout, so an init container there would re-run the
# one-shot on every tick to no purpose. `job` DOES, so that a seed
# one-shot cannot beat the migration that creates the table it seeds.
render_workload = lambda c: any, namespace: str, env_registry: str, image_tag: str, part_of: str = "", gating: [any] = [] -> [any] {
    render_service(c, namespace, env_registry, image_tag, part_of, gating) if c.kind == "service" else \
        (render_worker(c, namespace, env_registry, image_tag, part_of, gating) if c.kind == "worker" else \
        (render_cron(c, namespace, env_registry, image_tag, part_of) if c.kind == "cron" else \
        (render_job(c, namespace, env_registry, image_tag, part_of, gating) if c.kind == "job" else \
        (render_operator(c, namespace, env_registry, image_tag, part_of, gating) if c.kind == "operator" else []))))
}

# Dispatch one workload IN THE CONTEXT of the full workload list, so a
# one-shot job's `before` can be lowered onto the workloads it gates.
#
# Ordering is a relation between two workloads, so it cannot be resolved
# from one workload alone — this is the entry point render_workloads
# uses. A job gates a workload by contributing an initContainer to it,
# which is why the dependent needs to see the job, not the other way round.
# (`siblings` rather than `all`: `all` is a KCL builtin quantifier.)
render_workload_in = lambda c: any, siblings: [any], namespace: str, env_registry: str, image_tag: str, part_of: str = "" -> [any] {
    render_workload(c, namespace, env_registry, image_tag, part_of, \
        _gating_jobs(c, siblings, _resolved_image(c, env_registry, image_tag)))
}
