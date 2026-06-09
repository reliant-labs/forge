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
_k8s = forge.K8sCluster {
    cluster = "k3d-example"
    namespace = "example-dev"
    registry = "localhost:5050"
    replicas = 1
    ports = [8080]
}

_bundle = forge.Bundle {
    services = [
        forge.Service {
            name = "workspace-proxy"
            image = "example"
            deploy = _k8s
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
            host = "workspaces.localhost"
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
}

_env = forge.RenderEnv {
    cluster = _k8s
    image_tag = forge.image_tag("dev")
    network_policies = False
}

manifests = forge.render_manifests(_bundle, _env)
