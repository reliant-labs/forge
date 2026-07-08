"""
NEGATIVE: a host-targeted Service (host set) that ALSO sets a k8s-only field
must FAIL the Service check.

The host adapter (`_service_to_host`) projects only name/image/command/args/
env/build/host — it silently IGNORES health/security/volumes/sidecars/init/k8s.
So `Service { host = {...}, health = {...} }` would compile and silently DROP
the probe (a footgun). The Service `check` block rejects it at load time.
"""

import forge

_svc = forge.Service {
    name = "admin-server"
    image = "control-plane"
    host = forge.HostOverrides {runner = "air", air_config = ".air.admin.toml"}
    # k8s-only — silently dropped on the host target → the check must reject it.
    health = forge.HealthProbe {http_path = "/healthz", port = 8090}
}
