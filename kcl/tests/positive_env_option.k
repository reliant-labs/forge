"""
Positive test — `option("env")` plumbing.

The forge CLI passes `-D env=<env>` on every `kcl run` invocation so
user main.k files can read the current environment via the KCL
`option()` builtin and conditionally include manifests per-env. The
canonical use is suppressing in-cluster infra (NATS / Temporal /
LiteLLM) on `dev-host` envs where docker-compose provides those
services, while still shipping them on k3d / staging / prod.

Run me with:

    kcl run -D env=dev-host kcl/tests/positive_env_option.k

The asserts below pin the conditional-include pattern documented in
kcl/README.md.
"""

import forge

# The canonical conditional pattern. _env_name reads the -D env=<name>
# binding the forge CLI passes; downstream branches gate
# `additional_manifests` (or any other field) on its value.
_env_name = option("env") or "dev"
_is_dev_host = _env_name == "dev-host"

# In-cluster NATS — only shipped to envs that DON'T already provide
# NATS via docker-compose. Real projects expand this to a list of
# Deployment/Service/ConfigMap manifests; the exact contents don't
# matter for this test, only the conditional inclusion.
_nats_manifests = [] if _is_dev_host else [
    {
        apiVersion = "v1"
        kind = "Service"
        metadata = {name = "nats"}
    }
]

# A Service so the bundle isn't empty (forge.Bundle requires nothing
# but the renderer's downstream consumers do).
_svc = forge.Service {
    name = "api"
    image = "myapp:dev"
    deploy = forge.HostDeploy {runner = "go-run"}
}

_bundle = forge.Bundle {
    services = [_svc]
    additional_manifests = _nats_manifests
}

_k8s = forge.K8sCluster {
    cluster = "k3d-myapp"
    namespace = "myapp-" + _env_name
    registry = "localhost:5050"
}

output = forge.render(_bundle)
manifests = forge.render_manifests(_bundle, forge.RenderEnv {cluster = _k8s})

# ── Invariants ─────────────────────────────────────────────────────

# option("env") flows through.
assert_env_name = _env_name == option("env") or _env_name == "dev"

# Conditional include matches the env.
assert_nats_included_iff_not_dev_host = len(_nats_manifests) == (0 if _is_dev_host else 1)

# additional_manifests propagates through the manifest renderer too.
# Count NATS Service entries; conditional skip on dev-host drops it to
# zero, non-dev-host envs see exactly one.
_nats_in_manifests_count = sum([1 if (m.kind == "Service" and m.metadata.name == "nats") else 0 for m in manifests])
assert_nats_in_manifests_iff_not_dev_host = _nats_in_manifests_count == (0 if _is_dev_host else 1)
