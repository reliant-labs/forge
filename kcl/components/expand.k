"""
Forge KCL — component expansion (deploy-as-data normalization).

Pure render functions that take a typed forge.Component (Server /
Worker / Cron / Operator / Binary) and emit its Kubernetes resources.
This is where the denormalized component shape forge serializes to
`components_gen.json` gets normalized into k8s manifests — forge owns
ZERO of this; the schema hierarchy + these expanders own ALL of it.

Per kind:
  * Server   → Deployment + Service (named ports → containerPort/ServicePort)
  * Worker   → Deployment
  * Cron     → CronJob (or one-shot Job when schedule == "")
  * Operator → manager Deployment + cluster-scoped RBAC
  * Binary   → Deployment running command (["/app/<bin>"])

Lambdas take `any` for the component arg by convention — see the
package-identity note in `lib/services.k` for why KCL forces dynamic
field access here.
"""

import ..lib.rbac as rbac_lib
import ..lib.quantity as qty_lib

# `part_of` is the PROJECT the component belongs to — the "higher level
# application" the k8s recommended-labels spec means. It is what CD
# pipelines select on to find every workload of one release
# (`-l app.kubernetes.io/part-of=<project>`); labelling it with the
# component's own name made that selector match nothing. Empty falls
# back to the component name so a caller that has no project context
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

# Resolve the container image: the component's overlay image (set per
# env, typically the project image so shared-binary components all run
# one image), falling back to the component name.
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
# answers on its PORT. A `binary` runs an arbitrary entrypoint and a
# `cron` runs to completion, so neither is probed unless the component
# DECLARES a probe via `health_check`. An HTTP probe against a process
# that serves no HTTP is not a safety net, it is a guaranteed
# CrashLoopBackOff — probes must be absent rather than present-and-wrong.
_PROBED_KINDS = ["server", "worker"]

# Read one env var's literal value off a component. The per-env
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
# component's `http` port, else its first declared port, else the PORT
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
# container ({} when the component gets none).
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
# single rollout. Derived from the component's OWN env so retuning
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

# The DEPLOY-TIME migration step, as an initContainer.
#
# It runs the SAME image as the app (the migrations are embedded in that
# binary — there is no db/migrations directory in the runtime image) with
# the SAME env (so it reads the same DATABASE_URL, from the same Secret),
# and exits 0 once the schema is current. The app container does not start
# until it does — which is the entire point: a schema-changing release must
# never serve a request against the old schema.
#
# Resources are the DEFAULT block, not the workload's per-env block: k8s
# computes a pod's effective request as max(largest init request, sum of
# container requests), so a small init request is free, while inheriting a
# 2-CPU app request would inflate scheduling for a step that runs for a
# second.
#
# `migrate` empty => no init container, and the pod renders BYTE-IDENTICALLY
# to what it did before this existed.
_migrate_init = lambda image: str, migrate: [str], container_env: [any] -> [any] {
    [{
        name = "migrate"
        image = image
        imagePullPolicy = _pull_policy(image)
        command = migrate
        resources = _resource_block(_default_resources)
        if container_env:
            env = container_env
        securityContext = _default_container_security_context
    }] if migrate else []
}

# Shared Deployment builder. `service_account` is set for operators
# (which need a bound SA for cluster RBAC); empty otherwise. `migrate` is
# the deploy-time migration argv (see _migrate_init); empty for none.
_deployment = lambda c: any, namespace: str, image: str, ports: [any], extra_env: [any], service_account: str, part_of: str = "", migrate: [str] = [] -> any {
    _name = c.name
    _labels = _managed_labels(_name, part_of)
    _resources = c.resources if c.resources else _default_resources
    _replicas = c.replicas
    # A probed component always declares the port it answers on, even
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
    _init_containers = _migrate_init(image, migrate, _all_env)
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
                    # Absent (not empty) when there is no migration step, so a
                    # project without migrations renders exactly as before.
                    if _init_containers:
                        initContainers = _init_containers
                    containers = [_container | _probes]
                }
            }
        }
    }
}

# Server → Deployment + Service. Named ports become both
# containerPorts and ServicePorts.
render_server = lambda c: any, namespace: str, env_registry: str, image_tag: str, part_of: str = "", migrate: [str] = [] -> [any] {
    _name = c.name
    _labels = _managed_labels(_name, part_of)
    _image = _resolved_image(c, env_registry, image_tag)
    _ports = c.ports if len(c.ports) > 0 else [{name = "http", port = 8080, protocol = "tcp", expose = True}]
    _container_port_list = [{containerPort = p.port, protocol = "TCP", name = p.name} for p in _ports]
    # Bind the component's own ServiceAccount. Leaving it unset ran
    # every pod as the namespace `default` SA — see lib/rbac.k.
    _deploy = _deployment(c, namespace, _image, _container_port_list, [], _name, part_of, migrate)
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

# Worker → Deployment only (no Service).
render_worker = lambda c: any, namespace: str, env_registry: str, image_tag: str, part_of: str = "", migrate: [str] = [] -> [any] {
    _image = _resolved_image(c, env_registry, image_tag)
    [_deployment(c, namespace, _image, [], [], c.name, part_of, migrate)] + _pdb(c, namespace, part_of) \
        + rbac_lib.render_service_account(c.name, namespace, part_of)
}

# Binary → Deployment running `command` (["/app/<bin>"]).
# Same shape as a worker; the distinct entrypoint comes from `command`.
render_binary = lambda c: any, namespace: str, env_registry: str, image_tag: str, part_of: str = "", migrate: [str] = [] -> [any] {
    _image = _resolved_image(c, env_registry, image_tag)
    [_deployment(c, namespace, _image, [], [], c.name, part_of, migrate)] + _pdb(c, namespace, part_of) \
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

# Operator → manager Deployment + cluster-scoped RBAC. The Deployment
# binds the operator's ServiceAccount; leader-election env is injected.
render_operator = lambda c: any, namespace: str, env_registry: str, image_tag: str, part_of: str = "", migrate: [str] = [] -> [any] {
    _name = c.name
    _image = _resolved_image(c, env_registry, image_tag)
    _leader_env = [{name = "LEADER_ELECTION", value = "true"}] if c.leader_election else []
    _deploy = _deployment(c, namespace, _image, [], _leader_env, _name, part_of, migrate)
    # Operators get cluster RBAC with the default config-read rules
    # (rbac_lib adds them); component CRDs don't carry custom rules in
    # the denormalized shape, so pass an empty rule set.
    [_deploy] + rbac_lib.render_cluster_rbac(_name, namespace, {rules = []}, [], part_of)
}

# Dispatch a single component on its kind. Each branch produces the
# kind's full manifest list. Unknown kinds produce nothing.
#
# `migrate` (the deploy-time migration argv) reaches every DEPLOYMENT-shaped
# kind. `cron` is deliberately excluded: a CronJob pod fires on a schedule,
# not as part of a release rollout, so an init container there would re-run
# the migrator on every tick to no purpose.
render_component = lambda c: any, namespace: str, env_registry: str, image_tag: str, part_of: str = "", migrate: [str] = [] -> [any] {
    render_server(c, namespace, env_registry, image_tag, part_of, migrate) if c.kind == "server" else \
        (render_worker(c, namespace, env_registry, image_tag, part_of, migrate) if c.kind == "worker" else \
        (render_binary(c, namespace, env_registry, image_tag, part_of, migrate) if c.kind == "binary" else \
        (render_cron(c, namespace, env_registry, image_tag, part_of) if c.kind == "cron" else \
        (render_operator(c, namespace, env_registry, image_tag, part_of, migrate) if c.kind == "operator" else []))))
}
