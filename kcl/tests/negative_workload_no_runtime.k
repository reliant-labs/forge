"""
Negative test — a workload with no runtime: neither its own `runtime` nor a
Bundle.runtime default.

Every workload must resolve to exactly one place it runs (ADR 0002 §2). The
old rule was "a workload without a host block needs a cluster_target" —
an env-wide guess about where it went. Now there is nothing to guess: an
unbound workload is refused, by name.

Replaces negative_workloads_without_cluster_target.k and
negative_self_hosted_simple_backend_no_cluster.k.
"""

# expect: orphan
# expect: runtime

import forge
import forge.workloads as fw

output = forge.render(forge.Bundle {
    project = "demo"
    workloads = [fw.Workload {name = "orphan", image = "orphan-img"}]
})
