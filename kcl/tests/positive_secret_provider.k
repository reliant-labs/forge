"""
Positive tests for the bundle-level secret PROVIDER declaration.

A secret provider declares WHERE secret values come from for one env.
KCL only emits the DECLARATION (type + path) — secret VALUES never
enter KCL render output. The declaration rides in the ENTITY output
(forge.render) alongside `services`, and is ABSENT (not null) when the
bundle declares no provider. It is NEVER emitted into the k8s manifest
stream (forge.render_manifests).

Exercises:

  * DotenvSecrets projects {type:"dotenv", path:...} into output.
  * ExternalSecrets projects {type:"external"} into output.
  * No provider => `secret_provider` key absent from output.
  * Provider declaration never appears in render_manifests.
"""

import forge

_target = forge.ClusterTarget {
    cluster = "k3d-dev"
    namespace = "dev"
    registry = "localhost:5051"
}

_dotenv_bundle = forge.Bundle {
    cluster_target = _target
    services = [forge.Service {
        name = "api"
        image = "api:dev"
        deploy = _target.deploy
    }]
    secret_provider = forge.DotenvSecrets {
        path = ".env.dev.secrets"
    }
}

_external_bundle = forge.Bundle {
    services = []
    secret_provider = forge.ExternalSecrets {}
}

_no_provider_bundle = forge.Bundle {
    services = []
}

output_dotenv = forge.render(_dotenv_bundle)
output_external = forge.render(_external_bundle)
output_none = forge.render(_no_provider_bundle)
manifests_dotenv = forge.render_manifests(_dotenv_bundle)

# ── Invariants ─────────────────────────────────────────────────────

# DotenvSecrets projects type + path into the ENTITY output.
assert_dotenv_type = output_dotenv.secret_provider.type == "dotenv"
assert_dotenv_path = output_dotenv.secret_provider.path == ".env.dev.secrets"

# ExternalSecrets projects type into the ENTITY output (no path).
assert_external_type = output_external.secret_provider.type == "external"

# No provider => key ABSENT from the entity output (not null garbage).
assert_none_absent = "secret_provider" not in output_none

# The provider declaration NEVER appears in the manifest stream — no
# Secret manifest is rendered from a dotenv provider by KCL.
assert_no_secret_manifest = len([m for m in manifests_dotenv if m.kind == "Secret"]) == 0
