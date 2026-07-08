"""
Tiny example project illustrating every typed entity.

Run:

    cd kcl/example
    kcl run dev -S output --format json

…which should print the JSON contract shape documented at the top of
`kcl/schema.k`.
"""

import forge

# Top-level identifiers stay underscore-prefixed so KCL's default
# output projects only `output` and nothing else. The forge CLI then
# selects `-S output` for the clean shape.

# SCHEMA INHERITANCE — a project may declare `schema X(forge.RenderedWorkload)` to
# add its OWN typed/required fields while forge renders the result exactly
# like a plain forge.RenderedWorkload. The render layer is typed on the base schema,
# so the subtype passes through and projects the same JSON contract; the
# extra fields ride on the typed value but aren't part of the contract.
schema TenantService(forge.RenderedWorkload):
    # An extra REQUIRED field (no default) — enforced at parse time.
    region: str
    tier: "free" | "pro" = "free"

    check:
        region, "TenantService.region is required"

_bundle = forge.Bundle {
    services = [
        # An app schema inheriting forge.RenderedWorkload — renders identically to a
        # plain forge.RenderedWorkload (Deployment + Service + RBAC), plus enforces
        # its own required `region` at parse time.
        TenantService {
            name = "tenant-api"
            image = "example"
            region = "us-east-1"
            tier = "pro"
            deploy = forge.K8sCluster {
                cluster = "k3d-example"
                namespace = "example-dev"
                registry = "localhost:5050"
                ports = [8081]
            }
        }
        # Host-mode dev server with live-reload.
        #
        # env_vars carries reproducible per-env config (DATABASE_URL,
        # LOG_LEVEL, …). secrets_file points at a gitignored dotenv
        # for STRIPE_*, JWT_*, etc. forge run / forge up load
        # secrets_file first, then layer env_vars on top so KCL wins.
        forge.RenderedWorkload {
            name = "admin-server"
            image = "example"
            deploy = forge.HostDeploy {
                runner = "air"
                air_config = ".air.toml"
                env_vars = [
                    forge.EnvVar { name = "LOG_LEVEL", value = "debug" }
                    forge.EnvVar { name = "DATABASE_URL", value = "postgres://localhost:5432/example_dev?sslmode=disable" }
                ]
                secrets_file = ".env.dev.secrets"
            }
        }
        # Cluster-deployed proxy. Routed via Gateway API — see the
        # `gateways` and `http_routes` blocks below.
        forge.RenderedWorkload {
            name = "workspace-proxy"
            image = "example"
            deploy = forge.K8sCluster {
                cluster = "k3d-example"
                namespace = "example-dev"
                registry = "localhost:5050"
                replicas = 1
                ports = [8080]
            }
        }
        # Build-only CLI with two ldflag variants.
        forge.RenderedWorkload {
            name = "reliant-daemon"
            deploy = forge.BuildOnly {
                build_variants = [
                    forge.BuildVariant {
                        name = "dev"
                        ldflags = ["-X main.Env=dev"]
                    }
                    forge.BuildVariant {
                        name = "prod"
                        ldflags = ["-X main.Env=prod"]
                    }
                ]
            }
        }
        # Build-only IMAGE via `deploy = None`: forge builds + pushes the
        # image but renders no Deployment/Service. Use it for a base image
        # other workloads reference at runtime (e.g. per-user pods pull it).
        forge.RenderedWorkload {
            name = "workspace-base"
            image = "workspace-base"
            deploy = None
            build = forge.DockerBuild {
                dockerfile = "docker/Dockerfile.workspace-base"
            }
        }
    ]
    operators = [
        forge.Operator {
            name = "workspace-controller"
            image = "example"
            crds = ["Workspace"]
            cluster_rbac = forge.ClusterRBAC {
                rules = [
                    {
                        apiGroups = ["forge.io"]
                        resources = ["workspaces"]
                        verbs = ["get", "list", "watch", "update", "patch"]
                    }
                ]
            }
            leader_election = True
        }
    ]
    frontends = [
        forge.Frontend {
            name = "admin-web"
            type = "nextjs"
            path = "frontends/admin-web"
            dev_runner = "npm"
        }
    ]
    cronjobs = [
        # Scheduled CronJob.
        forge.CronJob {
            name = "billing-sweeper"
            schedule = "@hourly"
            image = "example"
            command = ["./example", "cron", "billing-sweep"]
        }
        # One-shot Job (schedule omitted).
        forge.CronJob {
            name = "db-bootstrap"
            image = "example"
            command = ["./example", "migrate", "up"]
        }
    ]
    # Gateway API ingress topology. In a real project this block lives
    # in deploy/kcl/ingress.k and per-env files re-export with
    # overrides; inlined here for the single-file example.
    gateways = [
        forge.Gateway {
            name = "public"
            listeners = [
                forge.GatewayListener {
                    name = "http"
                    port = 18080
                    protocol = "HTTP"
                }
            ]
        }
    ]
    http_routes = [
        forge.HTTPRoute {
            name = "workspace-proxy"
            gateway = "public"
            listener = "http"
            service = "workspace-proxy"
            port = 8080
        }
    ]
    # DECLARED external PREREQUISITES — out-of-band facts this env depends
    # on that forge does NOT create. Modeling them (vs a docstring) makes
    # them a render-time checklist + a deploy preflight (a declared-required
    # ExternalSecret that's absent on the live target BLOCKS).
    required_secrets = [
        # cert-manager's DNS-01 ClusterIssuer reads this Secret out-of-band
        # in the cert-manager namespace; its absence hangs ACME silently.
        forge.ExternalSecret {
            name = "cloudflare-api-token"
            namespace = "cert-manager"
            keys = ["api-token"]
            reason = "cert-manager DNS-01 Cloudflare API token"
        }
    ]
    required_dns = [
        # The load-bearing wildcard — its omission hangs both the DNS-01
        # wildcard cert and all workspace-proxy traffic. Target is an
        # ephemeral LB IP, unknown at author time.
        forge.DNSRecord {
            host = "*.workspaces.example.com"
            reason = "DNS-01 wildcard cert + workspace-proxy traffic"
        }
    ]
}

output = forge.render(_bundle)
