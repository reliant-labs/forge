"""
Positive test — a SimpleBackend that sets only its REQUESTS.

"Set a request, leave the limit" is the most common resources block anyone
writes. The generated KCL used to default each limit to the static entry
shape (250m / 1 GiB), so a 500m request carried a 250m limit, and Go's
Validate refused the spec it had just been handed.

An unset limit means "equal to its request". A static default cannot
express that, so the tier schema leaves the limits UNSET, and the record
(the hosted CR spec) omits them. Go's Resources.WithDefaults resolves them
to the requests when the record is rendered or reconciled. internal/cli
TestSimpleBackend_RequestOnlyLimitsEqualRequests asserts the rendered
Deployment's limits.
"""

import forge
import forge.tiers

_manifests = forge.render_manifests(forge.Bundle {
    project = "p"
    cluster_target = forge.ClusterTarget { cluster = "c", namespace = "n", registry = "r.io/x" }
    services = [forge.RenderedWorkload {
        name = "w"
        deploy = forge.SimpleBackend {
            cluster = "c"
            namespace = "n"
            spec = tiers.SimpleBackend {
                image = "ghcr.io/a/w:v1"
                ports = [3000]
                resources = tiers.Resources { cpuRequestMillicores = 500, memoryRequestBytes = 2147483648 }
            }
        }
    }]
}, "t", {}, False)

_res = [m for m in _manifests if m.kind == "SimpleBackend"][0].spec.resources

assert_cpu_request = _res.cpuRequestMillicores == 500
assert_mem_request = _res.memoryRequestBytes == 2147483648
# Absent, not a static 250m / 1 GiB that sits below the request.
assert_cpu_limit_unset = "cpuLimitMillicores" not in _res
assert_mem_limit_unset = "memoryLimitBytes" not in _res
