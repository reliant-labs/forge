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

_bundle = forge.Bundle {
    services = [
        # Host-mode dev server with live-reload.
        #
        # env_vars carries reproducible per-env config (DATABASE_URL,
        # LOG_LEVEL, …). secrets_file points at a gitignored dotenv
        # for STRIPE_*, JWT_*, etc. forge run / forge up load
        # secrets_file first, then layer env_vars on top so KCL wins.
        forge.Service {
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
        # Cluster-deployed proxy with ingress.
        forge.Service {
            name = "workspace-proxy"
            image = "example"
            deploy = forge.K8sDeploy {
                replicas = 1
                ports = [8080]
                ingress = forge.Ingress {
                    host = "workspaces.localhost"
                    tls = False
                }
            }
        }
        # Build-only CLI with two ldflag variants.
        forge.Service {
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
}

output = forge.render(_bundle)
