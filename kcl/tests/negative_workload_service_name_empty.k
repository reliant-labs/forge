"""
Negative test — `service_name` cannot be empty.

An empty OTEL_SERVICE_NAME is what an author writes in `env` to opt out; an
empty `service_name` would instead silently report under no identity. Unset it
to use the workload name.
"""

# expect: service_name must not be empty

import forge.workloads as fw

_bad = fw.Workload {
    name = "api"
    service_name = ""
}
