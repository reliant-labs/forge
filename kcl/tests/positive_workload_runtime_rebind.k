"""
Positive test — ONE declaration, TWO runtimes (ADR 0002 §1-§2).

A workload is declared once — command/args, ports, env, probes, build —
and each env BINDS it to a runtime with `|`. Nothing runtime-independent is
restated per runtime. This is the whole point of the model, so it is pinned
on the same base:

  * `wl.admin | {runtime = OnHost {...}}` → a host process: runtime block
    with the runner, and the SAME spec (args, env, ports) the cluster
    binding carries. No image: a host process runs the build, not an image.
  * `wl.admin | {runtime = OnCluster {...}}` → a Workload record whose spec
    equals the host binding's spec except `image`, which is resolved from
    the build output for the cluster registry.

It also pins the no-leakage property the old host adapter had to prove by
hand: adding a host sibling does not change the cluster record.
"""

import forge
import forge.workloads as fw

# The declaration — what deploy/kcl/workloads.k states once.
_admin = fw.Workload {
    name = "admin-server"
    build = forge.GoBuild {cmd = "./cmd/control-plane", output_name = "control-plane"}
    image = "localhost:5051/control-plane"
    args = ["admin"]
    ports = [fw.Port {name = "http", port = 8090, expose = True}]
    env = {
        APP_ENV = "dev"
        DATABASE_URL = forge.SecretRef {name = "app-secrets", key = "DATABASE_URL"}
    }
}

_k3d = forge.ClusterTarget {cluster = "k3d-cp", namespace = "cp-dev"}

_host = _admin | {runtime = forge.OnHost {runner = "air", air_config = ".air.admin.toml", listen_ports = [8090]}}
_cluster = _admin | {name = "admin-server-k8s", runtime = forge.OnCluster {target = _k3d}}

_mixed = forge.render(forge.Bundle {project = "cp", workloads = [_host, _cluster]})
_pure = forge.render(forge.Bundle {project = "cp", workloads = [_cluster]})

_w = {w.name: w for w in _mixed.workloads}
_h = _w["admin-server"]
_c = _w["admin-server-k8s"]

# ── The host binding ───────────────────────────────────────────────
assert_host_runtime = _h.runtime.type == "host" and _h.runtime.runner == "air"
assert_host_air_config = _h.runtime.air_config == ".air.admin.toml"
assert_host_build = _h.build.type == "go" and _h.build.cmd == "./cmd/control-plane"
# Env declared once reaches the host process through the spec, including
# the secret reference (resolved by forge env up at launch).
_henv = {e.name: e for e in _h.spec.env}
assert_host_env_value = _henv["APP_ENV"].value == "dev"
assert_host_env_secret = _henv["DATABASE_URL"].secretRef.name == "app-secrets"
assert_host_no_record = len([m for m in _mixed.manifests if m.kind == "Workload" and m.metadata.name == "admin-server"]) == 0

# ── The cluster binding: same facts, never restated ────────────────
assert_cluster_runtime = _c.runtime.type == "cluster" and _c.runtime.namespace == "cp-dev"
assert_same_args = _c.spec.args == _h.spec.args
assert_same_ports = _c.spec.ports == _h.spec.ports
assert_same_env = _c.spec.env == _h.spec.env
# Only the image differs: the cluster pulls the pushed build output.
assert_cluster_image_resolved = _c.spec.image.startswith("localhost:5051/control-plane")

# ── No leakage: the host sibling does not perturb the cluster record ─
_rec = lambda out: any -> any {
    [m for m in out.manifests if m.kind == "Workload" and m.metadata.name == "admin-server-k8s"][0]
}
assert_record_unperturbed = _rec(_mixed) == _rec(_pure)
