"""
Negative test — two things in one env share a name across workloads, infra
and frontends.

That set is the WorkloadURL / `before` / --target namespace (p2 map §9.1):
a duplicate makes every reference to it ambiguous, and two cluster workloads
of the same name would apply over each other. Refused, naming it.
"""

# expect: api

import forge
import forge.workloads as fw

output = forge.render(forge.Bundle {
    project = "demo"
    runtime = forge.OnHost {}
    workloads = [fw.Workload {name = "api", build = forge.GoBuild {cmd = "./cmd/demo"}, args = ["api"]}]
    frontends = [forge.Frontend {name = "api", path = "frontends/api"}]
})
