"""
Positive tests for the RenderedSecrets secret provider (dotenv-sourced).

RenderedSecrets declares named k8s Secrets EXPLICITLY (name + per-key
source). This file exercises the `from="dotenv"` source (the env-agnostic
path — no inline values, so no `-D env` gate). The `from="literal"`
dev/e2e gating has its own env-bound test
(positive_rendered_secrets_literal.k) and negative test
(negative_rendered_secrets_literal_prod.k).

KCL only emits the DECLARATION (name + per-key from/key) — for dotenv
keys the VALUE never enters KCL output. The declaration rides in the
ENTITY output alongside other providers, and is NEVER a rendered manifest
(forge renders + applies the Secrets CLI-side, per cluster).
"""

import forge

_target = forge.ClusterTarget {
    cluster = "k3d-dev"
    namespace = "dev"
    registry = "localhost:5051"
}

_bundle = forge.Bundle {
    cluster_target = _target
    services = [forge.RenderedWorkload {
        name = "api"
        image = "api:dev"
        deploy = _target.deploy
        env_vars = [forge.EnvVar {
            name = "DB_PASSWORD"
            secret_ref = "db-credentials"
            secret_key = "password"
        }]
    }]
    secret_provider = forge.RenderedSecrets {
        secrets = [
            forge.RenderedSecret {
                name = "db-credentials"
                keys = {
                    "password" = forge.RenderedSecretKey {
                        from = "dotenv"
                        key = "DB_PASSWORD"
                    }
                    "username" = forge.RenderedSecretKey {
                        from = "dotenv"
                        key = "DB_USER"
                    }
                }
            }
        ]
    }
}

output = forge.render(_bundle)
manifests = forge.render_manifests(_bundle)

# ── Invariants ─────────────────────────────────────────────────────

# The provider type + declared Secret name project into the entity output.
assert_type = output.secret_provider.type == "rendered"
assert_secret_name = output.secret_provider.secrets[0].name == "db-credentials"

# Each key's SOURCE (from + key) projects — but no VALUE (dotenv keys
# carry only the reference).
assert_pw_from = output.secret_provider.secrets[0].keys.password.from == "dotenv"
assert_pw_key = output.secret_provider.secrets[0].keys.password.key == "DB_PASSWORD"
assert_pw_no_value = output.secret_provider.secrets[0].keys.password.value == Undefined

# The provider declaration NEVER appears in the manifest stream — forge
# renders + applies the Secret CLI-side, not via KCL.
assert_no_secret_manifest = len([m for m in manifests if m.kind == "Secret"]) == 0
