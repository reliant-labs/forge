"""
Render contract — the secrets components in the render output.

forge.ExternalSecretsOperator lowers to one hardened helm chart (SF4) plus an
`external_secrets_operators` record the release shape projects. forge.OpenBao,
bootstrap_source and the store-backed forge.ExternalSecret are still
declarations only; this golden changes the day one of them is wired, which is
the point: that change is then a reviewed contract change.
"""

# kcl-args: env=staging

import forge

_hub = forge.ClusterTarget {cluster = "gke_acme_us-central1_prod", namespace = "acme-prod", platform = "amd64"}

output = forge.render(forge.Bundle {
    project = "secrets"
    cluster_target = _hub
    openbao = forge.OpenBao {}
    external_secrets_operators = [forge.ExternalSecretsOperator {cluster = _hub}]
    bootstrap_source = forge.GCPSecretManager {project = "acme-prod"}
    required_secrets = [
        forge.ExternalSecret {
            name = "app-db-role"
            namespace = "acme-prod"
            keys = ["password"]
            reason = "role the app dials the database as"
            source = forge.StoreSource {
                store = forge.OpenBaoStore {}
                data = {"password" = "APP_DB_ROLE_PASSWORD"}
                generate = {"APP_DB_ROLE_PASSWORD" = forge.GeneratedSecret {kind = "password"}}
            }
        }
    ]
})
