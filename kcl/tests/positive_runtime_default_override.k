"""
Positive test — `Bundle.runtime` is the DEFAULT; a workload's own `runtime`
OVERRIDES it (ADR 0002 §2, the hounders example).

    bundle = forge.Bundle {
        runtime = forge.OnHost {runner = "air"}      # default for this env
        workloads = [wl.membership, wl.migrate,
                     wl.search | {runtime = forge.OnCluster {target = _k3d}}]
    }

Pins that the default applies to every unbound workload (including a job),
that the override wins for exactly the workload that states it, and that
the effective runtime is what output.workloads[].runtime carries — Go never
has to know about the default.
"""

import forge
import forge.workloads as fw

_k3d = forge.ClusterTarget {cluster = "k3d-hounders", namespace = "hounders-dev", registry = "localhost:5050"}

_build = forge.GoBuild {cmd = "./cmd/hounders", output_name = "hounders"}
_membership = fw.Workload {name = "membership", build = _build, args = ["membership"]}
_migrate = fw.Workload {name = "migrate", kind = "job", build = _build, args = ["db", "migrate", "up"], before = [fw.BEFORE_ALL]}
_search = fw.Workload {name = "search", build = _build, image = "hounders", args = ["search"]}

output = forge.render(forge.Bundle {
    project = "hounders"
    runtime = forge.OnHost {runner = "air", air_config = ".air.toml"}
    workloads = [
        _membership
        _migrate
        _search | {runtime = forge.OnCluster {target = _k3d}}
    ]
})
_rt = {w.name: w.runtime for w in output.workloads}

assert_default_applies = _rt["membership"].type == "host" and _rt["membership"].runner == "air"
assert_default_applies_to_jobs = _rt["migrate"].type == "host"
# The default's fields come along whole (air_config), not just its type.
assert_default_fields = _rt["membership"].air_config == ".air.toml"
assert_override_wins = _rt["search"].type == "cluster" and _rt["search"].namespace == "hounders-dev"
assert_override_is_local = _rt["membership"].type != _rt["search"].type
# The base declaration is untouched by the binding.
assert_base_unbound = _search.runtime == Undefined or _search.runtime == None

_records = [m.metadata.name for m in output.manifests if m.kind == "Workload"]
assert_only_override_has_record = _records == ["search"]

# A Bundle with NO default where every workload binds its own is fine too.
_bound = forge.render(forge.Bundle {
    project = "hounders"
    workloads = [_membership | {runtime = forge.OnHost {}}]
})
assert_no_default_needed = _bound.workloads[0].runtime.type == "host"
