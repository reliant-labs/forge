"""
Positive test — the probe DEFAULT is written by the lowering, per ADR 0002 §5:

  * a SERVICE that forge BUILDS gets HTTP readiness /readyz + liveness
    /healthz on its `http` port, written into the spec EXPLICITLY (paths and
    port). The lowering knows `build`; every runtime (and the hosted
    platform) sees the same probe, and nothing is inferred downstream.
  * a service with a THIRD-PARTY image (no build) gets NO probes in the
    spec: forge cannot know that image serves /readyz. Go's policy then
    gives it TCP on its first port (WorkloadSpec.EffectiveProbes).
  * a worker or job gets none unless it declares one.
  * a declared probe is kept verbatim (the default never overwrites it).

This closes the Hounders defect the ADR opens with: a hosted `api` that ran
with NO probes because the hosted path had no default.
"""

import forge
import forge.workloads as fw

_k3d = forge.ClusterTarget {cluster = "k3d-demo", namespace = "demo-dev", registry = "localhost:5050"}
_build = forge.GoBuild {cmd = "./cmd/demo", output_name = "demo"}
_pinned = "ghcr.io/acme/api@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

output = forge.render(forge.Bundle {
    project = "demo"
    control_plane = forge.ControlPlane {endpoint = "https://api.example.com"}
    runtime = forge.OnCluster {target = _k3d}
    workloads = [
        # forge-built service, cluster
        fw.Workload {name = "api", build = _build, image = "demo", args = ["api"], ports = [fw.Port {name = "http", port = 8080}]}
        # forge-built service, HOSTED — the Hounders case
        fw.Workload {name = "hosted-api", build = _build, image = "demo", args = ["api"], ports = [fw.Port {name = "http", port = 8080, expose = True}], runtime = forge.OnHosted {}}
        # forge-built service whose http port is not the first
        fw.Workload {name = "dual", build = _build, image = "demo", args = ["dual"], ports = [fw.Port {name = "grpc", port = 9090}, fw.Port {name = "http", port = 8081}]}
        # third-party image, no build
        fw.Workload {name = "redis", image = "redis:7.2", ports = [fw.Port {name = "redis", port = 6379}]}
        # forge-built worker and job: no default
        fw.Workload {name = "sync", kind = "worker", build = _build, image = "demo", args = ["sync"]}
        fw.Workload {name = "seed", kind = "job", build = _build, image = "demo", args = ["seed"]}
        # a declared probe is kept
        fw.Workload {
            name = "custom"
            build = _build
            image = "demo"
            args = ["custom"]
            ports = [fw.Port {name = "http", port = 8080}]
            probes = fw.Probes {readinessPath = "/ready", livenessPath = "/live", periodSeconds = 10}
        }
    ]
})
_p = {w.name: w.spec.probes if "probes" in w.spec else None for w in output.workloads}

# A forge-built service gets EXPLICIT HTTP probes: both paths and the port
# are written into the spec, so a CR on the wire is self-describing. The
# port is the one NAMED http, not merely the first.
_explicit = lambda p: any, port: int -> bool {
    p != None and p.readinessPath == "/readyz" and p.livenessPath == "/healthz" and p.port == port
}
assert_built_service_probed = _explicit(_p["api"], 8080)
assert_hosted_built_service_probed = _explicit(_p["hosted-api"], 8080)
assert_probe_port_is_http = _explicit(_p["dual"], 8081)
assert_default_timings = _p["api"].periodSeconds == 5 and _p["api"].failureThreshold == 3 and _p["api"].timeoutSeconds == 3
assert_third_party_no_spec_probes = _p["redis"] == None
assert_worker_no_default = _p["sync"] == None
assert_job_no_default = _p["seed"] == None
assert_declared_probe_kept = _p["custom"].readinessPath == "/ready" and _p["custom"].livenessPath == "/live" and _p["custom"].periodSeconds == 10

# The default rides the RECORD too — Go renders what the spec says.
_rec = [m for m in output.manifests if m.kind == "Workload" and m.metadata.name == "api"][0]
assert_record_carries_default = _rec.spec.probes == _p["api"]
