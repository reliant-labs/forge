"""
Positive test — every component that serves a port ships readiness +
liveness probes, a drain window that outlasts its own shutdown
sequence, and (above one replica) a disruption budget and a spread
constraint.

The gap this pins closed: `kcl run deploy/kcl/prod | grep -c probe`
returned ZERO on a fresh scaffold. Every Deployment forge emitted had
no readiness gate (a wedged pod entered the Service endpoints
immediately and a bad image reached every replica at line speed), no
liveness probe (a deadlocked process was never restarted), and an
implicit 30s terminationGracePeriodSeconds against a 5s + 30s drain —
so the kubelet SIGKILLed the app mid-drain on every rollout.
"""

import forge.workloads as fw

_components = [
    fw.Workload {
        kind = "service"
        name = "api"
        ports = [
            fw.Port {name = "http", port = 8080, expose = True}
            fw.Port {name = "grpc", port = 9090, expose = True}
        ]
        replicas = 3
        env = {"PRE_STOP_DELAY" = "5s", "SHUTDOWN_TIMEOUT" = "30s"}
    }
    # A worker declares no Service, but it runs through the same
    # serverkit listener and answers /healthz + /readyz on PORT.
    fw.Workload {
        kind = "worker"
        name = "sync"
        replicas = 1
        env_vars = [{name = "PORT", value = "9000"}]
    }
    # An operator runs a controller-runtime manager, not an HTTP mux. It
    # gets NO probes: an HTTP probe against a process that serves no HTTP
    # is a guaranteed CrashLoopBackOff, not a safety net.
    fw.Workload {
        kind = "operator"
        name = "proxy"
        command = ["/app/demo", "proxy"]
        crds = ["Widget"]
    }
    # ...unless it DECLARES one. health_check is the escape hatch, and
    # an exec probe works for a non-HTTP process.
    fw.Workload {
        kind = "operator"
        name = "relay"
        command = ["/app/demo", "relay"]
        crds = ["Gadget"]
        health_check = {exec_command = ["/app/demo", "relay", "ping"]}
    }
]

_env = fw.WorkloadEnv {
    namespace = "demo-prod"
    project = "demo"
    registry = "ghcr.io/demo"
    image_tag = "v1"
    network_policies = False
}

manifests = fw.render_workloads(_components, _env)

_deploy_of = lambda name: str -> any {
    [m for m in manifests if m.kind == "Deployment" and m.metadata.name == name][0]
}
_container_of = lambda name: str -> any {
    _deploy_of(name).spec.template.spec.containers[0]
}

# --- Server: probed on its `http` port, not an arbitrary one ---
_api = _container_of("api")
assert_api_readiness = _api.readinessProbe.httpGet.path == "/readyz" and _api.readinessProbe.httpGet.port == 8080
assert_api_liveness = _api.livenessProbe.httpGet.path == "/healthz" and _api.livenessProbe.httpGet.port == 8080

# Readiness gates traffic: fast period, bounded failures, no startup delay.
assert_readiness_is_fast = _api.readinessProbe.periodSeconds == 5 and _api.readinessProbe.failureThreshold == 3
# Liveness restarts: it must not fire before readiness has had its full
# budget, or a slow start reads as a deadlock and the pod restart-loops.
assert_liveness_outlasts_readiness = \
    _api.livenessProbe.initialDelaySeconds > \
    _api.readinessProbe.initialDelaySeconds + _api.readinessProbe.periodSeconds * _api.readinessProbe.failureThreshold

# --- Drain window covers PRE_STOP_DELAY + SHUTDOWN_TIMEOUT ---
# serverkit's SIGTERM path is: flip /readyz, sleep PRE_STOP_DELAY, then
# shut down bounded by SHUTDOWN_TIMEOUT. 5 + 30 = 35 > k8s's implicit
# 30s default, so without this the process is SIGKILLed mid-drain.
assert_grace_covers_drain = _deploy_of("api").spec.template.spec.terminationGracePeriodSeconds >= 35

# --- Worker: probed on the PORT it is configured to listen on ---
_sync = _container_of("sync")
assert_worker_probed_on_configured_port = _sync.readinessProbe.httpGet.port == 9000
# ...and declares that port, so the manifest says what it serves.
assert_worker_declares_its_port = _sync.ports[0].containerPort == 9000

# --- Unprobed kind: no probes unless declared ---
_proxy = _container_of("proxy")
assert_custom_entrypoint_unprobed = "readinessProbe" not in _proxy and "livenessProbe" not in _proxy
assert_custom_entrypoint_declares_no_port = "ports" not in _proxy

# --- Unprobed kind with a declared exec probe: probed, without inventing HTTP ---
_relay = _container_of("relay")
assert_declared_exec_probe = _relay.readinessProbe.exec.command == ["/app/demo", "relay", "ping"]
assert_declared_exec_liveness = _relay.livenessProbe.exec.command == ["/app/demo", "relay", "ping"]

# --- Multi-replica workloads get a budget and a spread ---
_pdbs = [m for m in manifests if m.kind == "PodDisruptionBudget"]
assert_pdb_only_for_multi_replica = len(_pdbs) == 1 and _pdbs[0].metadata.name == "api-pdb"
assert_pdb_keeps_one_up = _pdbs[0].spec.maxUnavailable == 1
_api_spec = _deploy_of("api").spec.template.spec
assert_spread_across_nodes = _api_spec.topologySpreadConstraints[0].topologyKey == "kubernetes.io/hostname"
# Soft on purpose: a hard constraint strands replicas Pending forever
# on a single-node cluster, and an unschedulable pod is worse than a
# colocated one.
assert_spread_is_soft = _api_spec.topologySpreadConstraints[0].whenUnsatisfiable == "ScheduleAnyway"
# A single replica gets neither — a PDB there is vacuous or blocks drains.
assert_single_replica_no_spread = "topologySpreadConstraints" not in _deploy_of("sync").spec.template.spec
