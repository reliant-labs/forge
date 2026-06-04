"""
Render the same Bundle as `main.k` but produce k8s manifest YAML
instead of the JSON contract. Run with:

    kcl run manifests.k

Outputs YAML by default (kcl's natural format), which is what
`kubectl apply -f` consumes.
"""

import forge

# Same Bundle as main.k. We duplicate it here rather than import
# from main.k because KCL's -S `output` selection only works against
# a single entrypoint file.
_bundle = forge.Bundle {
    services = [
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
        }
    ]
    cronjobs = [
        forge.CronJob {
            name = "billing-sweeper"
            schedule = "@hourly"
            image = "example"
            command = ["./example", "cron", "billing-sweep"]
        }
    ]
}

_env = forge.RenderEnv {
    namespace = "example-dev"
    image_registry = "localhost:5050"
    image_tag = "latest"
    network_policies = False
}

manifests = forge.render_manifests(_bundle, _env)
